package ingest

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"immersive/internal/lore"
)

// The vision model reads each page on its own, so a character with no name
// yet gets a label from their looks ("[?] girl · long black hair · glasses"),
// and the name, once the story says it, comes pages later. The map step reads
// the story in order with the characters known so far, and reports the
// labels it recognises as someone known in "aliases". Those links merge the
// labels into one character, under their real name when there is one.

// rawEntity is one character, place... as the map step reports it.
type rawEntity struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Description string   `json:"description"`
	Quotes      []string `json:"quotes"`
	Aliases     []string `json:"aliases"` // other labels this passage or earlier ones used for them
}

// isLabel reports whether a name is a description standing in for a name.
func isLabel(name string) bool {
	return strings.HasPrefix(strings.TrimSpace(name), "[?]")
}

func key(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

// identities links names that are the same character (union-find on keys).
type identities struct {
	parent map[string]string
	shown  map[string]string // key → name as written
}

func newIdentities() *identities {
	return &identities{parent: map[string]string{}, shown: map[string]string{}}
}

func (id *identities) add(name string) string {
	k := key(name)
	if _, ok := id.parent[k]; !ok {
		id.parent[k] = k
		id.shown[k] = strings.TrimSpace(name)
	}
	return k
}

func (id *identities) root(k string) string {
	for id.parent[k] != k {
		id.parent[k] = id.parent[id.parent[k]]
		k = id.parent[k]
	}
	return k
}

// link makes alias the same character as name. A real name wins over a
// label; otherwise the name that claimed the alias does.
func (id *identities) link(name, alias string) {
	a, b := id.root(id.add(name)), id.root(id.add(alias))
	if a == b {
		return
	}
	if isLabel(id.shown[a]) && !isLabel(id.shown[b]) {
		a, b = b, a
	}
	id.parent[b] = a
}

// Resolve gives the name a character is known by in the end.
func (id *identities) Resolve(name string) string {
	k := key(name)
	if _, ok := id.parent[k]; !ok {
		return strings.TrimSpace(name)
	}
	return id.shown[id.root(k)]
}

// mergeEntities groups what the passages say about each character, place...
// under one name per identity, notes and quotes in story order.
func mergeEntities(results []sectionResult) ([]*entity, *identities) {
	id := newIdentities()
	for _, r := range results {
		for _, e := range r.Entities {
			if strings.TrimSpace(e.Name) == "" {
				continue
			}
			id.add(e.Name)
			for _, a := range e.Aliases {
				if strings.TrimSpace(a) != "" && key(a) != key(e.Name) {
					id.link(e.Name, a)
				}
			}
		}
	}
	byRoot := map[string]*entity{}
	var order []*entity
	for _, r := range results {
		for _, e := range r.Entities {
			if strings.TrimSpace(e.Name) == "" || strings.TrimSpace(e.Description) == "" {
				continue
			}
			root := id.root(key(e.Name))
			ent := byRoot[root]
			if ent == nil {
				ent = &entity{Name: id.shown[root], Type: e.Type}
				byRoot[root] = ent
				order = append(order, ent)
			}
			if ent.Type == "" {
				ent.Type = e.Type
			}
			ent.notes = append(ent.notes, strings.TrimSpace(e.Description))
			ent.addQuotes(e.Quotes)
		}
	}
	// Every other name of an identity is an alias of it.
	for k := range id.parent {
		root := id.root(k)
		if ent := byRoot[root]; ent != nil && k != root && !containsFold(ent.aliases, id.shown[k]) {
			ent.aliases = append(ent.aliases, id.shown[k])
		}
	}
	for _, ent := range order {
		sort.Strings(ent.aliases)
	}
	return order, id
}

// asResult turns stored profiles into a first "passage", so merging new
// passages with them links names and aliases the same way.
func asResult(old []lore.Entity) sectionResult {
	var r sectionResult
	for _, o := range old {
		r.Entities = append(r.Entities, rawEntity{Name: o.Name, Type: o.Type, Description: o.Profile, Quotes: o.Quotes, Aliases: o.Aliases})
	}
	return r
}

// knownCharacters lists the characters met so far for the map step, in at
// most maxChars: name, a glimpse of their looks, their other labels. The
// most talked-about come first.
func knownCharacters(ents []*entity, maxChars int) string {
	var chars []*entity
	for _, e := range ents {
		if (lore.Entity{Type: e.Type}).IsCharacter() {
			chars = append(chars, e)
		}
	}
	sort.SliceStable(chars, func(i, j int) bool { return len(chars[i].notes) > len(chars[j].notes) })
	var sb strings.Builder
	for _, e := range chars {
		line := fmt.Sprintf("- %s: %s", e.Name, firstRunes(e.notes[0], 140))
		if len(e.aliases) > 0 {
			line += " [also called: " + strings.Join(e.aliases, "; ") + "]"
		}
		if utf8.RuneCountInString(sb.String())+utf8.RuneCountInString(line) > maxChars {
			break
		}
		sb.WriteString(line + "\n")
	}
	return strings.TrimSpace(sb.String())
}

func firstRunes(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}
