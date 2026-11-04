package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"immersive/internal/llm"
	"immersive/internal/lore"
	"immersive/internal/rag"
	"immersive/internal/store"
)

// memVectors keeps documents in memory, filtered by kb and level.
type memVectors struct {
	mu   sync.Mutex
	docs map[string]rag.Doc
}

func (m *memVectors) SearchKBs(context.Context, rag.Spaces, string, rag.Filter, int) ([]rag.Hit, error) {
	return nil, nil
}
func (m *memVectors) GetTexts(_ context.Context, _ rag.Space, f rag.Filter, _ int) ([]rag.Hit, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []rag.Hit
	for id, d := range m.docs {
		if m.match(d, f) {
			out = append(out, rag.Hit{ID: id, Text: d.Text, Metadata: d.Metadata})
		}
	}
	return out, nil
}
func (m *memVectors) AddTexts(_ context.Context, _ rag.Space, docs []rag.Doc) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, d := range docs {
		m.docs[d.ID] = d
	}
	return nil
}
func (m *memVectors) Delete(_ context.Context, f rag.Filter) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, d := range m.docs {
		if m.match(d, f) {
			delete(m.docs, id)
		}
	}
	return nil
}
func (m *memVectors) match(d rag.Doc, f rag.Filter) bool {
	if len(f.KBIDs) > 0 && d.Metadata["kb_id"] != f.KBIDs[0] {
		return false
	}
	if len(f.Levels) == 0 {
		return true
	}
	for _, l := range f.Levels {
		if d.Metadata["level"] == l {
			return true
		}
	}
	return false
}
func (m *memVectors) count(level string) int {
	hits, _ := m.GetTexts(context.Background(), rag.Space{}, rag.Filter{Levels: []string{level}}, 0)
	return len(hits)
}

// storyModel records prompts and answers each kind of map call. Its context
// window is ctx tokens (0 = unknown).
type storyModel struct {
	mu      sync.Mutex
	prompts []string
	onCall  func()
	ctx     int
}

func (*storyModel) SupportsVision() bool                         { return false }
func (*storyModel) ListModels(context.Context) ([]string, error) { return nil, nil }
func (m *storyModel) ContextTokens() int                         { return m.ctx }
func (m *storyModel) Stream(_ context.Context, r llm.Request, _ func(string)) (string, error) {
	p := r.Messages[0].Parts[0].Text
	m.mu.Lock()
	m.prompts = append(m.prompts, p)
	m.mu.Unlock()
	if m.onCall != nil {
		m.onCall()
	}
	switch {
	case strings.Contains(p, `"entities"`):
		passage := between(p, "<passage>", "</passage>")
		sum, _ := json.Marshal("Tóm tắt: " + string([]rune(passage)[:min(len([]rune(passage)), 12)]))
		desc, _ := json.Marshal("Trong " + passage + ", " + strings.Repeat("Lâm Phong luyện kiếm. ", 70))
		return fmt.Sprintf(`{"summary":%s,"entities":[{"name":"Lâm Phong","type":"character","description":%s}]}`, sum, desc), nil
	case strings.Contains(p, "Merge these notes"):
		return "Hồ sơ gộp của Lâm Phong", nil
	}
	return "?", nil
}
func (m *storyModel) count(sub string) int {
	n := 0
	for _, p := range m.prompts {
		if strings.Contains(p, sub) {
			n++
		}
	}
	return n
}

// chapterPrompts are the map calls made, in order.
func (m *storyModel) chapterPrompts() []string {
	var out []string
	for _, p := range m.prompts {
		if strings.Contains(p, `"entities"`) {
			out = append(out, p)
		}
	}
	return out
}

func between(s, a, b string) string {
	i := strings.Index(s, a)
	j := strings.Index(s, b)
	if i < 0 || j < i {
		return ""
	}
	return strings.TrimSpace(s[i+len(a) : j])
}

