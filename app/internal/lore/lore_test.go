package lore

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"immersive/internal/store"
)

func TestMapFitsAndReadChapterPages(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	kb := store.KnowledgeBase{Name: "Huyết Long"}
	st.SaveKnowledgeBase(&kb)
	src := store.Source{KBID: kb.ID, Filename: "a.pdf", Status: "done"}
	st.InsertSource(&src)
	var chs []store.Chapter
	for i := 1; i <= 40; i++ {
		chs = append(chs, store.Chapter{KBID: kb.ID, Title: fmt.Sprintf("Chương %d", i), Pages: fmt.Sprintf("%d-%d", i*10-9, i*10),
			Summary: fmt.Sprintf("Sự kiện chính của chương %d. %s", i, strings.Repeat("Chi tiết thêm. ", 10)), Cast: []string{"Lâm Phong"}})
	}
	st.SetChapters(src.ID, chs)
	l := &Lore{St: st, PagesDir: dir}

	full := l.Map([]string{kb.ID}, 100000)
	if !strings.Contains(full, "40. Chương 40 (tr. 391-400): Sự kiện chính của chương 40. Chi tiết") {
		t.Errorf("full map:\n%s", full)
	}
	short := l.Map([]string{kb.ID}, 3000)
	if n := len([]rune(short)); n > 3000 || strings.Contains(short, "Chi tiết") {
		t.Errorf("shortened map is %d chars:\n%s", n, short)
	}
	tiny := l.Map([]string{kb.ID}, 800)
	if n := len([]rune(tiny)); n > 1000 || !strings.Contains(tiny, "read_chapter") || !strings.Contains(tiny, "40. Chương 40") {
		t.Errorf("sampled map is %d chars:\n%s", n, tiny)
	}

	// read_chapter returns the chapter's pages and says where to go on.
	var pages []Page
	for p := 21; p <= 30; p++ {
		pages = append(pages, Page{Page: p, Text: fmt.Sprintf("Trang %d: %s", p, strings.Repeat("lời thoại ", 30))})
	}
	b, _ := json.Marshal(pages)
	os.MkdirAll(filepath.Join(dir, src.ID), 0o755)
	os.WriteFile(TextPath(dir, src.ID), b, 0o644)
	out := l.ReadChapter([]string{kb.ID}, "", 3, 0, 1000)
	if !strings.Contains(out, "[Trang 21]") || strings.Contains(out, "[Trang 30]") || !strings.Contains(out, "from_page=") {
		t.Errorf("first read:\n%s", out)
	}
	next := l.ReadChapter([]string{kb.ID}, "", 3, 29, 1000)
	if strings.Contains(next, "[Trang 28]") || !strings.Contains(next, "[Trang 30]") {
		t.Errorf("continued read:\n%s", next)
	}
	if out := l.ReadChapter([]string{kb.ID}, "", 99, 0, 1000); !strings.Contains(out, "40 chương") {
		t.Errorf("missing chapter: %s", out)
	}
}
