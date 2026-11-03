// Package ingest turns uploaded documents into RAG knowledge:
//
//	extract pages ─┬─ text pages ──────────────┐
//	               └─ image pages ─┬─ vision ──┤→ raw chunks ─→ section summaries + entities ─→ overview
//	                               └─ embed ───→ page image vectors (no LLM tokens)
//
// Everything is embedded and stored in the vector store; nothing is kept in the model's context.
package ingest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"immersive/internal/llm"
	"immersive/internal/rag"
	"immersive/internal/store"
	"immersive/internal/worker"
)

type Options struct {
	SectionChars int  // text per summarization call
	ChunkChars   int  // raw chunk size
	KeepRaw      bool // also embed original text, not just summaries
	Concurrency  int  // parallel LLM calls
	VisionBatch  int  // pages per vision call
}

type Service struct {
	st       *store.Store
	w        *worker.Manager
	rag      *rag.Store
	lookup   lookup // what generation reads from the vector store (s.rag; swapped in tests)
	spaces   rag.Spaces
	resolve  llm.Resolver
	opts     func() Options
	pagesDir string
	srcDir   string
	emit     func(event string, data any)

	queue   chan string
	mu      sync.Mutex
	cancels map[string]context.CancelFunc
}

func New(st *store.Store, w *worker.Manager, r *rag.Store, spaces rag.Spaces, resolve llm.Resolver, opts func() Options,
	srcDir, pagesDir string, emit func(string, any)) *Service {
	s := &Service{st: st, w: w, rag: r, spaces: spaces, resolve: resolve, opts: opts, srcDir: srcDir, pagesDir: pagesDir,
		emit: emit, queue: make(chan string, 256), cancels: map[string]context.CancelFunc{}}
	s.lookup = r
	go s.loop()
	return s
}

// AddSource copies the file into app storage and queues it for processing.
func (s *Service) AddSource(kbID, path, comicMode string) (store.Source, error) {
	if comicMode == "" {
		comicMode = "vision"
	}
	src := store.Source{ID: store.NewID(), KBID: kbID, Filename: filepath.Base(path), ComicMode: comicMode, Status: "queued"}
	src.Path = filepath.Join(s.srcDir, src.ID+strings.ToLower(filepath.Ext(path)))
	if err := copyFile(path, src.Path); err != nil {
		return src, err
	}
	if err := s.st.InsertSource(&src); err != nil {
		return src, err
	}
	s.queue <- src.ID
	return src, nil
}

// Reprocess clears a source's vectors and queues it again.
func (s *Service) Reprocess(ctx context.Context, id string) error {
	if err := s.rag.Delete(ctx, rag.Filter{Source: id}); err != nil {
		return err
	}
	if err := s.st.UpdateSourceStatus(id, "queued", "", ""); err != nil {
		return err
	}
	s.queue <- id
	return nil
}

func (s *Service) DeleteSource(ctx context.Context, id string) error {
	s.mu.Lock()
	if c := s.cancels[id]; c != nil {
		c()
	}
	s.mu.Unlock()
	src, err := s.st.Source(id)
	if err != nil {
		return err
	}
	if err := s.rag.Delete(ctx, rag.Filter{Source: id}); err != nil {
		return err
	}
	os.RemoveAll(filepath.Join(s.pagesDir, id))
	os.Remove(src.Path)
	return s.st.DeleteSource(id)
}

func (s *Service) loop() {
	for id := range s.queue {
		ctx, cancel := context.WithCancel(context.Background())
		s.mu.Lock()
		s.cancels[id] = cancel
		s.mu.Unlock()

		if err := s.process(ctx, id); err != nil {
			log.Printf("ingest %s: %v", id, err)
			s.status(id, "error", "", err.Error())
		}

		cancel()
		s.mu.Lock()
		delete(s.cancels, id)
		s.mu.Unlock()
	}
}

func (s *Service) status(id, status, progress, errMsg string) {
	if errMsg != "" {
		log.Printf("ingest %s: %s — %s — LỖI: %s", id, status, progress, errMsg)
	} else {
		log.Printf("ingest %s: %s — %s", id, status, progress)
	}
	s.st.UpdateSourceStatus(id, status, progress, errMsg)
	if src, err := s.st.Source(id); err == nil {
		s.emit("ingest:progress", src)
	}
}

type page struct {
	Index     int    `json:"index"` // 0-based
	Kind      string `json:"kind"`  // text | image
	Text      string `json:"text"`
	ImagePath string `json:"image_path"`
}

