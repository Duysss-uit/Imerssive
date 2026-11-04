// Package llm defines a provider-neutral chat interface. Each backend
// (OpenAI-compatible, Anthropic) implements Provider; the rest of the app
// never talks to a vendor SDK directly.
package llm

import (
	"context"
	"errors"
)

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Part is one piece of message content: either text or an inline image.
type Part struct {
	Text      string
	ImageB64  string // base64 without data: prefix
	ImageMIME string // e.g. image/png
}

type Message struct {
	Role   Role
	Parts  []Part
	Calls  []ToolCall // assistant: tool calls it made
	CallID string     // RoleTool: the call this result answers
}

func TextMessage(role Role, text string) Message {
	return Message{Role: role, Parts: []Part{{Text: text}}}
}

type Request struct {
	System    string
	Messages  []Message
	MaxTokens int
	Purpose   string // what the call is for; only used in logs
	// OnReasoning receives a reasoning model's thinking as it streams (may be nil).
	OnReasoning func(string)
	// Tools the model may call (providers implementing ToolUser). With
	// NoMoreTools set the model must answer without calling them.
	Tools       []Tool
	NoMoreTools bool
}

type Provider interface {
	// Stream generates a reply, calling onDelta for every text chunk (onDelta may be nil).
	// It returns the full text.
	Stream(ctx context.Context, req Request, onDelta func(string)) (string, error)
	ListModels(ctx context.Context) ([]string, error)
	SupportsVision() bool
}

// Complete is a non-streaming convenience wrapper.
func Complete(ctx context.Context, p Provider, req Request) (string, error) {
	return p.Stream(ctx, req, nil)
}

var ErrRefusal = errors.New("model declined to answer")

type Kind string

const (
	KindOpenAI     Kind = "openai"
	KindAnthropic  Kind = "anthropic"
	KindOpenRouter Kind = "openrouter"
	KindNvidia     Kind = "nvidia"
	KindLocal      Kind = "local"
)

// Config is everything needed to build a Provider.
type Config struct {
	Kind        Kind
	BaseURL     string
	Model       string
	APIKey      string
	Vision      bool
	Temperature float64 // 0 = provider default (ignored by Anthropic)
	Effort      string  // Anthropic only: low|medium|high|xhigh|max, empty = default
	NoThinking  bool    // ask a reasoning model to answer without thinking (OpenAI-compatible kinds)
	// ContextWindow is the model's context length in tokens (0 = unknown);
	// prompts are sized to fit it (see Budget).
	ContextWindow int
}

// DefaultBaseURL returns the preset endpoint for a kind.
func DefaultBaseURL(k Kind) string {
	switch k {
	case KindOpenAI:
		return "https://api.openai.com/v1"
	case KindOpenRouter:
		return "https://openrouter.ai/api/v1"
	case KindNvidia:
		return "https://integrate.api.nvidia.com/v1"
	case KindLocal:
		return "http://localhost:11434/v1" // Ollama; LM Studio uses http://localhost:1234/v1
	}
	return ""
}

func New(cfg Config) (Provider, error) {
	if cfg.Model == "" {
		return nil, errors.New("provider has no model configured")
	}
	switch cfg.Kind {
	case KindAnthropic:
		return newAnthropic(cfg), nil
	case KindOpenAI, KindOpenRouter, KindNvidia, KindLocal:
		if cfg.BaseURL == "" {
			cfg.BaseURL = DefaultBaseURL(cfg.Kind)
		}
		return newOpenAICompat(cfg), nil
	}
	return nil, errors.New("unknown provider kind: " + string(cfg.Kind))
}
