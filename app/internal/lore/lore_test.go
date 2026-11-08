package lore

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"immersive/internal/rag"
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
	chs[0].When = "Day 1, dawn"
	st.SetChapters(src.ID, chs)
	l := &Lore{St: st, PagesDir: dir}
	if got, _ := st.Chapters(kb.ID); got[0].When != "Day 1, dawn" || !strings.HasPrefix(MapEntry(got[0]), "1. Chương 1 (p. 1-10) [When: Day 1, dawn]: Sự kiện") {
		t.Errorf("when not kept or not shown: %q", MapEntry(got[0]))
	}

	full := l.Map([]string{kb.ID}, 100000)
	if !strings.Contains(full, "40. Chương 40 (p. 391-400): Sự kiện chính của chương 40. Chi tiết") {
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
	if !strings.Contains(out, "[Page 21]") || strings.Contains(out, "[Page 30]") || !strings.Contains(out, "from_page=") {
		t.Errorf("first read:\n%s", out)
	}
	next := l.ReadChapter([]string{kb.ID}, "", 3, 29, 1000)
	if strings.Contains(next, "[Page 28]") || !strings.Contains(next, "[Page 30]") {
		t.Errorf("continued read:\n%s", next)
	}
	if out := l.ReadChapter([]string{kb.ID}, "", 99, 0, 1000); !strings.Contains(out, "40 chapters") {
		t.Errorf("missing chapter: %s", out)
	}
}

func TestQuotesAndMentions(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	kb := store.KnowledgeBase{Name: "Huyết Long"}
	st.SaveKnowledgeBase(&kb)
	src := store.Source{KBID: kb.ID, Filename: "c1.cbz", Status: "done"}
	st.InsertSource(&src)
	pages := []Page{
		{Page: 1, Text: "Characters: Lâm Phong, Mai Linh\nPanels:\n1. Lâm Phong rút kiếm.\n   Lâm Phong: \"Ta sẽ không để ai chạm vào nàng!\"\n   SFX: keng"},
		{Page: 2, Text: "1. Mai Linh khóc.\n   Mai Linh: “Huynh đừng đi mà…”\n   (narration) \"Đêm ấy trời đổ mưa.\""},
		{Page: 3, Text: "   Phong: \"Đợi ta trở về, nhất định.\""},
	}
	b, _ := json.Marshal(pages)
	os.MkdirAll(filepath.Join(dir, src.ID), 0o755)
	os.WriteFile(TextPath(dir, src.ID), b, 0o644)
	vec := memVec{{Text: "Lâm Phong (character): Kiếm sĩ trẻ.", Metadata: map[string]any{"name": "Lâm Phong", "quotes": "Kiếm này tên là Huyết Long."}}}
	l := &Lore{St: st, Vec: vec, Spaces: func(string) (rag.Space, error) { return rag.Space{}, nil }, PagesDir: dir}

	q := l.Quotes(context.Background(), []string{kb.ID}, []string{"Lâm Phong", "Mai Linh", "Hắc Long"}, 3, 160)
	lp := strings.Join(q["lâm phong"], "|")
	for _, want := range []string{"Kiếm này tên là Huyết Long.", "Ta sẽ không để ai chạm vào nàng!", "Đợi ta trở về, nhất định."} {
		if !strings.Contains(lp, want) {
			t.Errorf("Lâm Phong quotes lack %q: %q", want, lp)
		}
	}
	if len(q["mai linh"]) != 1 || q["mai linh"][0] != "Huynh đừng đi mà…" || q["hắc long"] != nil {
		t.Errorf("quotes = %q", q)
	}

	ents := []Entity{{Name: "Mai Linh"}, {Name: "An"}, {Name: "Hắc Sơn"}, {Name: "Lâm Phong"}}
	got := Mentioned(ents, "Lâm Phong đi qua Hắc Sơn. Bàn tay Mai Linh run rẩy.", map[string]bool{"lâm phong": true})
	if len(got) != 2 || got[0].Name != "Mai Linh" || got[1].Name != "Hắc Sơn" {
		t.Errorf("mentioned = %+v (want most recent first, whole words only, player skipped)", got)
	}
}

type memVec []rag.Hit

func (m memVec) SearchKBs(context.Context, rag.Spaces, string, rag.Filter, int) ([]rag.Hit, error) {
	return m, nil
}
func (m memVec) GetTexts(context.Context, rag.Space, rag.Filter, int) ([]rag.Hit, error) {
	return m, nil
}
func (memVec) AddTexts(context.Context, rag.Space, []rag.Doc) error { return nil }
func (memVec) Delete(context.Context, rag.Filter) error             { return nil }
