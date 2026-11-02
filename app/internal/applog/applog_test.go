package applog

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"immersive/internal/llm"
)

type fakeProvider struct{ llm.Provider }

func (fakeProvider) Stream(_ context.Context, _ llm.Request, onDelta func(string)) (string, error) {
	onDelta("[Narrator]\nMưa ")
	onDelta("rơi.")
	return "", errors.New("stream cut") // returns nothing, like a failed stream
}

func TestModelLog(t *testing.T) {
	dir := t.TempDir()
	l, err := Setup(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { l.Close(); log.SetOutput(os.Stderr) }()

	p := llm.Logged(fakeProvider{}, "Test", "m-1", "chat", func(c llm.Call) { l.Model(c, ModelFull) })
	var shown string
	p.Stream(context.Background(), llm.Request{Purpose: "chat · thử", System: "SYS-PROMPT",
		Messages: []llm.Message{
			llm.TextMessage(llm.RoleUser, "xin chào"),
			{Role: llm.RoleUser, Parts: []llm.Part{{ImageB64: strings.Repeat("A", 4096), ImageMIME: "image/png"}}},
		}}, func(d string) { shown += d })
	if shown != "[Narrator]\nMưa rơi." {
		t.Fatalf("deltas not passed through: %q", shown)
	}

	day := time.Now().Format("2006-01-02")
	model, _ := os.ReadFile(filepath.Join(dir, "model-"+day+".log"))
	for _, want := range []string{"chat · thử", "Test · m-1", "SYS-PROMPT", "xin chào", "[ảnh image/png, ~3 KB]", "Mưa rơi.", "LỖI: stream cut"} {
		if !strings.Contains(string(model), want) {
			t.Errorf("model log missing %q:\n%s", want, model)
		}
	}
	activity, _ := os.ReadFile(filepath.Join(dir, "app-"+day+".log"))
	if !strings.Contains(string(activity), "model: chat · thử") {
		t.Errorf("activity log missing model summary:\n%s", activity)
	}

	l.Model(llm.Call{Request: llm.Request{Purpose: "chỉ phản hồi", System: "SECRET-SYS"}, Response: "ok"}, ModelResponse)
	model, _ = os.ReadFile(filepath.Join(dir, "model-"+day+".log"))
	if strings.Contains(string(model), "SECRET-SYS") || !strings.Contains(string(model), "chỉ phản hồi") {
		t.Errorf("response level should log the reply but not the prompt:\n%s", model)
	}
}
