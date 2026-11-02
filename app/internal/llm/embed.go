package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Embedder turns text into vectors through a provider's API. query selects the
// query side for asymmetric models (NVIDIA retrieval models need it).
type Embedder interface {
	Embed(ctx context.Context, texts []string, query bool) ([][]float32, error)
}

// NewEmbedder builds an embedder for cfg.Model on an OpenAI-compatible
// /embeddings endpoint. Anthropic has no embeddings API.
func NewEmbedder(cfg Config) (Embedder, error) {
	if cfg.Model == "" {
		return nil, errors.New("chưa chọn model embedding")
	}
	switch cfg.Kind {
	case KindOpenAI, KindOpenRouter, KindNvidia, KindLocal:
		if cfg.BaseURL == "" {
			cfg.BaseURL = DefaultBaseURL(cfg.Kind)
		}
		return newOpenAICompat(cfg), nil
	case KindAnthropic:
		return nil, errors.New("Claude không có API embedding — hãy chọn OpenAI, OpenRouter, NVIDIA hoặc Local")
	}
	return nil, errors.New("unknown provider kind: " + string(cfg.Kind))
}

const embedBatch = 32

func (p *openAICompat) Embed(ctx context.Context, texts []string, query bool) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for i := 0; i < len(texts); i += embedBatch {
		vecs, err := p.embedBatch(ctx, texts[i:min(i+embedBatch, len(texts))], query)
		if err != nil {
			return nil, err
		}
		out = append(out, vecs...)
	}
	return out, nil
}

func (p *openAICompat) embedBatch(ctx context.Context, texts []string, query bool) ([][]float32, error) {
	body := map[string]any{"model": p.cfg.Model, "input": texts, "encoding_format": "float"}
	if p.cfg.Kind == KindNvidia {
		body["input_type"] = map[bool]string{true: "query", false: "passage"}[query]
		body["truncate"] = "END"
	}
	req, err := p.newRequest(ctx, http.MethodPost, "/embeddings", body)
	if err != nil {
		return nil, err
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("%s embeddings %d: %s", p.cfg.Kind, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var res struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
	}
	if len(res.Data) != len(texts) {
		return nil, fmt.Errorf("%s embeddings: nhận %d vector cho %d đoạn", p.cfg.Kind, len(res.Data), len(texts))
	}
	vecs := make([][]float32, len(texts))
	for _, d := range res.Data {
		if d.Index < 0 || d.Index >= len(vecs) {
			return nil, fmt.Errorf("%s embeddings: index %d ngoài phạm vi", p.cfg.Kind, d.Index)
		}
		vecs[d.Index] = d.Embedding
	}
	return vecs, nil
}
