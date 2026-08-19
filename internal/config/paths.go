package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	dirName             = ".blamely"
	dbFileName          = "db.sqlite"
	portFileName        = "daemon.port"
	pidFileName         = "daemon.pid"
	lockFileName        = "daemon.lock"
	socketFileName      = "daemon.sock"
	stateFileName       = "state.json"
	hooksDirName        = "git-hooks"
	logFileName         = "daemon.log"
	excludeFileName     = "exclude"
	claudeDirName       = ".claude"
	claudeSettings      = "settings.json"
	claudeProjects      = "projects"
	codexDirName        = ".codex"
	codexSessions       = "sessions"
	codexConfig         = "config.toml"
	cursorDirName       = ".cursor"
	cursorHooks         = "hooks.json"
	copilotDirName      = ".copilot"
	copilotHooksSubdir  = "hooks"
	copilotHookFile     = "blamely.json"
	copilotSessionState = "session-state"
	geminiDirName       = ".gemini"
	geminiSettings      = "settings.json"
	devinDirName        = "devin"
	devinConfig         = "config.json"
	devinStateDirName   = ".devin"
)

func Home() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("user home: %w", err)
	}
	return home, nil
}

func BlamelyDir() (string, error) {
	home, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, dirName), nil
}

func DBPath() (string, error) {
	d, err := BlamelyDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, dbFileName), nil
}

func PortFile() (string, error) {
	d, err := BlamelyDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, portFileName), nil
}

// PidFile is where the running daemon records its OS process id, so `blamely
// uninstall` can terminate that exact process synchronously instead of relying
// on a racy by-image-name taskkill.
func PidFile() (string, error) {
	d, err := BlamelyDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, pidFileName), nil
}

// LockFile is the path the daemon holds an exclusive OS lock on for its whole
// lifetime, enforcing a single running instance. Unlike the /health probe (a
// best-effort check that races when launchers start concurrently), an exclusive
// file lock can be held by only one process at a time, so a second daemon — from
// the logon task, an editor plugin spawn, the keepalive task, or install —
// fails to acquire it and exits instead of binding a second ephemeral port.
func LockFile() (string, error) {
	d, err := BlamelyDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, lockFileName), nil
}

func SocketFile() (string, error) {
	d, err := BlamelyDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, socketFileName), nil
}

func StateFile() (string, error) {
	d, err := BlamelyDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, stateFileName), nil
}

func GitHooksDir() (string, error) {
	d, err := BlamelyDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, hooksDirName), nil
}

func LogFile() (string, error) {
	d, err := BlamelyDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, logFileName), nil
}

// ExcludeFile returns the path to the user's exclude list. Paths matching
// this list are skipped at diff time and never appear in attribution.
func ExcludeFile() (string, error) {
	d, err := BlamelyDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, excludeFileName), nil
}

// dedupExisting returns the input paths cleaned, de-duplicated (order-preserving),
// and dropping empties. It does NOT stat — callers that only want existing dirs
// filter separately; watchers poll for dirs that may appear later.
func dedupExisting(paths []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if strings.TrimSpace(p) == "" {
			continue
		}
		c := filepath.Clean(p)
		if seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	return out
}

// CodexBaseDirs returns the UNION of every Codex home to scan, in order: the home
// default (~/.codex) ALWAYS first, then $CODEX_HOME, then every extra dir from
// config (tools.codex_homes). De-duplicated. The default is never dropped — custom
// dirs are additive, so a machine with both a personal and a corp Codex is covered.
func CodexBaseDirs() []string {
	var dirs []string
	if home, err := Home(); err == nil {
		dirs = append(dirs, filepath.Join(home, codexDirName))
	}
	if env := strings.TrimSpace(os.Getenv("CODEX_HOME")); env != "" {
		dirs = append(dirs, env)
	}
	dirs = append(dirs, LoadConfig().Tools.CodexHomes...)
	return dedupExisting(dirs)
}

// ClaudeBaseDirs mirrors CodexBaseDirs for Claude: ~/.claude ALWAYS first, then
// $CLAUDE_CONFIG_DIR, then tools.claude_config_dirs. De-duplicated, default kept.
func ClaudeBaseDirs() []string {
	var dirs []string
	if home, err := Home(); err == nil {
		dirs = append(dirs, filepath.Join(home, claudeDirName))
	}
	if env := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); env != "" {
		dirs = append(dirs, env)
	}
	dirs = append(dirs, LoadConfig().Tools.ClaudeConfigDirs...)
	return dedupExisting(dirs)
}

