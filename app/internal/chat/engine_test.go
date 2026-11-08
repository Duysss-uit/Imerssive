package chat

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"immersive/internal/llm"
	"immersive/internal/lore"
	"immersive/internal/rag"
	"immersive/internal/store"
)

// memVectors holds documents in memory; every search returns all of the
// matching works' documents.
type memVectors struct {
	mu   sync.Mutex
	docs []rag.Doc
}

func (m *memVectors) SearchKBs(_ context.Context, _ rag.Spaces, _ string, f rag.Filter, _ int) ([]rag.Hit, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []rag.Hit
	for _, d := range m.docs {
		for _, id := range f.KBIDs {
			if d.Metadata["kb_id"] == id {
				out = append(out, rag.Hit{ID: d.ID, Text: d.Text, Metadata: d.Metadata})
			}
		}
	}
	return out, nil
}
func (m *memVectors) GetTexts(ctx context.Context, _ rag.Space, f rag.Filter, _ int) ([]rag.Hit, error) {
	return m.SearchKBs(ctx, nil, "", f, 0)
}
func (*memVectors) SearchImages(context.Context, string, rag.Filter, int) ([]rag.Hit, error) {
	return nil, nil
}
func (m *memVectors) AddTexts(_ context.Context, _ rag.Space, docs []rag.Doc) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.docs = append(m.docs, docs...)
	return nil
}
func (*memVectors) Delete(context.Context, rag.Filter) error { return nil }

// toolModel looks a character up once, then answers; it also keeps the
// divergence log when asked.
type toolModel struct {
	mu   sync.Mutex
	reqs []llm.Request
}

func (*toolModel) SupportsVision() bool                         { return false }
func (*toolModel) ListModels(context.Context) ([]string, error) { return nil, nil }
func (m *toolModel) Stream(ctx context.Context, r llm.Request, d func(string)) (string, error) {
	m.mu.Lock()
	m.reqs = append(m.reqs, r)
	m.mu.Unlock()
	if strings.Contains(r.Messages[0].Parts[0].Text, "divergence log") {
		return "- Hiện tại: Mai Linh đã được cứu khỏi Hắc Sơn.", nil
	}
	return "...", nil
}
func (m *toolModel) Turn(_ context.Context, r llm.Request, d func(string)) (llm.Turn, error) {
	m.mu.Lock()
	m.reqs = append(m.reqs, r)
	m.mu.Unlock()
	if last := r.Messages[len(r.Messages)-1]; last.Role != llm.RoleTool {
		return llm.Turn{Calls: []llm.ToolCall{{ID: "c1", Name: "get_character", Args: `{"name":"mai linh"}`}}}, nil
	}
	d("[Mai Linh]\n")
	d(`"Huynh đến rồi!"`)
	return llm.Turn{Text: "[Mai Linh]\n\"Huynh đến rồi!\""}, nil
}

