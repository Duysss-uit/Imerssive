// Package lore is what the models know about a work and how they look it up.
//
// A work is kept as:
//   - a story map: one short entry per chapter, in story order, small enough
//     to sit in the model's context (Map);
//   - profiles of its characters, places, factions... (Entities);
//   - the original text and page transcriptions, embedded for search, and
//     readable chapter by chapter (ReadChapter).
//
// The model reads the map and looks up the rest with the tools in Tools.
package lore

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"immersive/internal/llm"
	"immersive/internal/rag"
	"immersive/internal/store"
)

// Vectors is the part of the vector store lore and ingest use (rag.Store;
// swapped in tests).
type Vectors interface {
	SearchKBs(ctx context.Context, spaces rag.Spaces, query string, f rag.Filter, k int) ([]rag.Hit, error)
	GetTexts(ctx context.Context, sp rag.Space, f rag.Filter, limit int) ([]rag.Hit, error)
	AddTexts(ctx context.Context, sp rag.Space, docs []rag.Doc) error
	Delete(ctx context.Context, f rag.Filter) error
}

type Lore struct {
	St       *store.Store
	Vec      Vectors
	Spaces   rag.Spaces
	PagesDir string // <PagesDir>/<source id>/text.json holds what was read from a source
}

// TextPath is where a source's text (pages and page transcriptions) is kept.
func TextPath(pagesDir, sourceID string) string {
	return filepath.Join(pagesDir, sourceID, "text.json")
}

// Page is the text of one page of a source, or a vision transcription of it.
type Page struct {
	Page int // 1-based
	Text string
}

// LoadPages reads a source's saved text.
func LoadPages(path string) ([]Page, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var pages []Page
	return pages, json.Unmarshal(b, &pages)
}

// ---- profiles ----

// Entity is one profile the ingest pipeline built (character, place, faction...).
type Entity struct {
	Name, Type, Profile string
	Quotes              []string // characteristic lines they say in the source
	Aliases             []string // other names and labels the source used for them
}

// Entities lists the profiles stored for the knowledge bases.
func (l *Lore) Entities(ctx context.Context, kbIDs []string) ([]Entity, error) {
	var out []Entity
	for _, id := range kbIDs {
		sp, err := l.Spaces(id)
		if err != nil {
			return nil, err
		}
		hits, err := l.Vec.GetTexts(ctx, sp, rag.Filter{KBIDs: []string{id}, Levels: []string{rag.LevelEntity}}, 1000)
		if err != nil {
			return nil, err
		}
		for _, h := range hits {
			e := ParseEntity(h.Str("name"), h.Text)
			if q := h.Str("quotes"); q != "" {
				e.Quotes = strings.Split(q, "\n")
			}
			if a := h.Str("aliases"); a != "" {
				e.Aliases = strings.Split(a, "\n")
			}
			out = append(out, e)
		}
	}
	return out, nil
}

// ParseEntity splits a stored profile "Name (type): profile".
func ParseEntity(name, text string) Entity {
	e := Entity{Name: name, Profile: text}
	if head, rest, ok := strings.Cut(text, ": "); ok && strings.HasSuffix(head, ")") {
		if i := strings.LastIndex(head, " ("); i > 0 {
			e.Type = head[i+2 : len(head)-1]
			e.Profile = rest
			if e.Name == "" {
				e.Name = head[:i]
			}
		}
	}
	return e
}

// IsCharacter reports whether the profile is about a person (types come from the model, in any language).
func (e Entity) IsCharacter() bool {
	t := strings.ToLower(e.Type)
	return strings.Contains(t, "character") || strings.Contains(t, "nhân vật") || strings.Contains(t, "person")
}

// ---- the story map ----

// MapEntry renders one chapter as a line of the story map.
func MapEntry(c store.Chapter) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d. %s", c.No, c.Title)
	if c.Pages != "" {
		fmt.Fprintf(&sb, " (p. %s)", c.Pages)
	}
	if c.When != "" {
		fmt.Fprintf(&sb, " [When: %s]", c.When)
	}
	sb.WriteString(": " + strings.TrimSpace(c.Summary))
	if len(c.Cast) > 0 {
		sb.WriteString(" [Characters: " + strings.Join(c.Cast, ", ") + "]")
	}
	return sb.String()
}

