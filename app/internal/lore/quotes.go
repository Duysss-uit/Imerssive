package lore

import (
	"context"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// Real lines a character says in the source keep their voice better than any
// description of how they talk. They come from two places:
//   - comic transcriptions, which write dialogue as `Name: "line"`;
//   - the quotes the map step picked for each profile (novels, where the
//     speaker is in the prose).

// A transcribed line of dialogue: `Speaker: "line"`, possibly numbered.
var dialogueLine = regexp.MustCompile(`^\s*(?:\d+[.)]\s*)?([^:"“”「」\n]{1,80}?)\s*:\s*["“「『](.{4,300}?)["”」』]\s*$`)

type quoteIndex struct {
	mod   time.Time
	lines map[string][]string // lower-case speaker → lines in story order
}

type quoteCache struct {
	mu   sync.Mutex
	srcs map[string]quoteIndex // source id → its dialogue
	ents map[string]entCache   // kb id → profiles, briefly
}

type entCache struct {
	at   time.Time
	ents []Entity
}

var cache = quoteCache{srcs: map[string]quoteIndex{}, ents: map[string]entCache{}}

// CachedEntities is Entities kept for a minute: chat reads the profiles every
// turn, and they only change when a work is mapped again.
func (l *Lore) CachedEntities(ctx context.Context, kbIDs []string) []Entity {
	var out []Entity
	for _, id := range kbIDs {
		cache.mu.Lock()
		c, ok := cache.ents[id]
		cache.mu.Unlock()
		if !ok || time.Since(c.at) > time.Minute {
			ents, err := l.Entities(ctx, []string{id})
			if err != nil {
				continue
			}
			c = entCache{time.Now(), ents}
			cache.mu.Lock()
			cache.ents[id] = c
			cache.mu.Unlock()
		}
		out = append(out, c.ents...)
	}
	return out
}

// dialogue returns a source's transcribed dialogue by speaker, re-reading
// the file only when it changed.
func (l *Lore) dialogue(sourceID string) map[string][]string {
	path := TextPath(l.PagesDir, sourceID)
	fi, err := os.Stat(path)
	if err != nil {
		return nil
	}
	cache.mu.Lock()
	idx, ok := cache.srcs[sourceID]
	cache.mu.Unlock()
	if ok && idx.mod.Equal(fi.ModTime()) {
		return idx.lines
	}
	pages, err := LoadPages(path)
	if err != nil {
		return nil
	}
	lines := map[string][]string{}
	for _, p := range pages {
		for _, line := range strings.Split(p.Text, "\n") {
			if m := dialogueLine.FindStringSubmatch(line); m != nil {
				who := strings.ToLower(strings.TrimSpace(m[1]))
				lines[who] = append(lines[who], strings.TrimSpace(m[2]))
			}
		}
	}
	cache.mu.Lock()
	cache.srcs[sourceID] = quoteIndex{fi.ModTime(), lines}
	cache.mu.Unlock()
	return lines
}

// Quotes picks up to n lines each of the named characters says in the
// works, spread over the story, each at most maxRunes long. Keys of the
// result are the names in lower case.
func (l *Lore) Quotes(ctx context.Context, kbIDs, names []string, n, maxRunes int) map[string][]string {
	out := map[string][]string{}
	if n <= 0 || len(names) == 0 {
		return out
	}
	all := map[string][]string{}
	for _, id := range kbIDs {
		srcs, _ := l.St.Sources(id)
		for _, src := range srcs {
			for who, lines := range l.dialogue(src.ID) {
				all[who] = append(all[who], lines...)
			}
		}
	}
	profileQuotes := map[string][]string{}
	aliases := map[string][]string{}
	for _, e := range l.CachedEntities(ctx, kbIDs) {
		k := strings.ToLower(strings.TrimSpace(e.Name))
		profileQuotes[k] = e.Quotes
		for _, a := range e.Aliases {
			aliases[k] = append(aliases[k], strings.ToLower(strings.TrimSpace(a)))
		}
	}
	for _, name := range names {
		key := strings.ToLower(strings.TrimSpace(name))
		if key == "" {
			continue
		}
		cands := append([]string{}, profileQuotes[key]...)
		for who, lines := range all {
			if sameSpeaker(who, key) || slices.Contains(aliases[key], who) {
				cands = append(cands, lines...)
			}
		}
		var fit []string
		seen := map[string]bool{}
		for _, q := range cands {
			r := utf8.RuneCountInString(q)
			if r < 8 || r > maxRunes || seen[q] {
				continue
			}
			seen[q] = true
			fit = append(fit, q)
		}
		if len(fit) == 0 {
			continue
		}
		// Spread over the story; the longer lines say more about how they talk.
		var picked []string
		for i := 0; i < n && i < len(fit); i++ {
			lo, hi := i*len(fit)/n, (i+1)*len(fit)/n
			best := fit[lo]
			for _, q := range fit[lo:max(hi, lo+1)] {
				if len(q) > len(best) {
					best = q
				}
			}
			picked = append(picked, best)
		}
		out[key] = picked
	}
	return out
}

// sameSpeaker matches a transcribed speaker to a profile name: equal, or one
// is a whole-word part of the other ("Phong" / "Lâm Phong").
func sameSpeaker(who, name string) bool {
	if who == name {
		return true
	}
	if utf8.RuneCountInString(who) < 3 || utf8.RuneCountInString(name) < 3 {
		return false
	}
	return MentionAt(name, who) >= 0 || MentionAt(who, name) >= 0
}

// MentionAt returns the last position of name in text as a whole word
// (case-insensitive; text must already be lower case), or -1.
func MentionAt(text, name string) int {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return -1
	}
	for end := len(text); end > 0; {
		i := strings.LastIndex(text[:end], name)
		if i < 0 {
			return -1
		}
		before, _ := utf8.DecodeLastRuneInString(text[:i])
		after, _ := utf8.DecodeRuneInString(text[i+len(name):])
		if !isWordRune(before) && !isWordRune(after) {
			return i
		}
		end = i
	}
	return -1
}

func isWordRune(r rune) bool {
	return r != utf8.RuneError && (unicode.IsLetter(r) || unicode.IsDigit(r))
}

// Mentioned lists the profiles named in text, most recently mentioned first.
// skip holds lower-case names to leave out.
func Mentioned(ents []Entity, text string, skip map[string]bool) []Entity {
	text = strings.ToLower(text)
	type hit struct {
		e   Entity
		pos int
	}
	var hits []hit
	for _, e := range ents {
		key := strings.ToLower(strings.TrimSpace(e.Name))
		if utf8.RuneCountInString(key) < 2 || skip[key] {
			continue
		}
		pos := MentionAt(text, key)
		for _, a := range e.Aliases { // a name they went by earlier
			if !strings.HasPrefix(a, "[?]") && utf8.RuneCountInString(a) >= 2 {
				pos = max(pos, MentionAt(text, a))
			}
		}
		if pos >= 0 {
			hits = append(hits, hit{e, pos})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].pos > hits[j].pos })
	out := make([]Entity, len(hits))
	for i, h := range hits {
		out[i] = h.e
	}
	return out
}
