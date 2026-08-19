package install

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blamely/blamely/internal/config"
)

// setupFakeDevinHome points config.DevinConfigPath at a temp dir and optionally
// seeds a config.json. XDG_CONFIG_HOME and APPDATA are cleared/redirected too,
// since DevinConfigDir consults both — without that a developer's real config
// would be the test target.
func setupFakeDevinHome(t *testing.T, content string) string {
	t.Helper()
	home := fakeHomeDir(t)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))

	dir, err := config.DevinConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if content != "" {
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func devinConfigAt(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read devin config: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse devin config: %v", err)
	}
	return m
}

// devinPostToolUseGroups returns the PostToolUse matcher groups from a parsed config.
func devinPostToolUseGroups(t *testing.T, m map[string]any) []any {
	t.Helper()
	hooks, _ := m["hooks"].(map[string]any)
	if hooks == nil {
		t.Fatalf("no hooks key: %#v", m)
	}
	groups, _ := hooks["PostToolUse"].([]any)
	return groups
}

func TestInstallDevinHook_NoExistingConfig(t *testing.T) {
	setupFakeDevinHome(t, "")
	added, configPath, err := InstallDevinHook("/usr/local/bin/blamely")
	if err != nil {
		t.Fatalf("InstallDevinHook: %v", err)
	}
	if !added {
		t.Error("expected added=true")
	}
	groups := devinPostToolUseGroups(t, devinConfigAt(t, configPath))
	if len(groups) != 1 {
		t.Fatalf("expected 1 PostToolUse group, got %d", len(groups))
	}
	grp, _ := groups[0].(map[string]any)
	if matcher, _ := grp["matcher"].(string); matcher != devinHookMatcher {
		t.Errorf("matcher = %q, want %q", matcher, devinHookMatcher)
	}
	inner, _ := grp["hooks"].([]any)
	if len(inner) != 1 {
		t.Fatalf("expected 1 hook, got %d", len(inner))
	}
	hm, _ := inner[0].(map[string]any)
	cmd, _ := hm["command"].(string)
	if !strings.Contains(cmd, devinBlamelyMarker) {
		t.Errorf("command %q missing marker %q", cmd, devinBlamelyMarker)
	}
}

func TestInstallDevinHook_Idempotent(t *testing.T) {
	setupFakeDevinHome(t, "")
	if _, _, err := InstallDevinHook("/bin/blamely"); err != nil {
		t.Fatal(err)
	}
	added, configPath, err := InstallDevinHook("/bin/blamely")
	if err != nil {
		t.Fatal(err)
	}
	if added {
		t.Error("second install reported a change; want no-op")
	}
	groups := devinPostToolUseGroups(t, devinConfigAt(t, configPath))
	if len(groups) != 1 {
		t.Fatalf("hook groups stacked: got %d, want 1", len(groups))
	}
}

// A re-install after the binary moved must replace the stale command, not add
// a second one alongside it.
func TestInstallDevinHook_ReplacesStaleBinaryPath(t *testing.T) {
	setupFakeDevinHome(t, "")
	if _, _, err := InstallDevinHook("/old/path/blamely"); err != nil {
		t.Fatal(err)
	}
	_, configPath, err := InstallDevinHook("/new/path/blamely")
	if err != nil {
		t.Fatal(err)
	}
	groups := devinPostToolUseGroups(t, devinConfigAt(t, configPath))
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	grp, _ := groups[0].(map[string]any)
	inner, _ := grp["hooks"].([]any)
	if len(inner) != 1 {
		t.Fatalf("expected 1 hook after re-install, got %d", len(inner))
	}
	hm, _ := inner[0].(map[string]any)
	if cmd, _ := hm["command"].(string); !strings.Contains(cmd, "/new/path/blamely") {
		t.Errorf("stale command survived: %q", cmd)
	}
}

