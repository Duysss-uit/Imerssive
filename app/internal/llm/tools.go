package llm

import (
	"context"
	"errors"
)

// RoleTool marks a message carrying the result of a tool call (Message.CallID).
const RoleTool Role = "tool"

// Tool is a function the model may ask the app to run.
type Tool struct {
	Name        string
	Description string
	Params      map[string]any // JSON Schema "properties"
	Required    []string
}

// ToolCall is one call the model asked for; Args is the raw JSON object.
type ToolCall struct {
	ID   string
	Name string
	Args string
}

// Turn is one model reply: text, and the tool calls it wants run before it continues.
type Turn struct {
	Text  string
	Calls []ToolCall
}

// ToolUser is implemented by providers that support tool calling. The
// conversation continues with an assistant Message carrying Calls, followed by
// one RoleTool Message per call.
type ToolUser interface {
	Turn(ctx context.Context, req Request, onDelta func(string)) (Turn, error)
}

// ErrNoTools means the provider or model can't call tools.
var ErrNoTools = errors.New("model không hỗ trợ gọi công cụ")

// RunTurn calls p with tools when it supports them.
func RunTurn(ctx context.Context, p Provider, req Request) (Turn, error) {
	if tu, ok := p.(ToolUser); ok {
		return tu.Turn(ctx, req, nil)
	}
	return Turn{}, ErrNoTools
}
