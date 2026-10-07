package gitutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Locate must return exactly what RepoID, Toplevel and HeadSHA return one by
// one — they key the working log (<branch>/<base_sha>/…) and the edits table, so
// any drift would file the same edit somewhere else.
func TestLocate_MatchesSeparateCalls(t *testing.T) {
	repo := initRepo(t)
	commitFile(t, repo, "a.txt", "a\n")
	if err := os.MkdirAll(filepath.Join(repo, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "sub", "b.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	unborn := initRepo(t) // no commit yet: the combined call fails on HEAD
	if err := os.WriteFile(filepath.Join(unborn, "c.txt"), []byte("c\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(t.TempDir(), "linked")
	if out, err := exec.Command("git", "-C", repo, "worktree", "add", "-q", wt).CombinedOutput(); err != nil {
		t.Fatalf("worktree add: %v\n%s", err, out)
	}

	cases := map[string]string{
		"committed file":         filepath.Join(repo, "a.txt"),
		"untracked file in sub":  filepath.Join(repo, "sub", "b.txt"),
		"not yet written":        filepath.Join(repo, "sub", "new.txt"),
		"repo root dir":          repo,
		"unborn repo":            filepath.Join(unborn, "c.txt"),
		"linked worktree":        filepath.Join(wt, "a.txt"),
		"outside any repository": filepath.Join(t.TempDir(), "x.txt"),
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			var want Location
			want.RepoID, _ = RepoID(p)
			if top, ok := Toplevel(p); ok {
				want.Toplevel = top
				want.HeadSHA = HeadSHA(top)
			}
			if got := Locate(p); got != want {
				t.Errorf("Locate(%s)\n got  %+v\n want %+v", p, got, want)
			}
		})
	}
}
