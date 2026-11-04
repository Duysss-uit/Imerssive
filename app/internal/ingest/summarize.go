package ingest

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	"immersive/internal/llm"
	"immersive/internal/lore"
	"immersive/internal/prompts"
	"immersive/internal/rag"
	"immersive/internal/store"
)

// A work goes through three steps, each finished for every queued document
// before the next starts:
//
//  1. read (process): extract pages; the vision model transcribes every comic
//     page, one page per call.
//  2. embed (embedKB): embed all that was read, text and page images.
//  3. map (summarizeKB): the story map, in story order. Each chapter is read
//     in windows sized to the model's context, its summary updated window by
//     window, so no call ever holds a whole chapter. The characters and places
//     met give one merged profile each. There is no summary of the whole work:
//     the models read the map and look the details up (package lore).
//
// Documents added later go through the same steps and continue the map: their
// chapters are read with the map so far as context, and their notes are
// merged into the existing profiles.

// reading reports whether a knowledge base still has documents to read.
func (s *Service) reading(kbID string) bool {
	srcs, _ := s.st.Sources(kbID)
	for _, src := range srcs {
		switch src.Status {
		case "queued", "extracting", "reading":
			return true
		}
	}
	return false
}

// Resummarize summarizes a knowledge base again from its first part, e.g. to
// merge what older versions summarized document by document.
func (s *Service) Resummarize(kbID string) error {
	if s.reading(kbID) {
		return fmt.Errorf("đang đọc tài liệu — đợi xong rồi tóm tắt lại")
	}
	// Documents that failed after being read join in again.
	srcs, _ := s.st.Sources(kbID)
	for _, src := range srcs {
		if _, err := os.Stat(s.textPath(src.ID)); src.Status == "error" && err == nil {
			s.status(src.ID, "read", "Chờ embedding cùng cả bộ truyện", "")
		}
	}
	s.queue <- "kb!:" + kbID
	return nil
}

