package prompts

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestEditableWithDefaults(t *testing.T) {
	d := filepath.Join(t.TempDir(), "prompts")
	if got, err := Init(d); err != nil || got != d {
		t.Fatalf("Init = %q, %v", got, err)
	}
	// Defaults are installed; the leading comment is not sent; placeholders fill.
	out := Get("entity-merge", "title", "Huyết Long", "notes", "Phần 1 {text}")
	if strings.Contains(out, "<!--") || !strings.Contains(out, `the work "Huyết Long"`) || !strings.HasSuffix(out, "Phần 1 {text}") {
		t.Errorf("entity-merge = %q", out)
	}

	// The user's edit applies to the next call.
	os.WriteFile(filepath.Join(d, "entity-merge.md"), []byte("<!-- note -->\nTóm tắt {title} ngắn gọn."), 0o644)
	if got := Get("entity-merge", "title", "X"); got != "Tóm tắt X ngắn gọn." {
		t.Errorf("edited = %q", got)
	}
	// Deleted: the default again, and Init doesn't overwrite edits.
	os.Remove(filepath.Join(d, "entity-merge.md"))
	if !strings.Contains(Get("entity-merge"), "Merge these notes") {
		t.Error("no fallback to the default")
	}
	os.WriteFile(filepath.Join(d, "vision.md"), []byte("mine"), 0o644)
	Init(d)
	if Get("vision") != "mine" {
		t.Error("Init overwrote an edited prompt")
	}
}

// Every placeholder a default uses is one the code fills (keeps files and code in sync).
func TestDefaultPlaceholders(t *testing.T) {
	want := map[string][]string{
		"chapter":      {"title", "chapter", "part", "parts", "pages", "story_so_far", "notes", "text"},
		"entity-merge": {"name", "type", "title", "notes"}, "vision": {"page", "title"},
		"chat-divergence": {"other", "player", "canon", "log", "excerpt"}, "chat-lore": nil,
		"character-card": {"name", "hint"}, "scenario": {"hint"}, "cast": {"title", "setting"},
		"research-tools": {"brief", "max"}, "research-plan": {"task", "brief", "max"},
		"research-knowledge": {"brief", "knowledge"}, "chat-memory": {"other", "player", "excerpt"},
		"analyst-system": nil, "chat-character": nil, "chat-narrator": nil,
	}
	placeholder := regexp.MustCompile(`\{([a-z_]+)\}`)
	files, _ := fs.Glob(defaults, "defaults/*.md")
	if len(files) != len(want) {
		t.Errorf("%d default prompts, test knows %d", len(files), len(want))
	}
	for name, keys := range want {
		text := leadingComment.ReplaceAllString(string(must(defaults.ReadFile("defaults/"+name+".md"))), "")
		text = strings.NewReplacer("{{char}}", "", "{{user}}", "").Replace(text)
		found := map[string]bool{}
		for _, m := range placeholder.FindAllStringSubmatch(text, -1) {
			found[m[1]] = true
		}
		for _, k := range keys {
			if !found[k] {
				t.Errorf("%s.md lacks {%s}", name, k)
			}
			delete(found, k)
		}
		for k := range found {
			t.Errorf("%s.md uses {%s}, which the code doesn't fill", name, k)
		}
	}
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}

// An unedited earlier default is upgraded, a retired one removed; edits stay.
func TestUpgradesUneditedDefaults(t *testing.T) {
	d := filepath.Join(t.TempDir(), "prompts")
	os.MkdirAll(d, 0o755)
	old := []byte("old vision prompt")
	sum := sha256.Sum256(old)
	retired := []byte("old overview prompt")
	rsum := sha256.Sum256(retired)
	saved := previous
	previous = map[string][]string{"vision": {hex.EncodeToString(sum[:])}, "overview": {hex.EncodeToString(rsum[:])},
		"overview-continue": {hex.EncodeToString(rsum[:])}}
	defer func() { previous = saved }()

	os.WriteFile(filepath.Join(d, "vision.md"), old, 0o644)
	os.WriteFile(filepath.Join(d, "overview.md"), retired, 0o644)
	os.WriteFile(filepath.Join(d, "overview-continue.md"), []byte("edited by the user"), 0o644)
	if _, err := Init(d); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(d, "vision.md")); string(b) == string(old) {
		t.Error("unedited old default not upgraded")
	}
	if _, err := os.Stat(filepath.Join(d, "overview.md")); err == nil {
		t.Error("unedited retired prompt not removed")
	}
	if _, err := os.Stat(filepath.Join(d, "overview-continue.md")); err != nil {
		t.Error("edited retired prompt removed")
	}
}
