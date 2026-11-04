package ingest

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// chapter is a stretch of a source that gets one entry in the story map.
type chapter struct {
	title string
	units []unit
}

func (c chapter) pages() string {
	if len(c.units) == 0 {
		return ""
	}
	return span{First: c.units[0].Page, Last: c.units[len(c.units)-1].Page}.pages()
}

func (c chapter) chars() int {
	n := 0
	for _, u := range c.units {
		n += len([]rune(u.Text))
	}
	return n
}

// A chapter heading on a line of its own: "Chương 12", "Chapter IV: ...",
// "Hồi 3", "第十二章", "Mở đầu"...
var headingRe = regexp.MustCompile(`(?i)^\s*(?:(?:chương|chuong|chapter|chap\.?|ch\.|hồi|quyển|tập|phần|episode|ep\.?)\s*(?:\d+|[ivxlcdm]+\b)` +
	`|第\s*[0-9０-９〇零一二三四五六七八九十百千]+\s*[章話话回節节]` +
	`|(?:prologue|epilogue|lời mở đầu|mở đầu|vĩ thanh|ngoại truyện)\s*(?:$|[:.\-–—]))`)

// minChapterChars keeps tables of contents and title pages from making
// chapters of their own: a heading this close to the previous one renames it.
const minChapterChars = 300

// splitChapters cuts a source into chapters at its chapter headings; without
// headings the source is one chapter (a manga chapter is usually one file).
// Chapters longer than maxChars are cut into parts.
func splitChapters(units []unit, filename string, maxChars int) []chapter {
	base := strings.TrimSuffix(filename, filepath.Ext(filename))
	cur := chapter{title: base}
	var out []chapter
	for _, u := range units {
		var lines []string
		flush := func() {
			if text := strings.TrimSpace(strings.Join(lines, "\n")); text != "" {
				cur.units = append(cur.units, unit{Page: u.Page, Text: text})
			}
			lines = nil
		}
		for _, line := range strings.Split(u.Text, "\n") {
			t := strings.TrimSpace(line)
			if len([]rune(t)) <= 80 && headingRe.MatchString(t) {
				flush()
				if cur.chars() >= minChapterChars {
					out = append(out, cur)
					cur = chapter{}
				}
				cur.title = t
			}
			lines = append(lines, line)
		}
		flush()
	}
	if len(cur.units) > 0 {
		out = append(out, cur)
	}
	if maxChars <= 0 {
		return out
	}
	var sized []chapter
	for _, c := range out {
		sized = append(sized, splitLong(c, maxChars)...)
	}
	return sized
}

// splitLong cuts a chapter longer than maxChars into parts at page ends.
func splitLong(c chapter, maxChars int) []chapter {
	if c.chars() <= maxChars {
		return []chapter{c}
	}
	var parts []chapter
	var cur chapter
	n := 0
	for _, u := range c.units {
		size := len([]rune(u.Text))
		if n+size > maxChars && len(cur.units) > 0 {
			parts = append(parts, cur)
			cur, n = chapter{}, 0
		}
		cur.units = append(cur.units, u)
		n += size
	}
	if len(cur.units) > 0 {
		parts = append(parts, cur)
	}
	for i := range parts {
		parts[i].title = fmt.Sprintf("%s (phần %d/%d)", c.title, i+1, len(parts))
	}
	return parts
}
