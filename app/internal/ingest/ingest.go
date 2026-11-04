// Package ingest turns uploaded documents into knowledge the models look up
// (see package lore):
//
//	extract pages ─┬─ text pages ──────────────────────────┐
//	               └─ image pages ─┬─ vision, page by page ─┤→ text.json → raw chunks
//	                               └─ CLIP (optional) ──────→ page image vectors
//
// Once every queued document of the work is read and embedded, the story map
// is built in one pass in story order (summarize.go): chapters, each read in
// windows that fit the model's context, give one short map entry each, and
// the characters and places met on the way give merged profiles.
package ingest

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"immersive/internal/llm"
	"immersive/internal/lore"
	"immersive/internal/prompts"
	"immersive/internal/rag"
	"immersive/internal/store"
	"immersive/internal/worker"
)

type Options struct {
	ChapterChars  int  // longest stretch of text one story-map entry covers
	ChunkChars    int  // raw chunk size
	KeepRaw       bool // also embed original text, not just summaries
	Concurrency   int  // parallel LLM calls
	VisionMaxSide int  // longest side, in pixels, of a page image sent to the vision model
}

type Service struct {
	st       *store.Store
	w        *worker.Manager
	rag      *rag.Store
	lore     *lore.Lore // map, profiles and lookups (its vectors are s.rag; swapped in tests)
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
	s.lore = &lore.Lore{St: st, Vec: r, Spaces: spaces, PagesDir: pagesDir}
	go s.loop()
	// Works left waiting for their summary when the app closed.
	if ids, err := st.PendingSummaryKBs(); err == nil {
		for _, id := range ids {
			s.queue <- "kb:" + id
		}
	}
	return s
}

// AddSources copies the files into app storage, in story order (natural file
// name order: "Chương 2" before "Chương 10"), then queues them. They are all
// recorded before any is read, so the whole batch is summarized in one pass.
func (s *Service) AddSources(kbID string, paths []string, comicMode string) ([]store.Source, error) {
	paths = slices.Clone(paths)
	slices.SortStableFunc(paths, func(a, b string) int { return naturalCompare(filepath.Base(a), filepath.Base(b)) })
	var out []store.Source
	for _, p := range paths {
		src, err := s.addSource(kbID, p, comicMode)
		if err != nil {
			return out, fmt.Errorf("%s: %w", p, err)
		}
		out = append(out, src)
	}
	for _, src := range out {
		s.queue <- src.ID
	}
	return out, nil
}

// addSource copies one file into app storage and records it, without queueing it.
func (s *Service) addSource(kbID, path, comicMode string) (store.Source, error) {
	if comicMode == "" {
		comicMode = "vision"
	}
	src := store.Source{ID: store.NewID(), KBID: kbID, Filename: filepath.Base(path), ComicMode: comicMode, Status: "queued"}
	src.Path = filepath.Join(s.srcDir, src.ID+strings.ToLower(filepath.Ext(path)))
	if err := copyFile(path, src.Path); err != nil {
		return src, err
	}
	return src, s.st.InsertSource(&src)
}

// naturalCompare orders strings with digit runs compared as numbers.
func naturalCompare(a, b string) int {
	for a != "" && b != "" {
		da, db := digitPrefix(a), digitPrefix(b)
		if da != "" && db != "" {
			na, nb := strings.TrimLeft(da, "0"), strings.TrimLeft(db, "0")
			if c := cmp.Compare(len(na), len(nb)); c != 0 {
				return c
			}
			if c := strings.Compare(na, nb); c != 0 {
				return c
			}
			a, b = a[len(da):], b[len(db):]
			continue
		}
		ra, sa := utf8.DecodeRuneInString(a)
		rb, sb := utf8.DecodeRuneInString(b)
		if c := cmp.Compare(unicode.ToLower(ra), unicode.ToLower(rb)); c != 0 {
			return c
		}
		a, b = a[sa:], b[sb:]
	}
	return cmp.Compare(len(a), len(b))
}