func (s *Service) process(ctx context.Context, id string) error {
	src, err := s.st.Source(id)
	if err != nil {
		return err
	}
	kb, err := s.st.KnowledgeBase(src.KBID)
	if err != nil {
		return err
	}
	sp, err := s.spaces(kb.ID)
	if err != nil {
		return err
	}
	opts := s.opts()
	title := kb.Name

	// 1. Extract
	s.status(id, "extracting", "Đang đọc tài liệu…", "")
	var ex struct {
		Pages []page `json:"pages"`
	}
	err = s.w.Call(ctx, "doc.extract", map[string]any{
		"path": src.Path, "out_dir": filepath.Join(s.pagesDir, id), "min_chars": 80, "max_side": 1400,
	}, &ex)
	if err != nil {
		return err
	}
	s.st.SetSourcePages(id, len(ex.Pages))

	var units []unit
	var imagePages []page
	for _, p := range ex.Pages {
		if p.Kind == "image" {
			imagePages = append(imagePages, p)
		} else if strings.TrimSpace(p.Text) != "" {
			units = append(units, unit{Page: p.Index + 1, Text: p.Text})
		}
	}

	// 2. Comic pages
	mode := src.ComicMode
	if len(imagePages) > 0 && (mode == "embed" || mode == "both") {
		s.status(id, "embedding", fmt.Sprintf("Embedding %d trang ảnh…", len(imagePages)), "")
		docs := make([]rag.ImageDoc, len(imagePages))
		for i, p := range imagePages {
			docs[i] = rag.ImageDoc{ID: fmt.Sprintf("%s:img:%d", id, p.Index), ImagePath: p.ImagePath,
				Metadata: meta(src, rag.LevelRaw, strconv.Itoa(p.Index+1), "")}
		}
		if err := s.rag.AddImages(ctx, docs); err != nil {
			return err
		}
	}
	if len(imagePages) > 0 && (mode == "vision" || mode == "both") {
		described, err := s.describePages(ctx, id, kb.VisionProvider, title, imagePages, opts)
		if err != nil {
			return err
		}
		units = append(units, described...)
		sortUnits(units)
	}

	if len(units) == 0 {
		if len(imagePages) > 0 {
			s.status(id, "done", fmt.Sprintf("Đã embed %d trang ảnh (không có văn bản để tóm tắt)", len(imagePages)), "")
			return nil
		}
		return fmt.Errorf("không trích xuất được nội dung nào từ tài liệu")
	}

	paras := paragraphs(units)

	// 3. Raw chunks
	if opts.KeepRaw {
		chunks := pack(paras, opts.ChunkChars, opts.ChunkChars/8)
		s.status(id, "embedding", fmt.Sprintf("Embedding %d đoạn gốc…", len(chunks)), "")
		docs := make([]rag.Doc, len(chunks))
		for i, c := range chunks {
			docs[i] = rag.Doc{ID: fmt.Sprintf("%s:raw:%d", id, i), Text: c.Text, Metadata: meta(src, rag.LevelRaw, c.pages(), "")}
		}
		if err := s.rag.AddTexts(ctx, sp, docs); err != nil {
			return err
		}
	}

	// 4. Section summaries + entities
	summarizer, err := s.resolve(llm.RoleSummarize, kb.LLMProvider)
	if err != nil {
		return err
	}
	sections := pack(paras, opts.SectionChars, 0)
	results := make([]sectionResult, len(sections))
	var done int
	var mu sync.Mutex
	err = parallel(ctx, len(sections), opts.Concurrency, func(i int) error {
		r, err := s.summarizeSection(ctx, summarizer, title, i, len(sections), sections[i].Text)
		if err != nil {
			return fmt.Errorf("tóm tắt phần %d: %w", i+1, err)
		}
		results[i] = r
		mu.Lock()
		done++
		s.status(id, "summarizing", fmt.Sprintf("Tóm tắt %d/%d phần…", done, len(sections)), "")
		mu.Unlock()
		return nil
	})
	if err != nil {
		return err
	}

	var docs []rag.Doc
	summaries := make([]string, len(sections))
	for i, r := range results {
		summaries[i] = fmt.Sprintf("(Phần %d, tr. %s) %s", i+1, sections[i].pages(), r.Summary)
		docs = append(docs, rag.Doc{ID: fmt.Sprintf("%s:sec:%d", id, i), Text: r.Summary,
			Metadata: meta(src, rag.LevelSection, sections[i].pages(), "")})
	}

	// 5. Entity profiles, merged across sections
	s.status(id, "summarizing", "Tổng hợp hồ sơ nhân vật & địa danh…", "")
	entities := mergeEntities(results)
	err = parallel(ctx, len(entities), opts.Concurrency, func(i int) error {
		e := entities[i]
		if len(e.notes) > 1 && totalLen(e.notes) > 1200 {
			merged, err := llm.Complete(ctx, summarizer, llm.Request{Purpose: "gộp hồ sơ " + e.Name, System: analystSystem, MaxTokens: 6000,
				Messages: []llm.Message{llm.TextMessage(llm.RoleUser,
					fmt.Sprintf(entityMergePrompt, e.Name, e.Type, title, strings.Join(e.notes, "\n---\n")))}})
			if err != nil {
				return err
			}
			e.profile = strings.TrimSpace(merged)
		} else {
			e.profile = strings.Join(e.notes, "\n")
		}
		return nil
	})
	if err != nil {
		return err
	}
	for i, e := range entities {
		docs = append(docs, rag.Doc{ID: fmt.Sprintf("%s:ent:%d", id, i),
			Text:     fmt.Sprintf("%s (%s): %s", e.Name, e.Type, e.profile),
			Metadata: meta(src, rag.LevelEntity, "", e.Name)})
	}

	// 6. Overview (hierarchical reduce)
	s.status(id, "summarizing", "Viết tổng quan toàn bộ tác phẩm…", "")
	overview, err := s.reduce(ctx, summarizer, title, summaries)
	if err != nil {
		return err
	}
	docs = append(docs, rag.Doc{ID: id + ":overview", Text: overview, Metadata: meta(src, rag.LevelOverview, "", "")})

	s.status(id, "embedding", fmt.Sprintf("Embedding %d tóm tắt & hồ sơ…", len(docs)), "")
	if err := s.rag.AddTexts(ctx, sp, docs); err != nil {
		return err
	}
	s.refreshKBOverview(src.KBID, sp, id, overview)

	s.status(id, "done", fmt.Sprintf("%d trang · %d phần · %d hồ sơ", len(ex.Pages), len(sections), len(entities)), "")
	return nil
}

