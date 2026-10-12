package gitutil

import (
	"os"
	"path/filepath"
	"testing"
)

// Regression: a work tree reached through two aliases. git reports the top
// level as the volume's real path while the editor hands us the user-facing
// path (a Windows profile on a mounted volume, where EvalSymlinks does not
// follow the mount), so filepath.Rel can only say `..`. Every recorder then
// stored the ABSOLUTE path, which never matched the committed relative path,
// and the whole repo attributed to Human. Rel must ask git instead.
//
// The alias cannot be built portably, so the test drives the same code path
// from the other side: Toplevel is the real repo, and the path is given through
// a symlink to it (which the lexical attempts cannot relate to the real root
// until resolved — exercising the resolve step) and, for the git step, through
// a Location whose Toplevel is an alias nothing on this machine resolves.
func TestRel_SymlinkedAlias(t *testing.T) {
	repo := initRepo(t)
	commitFile(t, repo, "a.txt", "a\n")
	if err := os.MkdirAll(filepath.Join(repo, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "sub", "b.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink: %v", err)
	}

	cases := map[string]struct{ wt, p, want string }{
		"same spelling":                 {real, filepath.Join(real, "sub", "b.txt"), filepath.Join("sub", "b.txt")},
		"path through alias":            {real, filepath.Join(link, "sub", "b.txt"), filepath.Join("sub", "b.txt")},
		"root through alias":            {link, filepath.Join(real, "a.txt"), "a.txt"},
		"not yet written through alias": {real, filepath.Join(link, "sub", "new.txt"), filepath.Join("sub", "new.txt")},
		"new subtree through alias":     {real, filepath.Join(link, "x", "y", "new.txt"), filepath.Join("x", "y", "new.txt")},
		"directory through alias":       {real, filepath.Join(link, "sub"), "sub"},
		"top level itself":              {real, link, "."},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, ok := Rel(tc.wt, tc.p)
			if !ok || got != tc.want {
				t.Errorf("Rel(%s, %s) = (%q, %v), want %q", tc.wt, tc.p, got, ok, tc.want)
			}
		})
	}

	other := initRepo(t)
	if _, ok := Rel(real, filepath.Join(other, "c.txt")); ok {
		t.Error("a file in another repository must not resolve")
	}
	if _, ok := Rel(real, filepath.Join(t.TempDir(), "c.txt")); ok {
		t.Error("a file outside any repository must not resolve")
	}
}

// The git fallback proper: the caller's Toplevel is an alias this machine
// cannot resolve at all (the mounted-volume case), so only `rev-parse
// --show-prefix` from the path's own directory can name the file — and only
// when git agrees the path is in the SAME work tree.
func TestRel_UnresolvableAliasAsksGit(t *testing.T) {
	repo := initRepo(t)
	commitFile(t, repo, "a.txt", "a\n")
	if err := os.MkdirAll(filepath.Join(repo, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(repo)

	// Location as a hook would hold it for sub/b.txt when git spelled the top
	// level through an alias: the prefix came from the same rev-parse call.
	loc := Location{Toplevel: filepath.Join(string(filepath.Separator), "mount", "alias", "repo"),
		Prefix: "sub/", PrefixDir: filepath.Join(real, "sub"), PrefixKnown: true}
	if got, ok := loc.Rel(filepath.Join(real, "sub", "b.txt")); !ok || got != filepath.Join("sub", "b.txt") {
		t.Errorf("Location.Rel(file) = (%q, %v)", got, ok)
	}
	if got, ok := loc.Rel(filepath.Join(real, "sub")); !ok || got != "sub" {
		t.Errorf("Location.Rel(dir) = (%q, %v)", got, ok)
	}
	// A path below some OTHER directory can't reuse the prefix; it falls to
	// Rel, whose git step then refuses: git's top level is not the alias.
	if got, ok := loc.Rel(filepath.Join(real, "a.txt")); ok {
		t.Errorf("Location.Rel(other dir) = %q, want not ok (top level mismatch)", got)
	}
}

func TestLocate_Prefix(t *testing.T) {
	repo := initRepo(t)
	commitFile(t, repo, "a.txt", "a\n")
	if err := os.MkdirAll(filepath.Join(repo, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "sub", "b.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct{ p, prefix, rel string }{
		"root file":       {filepath.Join(repo, "a.txt"), "", "a.txt"},
		"file in sub":     {filepath.Join(repo, "sub", "b.txt"), "sub/", filepath.Join("sub", "b.txt")},
		"not yet written": {filepath.Join(repo, "sub", "new.txt"), "sub/", filepath.Join("sub", "new.txt")},
		"sub dir":         {filepath.Join(repo, "sub"), "sub/", "sub"},
		"root dir":        {repo, "", "."},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			loc := Locate(tc.p)
			if !loc.PrefixKnown || loc.Prefix != tc.prefix {
				t.Errorf("Locate(%s).Prefix = (%q, known=%v), want %q", tc.p, loc.Prefix, loc.PrefixKnown, tc.prefix)
			}
			if got, ok := loc.Rel(tc.p); !ok || got != tc.rel {
				t.Errorf("Locate(%s).Rel = (%q, %v), want %q", tc.p, got, ok, tc.rel)
			}
		})
	}
	if loc := Locate(filepath.Join(t.TempDir(), "x.txt")); loc.PrefixKnown {
		t.Errorf("outside a repo: PrefixKnown = true, Prefix %q", loc.Prefix)
	}
}
