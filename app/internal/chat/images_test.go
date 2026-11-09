package chat

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"immersive/internal/llm"
	"immersive/internal/lore"
	"immersive/internal/rag"
	"immersive/internal/store"
)

type fakeImager struct {
	mu   sync.Mutex
	reqs []llm.ImageRequest
}

func (f *fakeImager) Draw(_ context.Context, r llm.ImageRequest) (llm.Image, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, r)
	return llm.Image{B64: "iVBORw0KGgo=", MIME: "image/png"}, nil
}

// drawModel draws once, then answers; asked for a picture description
// (the Tạo ảnh button) it answers with JSON.
type drawModel struct {
	mu   sync.Mutex
	reqs []llm.Request
}

func (*drawModel) SupportsVision() bool                         { return false }
func (*drawModel) ListModels(context.Context) ([]string, error) { return nil, nil }
func (m *drawModel) Stream(_ context.Context, r llm.Request, _ func(string)) (string, error) {
	m.mu.Lock()
	m.reqs = append(m.reqs, r)
	m.mu.Unlock()
	return `Here: {"prompt": "Mai Linh smiles at dawn", "characters": ["Mai Linh"], "aspect": "portrait", "continue_previous": true}`, nil
}
func (m *drawModel) Turn(_ context.Context, r llm.Request, d func(string)) (llm.Turn, error) {
	m.mu.Lock()
	m.reqs = append(m.reqs, r)
	m.mu.Unlock()
	if strings.Contains(r.Messages[0].Parts[0].Text, "with one JSON object only") {
		if last := r.Messages[len(r.Messages)-1]; last.Role != llm.RoleTool {
			return llm.Turn{Calls: []llm.ToolCall{{ID: "l1", Name: "get_character", Args: `{"name": "Mai Linh"}`}}}, nil
		}
		return llm.Turn{Text: `{"prompt": "Mai Linh smiles at dawn", "characters": ["Mai Linh"], "aspect": "portrait", "continue_previous": true}`}, nil
	}
	if last := r.Messages[len(r.Messages)-1]; last.Role != llm.RoleTool {
		args := `{"prompt": "Lâm Phong breaks the cell door; Mai Linh looks up", "characters": ["Mai Linh", "Lâm Phong", "Bóng Ma"], "references": [{"chapter": 1, "page": "3"}], "aspect": "landscape"}`
		return llm.Turn{Text: "*Cửa ngục vỡ tung.*", Calls: []llm.ToolCall{{ID: "d1", Name: "generate_image", Args: args}}}, nil
	}
	d(`[Mai Linh]` + "\n" + `"Huynh!"`)
	return llm.Turn{Text: "[Mai Linh]\n\"Huynh!\""}, nil
}

