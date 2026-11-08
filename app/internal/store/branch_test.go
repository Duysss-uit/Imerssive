package store

import (
	"path/filepath"
	"testing"
)

func TestRollBackAndBranch(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c := Chat{Title: "Cứu Mai Linh", PersonaName: "Lâm Phong"}
	st.SaveChat(&c)
	var ids []int64
	for i, text := range []string{"mở đầu", "phá cửa", "Mai Linh khóc", "chạy trốn", "bị đuổi kịp", "giao chiến"} {
		role := "assistant"
		if i%2 == 1 {
			role = "user"
		}
		m, _ := st.AddMessage(c.ID, role, text)
		ids = append(ids, m.ID)
	}
	st.SetDivergence(c.ID, "- đã phá cửa ngục", ids[2])
	st.SetDivergence(c.ID, "- đã phá cửa ngục\n- đang chạy trốn", ids[4])
	st.MarkSummarized(c.ID, ids[3])
	if st.MaxSummarizedID(c.ID) != ids[3] {
		t.Errorf("max summarized = %d", st.MaxSummarizedID(c.ID))
	}

	// A branch from message 3 keeps the original whole.
	b, err := st.Branch(c, ids[2], "Cứu Mai Linh · nhánh")
	if err != nil {
		t.Fatal(err)
	}
	bm, _ := st.Messages(b.ID, false)
	if len(bm) != 3 || bm[2].Content != "Mai Linh khóc" || bm[0].Summarized {
		t.Fatalf("branch messages: %+v", bm)
	}
	if got, _ := st.Chat(b.ID); got.Divergence != "- đã phá cửa ngục" || got.DivergenceUpTo != bm[2].ID || got.Title != "Cứu Mai Linh · nhánh" {
		t.Errorf("branch chat: %+v", got)
	}
	if om, _ := st.Messages(c.ID, false); len(om) != 6 {
		t.Errorf("original lost messages: %d", len(om))
	}

	// Rolling back to message 4 drops the rest and the log written after it.
	if n, err := st.RollBack(c.ID, ids[3]); err != nil || n != 2 {
		t.Fatalf("rolled back %d, %v", n, err)
	}
	got, _ := st.Chat(c.ID)
	if got.Divergence != "- đã phá cửa ngục" || got.DivergenceUpTo != ids[2] {
		t.Errorf("divergence after rollback: %q up to %d", got.Divergence, got.DivergenceUpTo)
	}
	// Rolling back before any log leaves it empty.
	st.RollBack(c.ID, ids[0])
	if got, _ := st.Chat(c.ID); got.Divergence != "" || got.DivergenceUpTo != 0 {
		t.Errorf("divergence should be empty: %+v", got)
	}
}
