package ingest

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"immersive/internal/lore"
)

// unit is the text of one page (or one vision-transcribed comic page).
type unit = lore.Page

// span is a contiguous piece of text with the page range it came from.
type span struct {
	First, Last int
	Text        string
}

func (s span) pages() string {
	if s.First == s.Last {
		return fmt.Sprint(s.First)
	}
	return fmt.Sprintf("%d-%d", s.First, s.Last)
}

// paragraphs splits units into paragraph-sized pieces tagged with their page.
func paragraphs(units []unit) []span {
	var out []span
	for _, u := range units {
		for _, p := range strings.Split(u.Text, "\n\n") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			// Hard-wrap pathological paragraphs (e.g. PDFs with no blank lines).
			for len(p) > 2400 {
				end := 2400
				if i := strings.LastIndexAny(p[:2400], ".!?。\n"); i >= 800 {
					_, size := utf8.DecodeRuneInString(p[i:])
					end = i + size
				} else {
					for end > 0 && !utf8.RuneStart(p[end]) { // don't split a multi-byte character
						end--
					}
				}
				out = append(out, span{u.Page, u.Page, strings.TrimSpace(p[:end])})
				p = strings.TrimSpace(p[end:])
			}
			if p != "" {
				out = append(out, span{u.Page, u.Page, p})
			}
		}
	}
	return out
}

// pack greedily joins paragraphs into spans of about size chars; overlap
// carries the tail paragraphs of one span into the next.
func pack(paras []span, size, overlap int) []span {
	var out []span
	var cur []span
	curLen := 0
	flush := func() {
		if len(cur) == 0 {
			return
		}
		texts := make([]string, len(cur))
		for i, p := range cur {
			texts[i] = p.Text
		}
		out = append(out, span{cur[0].First, cur[len(cur)-1].Last, strings.Join(texts, "\n\n")})
		// keep a tail for overlap
		var tail []span
		tl := 0
		for i := len(cur) - 1; i >= 0 && tl < overlap; i-- {
			tail = append([]span{cur[i]}, tail...)
			tl += len(cur[i].Text)
		}
		if tl >= curLen { // never re-emit the whole span
			tail, tl = nil, 0
		}
		cur, curLen = tail, tl
	}
	for _, p := range paras {
		if curLen+len(p.Text) > size && curLen > 0 {
			flush()
		}
		cur = append(cur, p)
		curLen += len(p.Text) + 2
	}
	if curLen > 0 {
		if len(out) > 0 && overlap > 0 && curLen <= overlap {
			return out // only overlap left, already emitted
		}
		flush()
	}
	return out
}