// RenderMap is the full story map of chapters.
func RenderMap(chs []store.Chapter) string {
	lines := make([]string, len(chs))
	for i, c := range chs {
		lines[i] = MapEntry(c)
	}
	return strings.Join(lines, "\n")
}

// Map is the story map of the knowledge bases in at most maxChars: whole
// entries when they fit, otherwise each chapter shortened to its first
// sentence, and as a last resort only some chapters with a note that the
// others can be read with read_chapter.
func (l *Lore) Map(kbIDs []string, maxChars int) string {
	type work struct {
		name string
		chs  []store.Chapter
	}
	var works []work
	for _, id := range kbIDs {
		kb, err := l.St.KnowledgeBase(id)
		if err != nil {
			continue
		}
		chs, _ := l.St.Chapters(id)
		if len(chs) > 0 {
			works = append(works, work{kb.Name, chs})
		}
	}
	if len(works) == 0 {
		return ""
	}
	render := func(entry func(store.Chapter) string) string {
		var parts []string
		for _, w := range works {
			lines := make([]string, len(w.chs))
			for i, c := range w.chs {
				lines[i] = entry(c)
			}
			parts = append(parts, fmt.Sprintf("[%s · %d chapters]\n%s", w.name, len(w.chs), strings.Join(lines, "\n")))
		}
		return strings.Join(parts, "\n\n")
	}
	if out := render(MapEntry); len([]rune(out)) <= maxChars {
		return out
	}
	short := func(c store.Chapter) string {
		c.Summary = firstSentence(c.Summary)
		c.Cast = nil
		return MapEntry(c)
	}
	out := render(short)
	if len([]rune(out)) <= maxChars {
		return out
	}
	// Too many chapters even in short: keep every n-th one.
	total := 0
	for _, w := range works {
		total += len(w.chs)
	}
	step := (len([]rune(out)) + maxChars - 1) / max(maxChars, 1)
	for i := range works {
		var kept []store.Chapter
		for j, c := range works[i].chs {
			if j%step == 0 || j == len(works[i].chs)-1 {
				kept = append(kept, c)
			}
		}
		works[i].chs = kept
	}
	return render(short) + fmt.Sprintf("\n(Shortened map: only some of the %d chapters are shown; read the others with read_chapter.)", total)
}

func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, ".!?。"); i > 0 && i < len(s)-1 {
		return s[:i+1]
	}
	return s
}

// ---- tools ----

var (
	SearchTool = llm.Tool{
		Name: "search_story",
		Description: "Search the source work: character profiles, chapter summaries and original passages (dialogue, events). " +
			"Use one focused query per call: a name, a place, an event, a relationship.",
		Params:   map[string]any{"query": map[string]any{"type": "string", "description": "what to look up, in the work's language"}},
		Required: []string{"query"},
	}
	ReadChapterTool = llm.Tool{
		Name:        "read_chapter",
		Description: "Read the original text of a chapter from the story map: what happens and what is said, page by page.",
		Params: map[string]any{
			"chapter":   map[string]any{"type": "integer", "description": "chapter number in the story map"},
			"from_page": map[string]any{"type": "integer", "description": "optional: first page to read"},
			"work":      map[string]any{"type": "string", "description": "optional: name of the work, when several are loaded"},
		},
		Required: []string{"chapter"},
	}
	CharacterTool = llm.Tool{
		Name:        "get_character",
		Description: "Get the profile of a character (or place, faction...) of the work by name.",
		Params:      map[string]any{"name": map[string]any{"type": "string"}},
		Required:    []string{"name"},
	}
	Tools = []llm.Tool{SearchTool, ReadChapterTool, CharacterTool}
)

// Search answers one query against the knowledge bases.
func (l *Lore) Search(ctx context.Context, kbIDs []string, query string, k int) ([]rag.Hit, error) {
	return l.Vec.SearchKBs(ctx, l.Spaces, query, rag.Filter{KBIDs: kbIDs,
		Levels: []string{rag.LevelEntity, rag.LevelSection, rag.LevelRaw}}, k)
}

