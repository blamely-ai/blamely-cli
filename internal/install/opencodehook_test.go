package install

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The existing go test CI runs the real JS adapters too when Node is available.
// A Go-only installation remains buildable without a JavaScript toolchain.
func TestOpenCodeAdapters(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is unavailable; run node --test internal/install/opencode/bridge.test.mjs separately")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if output, err := exec.CommandContext(ctx, node, "--test", "opencode/bridge.test.mjs").CombinedOutput(); err != nil {
		t.Fatalf("OpenCode adapters: %v\n%s", err, output)
	}
}

func TestOpenCodeVersionFormats(t *testing.T) {
	for output, want := range map[string]int{"1.3.0": 1, "v1.3.0\n": 1, "2.0.24": 2, "opencode v2.0.24\n": 2, "opencode 2.0.24-beta.1": 2} {
		if got, err := parseOpenCodeMajor(output); err != nil || got != want {
			t.Fatalf("version %q: %d %v", output, got, err)
		}
	}
	for _, output := range []string{"3.0.0", "unknown", "2", "warning\n2.0.24"} {
		if _, err := parseOpenCodeMajor(output); err == nil {
			t.Fatalf("accepted unknown version %q", output)
		}
	}
}

func TestOpenCodeInstallVersionSwitchAndUninstall(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "opencode.jsonc")
	original := []byte("// user configuration\n{\"plugins\":[\"other-plugin\"]}\n")
	if err := os.WriteFile(config, original, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, major := range []int{1, 2, 1} {
		added, entry, err := InstallOpenCodePlugin(`C:\Program Files\Blamely\blamely.exe`, dir, major)
		if err != nil || !added {
			t.Fatalf("install V%d: %v %v", major, added, err)
		}
		data, err := os.ReadFile(entry)
		if err != nil || !bytes.HasPrefix(data, []byte(openCodeMarker)) || !bytes.Contains(data, []byte("../blamely/bridge.mjs")) {
			t.Fatalf("entry: %s %v", data, err)
		}
		if major == 1 && !bytes.Contains(data, []byte(`"tool.execute.before"`)) {
			t.Fatal("missing V1 hooks")
		}
		if major == 2 && !bytes.Contains(data, []byte(`id: "blamely.opencode"`)) {
			t.Fatal("missing V2 definition")
		}
		if added, _, err := InstallOpenCodePlugin(`C:\Program Files\Blamely\blamely.exe`, dir, major); err != nil || added {
			t.Fatalf("non-idempotent install: %v %v", added, err)
		}
	}
	bridge, _ := os.ReadFile(filepath.Join(dir, "blamely", "bridge.mjs"))
	if !bytes.Contains(bridge, []byte(`C:\\Program Files\\Blamely\\blamely.exe`)) {
		t.Fatal("binary path not JSON-escaped")
	}
	if data, _ := os.ReadFile(config); !bytes.Equal(data, original) {
		t.Fatal("user config modified")
	}
	userFile := filepath.Join(dir, "blamely", "user.txt")
	if err := os.WriteFile(userFile, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if removed, err := uninstallOpenCodePlugin(dir); err != nil || !removed {
		t.Fatalf("uninstall: %v %v", removed, err)
	}
	if _, err := os.Stat(userFile); err != nil {
		t.Fatal("user file removed")
	}
	if removed, err := uninstallOpenCodePlugin(dir); err != nil || removed {
		t.Fatalf("non-idempotent uninstall: %v %v", removed, err)
	}
}

func TestOpenCodeInstallPreservesUnmanagedFiles(t *testing.T) {
	dir := t.TempDir()
	entry := filepath.Join(dir, "plugins", "blamely.ts")
	if err := os.MkdirAll(filepath.Dir(entry), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entry, []byte("user plugin"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := InstallOpenCodePlugin("blamely", dir, 2); err == nil {
		t.Fatal("overwrote unmanaged file")
	}
	if _, err := os.Stat(filepath.Join(dir, "blamely", "bridge.mjs")); !os.IsNotExist(err) {
		t.Fatal("partially wrote helper before ownership check")
	}
	if removed, err := uninstallOpenCodePlugin(dir); err != nil || removed {
		t.Fatalf("removed unmanaged file: %v %v", removed, err)
	}
	if _, _, err := InstallOpenCodePlugin("blamely", dir, 3); err == nil {
		t.Fatal("accepted unknown major")
	}
}

func TestOpenCodeConfigPathsAndDetection(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("OPENCODE_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	dir, err := OpenCodeConfigDir()
	if err != nil || dir != filepath.Join(home, ".config", "opencode") {
		t.Fatalf("default: %s %v", dir, err)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	dir, _ = OpenCodeConfigDir()
	if !strings.Contains(dir, "xdg") {
		t.Fatal(dir)
	}
	t.Setenv("OPENCODE_CONFIG_DIR", filepath.Join(home, "custom"))
	dir, _ = OpenCodeConfigDir()
	if dir != filepath.Join(home, "custom") {
		t.Fatal(dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if !detectOpenCode().Present {
		t.Fatal("custom OpenCode config was not detected")
	}
}
