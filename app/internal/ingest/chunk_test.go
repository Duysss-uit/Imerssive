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

func TestSplitPages(t *testing.T) {
	group := []page{{Index: 9}, {Index: 10}}
	text := "=== PAGE 10 ===\nA: Xin chào\n\n=== PAGE 11 ===\nB: Tạm biệt"
	units := splitPages(text, group)
	if len(units) != 2 || units[0].Page != 10 || units[1].Text != "B: Tạm biệt" {
		t.Fatalf("unexpected units: %+v", units)
	}
	if u := splitPages("no markers", group); len(u) != 1 || u[0].Page != 10 {
		t.Fatalf("fallback failed: %+v", u)
	}
}

func TestParseJSONToleratesFences(t *testing.T) {
	var r sectionResult
	if err := ParseJSON("```json\n{\"summary\":\"ok\",\"entities\":[]}\n```", &r); err != nil || r.Summary != "ok" {
		t.Fatalf("got %v %+v", err, r)
	}
}
