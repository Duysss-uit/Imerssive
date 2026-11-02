// Package applog writes the app's logs under <data>/logs, one file per day:
//
//	app-YYYY-MM-DD.log    activity: everything written with the standard log package
//	model-YYYY-MM-DD.log  every model call: what was sent and the full reply
//
// Files older than KeepDays are deleted at startup.
package applog

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"immersive/internal/llm"
)

const KeepDays = 14

// Model log detail levels (setting "log.model").
const (
	ModelFull     = "full"     // prompt, history and reply
	ModelResponse = "response" // reply and call metadata only
	ModelOff      = "off"
)

type Logs struct {
	Dir   string
	app   *daily
	model *daily
}

// Setup creates the log directory, prunes old files and routes the standard
// logger to the activity log (and stderr, for `wails dev`).
func Setup(dir string) (*Logs, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	prune(dir)
	l := &Logs{Dir: dir, app: &daily{dir: dir, prefix: "app"}, model: &daily{dir: dir, prefix: "model"}}
	log.SetOutput(io.MultiWriter(os.Stderr, l.app))
	return l, nil
}

func (l *Logs) Close() {
	l.app.close()
	l.model.close()
}

// Model appends one model call to the model log at the given detail level.
func (l *Logs) Model(c llm.Call, level string) {
	if level == ModelOff {
		return
	}
	status := "OK"
	if c.Err != nil {
		status = "LỖI: " + c.Err.Error()
	}
	log.Printf("model: %s · %s/%s · %s · %d ký tự · %s", c.Request.Purpose, c.Provider, c.Model,
		c.Duration.Round(100*time.Millisecond), len([]rune(c.Response)), status)

	var b strings.Builder
	fmt.Fprintf(&b, "==================== %s ====================\n", c.Started.Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&b, "Mục đích : %s\n", or(c.Request.Purpose, "?"))
	fmt.Fprintf(&b, "Provider : %s · %s (vai trò %s)\n", c.Provider, c.Model, c.Role)
	fmt.Fprintf(&b, "Thời gian: %s · max_tokens %d\n", c.Duration.Round(time.Millisecond), c.Request.MaxTokens)
	fmt.Fprintf(&b, "Kết quả  : %s\n", status)
	if level == ModelFull {
		if c.Request.System != "" {
			section(&b, "SYSTEM", c.Request.System)
		}
		for i, m := range c.Request.Messages {
			section(&b, fmt.Sprintf("%s #%d", strings.ToUpper(string(m.Role)), i+1), partsText(m.Parts))
		}
	}
	if c.Reasoning != "" {
		section(&b, fmt.Sprintf("MODEL SUY NGHĨ (%d ký tự)", len([]rune(c.Reasoning))), c.Reasoning)
	}
	section(&b, "MODEL TRẢ VỀ", or(c.Response, "(trống)"))
	b.WriteString("\n")
	l.model.Write([]byte(b.String()))
}

func section(b *strings.Builder, title, body string) {
	fmt.Fprintf(b, "\n----- %s -----\n%s\n", title, strings.TrimRight(body, "\n"))
}

// partsText renders message parts, replacing images with a short placeholder.
func partsText(parts []llm.Part) string {
	var out []string
	for _, p := range parts {
		if p.ImageB64 != "" {
			out = append(out, fmt.Sprintf("[ảnh %s, ~%d KB]", p.ImageMIME, len(p.ImageB64)*3/4/1024))
		} else {
			out = append(out, p.Text)
		}
	}
	return strings.Join(out, "\n")
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// daily is an append-only writer that switches to a new file each day.
type daily struct {
	dir, prefix string
	mu          sync.Mutex
	day         string
	f           *os.File
}

func (d *daily) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if day := time.Now().Format("2006-01-02"); day != d.day || d.f == nil {
		if d.f != nil {
			d.f.Close()
		}
		f, err := os.OpenFile(filepath.Join(d.dir, d.prefix+"-"+day+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			d.f = nil
			return len(p), nil // logging must never break the app
		}
		d.f, d.day = f, day
	}
	return d.f.Write(p)
}

func (d *daily) close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.f != nil {
		d.f.Close()
		d.f = nil
	}
}

func prune(dir string) {
	entries, _ := os.ReadDir(dir)
	cutoff := time.Now().AddDate(0, 0, -KeepDays)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && !e.IsDir() && strings.HasSuffix(e.Name(), ".log") && info.ModTime().Before(cutoff) {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
