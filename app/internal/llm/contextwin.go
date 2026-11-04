package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// Context windows used when a provider's can't be found out. Local servers
// load models small by default (LM Studio: 4096), so assume little there.
const (
	DefaultContextLocal  = 4096
	DefaultContextRemote = 32768
	DefaultContextClaude = 200000
)

// CharsPerToken is a cautious estimate for prompt sizing: Vietnamese and
// Japanese text takes more tokens per character than English.
const CharsPerToken = 2

// ContextTokens is the context window p was configured with (detected or set
// by the user), or 0 when unknown.
func ContextTokens(p Provider) int {
	if c, ok := p.(interface{ ContextTokens() int }); ok {
		return c.ContextTokens()
	}
	return 0
}

func (p *openAICompat) ContextTokens() int      { return p.cfg.ContextWindow }
func (p *anthropicProvider) ContextTokens() int { return p.cfg.ContextWindow }
func (l *logged) ContextTokens() int            { return ContextTokens(l.Provider) }

// Budget splits p's context window between the prompt and the reply.
// maxTokens is wantOut capped so that the reply (thinking included) leaves
// room for a prompt; inChars is how many characters of prompt then fit.
func Budget(p Provider, wantOut int) (maxTokens, inChars int) {
	ctx := ContextTokens(p)
	if ctx <= 0 {
		ctx = DefaultContextRemote
	}
	maxTokens = min(wantOut, ctx*2/5)
	if wantOut <= 0 {
		maxTokens = ctx * 2 / 5
	}
	in := ctx - maxTokens - 300 // chat template and tool definitions
	return maxTokens, max(in*CharsPerToken, 1500)
}

// DetectContext asks the server how long a context the model has (loaded
// with, when the server says). It returns 0 when the server doesn't tell.
func DetectContext(ctx context.Context, cfg Config) int {
	if cfg.Kind == KindAnthropic {
		return DefaultContextClaude
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL(cfg.Kind)
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	p := newOpenAICompat(cfg)
	base := p.cfg.BaseURL
	root := strings.TrimSuffix(base, "/v1")

	// LM Studio's own API knows the length the model is loaded with.
	if cfg.Kind == KindLocal {
		var m map[string]any
		if getJSON(ctx, p, root+"/api/v0/models/"+cfg.Model, &m) == nil {
			if n := firstInt(m, "loaded_context_length", "max_context_length"); n > 0 {
				return n
			}
		}
	}
	// OpenRouter, vLLM and others list it with the models.
	var list struct {
		Data []map[string]any `json:"data"`
	}
	if getJSON(ctx, p, base+"/models", &list) == nil {
		for _, m := range list.Data {
			if id, _ := m["id"].(string); id == cfg.Model {
				if n := firstInt(m, "loaded_context_length", "context_length", "max_model_len", "context_window", "max_context_length"); n > 0 {
					return n
				}
				if tp, ok := m["top_provider"].(map[string]any); ok {
					if n := firstInt(tp, "context_length"); n > 0 {
						return n
					}
				}
			}
		}
	}
	return 0
}

func getJSON(ctx context.Context, p *openAICompat, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if p.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return &statusError{p.cfg.Kind, resp.StatusCode, ""}
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func firstInt(m map[string]any, keys ...string) int {
	for _, k := range keys {
		if f, ok := m[k].(float64); ok && f > 0 {
			return int(f)
		}
	}
	return 0
}
