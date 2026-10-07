package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blamely/blamely/internal/gitutil"
)

func TestWorktreeIngestion(t *testing.T) {
	root := t.TempDir()
	git := func(dir string, args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir, "-c", "core.hooksPath=", "-c", "user.name=Test", "-c", "user.email=test@example.com"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git(root, "init", "-qb", "main")
	os.WriteFile(filepath.Join(root, "file.txt"), []byte("main\n"), 0o644)
	git(root, "add", ".")
	git(root, "commit", "-qm", "initial")
	linked := filepath.Join(t.TempDir(), "linked")
	git(root, "worktree", "add", "-qb", "feature", linked)
	os.WriteFile(filepath.Join(linked, "file.txt"), []byte("linked HEAD\n"), 0o644)
	git(linked, "commit", "-qam", "feature base")
	base := git(linked, "rev-parse", "HEAD")
	os.WriteFile(filepath.Join(linked, "file.txt"), []byte("linked edit\n"), 0o644)
	repoID, _ := gitutil.RepoID(linked)
	checkout, _ := gitutil.Toplevel(linked)
	db := openTestDB(t)
	// Warm the main checkout cache before resolving the linked checkout.
	sessions.gitInfo(repoID)
	if err := validateAndStore(db, EditPayload{Tool: "codex", RepoPath: repoID, WorktreePath: checkout, FilePath: "file.txt", Lines: []Range{{Start: 1, End: 1}}}); err != nil {
		t.Fatal(err)
	}
	if err := (&dbSink{db: db}).Record(Event{Tool: "codex", RepoPath: repoID, WorktreePath: checkout, FilePath: "file.txt", Lines: []LineRange{{Start: 1, End: 1}}}); err != nil {
		t.Fatal(err)
	}
	edits, err := db.EditsForFileSince(repoID, "file.txt", 0)
	if err != nil || len(edits) != 2 {
		t.Fatalf("edits: %v %v", edits, err)
	}
	want, err := db.ResolveSession(repoID, "feature", base)
	if err != nil {
		t.Fatal(err)
	}
	for _, edit := range edits {
		if edit.Branch != "feature" || edit.SessionID.String != want {
			t.Fatalf("wrong branch/base session: %+v", edit)
		}
	}
	if got, ok, err := db.GetFileSnapshot(checkout, "file.txt"); err != nil || !ok || got != "linked edit\n" {
		t.Fatalf("worktree snapshot: %q %v %v", got, ok, err)
	}
	if _, ok, _ := db.GetFileSnapshot(repoID, "file.txt"); ok {
		t.Fatal("linked edit polluted main snapshot")
	}
}
