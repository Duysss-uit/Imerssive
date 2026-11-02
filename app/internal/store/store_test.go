package store

import (
	"path/filepath"
	"testing"
)

func TestScenarioCastAndChatMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	sc := Scenario{Title: "Đêm Thanh Vân", Cast: []CastMember{{Name: "Mai Linh", Description: "Thanh mai trúc mã."}}}
	if err := st.SaveScenario(&sc); err != nil {
		t.Fatal(err)
	}
	ch := Chat{Mode: ModeScene, ScenarioID: sc.ID, PersonaName: "Lâm Phong"}
	if err := st.SaveChat(&ch); err != nil {
		t.Fatal(err)
	}
	st.Close()

	// Reopening runs the migrations again; they must be idempotent.
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	got, err := st.Scenario(sc.ID)
	if err != nil || len(got.Cast) != 1 || got.Cast[0].Name != "Mai Linh" {
		t.Fatalf("cast not round-tripped: %+v %v", got, err)
	}
	gotChat, err := st.Chat(ch.ID)
	if err != nil || gotChat.Mode != ModeScene || gotChat.PersonaName != "Lâm Phong" {
		t.Fatalf("chat mode not round-tripped: %+v %v", gotChat, err)
	}
}