func digitPrefix(s string) string {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return s[:i]
}

// Reprocess clears a source's vectors and queues it again.
func (s *Service) Reprocess(ctx context.Context, id string) error {
	if err := s.lore.Vec.Delete(ctx, rag.Filter{Source: id}); err != nil {
		return err
	}
	s.st.SetChapters(id, nil) // its map entries are rebuilt once it's read again
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
	// A summary in progress included this document: stop it, it restarts without it.
	s.mu.Lock()
	if c := s.cancels["kb:"+src.KBID]; c != nil {
		c()
	}
	s.mu.Unlock()
	if err := s.lore.Vec.Delete(ctx, rag.Filter{Source: id}); err != nil {
		return err
	}
	os.RemoveAll(filepath.Join(s.pagesDir, id))
	os.Remove(src.Path)
	return s.st.DeleteSource(id)
}

// loop works through the queue: source ids to read and embed, and "kb:<id>"
// (or "kb!:<id>" to start over) to summarize a work.
func (s *Service) loop() {
	for id := range s.queue {
		if kbID, ok := strings.CutPrefix(id, "kb:"); ok {
			s.runKB(kbID, false)
			continue
		}
		if kbID, ok := strings.CutPrefix(id, "kb!:"); ok {
			s.runKB(kbID, true)
			continue
		}
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
		// Once the work has nothing left to read, summarize it in one pass.
		if src, err := s.st.Source(id); err == nil {
			s.runKB(src.KBID, false)
		}
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

// process is the reading step for one document: extract its pages and have
// the vision model read every comic page. Nothing is embedded yet: embedding
// waits until every queued document of the work has been read (embedKB), and
// the summary comes after that (summarizeKB).
func (s *Service) process(ctx context.Context, id string) error {
	src, err := s.st.Source(id)
	if err != nil {
		return err
	}
	kb, err := s.st.KnowledgeBase(src.KBID)
	if err != nil {
		return err
	}
	opts := s.opts()

	// 1. Extract
	s.status(id, "extracting", "Đang đọc tài liệu…", "")
	var ex struct {
		Pages []page `json:"pages"`
	}
	err = s.w.Call(ctx, "doc.extract", map[string]any{
		"path": src.Path, "out_dir": filepath.Join(s.pagesDir, id), "min_chars": 80, "max_side": max(opts.VisionMaxSide, 512),
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

	// 2. Comic pages: the vision model reads every one of them, one per call.
	mode := src.ComicMode
	if len(imagePages) > 0 && (mode == "vision" || mode == "both") {
		described, err := s.describePages(ctx, id, kb.VisionProvider, kb.Name, imagePages, opts)
		if err != nil {
			return err
		}
		units = append(units, described...)
		sortUnits(units)
	}
	var clip []page // pages to embed as images (no tokens), later with the rest
	if mode == "embed" || mode == "both" {
		clip = imagePages
	}
	if len(units) == 0 && len(clip) == 0 {
		return fmt.Errorf("không trích xuất được nội dung nào từ tài liệu")
	}

	// Keep what was read for the embedding and summary steps.
	if err := saveJSON(s.textPath(id), units); err != nil {
		return err
	}
	if err := saveJSON(s.imagesPath(id), clip); err != nil {
		return err
	}
	s.status(id, "read", fmt.Sprintf("%d trang · đã đọc, chờ embedding cùng cả bộ truyện", len(ex.Pages)), "")
	return nil
}

// embedSource embeds one read document: its text chunks, and its comic pages
// as images when that mode is on. A document with no text is then done.
func (s *Service) embedSource(ctx context.Context, src store.Source, sp rag.Space, opts Options) (hasText bool, err error) {
	units, err := loadUnits(s.textPath(src.ID))
	if err != nil {
		return false, err
	}
	var clip []page
	if b, err := os.ReadFile(s.imagesPath(src.ID)); err == nil {
		json.Unmarshal(b, &clip)
	}
	if len(clip) > 0 {
		s.status(src.ID, "embedding", fmt.Sprintf("Embedding %d trang ảnh…", len(clip)), "")
		docs := make([]rag.ImageDoc, len(clip))
		for i, p := range clip {
			docs[i] = rag.ImageDoc{ID: fmt.Sprintf("%s:img:%d", src.ID, p.Index), ImagePath: p.ImagePath,
				Metadata: meta(src, rag.LevelRaw, strconv.Itoa(p.Index+1), "")}
		}
		if err := s.rag.AddImages(ctx, docs); err != nil {
			return false, err
		}
	}
	if len(units) > 0 && opts.KeepRaw {
		chunks := pack(paragraphs(units), opts.ChunkChars, opts.ChunkChars/8)
		s.status(src.ID, "embedding", fmt.Sprintf("Embedding %d đoạn…", len(chunks)), "")
		docs := make([]rag.Doc, len(chunks))
		for i, c := range chunks {
			docs[i] = rag.Doc{ID: fmt.Sprintf("%s:raw:%d", src.ID, i), Text: c.Text, Metadata: meta(src, rag.LevelRaw, c.pages(), "")}
		}
		if err := s.lore.Vec.AddTexts(ctx, sp, docs); err != nil {
			return false, err
		}
	}
	return len(units) > 0, nil
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

// textPath is where a source's text is kept: for the embedding and map
// steps, and for read_chapter.
func (s *Service) textPath(id string) string {
	return lore.TextPath(s.pagesDir, id)
}

// imagesPath lists the comic pages of a source to embed as images.
func (s *Service) imagesPath(id string) string {
	return filepath.Join(s.pagesDir, id, "images.json")
}

func saveJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func loadUnits(path string) ([]unit, error) {
	return lore.LoadPages(path)
}

type sectionResult struct {
	Summary  string `json:"summary"`
	Entities []struct {
		Name        string `json:"name"`
		Type        string `json:"type"`
		Description string `json:"description"`
	} `json:"entities"`
}

// describePages has the vision model transcribe comic pages, one page per
// call (a page always fits the context; many run in parallel), with thinking
// off: a transcription needs eyes, not reasoning.
func (s *Service) describePages(ctx context.Context, id, providerID, title string, pages []page, opts Options) ([]unit, error) {
	vision, err := s.resolve(llm.RoleVision, providerID)
	if err != nil {
		return nil, err
	}
	if !vision.SupportsVision() {
		return nil, fmt.Errorf("provider cho vai trò 'vision' chưa bật hỗ trợ ảnh")
	}
	maxTokens, _ := llm.Budget(vision, 4000)
	out := make([]unit, len(pages))
	var done int
	var mu sync.Mutex
	err = parallel(ctx, len(pages), opts.Concurrency, func(i int) error {
		p := pages[i]
		data, err := os.ReadFile(p.ImagePath)
		if err != nil {
			return err
		}
		n := strconv.Itoa(p.Index + 1)
		text, err := llm.Complete(ctx, vision, llm.Request{Purpose: fmt.Sprintf("đọc truyện tranh %s · trang %s", title, n), MaxTokens: maxTokens,
			Messages: []llm.Message{{Role: llm.RoleUser, Parts: []llm.Part{
				{Text: prompts.Get("vision", "page", n, "title", title)},
				{ImageB64: base64.StdEncoding.EncodeToString(data), ImageMIME: mimeOf(p.ImagePath)},
			}}}})
		if err != nil {
			return fmt.Errorf("đọc trang %s: %w", n, err)
		}
		out[i] = unit{Page: p.Index + 1, Text: strings.TrimSpace(text)}
		mu.Lock()
		done++
		s.status(id, "reading", fmt.Sprintf("Đọc trang truyện tranh %d/%d…", done, len(pages)), "")
		mu.Unlock()
		return nil
	})
	if err != nil {
		return nil, err
	}
	units := out[:0]
	for _, u := range out {
		if u.Text != "" {
			units = append(units, u)
		}
	}
	return units, nil
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
