package ingest

import (
	"strings"
	"testing"

	"immersive/internal/lore"
)

// Labels from separate pages end up as one character once the map step links
// them, under the real name revealed later; an unlinked label stays apart.
func TestMergeEntitiesFollowsAliases(t *testing.T) {
	results := []sectionResult{
		{Entities: []rawEntity{
			{Name: "[?] boy · brown hair · thin", Type: "character", Description: "Bị gọi lên phòng thi.", Quotes: []string{"V-vâng!"}},
			{Name: "[?] woman · black hair · glasses", Type: "character", Description: "Giám khảo lạnh lùng."},
		}},
		{Entities: []rawEntity{
			{Name: "Số 105", Type: "character", Description: "Điểm thấp mọi bài thi.", Aliases: []string{"[?] boy · brown hair · thin"}},
		}},
		{Entities: []rawEntity{
			// The name comes out; the earlier label and number are the same person.
			{Name: "Sou", Type: "character", Description: "Tên thật là Sou.", Quotes: []string{"Mẹ ơi…"}, Aliases: []string{"Số 105"}},
			{Name: "[?] girl · red hair", Type: "character", Description: "Một người khác."},
		}},
	}
	ents, ids := mergeEntities(results)
	var sou *entity
	for _, e := range ents {
		if e.Name == "Sou" {
			sou = e
		}
	}
	if len(ents) != 3 || sou == nil {
		t.Fatalf("entities: %+v", ents)
	}
	if len(sou.notes) != 3 || sou.notes[0] != "Bị gọi lên phòng thi." || strings.Join(sou.quotes, "|") != "V-vâng!|Mẹ ơi…" {
		t.Errorf("Sou: notes %q quotes %q (want all three, in story order)", sou.notes, sou.quotes)
	}
	if strings.Join(sou.aliases, "|") != "Số 105|[?] boy · brown hair · thin" {
		t.Errorf("aliases = %q", sou.aliases)
	}
	if got := ids.Resolve("[?] boy · brown hair · thin"); got != "Sou" {
		t.Errorf("label resolves to %q", got)
	}

	// A real name is never replaced by a label, whoever claims whom.
	ents, _ = mergeEntities([]sectionResult{{Entities: []rawEntity{
		{Name: "Mai Linh", Type: "character", Description: "a"},
		{Name: "[?] girl · black hair", Type: "character", Description: "b", Aliases: []string{"Mai Linh"}},
	}}})
	if len(ents) != 1 || ents[0].Name != "Mai Linh" {
		t.Errorf("real name lost: %+v", ents)
	}

	// Stored profiles come first and keep their aliases.
	old := asResult([]lore.Entity{{Name: "Sou", Type: "character", Profile: "Hồ sơ cũ.", Aliases: []string{"Số 105"}}})
	ents, _ = mergeEntities([]sectionResult{old, {Entities: []rawEntity{{Name: "Số 105", Type: "character", Description: "Mới."}}}})
	if len(ents) != 1 || ents[0].Name != "Sou" || ents[0].notes[0] != "Hồ sơ cũ." {
		t.Errorf("continuation: %+v", ents)
	}
	if k := knownCharacters(ents, 500); !strings.Contains(k, "- Sou: Hồ sơ cũ. [also called: Số 105]") {
		t.Errorf("known = %q", k)
	}
}
