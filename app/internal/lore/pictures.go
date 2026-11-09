package lore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"immersive/internal/store"
)

// Picture is a page of the source drawn as an image, for an illustration to
// follow.
type Picture struct {
	Path  string
	Label string // what it is: "chapter 3, page 12 of Huyết Long"
}

// PageImage is where page n (1-based) of a source was rendered, or "" when
// the page is text, not a picture.
func (l *Lore) PageImage(sourceID string, n int) string {
	if n <= 0 {
		return ""
	}
	p := filepath.Join(l.PagesDir, sourceID, fmt.Sprintf("p%05d.jpg", n-1))
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// chapter finds chapter n of the story map (of the work named work, when set).
func (l *Lore) chapter(kbIDs []string, work string, n int) (ch *store.Chapter, kbName string, count int) {
	for _, id := range kbIDs {
		kb, err := l.St.KnowledgeBase(id)
		if work != "" && (err != nil || !strings.EqualFold(strings.TrimSpace(kb.Name), strings.TrimSpace(work))) {
			continue
		}
		chs, _ := l.St.Chapters(id)
		count = max(count, len(chs))
		if n >= 1 && n <= len(chs) {
			return &chs[n-1], kb.Name, count
		}
	}
	return nil, "", count
}

// ChapterPicture is page `page` of chapter n as an image (page 0 = the
// chapter's first drawn page).
func (l *Lore) ChapterPicture(kbIDs []string, work string, n, page int) (Picture, error) {
	ch, kbName, count := l.chapter(kbIDs, work, n)
	if ch == nil {
		return Picture{}, fmt.Errorf("there is no chapter %d (the map has %d chapters)", n, count)
	}
	first, last := pageRange(ch.Pages)
	if page <= 0 {
		for p := first; p > 0 && (last == 0 || p <= last) && p < first+200; p++ {
			if l.PageImage(ch.SourceID, p) != "" {
				page = p
				break
			}
		}
	}
	path := l.PageImage(ch.SourceID, page)
	if path == "" {
		return Picture{}, fmt.Errorf("page %d of chapter %d is not a picture", page, n)
	}
	return Picture{Path: path, Label: fmt.Sprintf("chapter %d, page %d of %s", ch.No, page, kbName)}, nil
}

// CharacterPicture finds the drawn page that shows the character best: the
// one whose transcription names them (or their aliases) most often.
func (l *Lore) CharacterPicture(ctx context.Context, kbIDs []string, name string) (Picture, bool) {
	names := []string{name}
	for _, e := range l.CachedEntities(ctx, kbIDs) {
		if strings.EqualFold(e.Name, name) || containsFold(e.Aliases, name) {
			names = append([]string{e.Name}, e.Aliases...)
			break
		}
	}
	var best Picture
	bestScore := 0
	for _, kbID := range kbIDs {
		kb, _ := l.St.KnowledgeBase(kbID)
		srcs, _ := l.St.Sources(kbID)
		for _, src := range srcs {
			pages, err := LoadPages(TextPath(l.PagesDir, src.ID))
			if err != nil {
				continue
			}
			for _, p := range pages {
				text := strings.ToLower(p.Text)
				score := 0
				for _, n := range names {
					if strings.HasPrefix(n, "[?]") && n != name {
						continue // a look-based label is too loose to count, unless asked for
					}
					score += mentions(text, n)
				}
				if score > bestScore {
					if path := l.PageImage(src.ID, p.Page); path != "" {
						bestScore = score
						best = Picture{Path: path, Label: fmt.Sprintf("page %d of %s", p.Page, kb.Name)}
					}
				}
			}
		}
	}
	return best, bestScore > 0
}

// mentions counts whole-word occurrences of name in lower-case text.
func mentions(text, name string) int {
	n := 0
	for end := len(text); end > 0; {
		i := MentionAt(text[:end], name)
		if i < 0 {
			break
		}
		n++
		end = i
	}
	return n
}

func containsFold(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(strings.TrimSpace(x), strings.TrimSpace(s)) {
			return true
		}
	}
	return false
}