func TestChatLooksUpAndKeepsDivergence(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	kb := store.KnowledgeBase{Name: "Huyết Long"}
	st.SaveKnowledgeBase(&kb)
	src := store.Source{KBID: kb.ID, Filename: "c1.pdf", Status: "done"}
	st.InsertSource(&src)
	st.SetChapters(src.ID, []store.Chapter{{KBID: kb.ID, Title: "Chương 1", Pages: "1-9", Summary: "Mai Linh bị bắt tới Hắc Sơn."}})
	vec := &memVectors{docs: []rag.Doc{{ID: "e1", Text: "Mai Linh (character): Thanh mai trúc mã của Lâm Phong.",
		Metadata: map[string]any{"kb_id": kb.ID, "level": rag.LevelEntity, "name": "Mai Linh"}}}}
	spaces := func(string) (rag.Space, error) { return rag.Space{}, nil }
	model := &toolModel{}
	var events []string
	var evMu sync.Mutex
	e := New(st, vec, &lore.Lore{St: st, Vec: vec, Spaces: spaces, PagesDir: dir}, spaces,
		func(string, string) (llm.Provider, error) { return model, nil },
		func() Options {
			return Options{RAGK: 4, ContextChars: 4000, Window: 40, MaxTokens: 2000, DivergenceEvery: 1, AutoProfiles: true}
		},
		func(ev string, _ any) { evMu.Lock(); events = append(events, ev); evMu.Unlock() })

	sc := store.Scenario{Title: "Cứu Mai Linh", KBIDs: []string{kb.ID}}
	st.SaveScenario(&sc)
	c := store.Chat{Mode: store.ModeScene, ScenarioID: sc.ID, PersonaName: "Lâm Phong"}
	if err := e.Create(&c); err != nil {
		t.Fatal(err)
	}
	st.AddMessage(c.ID, "user", "*phá cửa ngục tìm Mai Linh*")
	reply, err := e.generate(context.Background(), c.ID)
	if err != nil || !strings.Contains(reply, "Huynh đến rồi") {
		t.Fatalf("reply=%q err=%v", reply, err)
	}
	first := model.reqs[0]
	if !strings.Contains(first.System, "<story_map>") || !strings.Contains(first.System, "1. Chương 1 (p. 1-9): Mai Linh bị bắt") {
		t.Errorf("story map not in the system prompt:\n%s", first.System)
	}
	lastParts := first.Messages[len(first.Messages)-1].Parts
	if r := lastParts[len(lastParts)-1].Text; !strings.Contains(r, "reply in the language of Lâm Phong's message") {
		t.Errorf("per-turn reminder missing or unfilled: %q", r)
	}
	if len(first.Tools) != len(lore.Tools) {
		t.Errorf("tools offered: %d", len(first.Tools))
	}
	var present, memory string
	for _, p := range lastParts {
		if strings.HasPrefix(p.Text, "<characters_present>") {
			present = p.Text
		}
		if strings.HasPrefix(p.Text, "<memory>") {
			memory = p.Text
		}
	}
	if !strings.Contains(present, "Mai Linh (character): Thanh mai trúc mã") {
		t.Errorf("profile of the character just named not added: %q", present)
	}
	if strings.Contains(memory, "Thanh mai") {
		t.Error("with tools, source passages should be looked up, not retrieved into memory")
	}
	second := model.reqs[1].Messages
	if res := second[len(second)-1]; res.Role != llm.RoleTool || !strings.Contains(res.Parts[0].Text, "Thanh mai trúc mã") {
		t.Errorf("get_character result: %+v", res)
	}
	if !contains(events, "chat:lookup") {
		t.Errorf("events: %v", events)
	}

	// After a reply, the divergence log takes in what happened; it then sits
	// in the system prompt.
	st.AddMessage(c.ID, "assistant", reply)
	if err := e.divergence(context.Background(), c.ID, false); err != nil {
		t.Fatal(err)
	}
	got, _ := st.Chat(c.ID)
	if !strings.Contains(got.Divergence, "đã được cứu") || got.DivergenceUpTo == 0 {
		t.Fatalf("divergence = %q up to %d", got.Divergence, got.DivergenceUpTo)
	}
	calls := len(model.reqs)
	e.divergence(context.Background(), c.ID, false) // nothing new: no call
	if len(model.reqs) != calls {
		t.Error("divergence updated without new messages")
	}
	e.SetDivergence(c.ID, "- Lâm Phong mất một tay.")
	st.AddMessage(c.ID, "user", "*nhìn quanh*")
	e.generate(context.Background(), c.ID)
	if sys := model.reqs[len(model.reqs)-1].System; !strings.Contains(sys, "<divergence>\n- Lâm Phong mất một tay.") {
		t.Errorf("player's divergence log not in the system prompt:\n%s", sys)
	}
	if got, _ := st.Chat(c.ID); got.DivergenceUpTo == 0 {
		t.Error("editing the log lost its position")
	}

	// Sending nothing: the history ends with a reply, so the model is asked to
	// carry the story on, and nothing is saved for the player.
	st.AddMessage(c.ID, "assistant", "[Mai Linh]\n\"Đi thôi!\"")
	before, _ := st.Messages(c.ID, false)
	start := len(model.reqs)
	e.generate(context.Background(), c.ID)
	last := model.reqs[start].Messages // the first call of the turn, before any lookup
	if p := last[len(last)-1]; p.Role != llm.RoleUser || !slices.ContainsFunc(p.Parts, func(x llm.Part) bool { return strings.Contains(x.Text, "Lâm Phong says and does nothing this turn") }) {
		t.Errorf("continue turn: %+v", p)
	}
	if after, _ := st.Messages(c.ID, false); len(after) != len(before) {
		t.Error("continuing saved a player message")
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
