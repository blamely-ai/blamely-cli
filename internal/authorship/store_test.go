package authorship

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Update persists across calls and chains edits using the STORED baseline, so the
// caller need not re-supply it — the type-then-AI sequence works end to end on disk.
func TestUpdate_RoundTripAndChaining(t *testing.T) {
	repo := t.TempDir()
	const branch, base, rel = "main", "0000000", "src/app.go"

	// 1) Human types two lines (first observed edit, no stored baseline).
	if _, err := Update(repo, branch, base, rel, joinLines("h1", "h2"), "", human(), 1); err != nil {
		t.Fatal(err)
	}
	// 2) AI appends — Update diffs against the STORED baseline ("h1\nh2"), not a
	//    re-supplied one, so the human lines must survive.
	wl, err := Update(repo, branch, base, rel, joinLines("h1", "h2", "ai1"), "", ai("claude"), 2)
	if err != nil {
		t.Fatal(err)
	}
	got := typesByLine(wl, 3)
	want := []AuthorType{Human, Human, AI}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d: want %q, got %q", i+1, want[i], got[i])
		}
	}

	// Reload from disk → identical.
	reloaded, err := LoadWorkingLog(repo, branch, base, rel)
	if err != nil || reloaded == nil {
		t.Fatalf("reload: %v (nil=%v)", err, reloaded == nil)
	}
	if got := typesByLine(reloaded, 3); !equalTypes(got, want) {
		t.Errorf("reloaded mismatch: %v", got)
	}
	if reloaded.File != rel || reloaded.BaseSHA != base || reloaded.Schema != WorkingLogSchema {
		t.Errorf("metadata wrong: %+v", reloaded)
	}
}

func TestRenameWorkingLogCarriesBaselineAndReplacesDestination(t *testing.T) {
	repo := t.TempDir()
	const branch, base = "main", "base"
	if _, err := Update(repo, branch, base, "old.txt", joinLines("human", "AI"), joinLines("human"), ai("claude"), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := Update(repo, branch, base, "new/renamed.txt", "replaced", "", human(), 2); err != nil {
		t.Fatal(err)
	}
	if err := RenameWorkingLog(repo, branch, base, "old.txt", "new/renamed.txt"); err != nil {
		t.Fatal(err)
	}
	wl, err := LoadWorkingLog(repo, branch, base, "new/renamed.txt")
	if err != nil || wl == nil || wl.File != "new/renamed.txt" || !equalTypes(typesByLine(wl, 2), []AuthorType{Human, AI}) {
		t.Fatalf("rename log: %+v %v", wl, err)
	}
	if baseline, ok := loadBaseline(BaselinePath(repo, branch, base, "new/renamed.txt")); !ok || baseline != joinLines("human", "AI") {
		t.Fatalf("rename baseline: %q %v", baseline, ok)
	}
	if wl, err := LoadWorkingLog(repo, branch, base, "old.txt"); err != nil || wl != nil {
		t.Fatalf("stale source: %+v %v", wl, err)
	}
	if _, ok := loadBaseline(BaselinePath(repo, branch, base, "old.txt")); ok {
		t.Fatal("stale source baseline")
	}
	if err := RenameWorkingLog(repo, branch, base, "new/renamed.txt", "new/renamed.txt"); err != nil {
		t.Fatal(err)
	}
}

// Paths must be OS-safe for filenames with spaces and branches with slashes — the
// exact cases that broke the old commit-time diff-path parsing.
func TestPaths_SpacesAndSlashes(t *testing.T) {
	repo := filepath.FromSlash("/tmp/repo")
	p := WorkingLogPath(repo, "feature/login", "abc123", "pages/login page.html")
	// Branch slash must be sanitized to a single component; filename space preserved.
	if strings.Contains(p, "feature/login") || strings.Contains(p, "feature\\login") {
		t.Errorf("branch slash not sanitized into one component: %s", p)
	}
	if !strings.Contains(p, "login page.html.json") {
		t.Errorf("spaced filename not preserved: %s", p)
	}
}

// Two concurrent writers must not corrupt the log; the per-file lock serializes
// the read-modify-write and the final state is one of the valid sequential results.
func TestUpdate_ConcurrentWritersDoNotCorrupt(t *testing.T) {
	repo := t.TempDir()
	const branch, base, rel = "main", "0", "f.txt"
	if _, err := Update(repo, branch, base, rel, "base", "", human(), 1); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, _ = Update(repo, branch, base, rel, joinLines("base", "edit"), "", ai("claude"), int64(n+2))
		}(i)
	}
	wg.Wait()
	wl, err := LoadWorkingLog(repo, branch, base, rel)
	if err != nil || wl == nil {
		t.Fatalf("load after concurrent writes: %v", err)
	}
	// Must be valid JSON we can read back, with the human base line intact.
	if got := typesByLine(wl, 2); got[0] != Human {
		t.Errorf("base line should remain Human after concurrent edits, got %q", got[0])
	}
}

func equalTypes(a, b []AuthorType) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