// Run executes one tool call for the model and returns the result text, at
// most maxChars long. Mistakes come back as text the model can act on.
func (l *Lore) Run(ctx context.Context, kbIDs []string, c llm.ToolCall, maxChars int) string {
	var args struct {
		Query    string          `json:"query"`
		Name     string          `json:"name"`
		Work     string          `json:"work"`
		Chapter  json.RawMessage `json:"chapter"`
		FromPage json.RawMessage `json:"from_page"`
	}
	if err := json.Unmarshal([]byte(c.Args), &args); err != nil && strings.TrimSpace(c.Args) != "" {
		return "Error: the arguments are not valid JSON."
	}
	switch c.Name {
	case SearchTool.Name:
		if strings.TrimSpace(args.Query) == "" {
			return `Error: call search_story with {"query": "..."}.`
		}
		hits, err := l.Search(ctx, kbIDs, args.Query, 6)
		if err != nil {
			return "Search failed: " + err.Error()
		}
		if len(hits) == 0 {
			return "Nothing found for this query."
		}
		return rag.FormatContext(hits, maxChars)
	case ReadChapterTool.Name:
		n := looseInt(args.Chapter)
		if n <= 0 {
			return `Error: call read_chapter with {"chapter": <chapter number in the story map>}.`
		}
		return l.ReadChapter(kbIDs, args.Work, n, looseInt(args.FromPage), maxChars)
	case CharacterTool.Name:
		return l.Character(ctx, kbIDs, args.Name, maxChars)
	}
	return "Error: there is no tool named " + c.Name + "."
}

// looseInt reads a number the model may have sent as a number or a string.
func looseInt(raw json.RawMessage) int {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	n, _ := strconv.Atoi(s)
	if n == 0 {
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			n = int(f)
		}
	}
	return n
}

// ReadChapter returns chapter n's text from fromPage on (0 = its start), cut
// at maxChars with a note on where to continue.
func (l *Lore) ReadChapter(kbIDs []string, work string, n, fromPage, maxChars int) string {
	ch, _, count := l.chapter(kbIDs, work, n)
	if ch == nil {
		return fmt.Sprintf("There is no chapter %d (the map has %d chapters).", n, count)
	}
	pages, err := LoadPages(TextPath(l.PagesDir, ch.SourceID))
	if err != nil {
		return "The text of this chapter could not be read."
	}
	first, last := pageRange(ch.Pages)
	if fromPage > first {
		first = fromPage
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Chapter %d: %s\n", ch.No, ch.Title)
	for _, p := range pages {
		if p.Page < first || (last > 0 && p.Page > last) {
			continue
		}
		block := fmt.Sprintf("\n[Page %d]\n%s\n", p.Page, strings.TrimSpace(p.Text))
		if sb.Len() > 200 && len([]rune(sb.String()))+len([]rune(block)) > maxChars {
			fmt.Fprintf(&sb, "\n(Continues: call read_chapter with chapter=%d, from_page=%d.)", ch.No, p.Page)
			return sb.String()
		}
		sb.WriteString(block)
	}
	return headRunes(sb.String(), maxChars)
}

// pageRange parses "3" or "3-17"; last is 0 when open.
func pageRange(s string) (first, last int) {
	a, b, ok := strings.Cut(s, "-")
	first, _ = strconv.Atoi(strings.TrimSpace(a))
	if ok {
		last, _ = strconv.Atoi(strings.TrimSpace(b))
	} else {
		last = first
	}
	return first, last
}

// Character returns the profile named name, or the closest matches.
func (l *Lore) Character(ctx context.Context, kbIDs []string, name string, maxChars int) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return `Error: call get_character with {"name": "..."}.`
	}
	ents, err := l.Entities(ctx, kbIDs)
	if err != nil {
		return "Could not read the profiles: " + err.Error()
	}
	var partial []Entity
	for _, e := range ents {
		if strings.EqualFold(strings.TrimSpace(e.Name), name) || slices.ContainsFunc(e.Aliases, func(a string) bool { return strings.EqualFold(strings.TrimSpace(a), name) }) {
			return headRunes(fmt.Sprintf("%s (%s): %s", e.Name, e.Type, e.Profile), maxChars)
		}
		low, q := strings.ToLower(e.Name), strings.ToLower(name)
		if strings.Contains(low, q) || strings.Contains(q, low) {
			partial = append(partial, e)
		}
	}
	if len(partial) == 1 {
		e := partial[0]
		return headRunes(fmt.Sprintf("%s (%s): %s", e.Name, e.Type, e.Profile), maxChars)
	}
	if len(partial) > 1 {
		var names []string
		for _, e := range partial {
			names = append(names, e.Name)
		}
		return "Several profiles match: " + strings.Join(names, ", ") + ". Call again with the exact name."
	}
	return "No profile named " + name + ". Try search_story."
}

func headRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