func newMapService(t *testing.T, model *storyModel, vec *memVectors) (*Service, *store.Store, string) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	kb := store.KnowledgeBase{Name: "Huyết Long"}
	st.SaveKnowledgeBase(&kb)
	spaces := func(string) (rag.Space, error) { return rag.Space{}, nil }
	s := &Service{st: st, lore: &lore.Lore{St: st, Vec: vec, Spaces: spaces, PagesDir: dir}, pagesDir: dir, emit: func(string, any) {},
		cancels: map[string]context.CancelFunc{}, queue: make(chan string, 8), spaces: spaces,
		resolve: func(string, string) (llm.Provider, error) { return model, nil },
		opts:    func() Options { return Options{ChapterChars: 100000, ChunkChars: 1000, KeepRaw: true, Concurrency: 2} }}
	return s, st, kb.ID
}

func TestMapWholeWorkThenContinue(t *testing.T) {
	vec := &memVectors{docs: map[string]rag.Doc{}}
	model := &storyModel{}
	s, st, kb := newMapService(t, model, vec)
	var embeddedAtFirstCall = -1
	model.onCall = func() {
		if embeddedAtFirstCall < 0 {
			embeddedAtFirstCall = vec.count(rag.LevelRaw)
		}
	}
	// Documents are read and embedded first; the map waits for the last of them.
	add := func(name string) store.Source {
		src := store.Source{KBID: kb, Filename: name, Status: "read"}
		if err := st.InsertSource(&src); err != nil {
			t.Fatal(err)
		}
		if err := saveJSON(s.textPath(src.ID), []unit{{Page: 1, Text: "Lâm Phong lên đường (" + name + ")"}, {Page: 2, Text: "Hết (" + name + ")"}}); err != nil {
			t.Fatal(err)
		}
		return src
	}
	add("Chương 1.pdf")
	queued := store.Source{KBID: kb, Filename: "Chương 2.pdf", Status: "queued"}
	st.InsertSource(&queued)
	s.runKB(kb, false)
	if len(model.prompts) != 0 || vec.count(rag.LevelRaw) != 0 {
		t.Fatal("embedded or mapped while a document was still waiting to be read")
	}
	st.UpdateSourceStatus(queued.ID, "read", "", "")
	saveJSON(s.textPath(queued.ID), []unit{{Page: 1, Text: "Lâm Phong lên đường (2)"}})

	// One pass over both, in order: one map entry each, one merged profile.
	s.runKB(kb, false)
	if vec.count(rag.LevelRaw) != 2 || embeddedAtFirstCall != 2 {
		t.Errorf("embedded %d documents, %d of them before the first map call; want both", vec.count(rag.LevelRaw), embeddedAtFirstCall)
	}
	cps := model.chapterPrompts()
	if len(cps) != 2 {
		t.Fatalf("map calls = %d", len(cps))
	}
	if !strings.Contains(cps[0], "Chương 1 · Chương 1") || !strings.Contains(cps[0], "Đây là chương đầu tiên") {
		t.Errorf("first chapter prompt:\n%s", cps[0])
	}
	if !strings.Contains(between(cps[1], "<story_so_far>", "</story_so_far>"), "1. Chương 1 (tr. 1-2): Tóm tắt: Lâm Phong lê") {
		t.Errorf("second chapter doesn't see the first:\n%s", cps[1])
	}
	if model.count("Merge these notes") != 1 {
		t.Errorf("profile merges = %d", model.count("Merge these notes"))
	}
	chs, _ := st.Chapters(kb)
	if len(chs) != 2 || chs[1].No != 2 || chs[1].Cast[0] != "Lâm Phong" {
		t.Fatalf("chapters: %+v", chs)
	}
	if k, _ := st.KnowledgeBase(kb); !strings.Contains(k.Overview, "2. Chương 2") {
		t.Errorf("map = %q", k.Overview)
	}
	if vec.count(rag.LevelEntity) != 1 || vec.count(rag.LevelSection) != 2 {
		t.Errorf("stored: %d entities, %d chapters", vec.count(rag.LevelEntity), vec.count(rag.LevelSection))
	}
	srcs, _ := st.Sources(kb)
	for _, src := range srcs {
		if src.Status != "done" || src.Sections != 1 {
			t.Errorf("%s: %s, %d chapters", src.Filename, src.Status, src.Sections)
		}
	}

	// A later document continues the map.
	model.prompts = nil
	add("Chương 3.pdf")
	s.runKB(kb, false)
	cps = model.chapterPrompts()
	if len(cps) != 1 || !strings.Contains(cps[0], "Chương 3 · Chương 3") || !strings.Contains(cps[0], "2. Chương 2") {
		t.Errorf("continuation prompt:\n%q", cps)
	}
	if model.count("Hồ sơ gộp của Lâm Phong") != 1 {
		t.Error("existing profile not merged with the new notes")
	}
	if chs, _ := st.Chapters(kb); len(chs) != 3 || vec.count(rag.LevelEntity) != 1 || vec.count(rag.LevelSection) != 3 {
		t.Errorf("stored: %d chapters, %d entities, %d chapter docs", len(chs), vec.count(rag.LevelEntity), vec.count(rag.LevelSection))
	}

	// Starting over gives the same map, not a doubled one.
	model.prompts = nil
	s.Resummarize(kb)
	s.runKB(kb, true)
	if chs, _ := st.Chapters(kb); len(chs) != 3 || len(model.chapterPrompts()) != 3 || vec.count(rag.LevelSection) != 3 {
		t.Errorf("after starting over: %d chapters, %d calls, %d docs", len(chs), len(model.chapterPrompts()), vec.count(rag.LevelSection))
	}
}