func TestInstallDevinHook_PreservesUnrelatedSettingsAndHooks(t *testing.T) {
	seed := `{
  "agent": {"model": "claude-sonnet-4-6"},
  "hooks": {
    "PostToolUse": [
      {"matcher": "exec", "hooks": [{"type": "command", "command": "/usr/bin/audit.sh"}]}
    ],
    "SessionEnd": [
      {"matcher": "", "hooks": [{"type": "command", "command": "/usr/bin/cleanup.sh"}]}
    ]
  }
}`
	setupFakeDevinHome(t, seed)
	_, configPath, err := InstallDevinHook("/bin/blamely")
	if err != nil {
		t.Fatal(err)
	}
	m := devinConfigAt(t, configPath)

	agent, _ := m["agent"].(map[string]any)
	if agent == nil || agent["model"] != "claude-sonnet-4-6" {
		t.Errorf("unrelated agent settings lost: %#v", m["agent"])
	}
	hooks, _ := m["hooks"].(map[string]any)
	if _, ok := hooks["SessionEnd"]; !ok {
		t.Error("unrelated SessionEnd hooks removed")
	}
	groups, _ := hooks["PostToolUse"].([]any)
	if len(groups) != 2 {
		t.Fatalf("expected blamely group + user group, got %d", len(groups))
	}
	// blamely must run FIRST so a failing third-party hook cannot preempt it.
	first, _ := groups[0].(map[string]any)
	if matcher, _ := first["matcher"].(string); matcher != devinHookMatcher {
		t.Errorf("blamely group is not first; got matcher %q", matcher)
	}
}

// Devin documents its config as JSON *with comment support*, so a commented
// config must not make install fail.
func TestInstallDevinHook_TolerartesJSONComments(t *testing.T) {
	seed := `{
  // the model Devin should use
  "agent": {"model": "gpt-5"}, // trailing comment
  /* block
     comment */
  "theme_mode": "dark"
}`
	setupFakeDevinHome(t, seed)
	added, configPath, err := InstallDevinHook("/bin/blamely")
	if err != nil {
		t.Fatalf("InstallDevinHook on JSONC config: %v", err)
	}
	if !added {
		t.Error("expected added=true")
	}
	m := devinConfigAt(t, configPath)
	if m["theme_mode"] != "dark" {
		t.Errorf("setting after a block comment lost: %#v", m)
	}
	agent, _ := m["agent"].(map[string]any)
	if agent == nil || agent["model"] != "gpt-5" {
		t.Errorf("setting on a commented line lost: %#v", m["agent"])
	}
	if len(devinPostToolUseGroups(t, m)) != 1 {
		t.Error("hook not installed into JSONC config")
	}
}

func TestUninstallDevinHook_RemovesOnlyBlamely(t *testing.T) {
	seed := `{
  "hooks": {
    "PostToolUse": [
      {"matcher": "exec", "hooks": [{"type": "command", "command": "/usr/bin/audit.sh"}]}
    ]
  }
}`
	setupFakeDevinHome(t, seed)
	if _, _, err := InstallDevinHook("/bin/blamely"); err != nil {
		t.Fatal(err)
	}
	removed, err := UninstallDevinHook()
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatal("expected removed=true")
	}
	configPath, err := config.DevinConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	m := devinConfigAt(t, configPath)
	groups := devinPostToolUseGroups(t, m)
	if len(groups) != 1 {
		t.Fatalf("expected the user's group to survive alone, got %d", len(groups))
	}
	grp, _ := groups[0].(map[string]any)
	if matcher, _ := grp["matcher"].(string); matcher != "exec" {
		t.Errorf("wrong group survived: matcher %q", matcher)
	}
}

func TestUninstallDevinHook_NoConfigIsNotAnError(t *testing.T) {
	fakeHomeDir(t)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("APPDATA", t.TempDir())
	removed, err := UninstallDevinHook()
	if err != nil {
		t.Fatalf("UninstallDevinHook with no config: %v", err)
	}
	if removed {
		t.Error("expected removed=false")
	}
}

func TestStripJSONComments(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string // parsed value of key "u"
	}{
		{"line comment", "{\"u\": \"a\" // note\n}", "a"},
		{"block comment", `{/* x */ "u": "a"}`, "a"},
		{"url inside string survives", `{"u": "https://example.com/x"}`, "https://example.com/x"},
		{"escaped quote does not end string", `{"u": "a\"// b"}`, `a"// b`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var m map[string]any
			if err := json.Unmarshal(stripJSONComments([]byte(tc.in)), &m); err != nil {
				t.Fatalf("parse after strip: %v (input %s)", err, tc.in)
			}
			if got, _ := m["u"].(string); got != tc.want {
				t.Errorf("u = %q, want %q", got, tc.want)
			}
		})
	}
}
