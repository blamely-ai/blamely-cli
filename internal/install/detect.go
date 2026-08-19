package install

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/blamely/blamely/internal/config"
)

// Detected is the set of AI tools found on the current machine.
type Detected struct {
	Claude  ToolPresence
	Cursor  ToolPresence
	Codex   ToolPresence
	Copilot ToolPresence
	Gemini  ToolPresence
	Devin   ToolPresence
}

type ToolPresence struct {
	Present bool
	// Hints is the list of paths/binaries that triggered detection.
	Hints []string
}

func (p ToolPresence) FirstHint() string {
	if len(p.Hints) == 0 {
		return ""
	}
	return p.Hints[0]
}

func Detect() (*Detected, error) {
	d := &Detected{}
	d.Claude = detectClaude()
	d.Cursor = detectCursor()
	d.Codex = detectCodex()
	d.Copilot = detectCopilot()
	d.Gemini = detectGemini()
	d.Devin = detectDevin()
	return d, nil
}

func detectClaude() ToolPresence {
	var hints []string
	// Check every Claude config location in the union (default + custom), so a
	// corp CLAUDE_CONFIG_DIR counts as "present" too.
	for _, settings := range config.ClaudeSettingsPaths() {
		if fileExists(settings) {
			hints = append(hints, settings)
		} else if dirExists(filepath.Dir(settings)) {
			hints = append(hints, filepath.Dir(settings))
		}
	}
	if path, ok := lookPath("claude"); ok {
		hints = append(hints, path)
	}
	return presence(hints)
}

func detectCursor() ToolPresence {
	home, err := config.Home()
	if err != nil {
		return ToolPresence{}
	}
	var hints []string
	for _, c := range cursorCandidates(home) {
		if pathExists(c) {
			hints = append(hints, c)
		}
	}
	if path, ok := lookPath("cursor"); ok {
		hints = append(hints, path)
	}
	return presence(hints)
}

func cursorCandidates(home string) []string {
	common := []string{
		filepath.Join(home, ".cursor"),
		filepath.Join(home, ".cursor", "extensions"),
		filepath.Join(home, ".cursor-server"),
	}
	switch runtime.GOOS {
	case "darwin":
		return append(common,
			"/Applications/Cursor.app",
			filepath.Join(home, "Library", "Application Support", "Cursor"),
			filepath.Join(home, "Library", "Application Support", "Cursor", "logs"),
			filepath.Join(home, "Library", "Application Support", "Cursor", "User"),
		)
	case "windows":
		out := common
		if appData := os.Getenv("APPDATA"); appData != "" {
			out = append(out,
				filepath.Join(appData, "Cursor"),
				filepath.Join(appData, "Cursor", "logs"),
			)
		}
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			out = append(out, filepath.Join(local, "Programs", "cursor"))
		}
		return out
	default: // linux
		return append(common,
			filepath.Join(home, ".config", "Cursor"),
			filepath.Join(home, ".config", "Cursor", "logs"),
			"/opt/cursor",
			"/usr/share/cursor",
		)
	}
}

func detectCodex() ToolPresence {
	var hints []string
	// Every Codex home in the union (default + custom): the sessions/ dir if present,
	// else the base dir itself.
	for _, base := range config.CodexBaseDirs() {
		if s := filepath.Join(base, "sessions"); pathExists(s) {
			hints = append(hints, s)
		} else if pathExists(base) {
			hints = append(hints, base)
		}
	}
	if path, ok := lookPath("codex"); ok {
		hints = append(hints, path)
	}
	return presence(hints)
}

func detectGemini() ToolPresence {
	var hints []string
	if settings, err := config.GeminiSettingsPath(); err == nil {
		if fileExists(settings) {
			hints = append(hints, settings)
		} else if dirExists(filepath.Dir(settings)) {
			hints = append(hints, filepath.Dir(settings))
		}
	}
	if path, ok := lookPath("gemini"); ok {
		hints = append(hints, path)
	}
	return presence(hints)
}

