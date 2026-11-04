package ingest

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"immersive/internal/llm"
	"immersive/internal/lore"
	"immersive/internal/rag"
	"immersive/internal/store"
)

// fakeLookup answers every search with one passage and lists two profiles.
type fakeLookup struct{ queries []string }

func (f *fakeLookup) SearchKBs(_ context.Context, _ rag.Spaces, q string, _ rag.Filter, _ int) ([]rag.Hit, error) {
	f.queries = append(f.queries, q)
	return []rag.Hit{{Text: "Mai Linh bị bắt tới núi Hắc Sơn.", Metadata: map[string]any{"level": rag.LevelSection}}}, nil
}

func (f *fakeLookup) GetTexts(context.Context, rag.Space, rag.Filter, int) ([]rag.Hit, error) {
	return []rag.Hit{
		{Text: "Lâm Phong (character): Kiếm sĩ trẻ làng Thanh Vân.", Metadata: map[string]any{"name": "Lâm Phong"}},
		{Text: "Hắc Sơn (place): Ngọn núi của ma tộc.", Metadata: map[string]any{"name": "Hắc Sơn"}},
	}, nil
}

const cardJSON = `{"name":"Lâm Phong","greeting":"*rút kiếm* Ai đó?"}`

// toolModel searches once, then answers.
type toolModel struct{ reqs []llm.Request }

func (*toolModel) SupportsVision() bool                         { return false }
func (*toolModel) ListModels(context.Context) ([]string, error) { return nil, nil }
func (m *toolModel) Stream(ctx context.Context, r llm.Request, d func(string)) (string, error) {
	t, err := m.Turn(ctx, r, d)
	return t.Text, err
}
func (m *toolModel) Turn(_ context.Context, r llm.Request, _ func(string)) (llm.Turn, error) {
	m.reqs = append(m.reqs, r)
	if last := r.Messages[len(r.Messages)-1]; last.Role != llm.RoleTool {
		return llm.Turn{Calls: []llm.ToolCall{{ID: "c1", Name: "search_story", Args: `{"query":"Mai Linh ở đâu"}`}}}, nil
	}
	return llm.Turn{Text: cardJSON}, nil
}

// plainModel can't call tools: it lists queries, then answers.
type plainModel struct{ prompts []string }

func (*plainModel) SupportsVision() bool                         { return false }
func (*plainModel) ListModels(context.Context) ([]string, error) { return nil, nil }
func (m *plainModel) Stream(_ context.Context, r llm.Request, _ func(string)) (string, error) {
	p := r.Messages[0].Parts[0].Text
	m.prompts = append(m.prompts, p)
	if strings.Contains(p, "JSON array") {
		return `["Lâm Phong kiếm", "Mai Linh"]`, nil
	}
	return cardJSON, nil
}

func newTestService(t *testing.T, p llm.Provider) (*Service, *fakeLookup, string) {
	st, err := store.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	kb := store.KnowledgeBase{Name: "Truyện"}
	st.SaveKnowledgeBase(&kb)
	src := store.Source{KBID: kb.ID, Filename: "c1.pdf", Status: "done"}
	st.InsertSource(&src)
	st.SetChapters(src.ID, []store.Chapter{{KBID: kb.ID, Title: "Chương 1", Pages: "1-5", Summary: "Lâm Phong đi cứu Mai Linh."}})
	look := &fakeLookup{}
	spaces := func(string) (rag.Space, error) { return rag.Space{}, nil }
	s := &Service{st: st, lore: &lore.Lore{St: st, Vec: look, Spaces: spaces}, spaces: spaces,
		resolve: func(string, string) (llm.Provider, error) { return p, nil }}
	return s, look, kb.ID
}

func TestResearchWithTools(t *testing.T) {
	m := &toolModel{}
	s, look, kb := newTestService(t, m)
	c, err := s.GenerateCharacter(context.Background(), []string{kb}, "Lâm Phong", "", "")
	if err != nil || c.Greeting == "" {
		t.Fatalf("card=%+v err=%v", c, err)
	}
	if len(look.queries) != 1 || look.queries[0] != "Mai Linh ở đâu" {
		t.Errorf("searches = %q", look.queries)
	}
	first := m.reqs[0].Messages[0].Parts[0].Text
	for _, want := range []string{"1. Chương 1 (tr. 1-5): Lâm Phong đi cứu Mai Linh", "Hắc Sơn (place)", "[Hồ sơ: Lâm Phong]", "search_story"} {
		if !strings.Contains(first, want) {
			t.Errorf("brief missing %q:\n%s", want, first)
		}
	}
	if strings.Contains(first, "núi Hắc Sơn.") {
		t.Error("search results should not be in the first prompt")
	}
	if len(m.reqs[0].Tools) != 3 {
		t.Errorf("tools offered: %+v", m.reqs[0].Tools)
	}
	second := m.reqs[1].Messages
	if res := second[len(second)-1]; res.Role != llm.RoleTool || res.CallID != "c1" || !strings.Contains(res.Parts[0].Text, "núi Hắc Sơn") {
		t.Errorf("tool result not sent back: %+v", res)
	}
}

func TestResearchWithoutTools(t *testing.T) {
	m := &plainModel{}
	s, look, kb := newTestService(t, m)
	c, err := s.GenerateCharacter(context.Background(), []string{kb}, "Lâm Phong", "", "")
	if err != nil || c.Greeting == "" {
		t.Fatalf("card=%+v err=%v", c, err)
	}
	if strings.Join(look.queries, "|") != "Lâm Phong kiếm|Mai Linh" {
		t.Errorf("planned searches = %q", look.queries)
	}
	if len(m.prompts) != 2 || !strings.Contains(m.prompts[1], "núi Hắc Sơn") {
		t.Errorf("results not given to the final prompt: %q", m.prompts)
	}
}

func TestParseEntity(t *testing.T) {
	e := lore.ParseEntity("Hina", "Hina (character): Teal hair: playful.")
	if e.Name != "Hina" || e.Type != "character" || e.Profile != "Teal hair: playful." || !e.IsCharacter() {
		t.Errorf("%+v", e)
	}
	if e := lore.ParseEntity("", "Hắc Sơn (place): núi"); e.Name != "Hắc Sơn" || e.IsCharacter() {
		t.Errorf("%+v", e)
	}
	if got := Shorten("Câu một. Câu hai dài hơn nhiều.", 15); got != "Câu một." {
		t.Errorf("Shorten = %q", got)
	}
}

func (f *fakeLookup) AddTexts(context.Context, rag.Space, []rag.Doc) error { return nil }
func (f *fakeLookup) Delete(context.Context, rag.Filter) error             { return nil }
