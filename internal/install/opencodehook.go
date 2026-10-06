package install

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/blamely/blamely/internal/procattr"
)

//go:embed opencode/bridge.mjs opencode/v1.mjs opencode/v2.mjs
var openCodePlugins embed.FS

const openCodeMarker = "// Blamely-managed OpenCode"

func OpenCodeConfigDir() (string, error) {
	if dir := os.Getenv("OPENCODE_CONFIG_DIR"); dir != "" {
		return filepath.Abs(dir)
	}
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "opencode"), nil
	}
	home, err := os.UserHomeDir()
	return filepath.Join(home, ".config", "opencode"), err
}

func openCodeMajor() (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := procattr.Hide(exec.CommandContext(ctx, "opencode", "--version")).Output()
	if err != nil {
		return 0, fmt.Errorf("detect OpenCode version: %w (use install-opencode --major 1 or 2)", err)
	}
	return parseOpenCodeMajor(string(out))
}

func parseOpenCodeMajor(output string) (int, error) {
	version := strings.TrimSpace(output)
	match := regexp.MustCompile(`^(?:opencode\s+)?v?([0-9]+)\.[0-9]+\.[0-9]+(?:[-+][^\s]+)?$`).FindStringSubmatch(version)
	if len(match) == 2 {
		major, err := strconv.Atoi(match[1])
		if err == nil && (major == 1 || major == 2) {
			return major, nil
		}
	}
	return 0, fmt.Errorf("unsupported OpenCode version %q (expected V1 or V2)", version)
}

func InstallOpenCodeHook(binaryPath string) (bool, string, error) {
	major, err := openCodeMajor()
	if err != nil {
		return false, "", err
	}
	dir, err := OpenCodeConfigDir()
	if err != nil {
		return false, "", err
	}
	return InstallOpenCodePlugin(binaryPath, dir, major)
}

// InstallOpenCodePlugin installs exactly one adapter. Helpers are outside the
// discovered plugins directory, so they cannot be loaded as additional plugins.
// It leaves opencode.json(c), dependency manifests and other plugins untouched.
func InstallOpenCodePlugin(binaryPath, dir string, major int) (bool, string, error) {
	entry := filepath.Join(dir, "plugins", "blamely.ts")
	if major != 1 && major != 2 {
		return false, entry, fmt.Errorf("OpenCode major must be 1 or 2")
	}
	adapter, _ := openCodePlugins.ReadFile(fmt.Sprintf("opencode/v%d.mjs", major))
	adapter = bytes.ReplaceAll(adapter, []byte(`"./bridge.mjs"`), []byte(`"../blamely/bridge.mjs"`))
	bridge, _ := openCodePlugins.ReadFile("opencode/bridge.mjs")
	encoded, _ := json.Marshal(binaryPath)
	bridge = bytes.Replace(bridge, []byte(`const binary = "blamely"`), append([]byte("const binary = "), encoded...), 1)
	files := []struct {
		path string
		data []byte
	}{
		{filepath.Join(dir, "blamely", "bridge.mjs"), bridge}, {entry, adapter},
	}
	// Check ownership of every target before changing any of them.
	for _, file := range files {
		info, err := os.Lstat(file.path)
		if err == nil && !info.Mode().IsRegular() {
			return false, entry, fmt.Errorf("refusing non-regular plugin file %s", file.path)
		}
		data, err := os.ReadFile(file.path)
		if err != nil && !os.IsNotExist(err) {
			return false, entry, err
		}
		if err == nil && !bytes.HasPrefix(data, []byte(openCodeMarker)) {
			return false, entry, fmt.Errorf("refusing to overwrite unmanaged OpenCode plugin %s", file.path)
		}
	}
	changed := false
	for _, file := range files {
		data, _ := os.ReadFile(file.path)
		if bytes.Equal(data, file.data) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(file.path), 0o755); err != nil {
			return changed, entry, err
		}
		if err := os.WriteFile(file.path, file.data, 0o644); err != nil {
			return changed, entry, err
		}
		changed = true
	}
	return changed, entry, nil
}

func UninstallOpenCodeHook() (bool, error) {
	dir, err := OpenCodeConfigDir()
	if err != nil {
		return false, err
	}
	return uninstallOpenCodePlugin(dir)
}

func uninstallOpenCodePlugin(dir string) (bool, error) {
	removed := false
	for _, path := range []string{filepath.Join(dir, "plugins", "blamely.ts"), filepath.Join(dir, "blamely", "bridge.mjs")} {
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return removed, err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return removed, err
		}
		if !bytes.HasPrefix(data, []byte(openCodeMarker)) {
			continue
		}
		if err := os.Remove(path); err != nil {
			return removed, err
		}
		removed = true
	}
	// Remove only an empty owned helper directory, never user files.
	_ = os.Remove(filepath.Join(dir, "blamely"))
	return removed, nil
}