// detectDevin looks for Devin CLI — Cognition's local terminal agent.
//
// Ambiguity worth knowing about: Devin ships two products that share these
// paths. The CLI is what blamely can hook; the Devin IDE (a Windsurf/VS Code
// fork) also creates ~/.devin and ~/.config/devin without the `devin` binary
// ever being installed. We stay permissive anyway — matching how every other
// tool here detects — because installing the hook is idempotent and harmless
// when the CLI shows up later. The `devin` binary is listed first so
// FirstHint() reports the strongest signal when it is present.
//
// Note the IDE's own edits are NOT covered by this hook: Devin Cloud sessions
// mutate files inside a remote sandbox and reach this machine only as a git
// pull, so there is no local edit for the daemon to observe.
func detectDevin() ToolPresence {
	var hints []string
	if path, ok := lookPath("devin"); ok {
		hints = append(hints, path)
	}
	// The CLI's user-wide config: ~/.config/devin/config.json (or the Windows
	// %APPDATA% equivalent). This is also where blamely installs the hook.
	if cfgPath, err := config.DevinConfigPath(); err == nil {
		if fileExists(cfgPath) {
			hints = append(hints, cfgPath)
		} else if dirExists(filepath.Dir(cfgPath)) {
			hints = append(hints, filepath.Dir(cfgPath))
		}
	}
	// ~/.devin holds local state and extensions for both surfaces.
	if stateDir, err := config.DevinStateDir(); err == nil && pathExists(stateDir) {
		hints = append(hints, stateDir)
	}
	return presence(hints)
}

func detectCopilot() ToolPresence {
	home, err := config.Home()
	if err != nil {
		return ToolPresence{}
	}
	var hints []string

	// Copilot CLI (proper) writes here.
	if p := filepath.Join(home, ".config", "github-copilot"); pathExists(p) {
		hints = append(hints, p)
	}

	// The standalone GitHub Copilot CLI keeps its config (and hooks) under
	// ~/.copilot — the same dir blamely installs its hook into (copilothook.go:
	// ~/.copilot/hooks/). Checking it here keeps detection aligned with what we
	// actually hook, on every OS (this was previously missed on Windows).
	if p := filepath.Join(home, ".copilot"); pathExists(p) {
		hints = append(hints, p)
	}

	// macOS-specific app support.
	if runtime.GOOS == "darwin" {
		for _, p := range []string{
			filepath.Join(home, "Library", "Application Support", "GitHub Copilot"),
			filepath.Join(home, "Library", "Application Support", "Code", "User", "globalStorage", "github.copilot"),
			filepath.Join(home, "Library", "Application Support", "Code", "User", "globalStorage", "github.copilot-chat"),
		} {
			if pathExists(p) {
				hints = append(hints, p)
			}
		}
	}

	// Windows-specific.
	if runtime.GOOS == "windows" {
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			for _, p := range []string{
				filepath.Join(local, "github-copilot"),
				filepath.Join(local, "Programs", "Microsoft VS Code", "resources", "app", "extensions"),
			} {
				if pathExists(p) {
					hints = append(hints, p)
				}
			}
		}
	}

	// Editor extension directories: VS Code, VS Code Insiders, Cursor (which
	// reuses the VS Code extension format). Glob for `github.copilot*` subdirs.
	for _, base := range []string{
		filepath.Join(home, ".vscode", "extensions"),
		filepath.Join(home, ".vscode-insiders", "extensions"),
		filepath.Join(home, ".cursor", "extensions"),
		filepath.Join(home, ".cursor-server", "extensions"),
	} {
		if matches, _ := filepath.Glob(filepath.Join(base, "github.copilot*")); len(matches) > 0 {
			hints = append(hints, matches[0])
		}
	}

	// `gh` extension list (last-resort).
	if path, ok := lookPath("gh"); ok {
		if out, err := exec.Command(path, "extension", "list").Output(); err == nil {
			if bytesContains(out, "copilot") {
				hints = append(hints, path+" (gh copilot extension)")
			}
		}
	}

	return presence(hints)
}

func presence(hints []string) ToolPresence {
	return ToolPresence{Present: len(hints) > 0, Hints: hints}
}

func bytesContains(haystack []byte, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if string(haystack[i:i+len(needle)]) == needle {
			return true
		}
	}
	return false
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func lookPath(bin string) (string, bool) {
	p, err := exec.LookPath(bin)
	if err != nil {
		return "", false
	}
	return p, true
}
