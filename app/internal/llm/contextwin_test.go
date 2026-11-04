package llm

import (
	"context"
	"net"
	"net/http"
	"testing"
)

// serve runs h on a local port and returns its base URL.
func serve(t *testing.T, h http.Handler) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return "http://" + ln.Addr().String()
}

func TestDetectContext(t *testing.T) {
	mux := http.NewServeMux()
	// LM Studio: the loaded length wins over the model's maximum.
	mux.HandleFunc("/api/v0/models/qwen", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":"qwen","max_context_length":131072,"loaded_context_length":8192}`))
	})
	// OpenRouter-style listing.
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"data":[{"id":"other","context_length":1000},{"id":"big","context_length":200000},{"id":"quiet"}]}`))
	})
	base := serve(t, mux) + "/v1"
	ctx := context.Background()
	if n := DetectContext(ctx, Config{Kind: KindLocal, BaseURL: base, Model: "qwen"}); n != 8192 {
		t.Errorf("LM Studio = %d", n)
	}
	if n := DetectContext(ctx, Config{Kind: KindOpenRouter, BaseURL: base, Model: "big"}); n != 200000 {
		t.Errorf("listing = %d", n)
	}
	if n := DetectContext(ctx, Config{Kind: KindNvidia, BaseURL: base, Model: "quiet"}); n != 0 {
		t.Errorf("unknown = %d", n)
	}
}

func TestBudget(t *testing.T) {
	small := newOpenAICompat(Config{ContextWindow: 4096})
	out, in := Budget(small, 6000)
	if out != 4096*2/5 || in != (4096-out-300)*CharsPerToken {
		t.Errorf("4096: out=%d in=%d", out, in)
	}
	if out, _ := Budget(newOpenAICompat(Config{ContextWindow: 128000}), 6000); out != 6000 {
		t.Errorf("large context should keep the wanted reply length, got %d", out)
	}
}
