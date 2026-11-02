package chat

import (
	"strings"
	"testing"

	"immersive/internal/store"
)

func TestBuildSceneSystem(t *testing.T) {
	scen := store.Scenario{Title: "Đêm Thanh Vân", Setting: "Ma tộc tấn công làng.", Cast: []store.CastMember{
		{Name: "Lâm Phong", Description: "Kiếm sĩ trẻ."},
		{Name: "Mai Linh", Description: "Thanh mai trúc mã."},
	}}
	cards := []store.Character{{Name: "mai linh", SpeakingStyle: "Nói nhỏ nhẹ."}}
	chat := store.Chat{Mode: store.ModeScene, PersonaName: "Lâm Phong", PersonaDesc: "Kiếm sĩ trẻ."}

	got := buildSceneSystem(scen, chat, cards)
	cast := got[strings.Index(got, "<cast>"):strings.Index(got, "</cast>")]
	if strings.Contains(cast, "Lâm Phong") {
		t.Error("the player's own role must not be in the narrator's cast")
	}
	if !strings.Contains(cast, "Mai Linh") || !strings.Contains(cast, "Nói nhỏ nhẹ.") {
		t.Errorf("cast should include Mai Linh with her card's speaking style:\n%s", cast)
	}
	if !strings.Contains(got, `<player_character name="Lâm Phong">`) || strings.Contains(got, "{{user}}") {
		t.Errorf("placeholders not filled:\n%s", got)
	}
}
