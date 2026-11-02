// Package integration exercises the real Go ↔ Python worker pipeline with a
// scripted LLM. Run with:
//
//	IMM_TEST_PDF=path/to/sample.pdf go test ./internal/integration -v
//
// Requires worker/.venv to be installed. Skipped otherwise.
package integration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"immersive/internal/chat"
	"immersive/internal/config"
	"immersive/internal/ingest"
	"immersive/internal/llm"
	"immersive/internal/rag"
	"immersive/internal/store"
	"immersive/internal/worker"
)

type fakeLLM struct {
	mu   sync.Mutex
	last llm.Request
}

func (f *fakeLLM) SupportsVision() bool                         { return true }
func (f *fakeLLM) ListModels(context.Context) ([]string, error) { return nil, nil }

func (f *fakeLLM) Stream(_ context.Context, r llm.Request, onDelta func(string)) (string, error) {
	f.mu.Lock()
	f.last = r
	f.mu.Unlock()
	var prompt string
	for _, p := range r.Messages[len(r.Messages)-1].Parts {
		prompt += p.Text
	}
	var out string
	switch {
	case strings.Contains(prompt, "comic/manga"):
		out = "=== PAGE 3 ===\nMột trang toàn màu đỏ, Lâm Phong hét lên: \"Mai Linh!\""
	case strings.Contains(prompt, `"entities"`):
		out = "```json\n" + `{"summary":"Ma tộc tấn công làng Thanh Vân; Mai Linh bị bắt tới núi Hắc Sơn.",
		"entities":[{"name":"Mai Linh","type":"character","description":"Thanh mai trúc mã của Lâm Phong, bị bắt tới núi Hắc Sơn."},
		{"name":"Lâm Phong","type":"character","description":"Kiếm sĩ trẻ, dùng kiếm Huyết Long."}]}` + "\n```"
	case strings.Contains(prompt, "character card"):
		out = `{"name":"Lâm Phong","tagline":"Kiếm sĩ làng Thanh Vân","greeting":"*rút kiếm* Ai đó?"}`
	case strings.Contains(prompt, "long-term memory"):
		out = "- Đã nói chuyện về Mai Linh"
	case strings.Contains(prompt, "overview"):
		out = "Tổng quan: câu chuyện về Lâm Phong đi cứu Mai Linh."
	default:
		out = "*nắm chặt kiếm* Mai Linh đang ở núi Hắc Sơn."
	}
	if onDelta != nil {
		for _, w := range strings.SplitAfter(out, " ") {
			onDelta(w)
		}
	}
	return out, nil
}

func TestPipeline(t *testing.T) {
	pdf := os.Getenv("IMM_TEST_PDF")
	dir, python := config.FindWorker()
	if pdf == "" || dir == "" || python == "python" {
		t.Skip("set IMM_TEST_PDF and install worker/.venv")
	}
	tmp := t.TempDir()
	st, err := store.Open(filepath.Join(tmp, "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	models, _ := os.UserConfigDir()
	w := worker.New(worker.Options{Python: python, Dir: dir, IdleFor: time.Minute, Env: []string{
		"IMM_CHROMA_DIR=" + filepath.Join(tmp, "chroma"),
		"IMM_MODELS_DIR=" + filepath.Join(models, "Immersive", "models"),
	}})
	defer w.Stop()
	r := rag.New(w)
	fake := &fakeLLM{}
	resolve := func(string, string) (llm.Provider, error) { return fake, nil }
	local := func(string) (rag.Space, error) { return rag.Space{}, nil }

	events := make(chan store.Source, 64)
	chatEvents := make(chan string, 256)
	emit := func(ev string, data any) {
		if s, ok := data.(store.Source); ok {
			events <- s
		} else {
			chatEvents <- ev
		}
	}
	os.MkdirAll(filepath.Join(tmp, "src"), 0o755)
	os.MkdirAll(filepath.Join(tmp, "pages"), 0o755)
	ing := ingest.New(st, w, r, local, resolve, func() ingest.Options {
		return ingest.Options{SectionChars: 4000, ChunkChars: 800, KeepRaw: true, Concurrency: 2, VisionBatch: 4}
	}, filepath.Join(tmp, "src"), filepath.Join(tmp, "pages"), emit)

	kb := store.KnowledgeBase{Name: "Huyết Long Kiếm"}
	st.SaveKnowledgeBase(&kb)
	src, err := ing.AddSource(kb.ID, pdf, "both")
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.After(5 * time.Minute)
wait:
	for {
		select {
		case s := <-events:
			t.Logf("ingest: %s %s %s", s.Status, s.Progress, s.Error)
			if s.ID == src.ID && s.Status == "done" {
				break wait
			}
			if s.Status == "error" {
				t.Fatalf("ingest failed: %s", s.Error)
			}
		case <-deadline:
			t.Fatal("ingest timed out")
		}
	}

	ctx := context.Background()
	hits, err := r.SearchTexts(ctx, rag.Space{}, "Mai Linh ở đâu?", rag.Filter{KBIDs: []string{kb.ID}}, 20)
	if err != nil {
		t.Fatal(err)
	}
	levels := map[string]int{}
	for _, h := range hits {
		levels[h.Str("level")]++
	}
	t.Logf("levels: %v", levels)
	for _, lv := range []string{rag.LevelRaw, rag.LevelSection, rag.LevelEntity, rag.LevelOverview} {
		if levels[lv] == 0 {
			t.Errorf("no %s documents stored", lv)
		}
	}
	imgs, err := r.SearchImages(ctx, "red page", rag.Filter{KBIDs: []string{kb.ID}}, 2)
	if err != nil || len(imgs) != 1 {
		t.Errorf("image search: %v %v", imgs, err)
	}
	if kb2, _ := st.KnowledgeBase(kb.ID); kb2.Overview == "" {
		t.Error("KB overview not set")
	}

	// Character generation
	c, err := ing.GenerateCharacter(ctx, []string{kb.ID}, "Lâm Phong", "", "")
	if err != nil || c.Greeting == "" {
		t.Fatalf("generate character: %+v %v", c, err)
	}
	st.SaveCharacter(&c)

	// Chat turn: retrieved memory must be attached to the last user message
	eng := chat.New(st, r, local, resolve, func() chat.Options {
		return chat.Options{RAGK: 6, ContextChars: 8000, Window: 40, MaxTokens: 512}
	}, emit)
	ch := store.Chat{CharacterID: c.ID, PersonaName: "Tiểu Vũ"}
	if err := eng.Create(&ch); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Send(ch.ID, "Mai Linh bị bắt đi đâu rồi?"); err != nil {
		t.Fatal(err)
	}
	for ev := range chatEvents {
		if ev == "chat:error" {
			t.Fatal("chat error")
		}
		if ev == "chat:done" {
			break
		}
	}
	fake.mu.Lock()
	req := fake.last
	fake.mu.Unlock()
	lastParts := req.Messages[len(req.Messages)-1].Parts
	if !strings.Contains(lastParts[0].Text, "<memory>") || !strings.Contains(lastParts[0].Text, "Hắc Sơn") {
		t.Errorf("memory not attached: %q", lastParts[0].Text)
	}
	if strings.Contains(req.System, "Hắc Sơn") || !strings.Contains(req.System, "Lâm Phong") {
		t.Error("system prompt should hold the card but not retrieved memory")
	}
	msgs, _ := st.Messages(ch.ID, false)
	if len(msgs) != 3 || msgs[2].Role != "assistant" {
		t.Errorf("expected greeting, user, reply; got %d messages", len(msgs))
	}
}
