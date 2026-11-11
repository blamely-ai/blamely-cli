package install

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/blamely/blamely/internal/config"
)

const (
	// Bash is included so file writes Claude performs via shell (e.g.
	// `printf > f`, `cat > f`, heredocs, scripts) — which bypass the
	// Write/Edit tools — still fire our PostToolUse hook. The recorder
	// attributes the source files that changed during the command.
	claudeHookMatcher = "Write|Edit|MultiEdit|NotebookEdit|Bash"
	// Marker omits the binary name on purpose: the command is
	// `<path> record claude`, and on Windows <path> ends in `blamely.exe`, so a
	// "blamely record claude" needle would never match. `record claude` is the
	// extension-agnostic, blamely-specific tail that's always present.
	blamelyHookMarker = "record claude"
)

// claudePreHookMatcher is the narrower set of tools the PreToolUse hook fires
// for: the editor tools, whose write targets a named file. Their pre-hook
// snapshots that one file (no git process), so the PostToolUse record credits the
// agent with only what it changed. Without it the first agent edit to a file
// diffs against the commit and claims every uncommitted line the editor plugin
// did not record — the user's own work included, adjacent or not.
//
// Bash is deliberately left out, and the reason is cost: an agent runs far more
// shell commands than edits, most of them read-only, and a shell pre-hook has to
// run `git status` to find what to snapshot (~3x an editor pre-hook). Shell
// writes keep their baselines from the post-hook instead
// (tools.captureBaselinesForUntouchedFiles). NotebookEdit names its target in a
// field the pre-hook does not read, so hooking it would only cost a process.
const claudePreHookMatcher = "Write|Edit|MultiEdit"

// claudeHookEvents is every event name we register the blamely command under.
// Adding to this list automatically gets the install path; uninstall scans
// the full `hooks` map regardless and will still find our markers.
//
// PreToolUse carries --pre (recordHookCommandForEvent), so it only snapshots the
// target's pre-edit content and never records an edit; it is matched to
// claudePreHookMatcher, not the PostToolUse matcher.
var claudeHookEvents = []string{"PostToolUse", "PreToolUse"}

// claudeHookMatcherFor is the matcher blamely's group uses for event.
func claudeHookMatcherFor(event string) string {
	if isPreEditHookEvent(event) {
		return claudePreHookMatcher
	}
	return claudeHookMatcher
}

// InstallClaudeHook ensures exactly ONE blamely record-hook is the FIRST hook
// of every event we care about under ~/.claude/settings.json (PostToolUse and
// PreToolUse). It strips any existing blamely hooks (deduping repeats and removing a
// stale binary path / older matcher) and prepends a single fresh one, so
// re-running `blamely install` never stacks duplicates and always pins blamely
// first. Idempotent — it only writes when the file actually changes. Preserves
// all unrelated keys (other matchers, user hooks, non-hook settings).
// It installs into EVERY Claude config location in the union (~/.claude and any
// custom CLAUDE_CONFIG_DIR / corp dir that exists), so a machine running Claude from
// a non-default home is wired up too — never just the default. When no location
// exists yet it creates the hook in the home default. `settingsPath` is a
// display string of the location(s) written; `added` is true if any changed.
func InstallClaudeHook(binaryPath string) (added bool, settingsPath string, err error) {
	targets := claudeHookTargets()
	written := make([]string, 0, len(targets))
	for _, p := range targets {
		a, werr := installClaudeHookAt(p, binaryPath)
		if werr != nil {
			return added, strings.Join(append(written, p), ", "), werr
		}
		written = append(written, p)
		added = added || a
	}
	return added, strings.Join(written, ", "), nil
}

