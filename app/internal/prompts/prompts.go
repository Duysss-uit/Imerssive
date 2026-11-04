// Package prompts loads the model prompts from Markdown files the user can
// edit. Each prompt is <dir>/<name>.md, read on every use, so an edit applies
// to the next model call without rebuilding. Missing files are written from
// the defaults built into the app (defaults/*.md); a file deleted while the
// app runs falls back to its default.
//
// A file may open with an HTML comment describing it; the comment is not sent
// to the model. Placeholders are {name}, filled by Get. {{char}} and {{user}}
// are left for the chat code to fill.
package prompts

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
)

//go:embed defaults/*.md
var defaults embed.FS

var (
	mu  sync.RWMutex
	dir string
)

// Init picks the prompts folder: the first candidate that can be created and
// written, and fills it with any default prompt that isn't there yet.
func Init(candidates ...string) (string, error) {
	var err error
	for _, d := range candidates {
		if err = install(d); err == nil {
			mu.Lock()
			dir = d
			mu.Unlock()
			return d, nil
		}
	}
	return "", err
}

func install(d string) error {
	if err := os.MkdirAll(d, 0o755); err != nil {
		return err
	}
	entries, err := defaults.ReadDir("defaults")
	if err != nil {
		return err
	}
	current := map[string]bool{}
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), ".md")
		current[name] = true
		path := filepath.Join(d, e.Name())
		if b, err := os.ReadFile(path); err == nil && !unedited(name, b) {
			continue // the user's version wins
		}
		b, _ := defaults.ReadFile("defaults/" + e.Name())
		if err := os.WriteFile(path, b, 0o644); err != nil {
			return err
		}
	}
	// Prompts the app no longer uses go too, unless the user edited them.
	for name := range previous {
		path := filepath.Join(d, name+".md")
		if b, err := os.ReadFile(path); err == nil && !current[name] && unedited(name, b) {
			os.Remove(path)
		}
	}
	return nil
}

// unedited reports whether b is an earlier default of prompt name.
func unedited(name string, b []byte) bool {
	sum := sha256.Sum256(b)
	return slices.Contains(previous[name], hex.EncodeToString(sum[:]))
}

// Dir is the folder the prompts are read from ("" before Init).
func Dir() string {
	mu.RLock()
	defer mu.RUnlock()
	return dir
}

var leadingComment = regexp.MustCompile(`(?s)\A\s*<!--.*?-->\s*`)

// Get returns prompt name with each {key} replaced: Get("section", "part", "3", "text", body).
func Get(name string, keyValues ...string) string {
	text := raw(name)
	if len(keyValues) == 0 {
		return text
	}
	pairs := make([]string, 0, len(keyValues))
	for i := 0; i+1 < len(keyValues); i += 2 {
		pairs = append(pairs, "{"+keyValues[i]+"}", keyValues[i+1])
	}
	return strings.NewReplacer(pairs...).Replace(text)
}

func raw(name string) string {
	var b []byte
	if d := Dir(); d != "" {
		b, _ = os.ReadFile(filepath.Join(d, name+".md"))
	}
	if len(b) == 0 {
		b, _ = defaults.ReadFile("defaults/" + name + ".md")
	}
	return strings.TrimSpace(leadingComment.ReplaceAllString(string(b), ""))
}