func meta(src store.Source, level, pages, name string) map[string]any {
	m := map[string]any{"kb_id": src.KBID, "source_id": src.ID, "level": level}
	if pages != "" {
		m["pages"] = pages
	}
	if name != "" {
		m["name"] = name
	}
	return m
}

// refreshKBOverview keeps the KB's display overview in sync. With several
// sources it simply concatenates per-source overviews.
func (s *Service) refreshKBOverview(kbID string, sp rag.Space, sourceID, overview string) {
	srcs, _ := s.st.Sources(kbID)
	if len(srcs) <= 1 {
		s.st.SetKBOverview(kbID, overview)
		return
	}
	hits, err := s.rag.GetTexts(context.Background(), sp, rag.Filter{KBIDs: []string{kbID}, Levels: []string{rag.LevelOverview}}, 50)
	if err != nil {
		return
	}
	parts := []string{overview}
	for _, h := range hits {
		if h.Str("source_id") != sourceID {
			parts = append(parts, h.Text)
		}
	}
	s.st.SetKBOverview(kbID, strings.Join(parts, "\n\n---\n\n"))
}

type sectionResult struct {
	Summary  string `json:"summary"`
	Entities []struct {
		Name        string `json:"name"`
		Type        string `json:"type"`
		Description string `json:"description"`
	} `json:"entities"`
}

func (s *Service) summarizeSection(ctx context.Context, p llm.Provider, title string, i, n int, text string) (sectionResult, error) {
	out, err := llm.Complete(ctx, p, llm.Request{Purpose: fmt.Sprintf("tóm tắt %s · phần %d/%d", title, i+1, n), System: analystSystem, MaxTokens: 10000,
		Messages: []llm.Message{llm.TextMessage(llm.RoleUser, fmt.Sprintf(sectionPrompt, i+1, n, title, text))}})
	if err != nil {
		return sectionResult{}, err
	}
	var r sectionResult
	if err := ParseJSON(out, &r); err != nil || r.Summary == "" {
		r = sectionResult{Summary: strings.TrimSpace(out)} // model ignored the format; keep the prose
	}
	return r, nil
}

// reduce collapses section summaries into one overview, in rounds if needed.
func (s *Service) reduce(ctx context.Context, p llm.Provider, title string, summaries []string) (string, error) {
	const budget = 60000
	for {
		groups := groupByLen(summaries, budget)
		next := make([]string, len(groups))
		err := parallel(ctx, len(groups), 3, func(i int) error {
			out, err := llm.Complete(ctx, p, llm.Request{Purpose: "tổng quan " + title, System: analystSystem, MaxTokens: 10000,
				Messages: []llm.Message{llm.TextMessage(llm.RoleUser, fmt.Sprintf(reducePrompt, title, strings.Join(groups[i], "\n\n")))}})
			next[i] = strings.TrimSpace(out)
			return err
		})
		if err != nil {
			return "", err
		}
		if len(next) == 1 {
			return next[0], nil
		}
		summaries = next
	}
}

var pageMarker = regexp.MustCompile(`(?m)^\s*=+\s*PAGE\s+(\d+)\s*=+\s*$`)

