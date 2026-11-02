package llm

import (
	"context"
	"fmt"
	"sort"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
)

type anthropicProvider struct {
	cfg    Config
	client anthropic.Client
}

func newAnthropic(cfg Config) *anthropicProvider {
	opts := []option.RequestOption{option.WithAPIKey(cfg.APIKey)}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	return &anthropicProvider{cfg: cfg, client: anthropic.NewClient(opts...)}
}

// All current Claude models accept image input.
func (p *anthropicProvider) SupportsVision() bool { return true }

// Models that accept the server-side refusal fallback ("default" routing).
var fallbackModels = map[string]bool{
	"claude-fable-5-1":  true,
	"claude-opus-5-5":   true,
	"claude-opus-5":     true,
	"claude-sonnet-5-5": true,
}

func (p *anthropicProvider) Stream(ctx context.Context, r Request, onDelta func(string)) (string, error) {
	maxTokens := r.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 64000
	}
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(p.cfg.Model),
		MaxTokens: int64(maxTokens),
		Messages:  toAnthropicMessages(r.Messages),
	}
	if r.System != "" {
		// The system prompt (rules + character card) is stable across turns: cache it.
		params.System = []anthropic.TextBlockParam{{
			Text:         r.System,
			CacheControl: anthropic.NewCacheControlEphemeralParam(),
		}}
	}
	if p.cfg.Effort != "" {
		params.OutputConfig = anthropic.OutputConfigParam{Effort: anthropic.OutputConfigEffort(p.cfg.Effort)}
	}

	var reqOpts []option.RequestOption
	if fallbackModels[p.cfg.Model] {
		// A declined request is re-served by a fallback model inside the same call.
		reqOpts = append(reqOpts,
			option.WithHeaderAdd("anthropic-beta", "server-side-fallback-2026-07-01"),
			option.WithJSONSet("fallbacks", "default"),
		)
	}

	stream := p.client.Messages.NewStreaming(ctx, params, reqOpts...)
	msg := anthropic.Message{}
	var text string
	for stream.Next() {
		ev := stream.Current()
		if err := msg.Accumulate(ev); err != nil {
			return text, err
		}
		if d, ok := ev.AsAny().(anthropic.ContentBlockDeltaEvent); ok {
			if td, ok := d.Delta.AsAny().(anthropic.TextDelta); ok {
				text += td.Text
				if onDelta != nil {
					onDelta(td.Text)
				}
			}
		}
	}
	if err := stream.Err(); err != nil {
		return text, err
	}
	if msg.StopReason == anthropic.StopReasonRefusal {
		return text, fmt.Errorf("%w (%s)", ErrRefusal, msg.StopDetails.Category)
	}
	return text, nil
}

func toAnthropicMessages(msgs []Message) []anthropic.MessageParam {
	out := make([]anthropic.MessageParam, 0, len(msgs))
	for _, m := range msgs {
		blocks := make([]anthropic.ContentBlockParamUnion, 0, len(m.Parts))
		for _, part := range m.Parts {
			if part.ImageB64 != "" {
				blocks = append(blocks, anthropic.NewImageBlockBase64(part.ImageMIME, part.ImageB64))
			} else if part.Text != "" {
				blocks = append(blocks, anthropic.NewTextBlock(part.Text))
			}
		}
		if len(blocks) == 0 {
			continue
		}
		if m.Role == RoleAssistant {
			out = append(out, anthropic.NewAssistantMessage(blocks...))
		} else {
			out = append(out, anthropic.NewUserMessage(blocks...))
		}
	}
	return out
}

func (p *anthropicProvider) ListModels(ctx context.Context) ([]string, error) {
	iter := p.client.Models.ListAutoPaging(ctx, anthropic.ModelListParams{})
	var ids []string
	for iter.Next() {
		ids = append(ids, iter.Current().ID)
	}
	if err := iter.Err(); err != nil {
		return nil, err
	}
	sort.Strings(ids)
	return ids, nil
}
