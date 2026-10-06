package authorship

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// GCWorkingLogs must prune a log whose base object is gone, while keeping a log at
// the real HEAD and a non-SHA base (INITIAL).
func TestGCWorkingLogs(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	git := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "core.hooksPath="}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	git("checkout", "-q", "-b", "main")
	os.WriteFile(filepath.Join(repo, "seed"), []byte("x\n"), 0o644)
	git("add", ".")
	git("commit", "-q", "-m", "c1")
	head := git("rev-parse", "HEAD")

	// A log at the real HEAD (must survive), a log at a bogus/dangling base (must be
	// pruned), and a non-SHA base (INITIAL, must survive).
	dangling := strings.Repeat("a", 40)
	if _, err := Update(repo, "main", head, "f.txt", "a\n", "", HumanAuthor(), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := Update(repo, "main", dangling, "g.txt", "b\n", "", HumanAuthor(), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := Update(repo, "main", "INITIAL", "h.txt", "c\n", "", HumanAuthor(), 1); err != nil {
		t.Fatal(err)
	}

	pruned, err := GCWorkingLogs(repo)
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 1 {
		t.Errorf("pruned: got %d, want 1", pruned)
	}
	exists := func(branch, base string) bool {
		_, e := os.Stat(workingLogDir(repo, branch, base))
		return e == nil
	}
	if !exists("main", head) {
		t.Error("HEAD-based log must survive")
	}
	if exists("main", dangling) {
		t.Error("dangling-base log must be pruned")
	}
	if !exists("main", "INITIAL") {
		t.Error("INITIAL (non-SHA) base must survive")
	}
}

// gcChainRepo builds a repo whose main branch is a linear chain of n commits (one
// fast-import, not n `git commit`s) and returns the repo and the commit SHAs,
// oldest first.
func gcChainRepo(t *testing.T, n int) (string, []string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	git := func(stdin string, args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "core.hooksPath="}, args...)...)
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("", "init", "-q")
	git("", "symbolic-ref", "HEAD", "refs/heads/main")
	var fi strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&fi, "commit refs/heads/main\nmark :%d\ncommitter t <t@t> %d +0000\ndata 2\nc\n", i, 1700000000+i)
		if i > 1 {
			fmt.Fprintf(&fi, "from :%d\n", i-1)
		}
		fmt.Fprintf(&fi, "M 644 inline f\ndata %d\n%d\n\n", len(strconv.Itoa(i))+1, i)
	}
	git(fi.String(), "fast-import", "--quiet")
	shas := strings.Fields(git("", "rev-list", "--reverse", "main"))
	if len(shas) != n {
		t.Fatalf("chain: got %d commits, want %d", len(shas), n)
	}
	return repo, shas
}

// Bases deep behind HEAD are pruned once their commits are on a remote, but a base
// backing a commit that never reached a remote (e.g. its push failed) survives at
// any depth, so a retried push can still re-attribute from it.
func TestGCWorkingLogs_DepthAndUnpushed(t *testing.T) {
	const n = workingLogRetainDepth + 10
	cases := []struct {
		name       string
		remote     bool
		pushedUpTo int // index of the commit origin/main points at; -1 = no tracking ref
		wantDeep   bool
	}{
		{"no remote: deep base pruned", false, -1, false},
		{"all pushed: deep base pruned", true, n - 1, false},
		{"nothing pushed: deep base kept", true, -1, true},
		{"push failed after first commit: deep base kept", true, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo, shas := gcChainRepo(t, n)
			run := func(args ...string) {
				if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v\n%s", args, err, out)
				}
			}
			if tc.remote {
				run("remote", "add", "origin", filepath.Join(repo, "nonexistent"))
			}
			if tc.pushedUpTo >= 0 {
				run("update-ref", "refs/remotes/origin/main", shas[tc.pushedUpTo])
			}
			deep, shallow := shas[1], shas[n-2] // depth n-2 > retain depth; depth 1
			for _, base := range []string{deep, shallow} {
				if _, err := Update(repo, "main", base, "f", "x\n", "", HumanAuthor(), 1); err != nil {
					t.Fatal(err)
				}
			}

			if _, err := GCWorkingLogs(repo); err != nil {
				t.Fatal(err)
			}
			exists := func(base string) bool {
				_, e := os.Stat(workingLogDir(repo, "main", base))
				return e == nil
			}
			if got := exists(deep); got != tc.wantDeep {
				t.Errorf("deep base exists = %v, want %v", got, tc.wantDeep)
			}
			if !exists(shallow) {
				t.Error("shallow base must always survive")
			}
		})
	}
}
