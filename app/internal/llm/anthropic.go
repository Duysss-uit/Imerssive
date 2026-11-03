package llm

import (
	"cmp"
	"context"
	"encoding/json"
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
	t, err := p.Turn(ctx, r, onDelta)
	return t.Text, err
}

func (p *anthropicProvider) Turn(ctx context.Context, r Request, onDelta func(string)) (Turn, error) {
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
	for _, t := range r.Tools {
		tool := anthropic.ToolUnionParamOfTool(anthropic.ToolInputSchemaParam{Properties: t.Params, Required: t.Required}, t.Name)
		tool.OfTool.Description = anthropic.String(t.Description)
		params.Tools = append(params.Tools, tool)
	}
	if len(r.Tools) > 0 && r.NoMoreTools {
		params.ToolChoice = anthropic.ToolChoiceUnionParam{OfNone: &anthropic.ToolChoiceNoneParam{}}
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
			return Turn{Text: text}, err
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
	turn := Turn{Text: text}
	for _, b := range msg.Content {
		if b.Type == "tool_use" {
			turn.Calls = append(turn.Calls, ToolCall{ID: b.ID, Name: b.Name, Args: string(b.Input)})
		}
	}
	if err := stream.Err(); err != nil {
		return turn, err
	}
	if msg.StopReason == anthropic.StopReasonRefusal {
		return turn, fmt.Errorf("%w (%s)", ErrRefusal, msg.StopDetails.Category)
	}
	return turn, nil
}

func toAnthropicMessages(msgs []Message) []anthropic.MessageParam {
	out := make([]anthropic.MessageParam, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == RoleTool {
			// Tool results go back as user content; consecutive results share one message.
			var text string
			for _, part := range m.Parts {
				text += part.Text
			}
			block := anthropic.NewToolResultBlock(m.CallID, text, false)
			if n := len(out); n > 0 && isToolResults(out[n-1]) {
				out[n-1].OfUser.Content = append(out[n-1].OfUser.Content, block)
			} else {
				out = append(out, anthropic.NewUserMessage(block))
			}
			continue
		}
		blocks := make([]anthropic.ContentBlockParamUnion, 0, len(m.Parts)+len(m.Calls))
		for _, part := range m.Parts {
			if part.ImageB64 != "" {
				blocks = append(blocks, anthropic.NewImageBlockBase64(part.ImageMIME, part.ImageB64))
			} else if part.Text != "" {
				blocks = append(blocks, anthropic.NewTextBlock(part.Text))
			}
		}
		for _, c := range m.Calls {
			blocks = append(blocks, anthropic.NewToolUseBlock(c.ID, json.RawMessage(cmp.Or(c.Args, "{}")), c.Name))
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

func isToolResults(m anthropic.MessageParam) bool {
	if m.OfUser == nil || len(m.OfUser.Content) == 0 {
		return false
	}
	for _, b := range m.OfUser.Content {
		if b.OfToolResult == nil {
			return false
		}
	}
	return true
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
