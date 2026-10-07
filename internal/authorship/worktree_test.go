package authorship

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blamely/blamely/internal/gitutil"
)

func TestLinkedWorktreeWorkingLogs(t *testing.T) {
	root := t.TempDir()
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "core.hooksPath=", "-c", "user.name=Test", "-c", "user.email=test@example.com"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git(root, "init", "-q", "-b", "main")
	file := filepath.Join(root, "file.txt")
	if err := os.WriteFile(file, []byte("human\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(root, "add", ".")
	git(root, "commit", "-qm", "initial")
	linked := filepath.Join(t.TempDir(), "linked")
	git(root, "worktree", "add", "-qb", "feature", linked)
	base := git(linked, "rev-parse", "HEAD")
	linkedFile := filepath.Join(linked, "file.txt")
	if err := CaptureBaseline(linkedFile); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(linkedFile, []byte("human\nAI\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wl, err := RecordEdit(linkedFile, Author{Type: AI, Tool: "codex", Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if wl.Lines[len(wl.Lines)-1].Author.Type != AI {
		t.Fatalf("missing AI attribution: %+v", wl)
	}
	path := WorkingLogPath(linked, "feature", base, "file.txt")
	if !strings.HasPrefix(path, gitutil.GitDir(linked)+string(filepath.Separator)) {
		t.Fatalf("log outside worktree gitdir: %s", path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if logs, err := ListWorkingLogs(linked, "feature", base); err != nil || len(logs) != 1 {
		t.Fatalf("list: %v %v", logs, err)
	}
	if _, err := GCWorkingLogs(linked); err != nil {
		t.Fatal(err)
	}
	if adopted := AdoptWorkingLogsAtBase(root, "main", base); adopted != 0 {
		t.Fatalf("main adopted linked worktree logs: %d", adopted)
	}
	// Even detached sibling checkouts at the same HEAD must not share a baseline.
	sibling := filepath.Join(t.TempDir(), "detached")
	git(root, "worktree", "add", "-q", "--detach", sibling, base)
	if WorkingLogPath(linked, "DETACHED", base, "file.txt") == WorkingLogPath(sibling, "DETACHED", base, "file.txt") {
		t.Fatal("detached logs collide")
	}
	if data, _ := os.ReadFile(file); string(data) != "human\n" {
		t.Fatal("main checkout was changed")
	}
}