// claudeHookTargets is the set of settings.json paths to install into: every union
// base whose dir already exists, or the home default if none do yet.
func claudeHookTargets() []string {
	var out []string
	for _, p := range config.ClaudeSettingsPaths() {
		if dirExists(filepath.Dir(p)) {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		if def, err := config.ClaudeSettingsPath(); err == nil {
			out = append(out, def)
		}
	}
	return out
}

// installClaudeHookAt merges the blamely record-hook into one settings.json,
// prepending it as the FIRST hook of every event we care about. Idempotent.
func installClaudeHookAt(settingsPath, binaryPath string) (added bool, err error) {
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		return false, fmt.Errorf("mkdir %s: %w", filepath.Dir(settingsPath), err)
	}

	root, err := readSettings(settingsPath)
	if err != nil {
		return false, err
	}
	before := canonJSON(root)

	hooks := getMap(root, "hooks", true)

	// Strip our hook from EVERY event first, not just the ones we are about to
	// register. An event we used to register and no longer do would otherwise
	// keep firing forever, because a
	// reinstall never visits it. Stripping the whole map makes install converge on
	// claudeHookEvents whichever build wrote the file.
	for event := range hooks {
		entries := getSlice(hooks, event)
		stripped := stripBlamelyMatcherGroups(entries, blamelyHookMarker)
		if len(stripped) == 0 {
			delete(hooks, event)
			continue
		}
		hooks[event] = stripped
	}
	for _, event := range claudeHookEvents {
		command := recordHookCommandForEvent(binaryPath, "claude", event)
		entries := prependIntoMatcherGroup(getSlice(hooks, event), claudeHookMatcherFor(event), command)
		hooks[event] = entries
	}
	root["hooks"] = hooks

	if canonJSON(root) == before {
		return false, nil
	}
	data, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return false, fmt.Errorf("marshal settings: %w", err)
	}
	if err := atomicWrite(settingsPath, data, 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// prependIntoMatcherGroup adds {type:command, command} as the FIRST hook of the
// matcher group whose `matcher` equals `matcher`; creates the group (as the
// first group) if none exists.
//
// Blamely runs first on purpose: hosts (Claude Code, Codex, …) execute an
// event's hooks in order and a hook that errors can abort the rest of the
// chain. If blamely were appended last, a failing third-party hook installed
// ahead of it would silently prevent blamely from ever recording the edit.
// Running first means blamely captures the change before anyone else can break
// the chain — and `blamely record` always exits 0 (see cmdRecord) so it never
// becomes that blocker for the tools that follow it.
func prependIntoMatcherGroup(groups []any, matcher, command string) []any {
	newHook := map[string]any{"type": "command", "command": command}
	for i, g := range groups {
		grp, ok := g.(map[string]any)
		if !ok {
			continue
		}
		if m, _ := grp["matcher"].(string); m != matcher {
			continue
		}
		inner := getSlice(grp, "hooks")
		inner = append([]any{newHook}, inner...)
		grp["hooks"] = inner
		groups[i] = grp
		return groups
	}
	newGroup := map[string]any{
		"matcher": matcher,
		"hooks":   []any{newHook},
	}
	return append([]any{newGroup}, groups...)
}

// UninstallClaudeHook removes ANY hook entry whose command contains
// `blamely record claude` from every event group under settings.hooks
// (PreToolUse, PostToolUse, etc.), across EVERY Claude config location in the
// union (default + custom). User hooks and unrelated keys are preserved. Returns
// true if something was removed from any location.
func UninstallClaudeHook() (removed bool, err error) {
	seen := map[string]bool{}
	paths := config.ClaudeSettingsPaths()
	if def, derr := config.ClaudeSettingsPath(); derr == nil {
		paths = append(paths, def)
	}
	for _, p := range paths {
		if seen[p] {
			continue
		}
		seen[p] = true
		r, uerr := uninstallClaudeHookAt(p)
		if uerr != nil {
			return removed, uerr
		}
		removed = removed || r
	}
	return removed, nil
}

func uninstallClaudeHookAt(settingsPath string) (removed bool, err error) {
	root, err := readSettings(settingsPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	hooks := getMap(root, "hooks", false)
	if hooks == nil {
		return false, nil
	}

	for event, raw := range hooks {
		groups, ok := raw.([]any)
		if !ok {
			continue
		}
		newGroups := groups[:0]
		eventChanged := false
		for _, g := range groups {
			grp, ok := g.(map[string]any)
			if !ok {
				newGroups = append(newGroups, g)
				continue
			}
			inner := getSlice(grp, "hooks")
			filtered := inner[:0]
			for _, h := range inner {
				hm, _ := h.(map[string]any)
				cmd, _ := hm["command"].(string)
				if cmd != "" && containsBlamelyHook(cmd) {
					removed = true
					eventChanged = true
					continue
				}
				filtered = append(filtered, h)
			}
			if len(filtered) == 0 {
				eventChanged = true
				continue
			}
			grp["hooks"] = filtered
			newGroups = append(newGroups, grp)
		}
		if !eventChanged {
			continue
		}
		if len(newGroups) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = newGroups
		}
	}
	if !removed {
		return false, nil
	}
	if len(hooks) == 0 {
		delete(root, "hooks")
	} else {
		root["hooks"] = hooks
	}
	data, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return false, fmt.Errorf("marshal settings: %w", err)
	}
	if err := atomicWrite(settingsPath, data, 0o644); err != nil {
		return false, err
	}
	return true, nil
}

