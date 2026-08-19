package install

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/blamely/blamely/internal/config"
)

// Devin CLI (Cognition) implements the same hooks contract as Claude Code:
// event names PreToolUse/PostToolUse, groups keyed by a `matcher` regex over
// the tool name, each holding a list of {type:"command", command:"..."} hooks.
//
// The one structural difference is WHERE the hooks live. Devin reads a
// standalone .devin/hooks.v1.json only at the PROJECT level; the single
// user-wide location that accepts hooks is its config file:
//
//	~/.config/devin/config.json         (macOS + Linux)
//	%APPDATA%\devin\config.json         (Windows)
//
// blamely installs globally, so that config file is the target.
//
//	{
//	  "hooks": {
//	    "PostToolUse": [
//	      {
//	        "matcher": "^(write|edit|apply_patch|notebook_edit|exec)$",
//	        "hooks": [ { "type": "command", "command": "/path/blamely record devin" } ]
//	      }
//	    ]
//	  }
//	}
const (
	// Devin's built-in file tools are lowercase: write, edit, apply_patch,
	// notebook_edit. `exec` is included for the same reason claudehook includes
	// Bash — Devin writes and deletes files through the shell too (heredocs,
	// `rm`, redirects), and those bypass the file tools entirely.
	//
	// Anchored on purpose: the matcher is a regex, and an unanchored `write`
	// would also match Devin's unrelated `write_to_process` shell tool.
	devinHookMatcher = "^(write|edit|apply_patch|notebook_edit|exec)$"
	// Marker omits the binary name: the command is `<path> record devin`, and
	// on Windows <path> ends in `blamely.exe`, so "blamely record devin" would
	// never match. `record devin` is the extension-agnostic tail always present.
	devinBlamelyMarker = "record devin"
)

// devinHookEvents mirrors claudehook.go: PostToolUse only. PreToolUse is
// deliberately excluded — at PreToolUse time a write still holds the OLD file
// content, which would be recorded as an AI claim over human-typed lines.
var devinHookEvents = []string{"PostToolUse"}

// InstallDevinHook merges blamely's record command into the `hooks` key of
// Devin CLI's user-wide config.json. Idempotent: it strips any existing blamely
// hook (deduping repeats and clearing a stale binary path or matcher) and
// prepends exactly one fresh entry, so re-running `blamely install` never
// stacks duplicates and always pins blamely first. All unrelated keys and
// third-party hooks are preserved.
func InstallDevinHook(binaryPath string) (added bool, configPath string, err error) {
	configPath, err = config.DevinConfigPath()
	if err != nil {
		return false, "", err
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		return false, configPath, fmt.Errorf("mkdir %s: %w", filepath.Dir(configPath), err)
	}

	root, hadComments, err := readDevinConfig(configPath)
	if err != nil {
		return false, configPath, err
	}
	before := canonJSON(root)

	hooks := getMap(root, "hooks", true)
	command := recordHookCommand(binaryPath, "devin")

	for _, event := range devinHookEvents {
		groups := getSlice(hooks, event)
		groups = stripBlamelyMatcherGroups(groups, devinBlamelyMarker)
		groups = prependIntoMatcherGroup(groups, devinHookMatcher, command)
		hooks[event] = groups
	}
	root["hooks"] = hooks

	if canonJSON(root) == before {
		return false, configPath, nil
	}

	data, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return false, configPath, fmt.Errorf("marshal config: %w", err)
	}
	if err := atomicWrite(configPath, data, 0o644); err != nil {
		return false, configPath, err
	}
	if hadComments {
		// Say it out loud rather than silently eating the user's annotations:
		// Devin accepts JSONC, but re-serialising through encoding/json can only
		// emit plain JSON. The settings themselves are preserved exactly.
		info("Devin CLI", "note: comments in config.json were dropped when the hook was merged · "+configPath)
	}
	return true, configPath, nil
}

// UninstallDevinHook removes ANY hook entry whose command contains
// `record devin` from every event group under config.hooks. User hooks and
// unrelated keys are preserved. Returns true if something was removed.
func UninstallDevinHook() (removed bool, err error) {
	configPath, err := config.DevinConfigPath()
	if err != nil {
		return false, err
	}
	if _, statErr := os.Stat(configPath); errors.Is(statErr, os.ErrNotExist) {
		return false, nil
	}
	root, _, err := readDevinConfig(configPath)
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
		changed := false
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
				if cmd != "" && containsSubstr(cmd, devinBlamelyMarker) {
					removed = true
					changed = true
					continue
				}
				filtered = append(filtered, h)
			}
			if len(filtered) == 0 {
				changed = true
				continue
			}
			grp["hooks"] = filtered
			newGroups = append(newGroups, grp)
		}
		if !changed {
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
		return false, fmt.Errorf("marshal config: %w", err)
	}
	if err := atomicWrite(configPath, data, 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// readDevinConfig is readSettings with JSONC tolerance. Devin documents its
// config as "JSON with comment support", so a user config carrying `//` or
// `/* */` comments is legal and must not make `blamely install` fail. Plain
// JSON is tried first so the overwhelmingly common case costs nothing;
// hadComments reports whether the fallback was needed, letting the caller warn
// that a rewrite cannot round-trip them.
func readDevinConfig(path string) (root map[string]any, hadComments bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) == 0 {
		return map[string]any{}, false, nil
	}
	if uerr := json.Unmarshal(data, &root); uerr == nil {
		if root == nil {
			return map[string]any{}, false, nil
		}
		return root, false, nil
	}
	stripped := stripJSONComments(data)
	if uerr := json.Unmarshal(stripped, &root); uerr != nil {
		return nil, false, fmt.Errorf("parse %s: %w", path, uerr)
	}
	if root == nil {
		return map[string]any{}, true, nil
	}
	return root, true, nil
}

// stripJSONComments blanks out `//` line comments and `/* */` block comments,
// leaving everything else — including byte offsets — untouched so a parse error
// still points at a meaningful position. It is string-literal aware: a `//`
// inside a JSON string (a URL, say) is left alone, and escape sequences are
// honoured so a trailing backslash cannot fake the closing quote.
func stripJSONComments(data []byte) []byte {
	out := make([]byte, len(data))
	copy(out, data)

	const (
		stCode = iota
		stString
		stLine
		stBlock
	)
	state := stCode
	escaped := false

	for i := 0; i < len(out); i++ {
		c := out[i]
		switch state {
		case stCode:
			switch {
			case c == '"':
				state = stString
			case c == '/' && i+1 < len(out) && out[i+1] == '/':
				state = stLine
				out[i], out[i+1] = ' ', ' '
				i++
			case c == '/' && i+1 < len(out) && out[i+1] == '*':
				state = stBlock
				out[i], out[i+1] = ' ', ' '
				i++
			}
		case stString:
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				state = stCode
			}
		case stLine:
			// Keep newlines so line numbers in a later parse error stay honest.
			if c == '\n' {
				state = stCode
			} else {
				out[i] = ' '
			}
		case stBlock:
			if c == '*' && i+1 < len(out) && out[i+1] == '/' {
				out[i], out[i+1] = ' ', ' '
				i++
				state = stCode
			} else if c != '\n' {
				out[i] = ' '
			}
		}
	}
	return out
}