// A chapter longer than the model's context is read in windows, the summary
// carried from one to the next.
func TestMapReadsLongChapterInWindows(t *testing.T) {
	vec := &memVectors{docs: map[string]rag.Doc{}}
	model := &storyModel{ctx: 3000}
	s, st, kb := newMapService(t, model, vec)
	src := store.Source{KBID: kb, Filename: "Tập 1.cbz", Status: "embedded"}
	st.InsertSource(&src)
	var units []unit
	for p := 1; p <= 6; p++ {
		units = append(units, unit{Page: p, Text: fmt.Sprintf("Trang %d: ", p) + strings.Repeat("Mai Linh chạy trốn. ", 40)})
	}
	saveJSON(s.textPath(src.ID), units)
	if err := s.summarizeKB(context.Background(), kb, false); err != nil {
		t.Fatal(err)
	}
	cps := model.chapterPrompts()
	if len(cps) < 3 {
		t.Fatalf("%d windows for a chapter far over the context", len(cps))
	}
	for i, p := range cps {
		if n := len([]rune(p)); n > 3000*llm.CharsPerToken {
			t.Errorf("window %d prompt is %d chars, over the context", i+1, n)
		}
	}
	if !strings.Contains(between(cps[1], "<chapter_summary_so_far>", "</chapter_summary_so_far>"), "Tóm tắt: Trang 1") {
		t.Errorf("second window lacks the running summary:\n%s", cps[1])
	}
	chs, _ := st.Chapters(kb)
	if len(chs) != 1 || chs[0].Pages != "1-6" || !strings.HasPrefix(chs[0].Summary, "Tóm tắt: Trang") || strings.HasPrefix(chs[0].Summary, "Tóm tắt: Trang 1") {
		t.Errorf("chapter = %+v (want the last window's summary)", chs)
	}
}

func TestNaturalCompare(t *testing.T) {
	names := []string{"Chương 10.pdf", "chương 2.pdf", "Chương 1.pdf"}
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && naturalCompare(names[j], names[j-1]) < 0; j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
	if strings.Join(names, ",") != "Chương 1.pdf,chương 2.pdf,Chương 10.pdf" {
		t.Errorf("order = %v", names)
	}
}
