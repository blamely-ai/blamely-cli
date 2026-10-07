package gitnotes

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blamely/blamely/internal/authorship"
	"github.com/blamely/blamely/internal/tools"
)

// Exercise the real CLI-hook -> working log -> committed note path with no daemon.
func TestLinkedWorktreeCodexCommit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
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
	// AttributeAndWrite runs git notes itself, with HOME pointing at a temp dir.
	git(root, "config", "user.name", "Test")
	git(root, "config", "user.email", "test@example.com")
	os.WriteFile(filepath.Join(root, "file.txt"), []byte("human\n"), 0o644)
	git(root, "add", ".")
	git(root, "commit", "-qm", "initial")
	linked := filepath.Join(t.TempDir(), "linked")
	git(root, "worktree", "add", "-qb", "feature", linked)
	file := filepath.Join(linked, "file.txt")
	// An unobserved human line must not become AI on the first hook observation.
	os.WriteFile(file, []byte("human\nhuman uncommitted\n"), 0o644)
	pre, _ := json.Marshal(map[string]any{"cwd": linked, "tool_name": "Edit", "tool_input": map[string]any{"file_path": file}})
	if err := tools.CaptureBaselineFromStdin(strings.NewReader(string(pre))); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(file, []byte("human\nhuman uncommitted\nAI\n"), 0o644)
	post, _ := json.Marshal(map[string]any{"cwd": linked, "tool_name": "Edit", "model": "test-model", "tool_input": map[string]any{"file_path": file, "old_string": "human\nhuman uncommitted\n", "new_string": "human\nhuman uncommitted\nAI\n"}})
	if err := tools.RecordCodexFromStdin(strings.NewReader(string(post))); err != nil {
		t.Fatal(err)
	}
	ctx, _ := authorship.ResolveContext(file)
	if wl, err := authorship.LoadWorkingLog(ctx.RepoRoot, ctx.Branch, ctx.BaseSHA, ctx.RelPath); err != nil || wl == nil {
		t.Fatalf("capture missing: %v", err)
	}
	git(linked, "add", ".")
	git(linked, "commit", "-qm", "feature edit")
	note, err := AttributeAndWrite(linked, git(linked, "rev-parse", "HEAD"))
	if err != nil {
		t.Fatal(err)
	}
	var ai, human bool
	for _, file := range note.Files {
		for _, line := range file.Lines {
			if line.Type == "add" && line.Start <= 3 && line.End >= 3 && line.AuthorType == "AI" && line.Tool == "codex" {
				ai = true
			}
			if line.Type == "add" && line.Start <= 2 && line.End >= 2 && line.AuthorType == "Human" {
				human = true
			}
		}
	}
	if !ai || !human {
		t.Fatalf("wrong committed attribution: %+v", note.Files)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "file.txt")); string(data) != "human\n" {
		t.Fatal("main checkout polluted")
	}
}
