package ingest

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPackCoversAllTextWithOverlap(t *testing.T) {
	var units []unit
	for p := 1; p <= 5; p++ {
		units = append(units, unit{Page: p, Text: strings.Repeat("Đoạn văn tiếng Việt có dấu. ", 20) + "\n\n" + strings.Repeat("Câu thứ hai. ", 30)})
	}
	paras := paragraphs(units)
	chunks := pack(paras, 1500, 200)
	if len(chunks) < 3 {
		t.Fatalf("expected several chunks, got %d", len(chunks))
	}
	if chunks[0].First != 1 || chunks[len(chunks)-1].Last != 5 {
		t.Fatalf("page range lost: first=%d last=%d", chunks[0].First, chunks[len(chunks)-1].Last)
	}
	for i, c := range chunks {
		if !utf8.ValidString(c.Text) {
			t.Fatalf("chunk %d is not valid UTF-8", i)
		}
	}
	sections := pack(paras, 4000, 0)
	total := 0
	for _, s := range sections {
		total += strings.Count(s.Text, "Câu thứ hai.")
	}
	if total != 5*30 {
		t.Fatalf("sections without overlap must contain every paragraph exactly once, got %d", total)
	}
}

func TestParagraphsHardWrapKeepsUTF8(t *testing.T) {
	long := strings.Repeat("ữ", 3000) // no punctuation, multi-byte runes
	for _, s := range paragraphs([]unit{{Page: 1, Text: long}}) {
		if !utf8.ValidString(s.Text) {
			t.Fatal("hard wrap split a multi-byte character")
		}
	}
}

func TestSplitChapters(t *testing.T) {
	long := strings.Repeat("Lâm Phong luyện kiếm. ", 30)
	units := []unit{
		{Page: 1, Text: "MỤC LỤC\nChương 1\nChương 2"}, // a table of contents makes no chapter of its own
		{Page: 2, Text: "Chương 1: Làng Thanh Vân\n" + long},
		{Page: 3, Text: long + "\nChương 2 Hắc Sơn\n" + long},
		{Page: 4, Text: long},
	}
	chs := splitChapters(units, "Huyết Long.pdf", 0)
	if len(chs) != 2 || chs[0].title != "Chương 1: Làng Thanh Vân" || chs[1].title != "Chương 2 Hắc Sơn" {
		t.Fatalf("chapters: %+v", chs)
	}
	if chs[0].pages() != "1-3" || chs[1].pages() != "3-4" || !strings.HasPrefix(chs[1].units[0].Text, "Chương 2") {
		t.Errorf("pages %s / %s, second starts %q", chs[0].pages(), chs[1].pages(), chs[1].units[0].Text[:20])
	}
	// No headings: one chapter named after the file, cut in parts when long.
	chs = splitChapters([]unit{{Page: 1, Text: long}, {Page: 2, Text: long}, {Page: 3, Text: long}}, "Tập 5.cbz", 1000)
	if len(chs) != 3 || chs[0].title != "Tập 5 (phần 1/3)" {
		t.Errorf("parts: %+v", chs)
	}
}

func TestParseJSONToleratesFences(t *testing.T) {
	var r sectionResult
	if err := ParseJSON("```json\n{\"summary\":\"ok\",\"entities\":[]}\n```", &r); err != nil || r.Summary != "ok" {
		t.Fatalf("got %v %+v", err, r)
	}
}
