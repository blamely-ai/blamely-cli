package gitnotes

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blamely/blamely/internal/authorship"
	"github.com/blamely/blamely/internal/tools"
)

func TestOpenCodeCaptureToCommit(t *testing.T) {
	for _, major := range []int{1, 2} {
		for _, linked := range []bool{false, true} {
			t.Run(fmt.Sprintf("V%d/linked=%v", major, linked), func(t *testing.T) {
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
				if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("human committed\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				git(root, "add", ".")
				git(root, "commit", "-qm", "initial")
				checkout := root
				if linked {
					checkout = filepath.Join(t.TempDir(), "linked")
					git(root, "worktree", "add", "-qb", "feature", checkout)
				}
				file := filepath.Join(checkout, "file.txt")
				before := "human committed\nhuman uncommitted\n"
				after := before + "AI\n"
				if err := os.WriteFile(file, []byte(after), 0o644); err != nil {
					t.Fatal(err)
				}
				raw, _ := json.Marshal(map[string]any{"cwd": checkout, "file_path": file, "before": before, "after": after, "version": major, "session_id": "opencode:test", "model": "openai/test"})
				err := tools.RecordOpenCodeFromStdin(strings.NewReader(string(raw)))
				ctx, _ := authorship.ResolveContext(file)
				if linked && strings.HasPrefix(authorship.WorkingLogPath(ctx.RepoRoot, ctx.Branch, ctx.BaseSHA, ctx.RelPath), filepath.Join(ctx.RepoRoot, ".git")+string(filepath.Separator)) {
					// This PR is independently based on main. Before the separate
					// worktree fix merges, refuse unsafe recording, not master attribution.
					if err == nil || !strings.Contains(err.Error(), "checkout-local") {
						t.Fatalf("expected safe refusal: %v", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				git(checkout, "add", ".")
				git(checkout, "commit", "-qm", "OpenCode edit")
				note, err := AttributeAndWrite(checkout, git(checkout, "rev-parse", "HEAD"))
				if err != nil {
					t.Fatal(err)
				}
				var ai, human bool
				for _, f := range note.Files {
					for _, line := range f.Lines {
						if line.Type != "add" {
							continue
						}
						if line.Start <= 3 && line.End >= 3 && line.AuthorType == "AI" && line.Tool == "opencode" {
							ai = true
						}
						if line.Start <= 2 && line.End >= 2 && line.AuthorType == "Human" {
							human = true
						}
					}
				}
				if !ai || !human {
					t.Fatalf("incorrect committed attribution: %+v", note.Files)
				}
			})
		}
	}
}