func TestChatDrawsWithReferences(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	kb := store.KnowledgeBase{Name: "Huyết Long"}
	st.SaveKnowledgeBase(&kb)
	src := store.Source{KBID: kb.ID, Filename: "c1.cbz", Status: "done"}
	st.InsertSource(&src)
	st.SetChapters(src.ID, []store.Chapter{{KBID: kb.ID, Title: "Chương 1", Pages: "1-3", Summary: "Mai Linh bị bắt."}})
	pages := filepath.Join(dir, "pages")
	os.MkdirAll(filepath.Join(pages, src.ID), 0o755)
	text, _ := json.Marshal([]lore.Page{{Page: 1, Text: "Hắc Sơn trong mưa."}, {Page: 2, Text: "[Mai Linh]: Cứu ta! Mai Linh khóc."}, {Page: 3, Text: "Ngục tối."}})
	os.WriteFile(lore.TextPath(pages, src.ID), text, 0o644)
	for _, n := range []string{"p00000.jpg", "p00001.jpg", "p00002.jpg"} {
		os.WriteFile(filepath.Join(pages, src.ID, n), []byte("jpeg "+n), 0o644)
	}
	card := store.Character{Name: "Lâm Phong", Avatar: "data:image/png;base64,QVZBVEFS"}
	st.SaveCharacter(&card)

	vec := &memVectors{}
	spaces := func(string) (rag.Space, error) { return rag.Space{}, nil }
	model := &drawModel{}
	var events []string
	var evMu sync.Mutex
	e := New(st, vec, &lore.Lore{St: st, Vec: vec, Spaces: spaces, PagesDir: pages}, spaces,
		func(string, string) (llm.Provider, error) { return model, nil },
		func() Options {
			return Options{RAGK: 4, ContextChars: 4000, Window: 40, MaxTokens: 2000, ImageMaxRefs: 4}
		},
		func(ev string, _ any) { evMu.Lock(); events = append(events, ev); evMu.Unlock() })
	imager := &fakeImager{}
	e.SetImaging(Imaging{Imager: func() (llm.Imager, error) { return imager, nil }, Dir: filepath.Join(dir, "images")})

	sc := store.Scenario{Title: "Cứu Mai Linh", KBIDs: []string{kb.ID}}
	st.SaveScenario(&sc)
	c := store.Chat{Mode: store.ModeScene, ScenarioID: sc.ID, PersonaName: "Lâm Phong"}
	if err := e.Create(&c); err != nil {
		t.Fatal(err)
	}
	st.AddMessage(c.ID, "user", "*phá cửa ngục*")
	reply, err := e.generate(context.Background(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	first := model.reqs[0]
	if !strings.Contains(first.System, "## Illustrations") || len(first.Tools) != len(lore.Tools)+1 {
		t.Errorf("drawing not offered: %d tools", len(first.Tools))
	}
	m := imageLine.FindStringSubmatch(reply)
	if m == nil || !strings.HasPrefix(reply, "*Cửa ngục vỡ tung.*\n\n[Image: "+c.ID+"/") || !strings.HasSuffix(reply, "\"Huynh!\"") {
		t.Fatalf("reply = %q", reply)
	}
	if p, _ := e.ImagePath(m[1]); func() bool { _, err := os.Stat(p); return err != nil }() {
		t.Errorf("picture not saved at %s", p)
	}

	if len(imager.reqs) != 1 {
		t.Fatalf("drawings: %d", len(imager.reqs))
	}
	r := imager.reqs[0]
	var labels []string
	for _, ref := range r.Refs {
		labels = append(labels, ref.Label)
	}
	if len(r.Refs) != 3 || r.Aspect != "landscape" {
		t.Fatalf("refs %v aspect %q", labels, r.Aspect)
	}
	if !strings.HasPrefix(labels[0], "Mai Linh") || r.Refs[0].B64 != "anBlZyBwMDAwMDEuanBn" { // page 2 names her most
		t.Errorf("Mai Linh's reference: %s %s", labels[0], r.Refs[0].B64)
	}
	if !strings.HasPrefix(labels[1], "Lâm Phong") || r.Refs[1].B64 != "QVZBVEFS" { // the card picture
		t.Errorf("Lâm Phong's reference: %s", labels[1])
	}
	if !strings.Contains(labels[2], "chapter 1, page 3") {
		t.Errorf("page reference: %s", labels[2])
	}
	tool := model.reqs[1].Messages[len(model.reqs[1].Messages)-1]
	if !strings.Contains(tool.Parts[0].Text, "No reference picture was found for: Bóng Ma") {
		t.Errorf("tool result: %s", tool.Parts[0].Text)
	}
	if !contains(events, "chat:image") {
		t.Errorf("events: %v", events)
	}

	// The model reads the picture back as a note, never as the marker.
	msg, _ := st.AddMessage(c.ID, "assistant", reply)
	st.AddMessage(c.ID, "user", "*ôm Mai Linh*")
	start := len(model.reqs)
	e.generate(context.Background(), c.ID)
	hist := model.reqs[start].Messages
	var seen string
	for _, h := range hist {
		for _, p := range h.Parts {
			seen += p.Text + "\n"
		}
	}
	if strings.Contains(seen, "[Image:") || !strings.Contains(seen, "(An illustration was shown to the player here: Lâm Phong breaks") {
		t.Errorf("history as the model sees it:\n%s", seen)
	}

	// The Minh hoạ button: the model writes the description, the picture is
	// added to the reply, carrying on from the previous one.
	before := len(imager.reqs)
	if err := e.Illustrate(c.ID, msg.ID); err != nil {
		t.Fatal(err)
	}
	var got store.Message
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		msgs, _ := st.Messages(c.ID, false)
		for _, x := range msgs {
			if x.ID == msg.ID {
				got = x
			}
		}
		if len(imageLine.FindAllString(got.Content, -1)) == 2 {
			break
		}
	}
	if n := len(imageLine.FindAllString(got.Content, -1)); n != 2 {
		t.Fatalf("illustrated message: %q", got.Content)
	}
	var looked bool
	for _, r := range model.reqs {
		for _, m := range r.Messages {
			if m.Role == llm.RoleTool && strings.Contains(r.Messages[0].Parts[0].Text, "Characters with a reference picture") {
				looked = true
			}
		}
	}
	if !looked {
		t.Error("writing the picture's description made no lookup")
	}
	if p := model.reqs[len(model.reqs)-1].Messages[0].Parts[0].Text; !strings.Contains(p, "- Mai Linh (drawn in the source, page 2 of Huyết Long)") {
		t.Errorf("pictures list: %s", p)
	}
	ill := imager.reqs[before]
	if !ill.Continue || ill.Aspect != "portrait" || !strings.HasPrefix(ill.Refs[0].Label, "the previous illustration") {
		t.Errorf("button drawing: continue=%v aspect=%q refs=%d", ill.Continue, ill.Aspect, len(ill.Refs))
	}

	// Deleting the chat removes its pictures.
	p, _ := e.ImagePath(c.ID)
	e.Delete(context.Background(), c.ID)
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Error("pictures of a deleted chat kept")
	}
}

