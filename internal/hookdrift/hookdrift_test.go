package hookdrift

import "testing"

func TestNoteReadReset(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	if got := Read(); len(got) != 0 {
		t.Fatalf("fresh tally = %v, want empty", got)
	}
	Note("copilot", "edit", []string{"toolName", "cwd"})
	Note("copilot", "rewrite", []string{"toolArgs"})
	e := Read()["copilot"]
	if e.Count != 2 || e.LastToolName != "rewrite" || len(e.LastKeys) != 1 || e.LastKeys[0] != "toolArgs" {
		t.Fatalf("entry = %+v", e)
	}
	if e.FirstSeen.IsZero() || e.LastSeen.Before(e.FirstSeen) {
		t.Errorf("timestamps = %v / %v", e.FirstSeen, e.LastSeen)
	}
	if err := Reset(); err != nil {
		t.Fatal(err)
	}
	if got := Read(); len(got) != 0 {
		t.Fatalf("after reset = %v, want empty", got)
	}
	if err := Reset(); err != nil {
		t.Fatalf("reset of a missing tally: %v", err)
	}
}