// CodexSessionsDirs is the per-base sessions/ dir for every Codex home in the union.
func CodexSessionsDirs() []string {
	bases := CodexBaseDirs()
	out := make([]string, 0, len(bases))
	for _, b := range bases {
		out = append(out, filepath.Join(b, codexSessions))
	}
	return out
}

// CodexConfigPaths is the per-base config.toml for every Codex home in the union.
func CodexConfigPaths() []string {
	bases := CodexBaseDirs()
	out := make([]string, 0, len(bases))
	for _, b := range bases {
		out = append(out, filepath.Join(b, codexConfig))
	}
	return out
}

// ClaudeSettingsPaths is the per-base settings.json for every Claude dir in the union.
func ClaudeSettingsPaths() []string {
	bases := ClaudeBaseDirs()
	out := make([]string, 0, len(bases))
	for _, b := range bases {
		out = append(out, filepath.Join(b, claudeSettings))
	}
	return out
}

// ClaudeProjectsDirs is the per-base projects/ dir for every Claude dir in the union.
func ClaudeProjectsDirs() []string {
	bases := ClaudeBaseDirs()
	out := make([]string, 0, len(bases))
	for _, b := range bases {
		out = append(out, filepath.Join(b, claudeProjects))
	}
	return out
}

// ClaudeSettingsPath returns the home-default Claude settings.json — the location to
// create the hook when none exists yet. Use ClaudeSettingsPaths() to scan all.
func ClaudeSettingsPath() (string, error) {
	home, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, claudeDirName, claudeSettings), nil
}

// CodexSessionsDir returns the home-default Codex sessions dir. Use CodexSessionsDirs()
// to scan the full union (default + custom).
func CodexSessionsDir() (string, error) {
	home, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, codexDirName, codexSessions), nil
}

// CodexConfigPath returns the home-default Codex config.toml — the location to create
// the hook when none exists yet. Use CodexConfigPaths() to scan all.
func CodexConfigPath() (string, error) {
	home, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, codexDirName, codexConfig), nil
}

func CursorHooksPath() (string, error) {
	home, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, cursorDirName, cursorHooks), nil
}

func CopilotHooksDir() (string, error) {
	home, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, copilotDirName, copilotHooksSubdir), nil
}

// CopilotSessionStateDir is ~/.copilot/session-state, where the Copilot CLI
// writes one <session-id>/events.jsonl per session (the source of per-turn
// model + output-token telemetry).
func CopilotSessionStateDir() (string, error) {
	home, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, copilotDirName, copilotSessionState), nil
}

func CopilotBlamelyHookPath() (string, error) {
	dir, err := CopilotHooksDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, copilotHookFile), nil
}

func GeminiSettingsPath() (string, error) {
	home, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, geminiDirName, geminiSettings), nil
}

// DevinConfigPath is Devin CLI's user-wide config file — the only global
// location that accepts a `hooks` key. Devin also reads project-level
// .devin/hooks.v1.json and .devin/config.json, but blamely installs globally so
// a single install covers every repo.
//
// Unlike every other tool here, Devin does NOT put its config under the home
// dir directly: it follows the XDG-ish ~/.config/devin layout on macOS and
// Linux alike (macOS included — it does not use ~/Library/Application Support
// for the CLI), and %APPDATA%\devin on Windows.
func DevinConfigPath() (string, error) {
	dir, err := DevinConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, devinConfig), nil
}

// DevinConfigDir is the directory holding DevinConfigPath.
func DevinConfigDir() (string, error) {
	if runtime.GOOS == "windows" {
		if appData := os.Getenv("APPDATA"); appData != "" {
			return filepath.Join(appData, devinDirName), nil
		}
		// APPDATA is effectively always set on Windows; fall through to the
		// home-relative default rather than failing outright.
		home, err := Home()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "AppData", "Roaming", devinDirName), nil
	}
	home, err := Home()
	if err != nil {
		return "", err
	}
	// Honour XDG_CONFIG_HOME when set — Devin resolves its config dir the same
	// way, so a user who relocates it must still get a wired-up hook.
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, devinDirName), nil
	}
	return filepath.Join(home, ".config", devinDirName), nil
}

// DevinStateDir is ~/.devin — the CLI's local state/extension dir. Used for
// detection only; nothing is written there.
func DevinStateDir() (string, error) {
	home, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, devinStateDirName), nil
}

func EnsureBlamelyDir() (string, error) {
	d, err := BlamelyDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(d, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", d, err)
	}
	return d, nil
}
