package authorship

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/blamely/blamely/internal/gitutil"
)

// Regression: when git spells the work tree through an alias the file path
// does not share (a Windows profile on a mounted volume: git says
// C:\SnapVolumesTemp\…\Users\u\repo, the editor says C:\Users\u\repo),
// ContextAt's filepath.Rel answered `..` and the file was treated as outside
// the work tree — so no working log was ever written and every edit in the
// repo committed as Human. The prefix Locate fetched from git names the file
// without either spelling.
func TestContextAt_AliasedToplevelUsesGitPrefix(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if err := os.MkdirAll(filepath.Join(repo, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(repo, "sub", "b.txt")
	if err := os.WriteFile(file, []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(repo)
	alias := filepath.Join(string(filepath.Separator), "mount", "alias", "repo") // resolves nowhere on this machine

	loc := gitutil.Location{RepoID: alias, Toplevel: alias, HeadSHA: "0123456789abcdef0123456789abcdef01234567",
		Prefix: "sub/", PrefixDir: filepath.Join(real, "sub"), PrefixKnown: true}
	ctx, ok := ContextAt(loc, file)
	if !ok {
		t.Fatal("ContextAt: not ok — file treated as outside the work tree")
	}
	if ctx.RelPath != "sub/b.txt" {
		t.Errorf("RelPath = %q, want sub/b.txt", ctx.RelPath)
	}
	if ctx.RepoRoot != alias {
		t.Errorf("RepoRoot = %q, want git's spelling %q", ctx.RepoRoot, alias)
	}
	if ctx.BaseSHA != loc.HeadSHA {
		t.Errorf("BaseSHA = %q", ctx.BaseSHA)
	}
}