func readSettings(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) == 0 {
		return map[string]any{}, nil
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if root == nil {
		return map[string]any{}, nil
	}
	return root, nil
}

func getMap(parent map[string]any, key string, create bool) map[string]any {
	if v, ok := parent[key].(map[string]any); ok {
		return v
	}
	if !create {
		return nil
	}
	m := map[string]any{}
	parent[key] = m
	return m
}

func getSlice(parent map[string]any, key string) []any {
	if v, ok := parent[key].([]any); ok {
		return v
	}
	return nil
}

// canonJSON returns a deterministic JSON encoding of v (encoding/json sorts map
// keys), used to detect whether a hook reconcile actually changed the file so
// install only rewrites when something differs.
func canonJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// stripBlamelyMatcherGroups removes every blamely hook (command containing
// marker) from each group's "hooks" array and drops any group left empty,
// returning the surviving groups. Works for both matcher-keyed groups (Claude,
// Gemini) and matcher-less groups (Codex) since it only looks at "hooks".
// Install calls this before re-adding a single fresh blamely hook, so repeated
// installs dedupe instead of stacking and a stale command/matcher is replaced.
func stripBlamelyMatcherGroups(groups []any, marker string) []any {
	out := make([]any, 0, len(groups))
	for _, g := range groups {
		grp, ok := g.(map[string]any)
		if !ok {
			out = append(out, g)
			continue
		}
		inner := getSlice(grp, "hooks")
		kept := make([]any, 0, len(inner))
		for _, h := range inner {
			hm, _ := h.(map[string]any)
			cmd, _ := hm["command"].(string)
			if cmd != "" && containsSubstr(cmd, marker) {
				continue
			}
			kept = append(kept, h)
		}
		if len(kept) == 0 {
			continue
		}
		grp["hooks"] = kept
		out = append(out, grp)
	}
	return out
}

// stripBlamelyEntries removes every blamely hook from a flat list of
// {command,…} entries (Cursor, Copilot), returning the survivors.
func stripBlamelyEntries(entries []any, marker string) []any {
	out := make([]any, 0, len(entries))
	for _, e := range entries {
		em, _ := e.(map[string]any)
		cmd, _ := em["command"].(string)
		if cmd != "" && containsSubstr(cmd, marker) {
			continue
		}
		out = append(out, e)
	}
	return out
}

func containsBlamelyHook(cmd string) bool {
	// Match any path ending in `... record claude`.
	for i := 0; i+len(blamelyHookMarker) <= len(cmd); i++ {
		if cmd[i:i+len(blamelyHookMarker)] == blamelyHookMarker {
			return true
		}
	}
	return false
}
