package install

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/blamely/blamely/internal/config"
)

// isolateGlobalGitConfig points `git config --global` at a fresh file under a
// fake home, so the tests never touch the developer's real ~/.gitconfig.
func isolateGlobalGitConfig(t *testing.T) (hooksDir string) {
	t.Helper()
	home := fakeHomeDir(t)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, ".gitconfig"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	dir, err := config.GitHooksDir()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestIsBlamelyHooksDir_Spellings(t *testing.T) {
	own := isolateGlobalGitConfig(t)
	other := filepath.Join(filepath.Dir(own), "my-hooks")

	cases := map[string]bool{
		own:                              true,
		filepath.ToSlash(own):            true,
		own + string(filepath.Separator): true,
		"~/.blamely/git-hooks":           true,
		"":                               false,
		"   ":                            false,
		other:                            false,
		".husky":                         false,
		"~/.githooks":                    false,
	}
	if runtime.GOOS == "windows" {
		cases[strings.ToUpper(own)] = true
		cases[`~\.blamely\git-hooks`] = true
	}
	for v, want := range cases {
		if got := isBlamelyHooksDir(v); got != want {
			t.Errorf("isBlamelyHooksDir(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestStashedPriorHooksPath(t *testing.T) {
	own := isolateGlobalGitConfig(t)
	user := filepath.Join(t.TempDir(), "user-hooks")

	tests := []struct {
		name       string
		current    string
		hasCurrent bool
		stashed    string
		hadStashed bool
		wantPrior  string
		wantHad    bool
	}{
		{"first install, nothing set", "", false, "", false, "", false},
		{"first install over a user dir", user, true, "", false, user, true},
		{"reinstall keeps the user dir stashed earlier", own, true, user, true, user, true},
		{"reinstall with nothing stashed", own, true, "", false, "", false},
		{"reinstall over a bad stash of our own dir", own, true, own, true, "", false},
		{"user changed hooksPath since install", user, true, "/elsewhere", true, user, true},
		{"hooksPath unset since install", "", false, user, true, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, had := stashedPriorHooksPath(tc.current, tc.hasCurrent, tc.stashed, tc.hadStashed)
			if p != tc.wantPrior || had != tc.wantHad {
				t.Errorf("got (%q, %v), want (%q, %v)", p, had, tc.wantPrior, tc.wantHad)
			}
		})
	}
}

// The reported machine: a state file whose stashed prior is Blamely's own dir.
// Uninstall must leave core.hooksPath unset, not pointing at a deleted directory.
func TestUninstallGitHook_OwnDirPriorIsUnset(t *testing.T) {
	own := isolateGlobalGitConfig(t)
	if _, _, err := InstallGitHook("blamely"); err != nil {
		t.Fatal(err)
	}
	if err := UninstallGitHook(filepath.ToSlash(own), true); err != nil {
		t.Fatal(err)
	}
	if v, ok := readGlobalConfig("core.hooksPath"); ok {
		t.Errorf("core.hooksPath = %q after uninstall, want unset", v)
	}
	if _, err := os.Stat(own); !os.IsNotExist(err) {
		t.Errorf("hooks dir %s still present (err=%v)", own, err)
	}
}

// Install, reinstall (what `blamely update` does), uninstall: the user's own
// global hooks dir must come back, not Blamely's.
func TestGitHook_ReinstallThenUninstallRestoresUserDir(t *testing.T) {
	isolateGlobalGitConfig(t)
	user := filepath.ToSlash(filepath.Join(t.TempDir(), "user-hooks"))
	if err := setGlobalConfig("core.hooksPath", user); err != nil {
		t.Fatal(err)
	}

	var s State
	for i := 0; i < 2; i++ {
		cur, has, err := InstallGitHook("blamely")
		if err != nil {
			t.Fatal(err)
		}
		s.PriorCoreHooksPath, s.HadCoreHooksPath = stashedPriorHooksPath(cur, has, s.PriorCoreHooksPath, s.HadCoreHooksPath)
	}
	if s.PriorCoreHooksPath != user || !s.HadCoreHooksPath {
		t.Fatalf("stash after reinstall = (%q, %v), want (%q, true)", s.PriorCoreHooksPath, s.HadCoreHooksPath, user)
	}

	if err := UninstallGitHook(s.PriorCoreHooksPath, s.HadCoreHooksPath); err != nil {
		t.Fatal(err)
	}
	if v, ok := readGlobalConfig("core.hooksPath"); !ok || v != user {
		t.Errorf("core.hooksPath after uninstall = (%q, %v), want %q", v, ok, user)
	}
}
