package llm

import (
	"context"
	"time"
)

// Call is one finished model call, as seen by a logging wrapper.
type Call struct {
	Provider  string // display name of the provider
	Model     string
	Role      string // chat / summarize / vision
	Request   Request
	Response  string // full answer the model sent back (partial if it failed or was stopped)
	Reasoning string // the model's thinking, for reasoning models
	Err       error
	Started   time.Time
	Duration  time.Duration
}

// Logged wraps p so every Stream call is reported to record once it ends.
func Logged(p Provider, provider, model, role string, record func(Call)) Provider {
	return &logged{Provider: p, provider: provider, model: model, role: role, record: record}
}

type logged struct {
	Provider
	provider, model, role string
	record                func(Call)
}

func (l *logged) Stream(ctx context.Context, r Request, onDelta func(string)) (string, error) {
	start := time.Now()
	var got, thought []byte // what actually arrived, in case the provider returns "" on error
	onReasoning := r.OnReasoning
	r.OnReasoning = func(s string) {
		thought = append(thought, s...)
		if onReasoning != nil {
			onReasoning(s)
		}
	}
	out, err := l.Provider.Stream(ctx, r, func(d string) {
		got = append(got, d...)
		if onDelta != nil {
			onDelta(d)
		}
	})
	resp := out
	if resp == "" {
		resp = string(got)
	}
	l.record(Call{Provider: l.provider, Model: l.model, Role: l.role, Request: r,
		Response: resp, Reasoning: string(thought), Err: err, Started: start, Duration: time.Since(start)})
	return out, err
}