// runKB embeds and then summarizes kbID once nothing is left to read,
// reporting failures on the documents involved.
func (s *Service) runKB(kbID string, fromStart bool) {
	if s.reading(kbID) {
		return // the last document to finish reading starts the next steps
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.cancels["kb:"+kbID] = cancel
	s.mu.Unlock()
	defer func() {
		cancel()
		s.mu.Lock()
		delete(s.cancels, "kb:"+kbID)
		s.mu.Unlock()
	}()
	err := s.embedKB(ctx, kbID)
	if err == nil {
		if s.reading(kbID) {
			return // more documents arrived meanwhile: they'll restart the steps
		}
		err = s.summarizeKB(ctx, kbID, fromStart)
	}
	if err == nil {
		return
	}
	log.Printf("ingest: xử lý bộ truyện %s: %v", kbID, err)
	canceled := ctx.Err() != nil // a document was deleted meanwhile
	back := map[string]string{"embedding": "read", "summarizing": "embedded"}
	srcs, _ := s.st.Sources(kbID)
	for _, src := range srcs {
		prev, ok := back[src.Status]
		switch {
		case !ok:
		case canceled:
			s.status(src.ID, prev, "Chờ xử lý tiếp cùng cả bộ truyện", "")
		default:
			s.status(src.ID, "error", "", err.Error())
		}
	}
	if canceled {
		s.queue <- "kb:" + kbID
	}
}

// embedKB embeds every document of the work that has been read, in story order.
func (s *Service) embedKB(ctx context.Context, kbID string) error {
	sp, err := s.spaces(kbID)
	if err != nil {
		return err
	}
	srcs, err := s.st.Sources(kbID)
	if err != nil {
		return err
	}
	opts := s.opts()
	for _, src := range srcs {
		if src.Status != "read" {
			continue
		}
		if _, err := os.Stat(s.textPath(src.ID)); err != nil {
			// Read by an older version, which kept nothing: read it again.
			if err := s.Reprocess(ctx, src.ID); err != nil {
				return err
			}
			continue
		}
		hasText, err := s.embedSource(ctx, src, sp, opts)
		if err != nil {
			return fmt.Errorf("embedding %s: %w", src.Filename, err)
		}
		if hasText {
			s.status(src.ID, "embedded", fmt.Sprintf("%d trang · chờ tóm tắt cùng cả bộ truyện", src.Pages), "")
		} else {
			s.status(src.ID, "done", fmt.Sprintf("Đã embed %d trang ảnh (không có văn bản để tóm tắt)", src.Pages), "")
		}
	}
	return nil
}

// summarizeKB builds (or continues) the story map of a work.
func (s *Service) summarizeKB(ctx context.Context, kbID string, fromStart bool) error {
	kb, err := s.st.KnowledgeBase(kbID)
	if err != nil {
		return err
	}
	sp, err := s.spaces(kbID)
	if err != nil {
		return err
	}
	srcs, err := s.st.Sources(kbID) // in story order
	if err != nil {
		return err
	}
	var pending, done []store.Source
	for _, src := range srcs {
		switch src.Status {
		case "embedded":
			pending = append(pending, src)
		case "done":
			done = append(done, src)
		}
	}
	// A part that slots in before what is already mapped (a reprocessed
	// chapter) breaks the story order: map the whole work again.
	if len(pending) > 0 && len(done) > 0 && done[len(done)-1].Seq > pending[0].Seq {
		fromStart = true
	}
	// Parts mapped by an older version (sections, no chapters): map it all again.
	if !fromStart {
		has := map[string]bool{}
		chs, _ := s.st.Chapters(kbID)
		for _, c := range chs {
			has[c.SourceID] = true
		}
		for _, src := range done {
			if src.Sections > 0 && !has[src.ID] {
				fromStart = true
			}
		}
	}
	if fromStart {
		pending = append(done, pending...)
		sortBySeq(pending)
	}
	if len(pending) == 0 {
		return nil
	}
	// Documents read by older versions kept no text: read them again first.
	var missing bool
	for _, src := range pending {
		if _, err := os.Stat(s.textPath(src.ID)); err != nil {
			missing = true
			log.Printf("ingest: %s chưa có văn bản đã lưu, đọc lại", src.Filename)
			if err := s.Reprocess(ctx, src.ID); err != nil {
				return err
			}
		}
	}
	if missing {
		if fromStart {
			// Remember to start over once they're read again.
			for _, src := range pending {
				if src.Status == "done" {
					s.status(src.ID, "embedded", "Chờ lập lại bản đồ cùng cả bộ truyện", "")
				}
			}
		}
		return nil
	}

	opts := s.opts()
	summarizer, err := s.resolve(llm.RoleSummarize, kb.LLMProvider)
	if err != nil {
		return err
	}
	maxOut, inChars := llm.Budget(summarizer, 4000)
	title := kb.Name
	var mapped []store.Chapter // the map so far, in story order
	if !fromStart {
		isPending := map[string]bool{}
		for _, src := range pending {
			isPending[src.ID] = true
		}
		chs, _ := s.st.Chapters(kbID)
		for _, c := range chs {
			if !isPending[c.SourceID] {
				mapped = append(mapped, c)
			}
		}
	}

	// 1. Chapters of every pending document, in story order.
	type todo struct {
		src store.Source
		ch  chapter
	}
	var todos []todo
	for _, src := range pending {
		units, err := loadUnits(s.textPath(src.ID))
		if err != nil {
			return fmt.Errorf("%s: %w", src.Filename, err)
		}
		for _, ch := range splitChapters(units, src.Filename, opts.ChapterChars) {
			todos = append(todos, todo{src, ch})
		}
	}
	progress := func(msg string) {
		for _, src := range pending {
			s.status(src.ID, "summarizing", msg, "")
		}
	}

	// 2. Read each chapter in windows, updating its summary as it goes. The
	// map so far is the context, so chapters go one after another.
	perSource := map[string][]store.Chapter{}
	var results []sectionResult
	var docs []rag.Doc
	for i, t := range todos {
		no := len(mapped) + 1
		label := fmt.Sprintf("Chương %d · %s", no, t.ch.title)
		storySoFar := tailRunes(lore.RenderMap(mapped), inChars/5)
		// What is left for the passage: the prompt itself, the story so far and
		// the chapter's running summary take the rest.
		window := max(inChars-len([]rune(storySoFar))-4500, 1200)
		spans := pack(paragraphs(t.ch.units), window, 0)
		running := ""
		var cast []string
		for w, sp := range spans {
			progress(fmt.Sprintf("Lập bản đồ truyện: chương %d/%d, đoạn %d/%d…", i+1, len(todos), w+1, len(spans)))
			r, err := s.readWindow(ctx, summarizer, maxOut, title, label, w, len(spans), storySoFar, running, sp)
			if err != nil {
				return fmt.Errorf("đọc %s: %w", label, err)
			}
			if r.Summary != "" {
				running = r.Summary
			}
			results = append(results, r)
			for _, e := range r.Entities {
				if (lore.Entity{Type: e.Type}).IsCharacter() && !containsFold(cast, e.Name) && strings.TrimSpace(e.Name) != "" {
					cast = append(cast, strings.TrimSpace(e.Name))
				}
			}
		}
		c := store.Chapter{KBID: kbID, SourceID: t.src.ID, Idx: len(perSource[t.src.ID]), No: no,
			Title: t.ch.title, Pages: t.ch.pages(), Summary: running, Cast: cast[:min(len(cast), 12)]}
		mapped = append(mapped, c)
		perSource[t.src.ID] = append(perSource[t.src.ID], c)
		docs = append(docs, rag.Doc{ID: fmt.Sprintf("%s:ch:%d", t.src.ID, c.Idx), Text: c.Title + ": " + c.Summary,
			Metadata: meta(t.src, rag.LevelSection, c.Pages, "")})
	}

	// 3. Profiles: what the new chapters say, merged into the existing profiles.
	progress("Cập nhật hồ sơ nhân vật & địa danh…")
	entities := mergeEntities(results)
	if !fromStart {
		entities = withExisting(s.existingEntities(ctx, kbID), entities)
	}
	err = parallel(ctx, len(entities), opts.Concurrency, func(i int) error {
		return s.mergeProfile(ctx, summarizer, maxOut, inChars, title, entities[i])
	})
	if err != nil {
		return err
	}
	kbSrc := store.Source{KBID: kbID} // profiles belong to the work, not a document
	for i, e := range entities {
		docs = append(docs, rag.Doc{ID: fmt.Sprintf("%s:ent:%d", kbID, i),
			Text: fmt.Sprintf("%s (%s): %s", e.Name, e.Type, e.profile), Metadata: meta(kbSrc, rag.LevelEntity, "", e.Name)})
	}

	// 4. Swap in the new map entries and profiles. Profiles are rewritten as a
	// whole; when starting over, the old chapter (and older section) summaries
	// go too. The overview of older versions is dropped.
	progress(fmt.Sprintf("Embedding %d mục bản đồ & hồ sơ…", len(docs)))
	levels := []string{rag.LevelEntity, rag.LevelOverview}
	if fromStart {
		levels = append(levels, rag.LevelSection)
	}
	if err := s.lore.Vec.Delete(ctx, rag.Filter{KBIDs: []string{kbID}, Levels: levels}); err != nil {
		return err
	}
	if err := s.lore.Vec.AddTexts(ctx, sp, docs); err != nil {
		return err
	}
	for _, src := range pending {
		if err := s.st.SetChapters(src.ID, perSource[src.ID]); err != nil {
			return err
		}
	}
	all, _ := s.st.Chapters(kbID)
	s.st.SetKBOverview(kbID, lore.RenderMap(all))
	for _, src := range pending {
		n := len(perSource[src.ID])
		s.st.SetSourceSections(src.ID, n)
		s.status(src.ID, "done", fmt.Sprintf("%d trang · %d chương trong bản đồ truyện", src.Pages, n), "")
	}
	log.Printf("ingest: bản đồ %q xong: %d chương mới, %d hồ sơ", title, len(todos), len(entities))
	return nil
}

// readWindow reads passage w of n of a chapter and returns the chapter's
// summary so far (running, updated with the passage) and who appears in it.
func (s *Service) readWindow(ctx context.Context, p llm.Provider, maxOut int, title, label string, w, n int, storySoFar, running string, sp span) (sectionResult, error) {
	if storySoFar == "" {
		storySoFar = "(Đây là chương đầu tiên.)"
	}
	if running == "" {
		running = "(Chưa có: đây là đoạn đầu của chương.)"
	}
	out, err := llm.Complete(ctx, p, llm.Request{Purpose: fmt.Sprintf("bản đồ %s · %s · đoạn %d/%d", title, label, w+1, n),
		System: prompts.Get("analyst-system"), MaxTokens: maxOut,
		Messages: []llm.Message{llm.TextMessage(llm.RoleUser, prompts.Get("chapter", "title", title, "chapter", label,
			"part", strconv.Itoa(w+1), "parts", strconv.Itoa(n), "pages", sp.pages(),
			"story_so_far", storySoFar, "notes", running, "text", sp.Text))}})
	if err != nil {
		return sectionResult{}, err
	}
	var r sectionResult
	if err := ParseJSON(out, &r); err != nil || r.Summary == "" {
		r = sectionResult{Summary: strings.TrimSpace(out)} // model ignored the format; keep the prose
	}
	r.Summary = strings.TrimSpace(r.Summary)
	return r, nil
}

// mergeProfile merges an entity's notes into one profile, in rounds when the
// notes don't fit the model's context together.
func (s *Service) mergeProfile(ctx context.Context, p llm.Provider, maxOut, inChars int, title string, e *entity) error {
	notes := e.notes
	if len(notes) <= 1 || totalLen(notes) <= 1200 {
		e.profile = strings.Join(notes, "\n")
		return nil
	}
	budget := max(inChars-1500, 1000)
	for {
		groups := groupByLen(notes, budget)
		if len(groups) == len(notes) {
			// Every note fills the window alone: shorten them so two fit.
			for i := range notes {
				notes[i] = headRunes(notes[i], budget/3)
			}
			groups = groupByLen(notes, budget)
		}
		next := make([]string, len(groups))
		for i, g := range groups {
			if len(g) == 1 {
				next[i] = g[0]
				continue
			}
			out, err := llm.Complete(ctx, p, llm.Request{Purpose: "gộp hồ sơ " + e.Name, System: prompts.Get("analyst-system"), MaxTokens: maxOut,
				Messages: []llm.Message{llm.TextMessage(llm.RoleUser,
					prompts.Get("entity-merge", "name", e.Name, "type", e.Type, "title", title, "notes", strings.Join(g, "\n---\n")))}})
			if err != nil {
				return err
			}
			next[i] = strings.TrimSpace(out)
		}
		if len(next) == 1 {
			e.profile = next[0]
			return nil
		}
		notes = next
	}
}

func containsFold(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(strings.TrimSpace(x), strings.TrimSpace(s)) {
			return true
		}
	}
	return false
}