func TestNoDrawingWhenOff(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	model := &drawModel{}
	spaces := func(string) (rag.Space, error) { return rag.Space{}, nil }
	vec := &memVectors{}
	e := New(st, vec, &lore.Lore{St: st, Vec: vec, Spaces: spaces, PagesDir: dir}, spaces,
		func(string, string) (llm.Provider, error) { return model, nil },
		func() Options { return Options{RAGK: 4, ContextChars: 4000, Window: 40, MaxTokens: 2000} },
		func(string, any) {})
	imager := &fakeImager{}
	available := true
	e.SetImaging(Imaging{Imager: func() (llm.Imager, error) { return imager, nil }, Dir: dir,
		Available: func() bool { return available }, Auto: func() bool { return false }})
	card := store.Character{Name: "Mai Linh"}
	st.SaveCharacter(&card)
	c := store.Chat{CharacterID: card.ID, PersonaName: "Lâm Phong"}
	if err := e.Create(&c); err != nil {
		t.Fatal(err)
	}
	msg, _ := st.AddMessage(c.ID, "user", "chào")
	if _, err := e.generate(context.Background(), c.ID); err != nil {
		t.Fatal(err)
	}
	// No work and drawing off: no tools at all, and no word about pictures.
	r := model.reqs[len(model.reqs)-1]
	if len(r.Tools) != 0 || strings.Contains(r.System, "Illustrations") {
		t.Errorf("drawing offered while off: %d tools", len(r.Tools))
	}
	if len(imager.reqs) != 0 {
		t.Error("drew while off")
	}

	// The Tạo ảnh button still works on any message, the player's too.
	if err := e.Illustrate(c.ID, msg.ID); err != nil {
		t.Fatal(err)
	}
	var msgs []store.Message
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if msgs, _ = st.Messages(c.ID, false); strings.Contains(msgs[0].Content, "[Image:") {
			break
		}
	}
	if len(imager.reqs) != 1 || !strings.Contains(msgs[0].Content, "[Image: "+c.ID+"/") {
		t.Errorf("button drawing on the player's message: %d drawings, %q", len(imager.reqs), msgs[0].Content)
	}

	// With no image provider the button says so.
	available = false
	if err := e.Illustrate(c.ID, msg.ID); err == nil {
		t.Error("the button worked without an image provider")
	}
}
