package install

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/blamely/blamely/internal/config"
)

// State is what we stash so `uninstall` can reverse the install.
type State struct {
	InstalledAt        time.Time `json:"installed_at"`
	BinaryPath         string    `json:"binary_path"`
	PriorCoreHooksPath string    `json:"prior_core_hookspath"`
	HadCoreHooksPath   bool      `json:"had_core_hookspath"`
	ClaudeHookAdded    bool      `json:"claude_hook_added"`
	CursorHookAdded    bool      `json:"cursor_hook_added"`
	CodexHookAdded     bool      `json:"codex_hook_added"`
	CopilotHookAdded   bool      `json:"copilot_hook_added"`
	GeminiHookAdded    bool      `json:"gemini_hook_added"`
	DevinHookAdded     bool      `json:"devin_hook_added"`
	// EditorExtensionsInstalled lists the editors (by label, e.g. "VS Code",
	// "Cursor", "Antigravity IDE") whose Blamely marketplace extension THIS
	// tool installed — never extensions the user installed themselves — so
	// `uninstall` removes only what it added.
	EditorExtensionsInstalled []string `json:"editor_extensions_installed,omitempty"`
	// JetBrainsPluginsInstalled lists the absolute <configDir>/plugins
	// directories THIS tool unzipped the Blamely plugin into — never plugins
	// the user installed via the IDE's own marketplace browser — so
	// `uninstall` removes only the directories it created.
	JetBrainsPluginsInstalled []string `json:"jetbrains_plugins_installed,omitempty"`
	PathEntryAdded            bool     `json:"path_entry_added"`
	PathRcFile                string   `json:"path_rc_file"`
	LaunchAgentInstalled      bool     `json:"launch_agent_installed"`
	GitHookInstalled          bool     `json:"git_hook_installed"`
}

func LoadState() (*State, error) {
	path, err := config.StateFile()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &State{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse state: %w", err)
	}
	return &s, nil
}

func SaveState(s *State) error {
	if _, err := config.EnsureBlamelyDir(); err != nil {
		return err
	}
	path, err := config.StateFile()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, data, 0o644)
}

// atomicWrite writes data to dst via a same-directory temp file + rename.
func atomicWrite(dst string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp("", "blamely-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), perm); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}
