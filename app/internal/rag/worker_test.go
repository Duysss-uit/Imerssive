package rag_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"immersive/internal/config"
	"immersive/internal/rag"
	"immersive/internal/worker"
)

// TestHybridSearchThroughWorker runs retrieval in the real Python worker. The
// space is an "API" one with hand-made vectors, so no embedding model loads.
func TestHybridSearchThroughWorker(t *testing.T) {
	dir, python := config.FindWorker()
	if dir == "" || python == "python" {
		t.Skip("worker/.venv not installed")
	}
	w := worker.New(worker.Options{Python: python, Dir: dir, IdleFor: time.Minute,
		Env: []string{"IMM_CHROMA_DIR=" + filepath.Join(t.TempDir(), "chroma")}})
	defer w.Stop()
	r := rag.New(w)
	ctx := context.Background()

	vec := map[string][]float32{"lâm phong": {0, 1, 0}, "bão": {0, 0, 1}}
	sp := rag.APISpace("p", "fake", func(_ context.Context, texts []string, _ bool) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i, t := range texts {
			out[i] = []float32{1, 0, 0}
			for word, v := range vec {
				if strings.Contains(strings.ToLower(t), word) {
					out[i] = v
				}
			}
		}
		return out, nil
	})
	meta := func(kb string) map[string]any { return map[string]any{"kb_id": kb, "level": rag.LevelRaw} }
	err := r.AddTexts(ctx, sp, []rag.Doc{
		{ID: "a", Text: "Mai Linh bị bắt tới núi Hắc Sơn.", Metadata: meta("k1")},
		{ID: "b", Text: "Lâm Phong luyện kiếm Huyết Long.", Metadata: meta("k1")},
		{ID: "c", Text: "Làng Thanh Vân trước cơn bão.", Metadata: meta("k1")},
		{ID: "d", Text: "Hắc Sơn trong một bộ truyện khác.", Metadata: meta("k2")},
	})
	if err != nil {
		t.Fatal(err)
	}
	spaces := func(string) (rag.Space, error) { return sp, nil }
	// "Hắc Sơn" embeds like "a" and "d"; only k1 may answer.
	hits, err := r.SearchKBs(ctx, spaces, "Hắc Sơn", rag.Filter{KBIDs: []string{"k1"}}, 2)
	if err != nil || len(hits) == 0 || hits[0].ID != "a" {
		t.Fatalf("hits=%+v err=%v", hits, err)
	}
	for _, h := range hits {
		if h.Str("kb_id") != "k1" {
			t.Errorf("hit from another work: %+v", h)
		}
	}
}
