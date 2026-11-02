package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"
)

// openAICompat covers every backend speaking the OpenAI Chat Completions
// protocol: OpenAI, OpenRouter, NVIDIA NIM, Ollama, LM Studio, vLLM...
type openAICompat struct {
	cfg  Config
	http *http.Client
}

func newOpenAICompat(cfg Config) *openAICompat {
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	// A healthy server answers a streaming request with headers right away; one that
	// stays silent this long is overloaded or stuck, so fail instead of waiting minutes.
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = 3 * time.Minute
	return &openAICompat{cfg: cfg, http: &http.Client{Timeout: 15 * time.Minute, Transport: tr}}
}

func (p *openAICompat) SupportsVision() bool { return p.cfg.Vision }

func (p *openAICompat) newRequest(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.cfg.BaseURL+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if p.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	}
	if p.cfg.Kind == KindOpenRouter {
		req.Header.Set("X-Title", "Immersive")
	}
	return req, nil
}

func (p *openAICompat) Stream(ctx context.Context, r Request, onDelta func(string)) (string, error) {
	msgs := make([]map[string]any, 0, len(r.Messages)+1)
	system := r.System
	if p.cfg.NoThinking && p.cfg.Kind == KindLocal {
		system = strings.TrimSpace(system + "\n/no_think") // Qwen-style soft switch; other models ignore it
	}
	if system != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": system})
	}
	for _, m := range r.Messages {
		msgs = append(msgs, map[string]any{"role": string(m.Role), "content": encodeParts(m.Parts)})
	}
	body := map[string]any{"model": p.cfg.Model, "messages": msgs, "stream": true}
	if p.cfg.Temperature > 0 {
		body["temperature"] = p.cfg.Temperature
	}
	if r.MaxTokens > 0 {
		if p.cfg.Kind == KindOpenAI {
			body["max_completion_tokens"] = r.MaxTokens
		} else {
			body["max_tokens"] = r.MaxTokens
		}
	}

	if p.cfg.NoThinking {
		// Each server family has its own switch; unknown fields are ignored by most.
		switch p.cfg.Kind {
		case KindOpenRouter:
			body["reasoning"] = map[string]any{"enabled": false}
		case KindOpenAI:
			body["reasoning_effort"] = "none"
		default: // NVIDIA NIM, vLLM, LM Studio, Ollama: passed to the chat template
			body["chat_template_kwargs"] = map[string]any{"enable_thinking": false, "thinking": false}
		}
	}

	resp, err := p.post(ctx, body)
	// A 400 about an optional field (token budget above this model's limit, or a
	// thinking switch it doesn't know): drop the optional fields and try once more.
	if e := (*statusError)(nil); errors.As(err, &e) && e.code == http.StatusBadRequest &&
		(strings.Contains(e.body, "max_") || strings.Contains(e.body, "reasoning") || strings.Contains(e.body, "chat_template")) {
		for _, k := range []string{"max_tokens", "max_completion_tokens", "reasoning", "reasoning_effort", "chat_template_kwargs"} {
			delete(body, k)
		}
		resp, err = p.post(ctx, body)
	}
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	// Reasoning models send their thinking apart from the answer: in a
	// reasoning_content / reasoning field (NVIDIA, LM Studio, OpenRouter...) or
	// inline as <think>...</think>. Only the answer is returned.
	var out strings.Builder
	var thinking int
	var finish string
	think := thinkSplitter{}
	emit := func(content, reasoning string) {
		if reasoning != "" {
			thinking += len([]rune(reasoning))
			if r.OnReasoning != nil {
				r.OnReasoning(reasoning)
			}
		}
		if content != "" {
			out.WriteString(content)
			if onDelta != nil {
				onDelta(content)
			}
		}
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					Reasoning        string `json:"reasoning"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if chunk.Error != nil {
			return out.String(), fmt.Errorf("%s: %s", p.cfg.Kind, chunk.Error.Message)
		}
		for _, c := range chunk.Choices {
			emit("", c.Delta.ReasoningContent+c.Delta.Reasoning)
			emit(think.feed(c.Delta.Content))
			if c.FinishReason != "" {
				finish = c.FinishReason
			}
			if c.FinishReason == "content_filter" {
				return out.String(), ErrRefusal
			}
		}
	}
	emit(think.flush())
	if err := sc.Err(); err != nil {
		return out.String(), err
	}
	if strings.TrimSpace(out.String()) == "" && thinking > 0 {
		if finish == "length" {
			// "length" also means the context window filled up (prompt + output),
			// which local servers load small by default. ~3 chars per token.
			if used := thinking / 3; r.MaxTokens == 0 || used < r.MaxTokens*2/3 {
				return "", fmt.Errorf("model bị cắt sau khoảng %d token suy nghĩ, còn xa giới hạn max_tokens: nhiều khả năng ngữ cảnh (context length) của model đã đầy vì prompt dài. Với LM Studio/Ollama, nạp lại model với Context Length lớn hơn (từ 16384 trở lên), hoặc bật “Tắt suy nghĩ” cho provider này trong Cài đặt", used)
			}
			return "", fmt.Errorf("model dùng hết %d token cho phần suy nghĩ (%d ký tự) nên chưa kịp trả lời: bật “Tắt suy nghĩ” cho provider này trong Cài đặt, hoặc tăng max tokens", r.MaxTokens, thinking)
		}
		return "", fmt.Errorf("model chỉ trả về phần suy nghĩ (%d ký tự), không có câu trả lời", thinking)
	}
	return out.String(), nil
}

type statusError struct {
	kind Kind
	code int
	body string
}

func (e *statusError) Error() string { return fmt.Sprintf("%s %d: %s", e.kind, e.code, e.body) }

// post sends a streaming chat request and returns the open 2xx response.
func (p *openAICompat) post(ctx context.Context, body map[string]any) (*http.Response, error) {
	req, err := p.newRequest(ctx, http.MethodPost, "/chat/completions", body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	start := time.Now()
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, p.noReply(err, time.Since(start))
	}
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, &statusError{p.cfg.Kind, resp.StatusCode, strings.TrimSpace(string(b))}
	}
	return resp, nil
}

// thinkSplitter separates inline <think>...</think> text from the answer in a
// stream whose chunks may cut a tag in half.
type thinkSplitter struct {
	in  bool
	buf string
}

// feed returns the answer and thinking text that can be released so far.
func (t *thinkSplitter) feed(s string) (content, reasoning string) {
	t.buf += s
	var c, r strings.Builder
	for {
		tag := "<think>"
		dst := &c
		if t.in {
			tag, dst = "</think>", &r
		}
		if i := strings.Index(t.buf, tag); i >= 0 {
			dst.WriteString(t.buf[:i])
			t.buf, t.in = t.buf[i+len(tag):], !t.in
			continue
		}
		// Hold back a tail that could be the start of the tag.
		keep := 0
		for n := min(len(tag)-1, len(t.buf)); n > 0; n-- {
			if strings.HasSuffix(t.buf, tag[:n]) {
				keep = n
				break
			}
		}
		dst.WriteString(t.buf[:len(t.buf)-keep])
		t.buf = t.buf[len(t.buf)-keep:]
		return c.String(), r.String()
	}
}

func (t *thinkSplitter) flush() (content, reasoning string) {
	s := t.buf
	t.buf = ""
	if t.in {
		return "", s
	}
	return s, ""
}

// noReply explains a request that got no answer at all, keeping the cause.
func (p *openAICompat) noReply(err error, waited time.Duration) error {
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return fmt.Errorf("%s (%s) không phản hồi sau %s: server đang quá tải hoặc model đang khởi động, thử lại sau hoặc chọn model khác (%w)",
			p.cfg.Kind, p.cfg.Model, waited.Round(time.Second), err)
	}
	return err
}

func encodeParts(parts []Part) any {
	textOnly := true
	for _, p := range parts {
		if p.ImageB64 != "" {
			textOnly = false
		}
	}
	if textOnly {
		var sb strings.Builder
		for i, p := range parts {
			if i > 0 {
				sb.WriteString("\n\n")
			}
			sb.WriteString(p.Text)
		}
		return sb.String()
	}
	arr := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		if p.ImageB64 != "" {
			arr = append(arr, map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": "data:" + p.ImageMIME + ";base64," + p.ImageB64},
			})
		} else {
			arr = append(arr, map[string]any{"type": "text", "text": p.Text})
		}
	}
	return arr
}

func (p *openAICompat) ListModels(ctx context.Context) ([]string, error) {
	req, err := p.newRequest(ctx, http.MethodGet, "/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("%s %d: %s", p.cfg.Kind, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(list.Data))
	for _, m := range list.Data {
		ids = append(ids, m.ID)
	}
	sort.Strings(ids)
	return ids, nil
}