// tailRunes keeps the last n runes of s, from a line start when possible.
func tailRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	t := string(r[len(r)-n:])
	if i := strings.Index(t, "\n"); i >= 0 && i < len(t)/2 {
		t = t[i+1:]
	}
	return "…\n" + t
}

// existingEntities reads the profiles already stored for the work.
func (s *Service) existingEntities(ctx context.Context, kbID string) []Entity {
	ents, err := s.Entities(ctx, []string{kbID})
	if err != nil {
		log.Printf("ingest: đọc hồ sơ cũ: %v", err)
	}
	return ents
}

// withExisting puts each existing profile first in the notes of the entity it
// describes, and keeps profiles the new parts don't mention unchanged.
func withExisting(old []Entity, fresh []*entity) []*entity {
	byName := map[string]*entity{}
	for _, e := range fresh {
		byName[strings.ToLower(strings.TrimSpace(e.Name))] = e
	}
	var out []*entity
	for _, o := range old {
		key := strings.ToLower(strings.TrimSpace(o.Name))
		if e, ok := byName[key]; ok {
			if e.notes == nil || e.notes[0] != o.Profile {
				e.notes = append([]string{o.Profile}, e.notes...)
			}
			if o.Type != "" {
				e.Type = o.Type
			}
			continue
		}
		e := &entity{Name: o.Name, Type: o.Type, notes: []string{o.Profile}}
		byName[key] = e
		out = append(out, e)
	}
	return append(out, fresh...)
}

func sortBySeq(srcs []store.Source) {
	for i := 1; i < len(srcs); i++ {
		for j := i; j > 0 && srcs[j].Seq < srcs[j-1].Seq; j-- {
			srcs[j], srcs[j-1] = srcs[j-1], srcs[j]
		}
	}
}
