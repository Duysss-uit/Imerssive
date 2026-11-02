package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestStreamSilentServer(t *testing.T) {
	release := make(chan struct{})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // never answers until the test ends
	})}
	go srv.Serve(ln)
	defer srv.Close()
	defer close(release)

	p, _ := New(Config{Kind: KindNvidia, BaseURL: "http://" + ln.Addr().String(), Model: "m"})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err = Complete(ctx, p, Request{Messages: []Message{TextMessage(RoleUser, "hi")}})
	if err == nil || !strings.Contains(err.Error(), "không phản hồi") || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want a clear no-reply error wrapping the deadline, got %v", err)
	}
}

// sse serves one canned streaming reply.
func sse(t *testing.T, chunks ...string) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return "http://" + ln.Addr().String()
}

func delta(field, s string, finish string) string {
	b, _ := json.Marshal(map[string]any{"choices": []map[string]any{{"delta": map[string]string{field: s}, "finish_reason": finish}}})
	return string(b)
}

func TestStreamReasoning(t *testing.T) {
	cases := map[string][]string{
		"reasoning_content": {delta("reasoning_content", "hmm ", ""), delta("reasoning_content", "ok", ""), delta("content", "Xin ", ""), delta("content", "chào", "stop")},
		"reasoning":         {delta("reasoning", "hmm ok", ""), delta("content", "Xin chào", "stop")},
		// inline tags, cut mid-tag across chunks
		"think tags": {delta("content", "<thi", ""), delta("content", "nk>hmm ok</th", ""), delta("content", "ink>Xin", ""), delta("content", " chào", "stop")},
	}
	for name, chunks := range cases {
		p, _ := New(Config{Kind: KindLocal, BaseURL: sse(t, chunks...), Model: "m"})
		var shown, thought string
		out, err := p.Stream(context.Background(), Request{OnReasoning: func(s string) { thought += s }}, func(d string) { shown += d })
		if err != nil || out != "Xin chào" || shown != "Xin chào" || strings.TrimSpace(thought) != "hmm ok" {
			t.Errorf("%s: out=%q shown=%q thought=%q err=%v", name, out, shown, thought, err)
		}
	}

	p, _ := New(Config{Kind: KindLocal, BaseURL: sse(t, delta("reasoning_content", "suy nghĩ mãi", "length")), Model: "m"})
	_, err := p.Stream(context.Background(), Request{MaxTokens: 4}, nil)
	if err == nil || !strings.Contains(err.Error(), "hết 4 token") {
		t.Errorf("want an out-of-tokens error, got %v", err)
	}
	// Cut long before max_tokens: the context window filled up.
	_, err = p.Stream(context.Background(), Request{MaxTokens: 1000}, nil)
	if err == nil || !strings.Contains(err.Error(), "context length") {
		t.Errorf("want a context-full error, got %v", err)
	}
}

func TestNoThinking(t *testing.T) {
	var bodies []map[string]any
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		bodies = append(bodies, b)
		if _, ok := b["chat_template_kwargs"]; ok && len(bodies) == 1 {
			http.Error(w, `{"error":"unknown field chat_template_kwargs"}`, http.StatusBadRequest)
			return
		}
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", delta("content", "ok", "stop"))
	})}
	go srv.Serve(ln)
	defer srv.Close()

	p, _ := New(Config{Kind: KindLocal, BaseURL: "http://" + ln.Addr().String(), Model: "m", NoThinking: true})
	out, err := p.Stream(context.Background(), Request{System: "sys", MaxTokens: 10}, nil)
	if err != nil || out != "ok" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if len(bodies) != 2 {
		t.Fatalf("want one retry after the 400, got %d requests", len(bodies))
	}
	first := bodies[0]
	if kw, _ := first["chat_template_kwargs"].(map[string]any); kw["enable_thinking"] != false {
		t.Errorf("thinking switch not sent: %v", first)
	}
	if sys := first["messages"].([]any)[0].(map[string]any)["content"]; sys != "sys\n/no_think" {
		t.Errorf("system prompt = %q", sys)
	}
	if _, ok := bodies[1]["chat_template_kwargs"]; ok {
		t.Error("retry should drop the thinking switch")
	}
}