// describePages sends comic pages to the vision model in small batches and
// turns its transcription into text units (one-time token cost per page).
func (s *Service) describePages(ctx context.Context, id, providerID, title string, pages []page, opts Options) ([]unit, error) {
	vision, err := s.resolve(llm.RoleVision, providerID)
	if err != nil {
		return nil, err
	}
	if !vision.SupportsVision() {
		return nil, fmt.Errorf("provider cho vai trò 'vision' chưa bật hỗ trợ ảnh")
	}
	batch := max(opts.VisionBatch, 1)
	nb := (len(pages) + batch - 1) / batch
	out := make([][]unit, nb)
	var done int
	var mu sync.Mutex
	err = parallel(ctx, nb, opts.Concurrency, func(b int) error {
		group := pages[b*batch : min((b+1)*batch, len(pages))]
		first, last := group[0].Index+1, group[len(group)-1].Index+1
		parts := []llm.Part{{Text: fmt.Sprintf(visionPrompt, first, last, title)}}
		for _, p := range group {
			data, err := os.ReadFile(p.ImagePath)
			if err != nil {
				return err
			}
			parts = append(parts, llm.Part{Text: fmt.Sprintf("Page %d:", p.Index+1)},
				llm.Part{ImageB64: base64.StdEncoding.EncodeToString(data), ImageMIME: mimeOf(p.ImagePath)})
		}
		text, err := llm.Complete(ctx, vision, llm.Request{Purpose: fmt.Sprintf("đọc truyện tranh %s · trang %d-%d", title, first, last), MaxTokens: 12000, Messages: []llm.Message{{Role: llm.RoleUser, Parts: parts}}})
		if err != nil {
			return fmt.Errorf("đọc trang %d-%d: %w", first, last, err)
		}
		out[b] = splitPages(text, group)
		mu.Lock()
		done += len(group)
		s.status(id, "reading", fmt.Sprintf("Đọc trang truyện tranh %d/%d…", done, len(pages)), "")
		mu.Unlock()
		return nil
	})
	if err != nil {
		return nil, err
	}
	var units []unit
	for _, u := range out {
		units = append(units, u...)
	}
	return units, nil
}

func splitPages(text string, group []page) []unit {
	locs := pageMarker.FindAllStringSubmatchIndex(text, -1)
	if len(locs) == 0 {
		return []unit{{Page: group[0].Index + 1, Text: strings.TrimSpace(text)}}
	}
	var units []unit
	for i, loc := range locs {
		n, _ := strconv.Atoi(text[loc[2]:loc[3]])
		end := len(text)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		body := strings.TrimSpace(text[loc[1]:end])
		if body != "" {
			units = append(units, unit{Page: n, Text: body})
		}
	}
	return units
}

// ---- helpers ----

type entity struct {
	Name, Type string
	notes      []string
	profile    string
}

func mergeEntities(results []sectionResult) []*entity {
	byKey := map[string]*entity{}
	var order []*entity
	for _, r := range results {
		for _, e := range r.Entities {
			name := strings.TrimSpace(e.Name)
			if name == "" || strings.TrimSpace(e.Description) == "" {
				continue
			}
			key := strings.ToLower(name)
			ent := byKey[key]
			if ent == nil {
				ent = &entity{Name: name, Type: e.Type}
				byKey[key] = ent
				order = append(order, ent)
			}
			ent.notes = append(ent.notes, strings.TrimSpace(e.Description))
		}
	}
	return order
}

func totalLen(ss []string) int {
	n := 0
	for _, s := range ss {
		n += len(s)
	}
	return n
}

func groupByLen(items []string, budget int) [][]string {
	var groups [][]string
	var cur []string
	n := 0
	for _, it := range items {
		if n+len(it) > budget && len(cur) > 0 {
			groups = append(groups, cur)
			cur, n = nil, 0
		}
		cur = append(cur, it)
		n += len(it)
	}
	if len(cur) > 0 {
		groups = append(groups, cur)
	}
	return groups
}

func sortUnits(units []unit) {
	for i := 1; i < len(units); i++ {
		for j := i; j > 0 && units[j].Page < units[j-1].Page; j-- {
			units[j], units[j-1] = units[j-1], units[j]
		}
	}
}

// parallel runs fn(0..n-1) with at most limit concurrent calls, stopping at the first error.
func parallel(ctx context.Context, n, limit int, fn func(i int) error) error {
	if limit < 1 {
		limit = 1
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error
	for i := 0; i < n; i++ {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := fn(i); err != nil {
				once.Do(func() { firstErr = err; cancel() })
			}
		}(i)
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}

// ParseJSON extracts the first JSON object from a model reply (tolerates code fences / chatter).
func ParseJSON(s string, v any) error {
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return fmt.Errorf("no JSON object in reply")
	}
	return json.Unmarshal([]byte(s[start:end+1]), v)
}

func mimeOf(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	}
	return "image/png"
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(to)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
