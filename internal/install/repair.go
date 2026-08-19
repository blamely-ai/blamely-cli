package install

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/blamely/blamely/internal/config"
)

const maxRepairDepth = 6

// GitHooksDirPath returns the path to Blamely's global git-hooks directory.
func GitHooksDirPath() (string, error) {
	return config.GitHooksDir()
}

// RepairResult summarises what Repair found and fixed.
type RepairResult struct {
	Found   []string // paths of stale hooks detected
	Removed []string // paths actually removed
	Errors  []string // non-fatal problems

	// HooksAdded lists "<Tool> (<path>)" for every AI-tool hook that was
	// missing and has now been configured — e.g. the user installed
	// Codex/Cursor/Gemini after `blamely install` already ran.
	HooksAdded []string
}

// Repair scans the user's home directory (up to maxRepairDepth levels) for
// .git/hooks/post-commit files that contain a blamely-managed marker
// ("blamely-cli hook (managed)" or the older Blamely BLAMELY= line) pointing
// at a stale binary, then removes them so git falls through to the global
// core.hooksPath hook instead.
//
// It never removes a hook that doesn't contain our known markers, and it
// never removes a hook that is already working (binary exists at the
// recorded path).
func Repair(dryRun bool) (*RepairResult, error) {
	home, err := config.Home()
	if err != nil {
		return nil, err
	}
	result := &RepairResult{}
	err = filepath.WalkDir(home, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil // skip unreadable dirs
		}
		// Prune dirs we should never descend into.
		if d.IsDir() {
			base := filepath.Base(p)
			if base == ".Trash" || base == "Library" && p == filepath.Join(home, "Library") {
				return filepath.SkipDir
			}
			depth := strings.Count(strings.TrimPrefix(p, home), string(filepath.Separator))
			if depth > maxRepairDepth {
				return filepath.SkipDir
			}
			return nil
		}
		// Only interested in .git/hooks/post-commit files.
		if filepath.Base(p) != "post-commit" {
			return nil
		}
		if !strings.Contains(p, filepath.Join(".git", "hooks")) {
			return nil
		}
		stale, why, removeErr := checkStaleHook(p)
		if removeErr != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", p, removeErr))
			return nil
		}
		if !stale {
			return nil
		}
		result.Found = append(result.Found, fmt.Sprintf("%s (%s)", p, why))
		if dryRun {
			return nil
		}
		if err := os.Remove(p); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("remove %s: %v", p, err))
		} else {
			result.Removed = append(result.Removed, p)
		}
		return nil
	})
	if err != nil {
		return result, err
	}
	addMissingToolHooks(dryRun, result)
	return result, nil
}

// addMissingToolHooks re-checks every AI tool blamely supports and configures
// the `blamely record <tool>` hook for any that are present but not yet
// wired up — the case where the user installs Cursor/Codex/Copilot/Gemini
// AFTER `blamely install` already ran, so install.Run's per-tool hook step
// never saw them. Each Install*Hook is idempotent (a no-op, added=false, when
// the hook is already configured), so this is safe to call on every repair.
//
// No-ops entirely under --dry-run (and if blamely itself isn't installed yet
// — `blamely install` is the right command in that case).
func addMissingToolHooks(dryRun bool, result *RepairResult) {
	if dryRun {
		return
	}
	binPath, err := InstalledBinaryPath()
	if err != nil {
		return
	}
	if _, err := os.Stat(binPath); err != nil {
		return
	}
	det, err := Detect()
	if err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("detect tools: %v", err))
		return
	}
	s, err := LoadState()
	if err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("load state: %v", err))
		return
	}

	for _, h := range []struct {
		name    string
		present bool
		install func(string) (bool, string, error)
		added   *bool
	}{
		{"Claude Code", det.Claude.Present, InstallClaudeHook, &s.ClaudeHookAdded},
		{"Cursor", det.Cursor.Present, InstallCursorHook, &s.CursorHookAdded},
		{"Codex CLI", det.Codex.Present, InstallCodexHook, &s.CodexHookAdded},
		{"GitHub Copilot", det.Copilot.Present, InstallCopilotHook, &s.CopilotHookAdded},
		{"Gemini CLI", det.Gemini.Present, InstallGeminiHook, &s.GeminiHookAdded},
		{"Devin CLI", det.Devin.Present, InstallDevinHook, &s.DevinHookAdded},
	} {
		if !h.present {
			continue
		}
		added, path, err := h.install(binPath)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s hook: %v", h.name, err))
			continue
		}
		if added {
			result.HooksAdded = append(result.HooksAdded, fmt.Sprintf("%s (%s)", h.name, path))
			*h.added = true
		}
	}
	if len(result.HooksAdded) > 0 {
		if err := SaveState(s); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("save state: %v", err))
		}
	}
}

// checkStaleHook reads the hook file and returns (true, reason, nil) when the
// hook is a blamely-managed script whose recorded binary no longer exists.
func checkStaleHook(path string) (stale bool, reason string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, "", err
	}
	content := string(data)

	// Pattern 1: legacy blamely-cli hook (the older project's format).
	//   ### blamely-cli hook (managed) begin ###
	if strings.Contains(content, "blamely-cli hook (managed)") {
		binPath := extractBlamelyCLIPath(content)
		if binPath == "" {
			// Has the marker but no extractable path — still ours, still stale.
			return true, "legacy blamely-cli hook (no binary path)", nil
		}
		if _, err := os.Stat(binPath); os.IsNotExist(err) {
			return true, fmt.Sprintf("legacy blamely-cli hook (binary missing: %s)", binPath), nil
		}
		// Binary still exists — not stale.
		return false, "", nil
	}

	// Pattern 2: our own hook (new Blamely).
	//   # Generated by Blamely. Do not edit by hand.
	if strings.Contains(content, "Generated by Blamely") {
		// If this is in a repo-local .git/hooks dir and core.hooksPath is set to
		// our global dir, the repo-local copy is redundant. Flag it for removal.
		hooksPath, set := readGlobalConfig("core.hooksPath")
		if set && hooksPath != "" {
			// Our global hook is active — the repo-local copy is a stale duplicate.
			return true, "duplicate Blamely hook (superseded by global core.hooksPath)", nil
		}
		return false, "", nil
	}

	return false, "", nil
}

// extractBlamelyCLIPath finds the binary path in a legacy blamely-cli hook.
// The line looks like:   BLAMELY='/path/to/blamely'
func extractBlamelyCLIPath(content string) string {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "BLAMELY=") {
			continue
		}
		path := strings.TrimPrefix(line, "BLAMELY=")
		path = strings.Trim(path, "'\"")
		return path
	}
	return ""
}
