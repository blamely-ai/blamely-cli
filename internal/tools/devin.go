package tools

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/blamely/blamely/internal/daemon"
	"github.com/blamely/blamely/internal/gitutil"
)

// devinGenType is how every Devin CLI edit is classified. Devin CLI is a
// terminal agent with no completion/Tab surface, so — like Codex and Gemini —
// everything it writes is "cli".
const devinGenType = "cli"

// devinHookPayload is the JSON Devin CLI sends on stdin for PreToolUse /
// PostToolUse hooks.
//
// Devin implements Claude Code's hooks contract, but its payload is NARROWER in
// two ways that matter here:
//
//   - No `cwd`. Every other tool tells us where the agent was working; Devin
//     does not, so repo resolution falls back to the hook process's own working
//     directory (Devin spawns the hook inside the project) — see devinCwd.
//   - No `transcript_path`. There is no session file to read, which means token
//     usage and model name are simply unavailable for Devin edits. hook_usage.go
//     has an explicit `devin` case so this does not silently fall through to the
//     Claude transcript parser.
//
// `prompt_id` is Devin-specific (Claude Code has no such field) and is the
// signal used to tell a Devin payload apart from a Claude one — see IsDevinHookPayload.
type devinHookPayload struct {
	HookEventName string          `json:"hook_event_name"`
	SessionID     string          `json:"session_id"`
	PromptID      string          `json:"prompt_id"`
	ToolName      string          `json:"tool_name"`
	ToolInput     json.RawMessage `json:"tool_input"`
	ToolResponse  json.RawMessage `json:"tool_response"`
	// Accepted when present so a future Devin release that starts sending them
	// is picked up without a code change. Absent in the documented contract.
	Cwd            string `json:"cwd"`
	TranscriptPath string `json:"transcript_path"`
}

// devinToolResponse is the PostToolUse result envelope. A failed tool wrote
// nothing, so recording it would attribute lines the agent never produced.
type devinToolResponse struct {
	Success *bool  `json:"success"`
	Error   string `json:"error"`
}

// RecordDevinFromStdin handles `blamely record devin` (Devin CLI PostToolUse).
func RecordDevinFromStdin(r io.Reader) error {
	raw, err := readHookPayload(r)
	if err != nil {
		return err
	}
	return recordDevinPayload(raw)
}

// recordDevinPayload is RecordDevinFromStdin past the stdin read, so the
// dispatch can be exercised in tests and reused by the Claude handler when it
// detects a misrouted Devin payload.
func recordDevinPayload(raw []byte) error {
	var p devinHookPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		log.Printf("blamely record devin: parse hook payload: %v", err)
		return nil
	}
	if devinToolFailed(p.ToolResponse) {
		return nil
	}

	cwd := devinCwd(p)

	switch p.ToolName {
	case "exec", "shell":
		// Devin deletes files through the shell (`rm`), which no file tool
		// reports. Mirrors the Gemini handler: deletions are recovered from the
		// command, shell-WRITTEN files are not attributed (Devin writes via its
		// file tools in practice).
		return recordShellDeletionsFrom(cwd, shellCommandFromInput(p.ToolInput), "devin", devinGenType, "", p.SessionID, p.TranscriptPath, "devin_shell_delete")
	case "apply_patch":
		return recordDevinPatch(p, cwd)
	}

	filePath, ranges, suggested, removed, newFullContent := extractDevinRanges(p, cwd)
	if filePath == "" {
		return nil
	}
	// A path that yielded nothing — no located lines, no removals, no content —
	// is a read or a tool shape we don't understand. Posting it would write an
	// empty edit row claiming the file for Devin without a single attributed
	// line. Our matcher never sends `read` here, but the Claude-settings reroute
	// path is not ours to control, so guard rather than assume.
	if ranges == nil && removed == nil && newFullContent == nil && suggested == 0 {
		return nil
	}
	return postDevinEdit(p, cwd, filePath, ranges, suggested, removed, newFullContent)
}

// postDevinEdit resolves one edited file to its repo and posts it to the daemon.
func postDevinEdit(p devinHookPayload, cwd, filePath string, ranges []LineRange, suggested int64, removed []DeletedLineHash, newFullContent *string) error {
	resolved := resolveSymlinks(devinAbsPath(filePath, cwd))
	repoPath, _ := gitutil.RepoID(resolved)
	if repoPath == "" && cwd != "" {
		repoPath, _ = gitutil.RepoID(resolveSymlinks(cwd))
	}
	wt, _ := gitutil.Toplevel(resolved)
	rel := resolved
	if wt != "" {
		if r, err := filepath.Rel(wt, resolved); err == nil && !strings.HasPrefix(r, "..") {
			rel = r
		}
	}

	// `write` replaces the whole file and carries no "before" content of its
	// own — ask the daemon for its cached snapshot so lines this write removed
	// are still detected.
	if newFullContent != nil {
		var wfRemoved []DeletedLineHash
		ranges, wfRemoved = ResolveWholeFileWrite(repoPath, rel, *newFullContent, ranges)
		removed = append(removed, wfRemoved...)
	}

	payload := daemon.EditPayload{
		Tool:           "devin",
		Confidence:     "high",
		GenType:        devinGenType,
		RepoPath:       repoPath,
		FilePath:       rel,
		SuggestedLines: suggested,
		Lines:          toDaemonRanges(ranges),
		RemovedLines:   toDaemonRemovedLines(removed),
		RawMeta: fmt.Sprintf(`{"session_id":%q,"prompt_id":%q,"tool":%q,"source":"devin_hook"}`,
			p.SessionID, p.PromptID, p.ToolName),
	}
	// No transcript means no usage to apply, but route through the same helper
	// so a future Devin release that adds transcript_path works for free.
	applyHookUsage(&payload, hookUsageOptions{
		transcriptPath: p.TranscriptPath,
		sessionID:      p.SessionID,
		tool:           "devin",
	})
	captureAuthorship(repoPath, rel, "devin", devinGenType, payload.Model)
	return postToDaemon(payload)
}

// recordDevinPatch handles `apply_patch`, which mutates several files in one
// call. Reuses the Codex patch parser — Devin speaks the same
// `*** Begin Patch` envelope.
func recordDevinPatch(p devinHookPayload, cwd string) error {
	for _, f := range parsePatchFiles(p.ToolInput) {
		if f.Path == "" {
			continue
		}
		var ranges []LineRange
		if f.StartLine > 0 && f.EndLine >= f.StartLine {
			ranges = []LineRange{{Start: f.StartLine, End: f.EndLine}}
		}
		suggested := int64(0)
		if len(ranges) > 0 {
			suggested = int64(f.EndLine - f.StartLine + 1)
		}
		if err := postDevinEdit(p, cwd, f.Path, ranges, suggested, nil, nil); err != nil {
			return err
		}
	}
	for _, path := range parsePatchDeletedFiles(p.ToolInput) {
		if path == "" {
			continue
		}
		if err := recordToolDeletionPath(devinAbsPath(path, cwd), cwd, "devin", devinGenType, "", p.SessionID, p.TranscriptPath, "devin_patch_delete"); err != nil {
			return err
		}
	}
	return nil
}

// extractDevinRanges maps a Devin file-tool call to the lines it produced.
//
// Devin's documented tool_input for both `write` and `edit` is {path, content},
// but its hooks contract is Claude-Code-compatible and builds have been seen
// using Claude's {file_path, old_string, new_string} shape for `edit`. Both are
// accepted rather than guessing which one this version speaks.
func extractDevinRanges(p devinHookPayload, cwd string) (string, []LineRange, int64, []DeletedLineHash, *string) {
	var in struct {
		Path      string `json:"path"`
		FilePath  string `json:"file_path"`
		Content   string `json:"content"`
		NewString string `json:"new_string"`
		OldString string `json:"old_string"`
	}
	if err := json.Unmarshal(p.ToolInput, &in); err != nil {
		return "", nil, 0, nil, nil
	}
	path := in.Path
	if path == "" {
		path = in.FilePath
	}
	if path == "" {
		return "", nil, 0, nil, nil
	}
	// Resolve BEFORE any of the helpers below read the file. Devin sends no cwd
	// and may send a project-relative path, so a bare relative path would
	// otherwise be read against whatever directory the hook happens to run in.
	path = devinAbsPath(path, cwd)

	// An old/new pair is a targeted replacement: narrow to the changed lines and
	// fingerprint what was removed.
	if in.OldString != "" || (in.NewString != "" && in.Content == "") {
		removed := RemovedLineHashes(in.OldString, in.NewString)
		if strings.TrimSpace(in.NewString) == "" && in.OldString != "" {
			// Pure deletion — nothing new to locate.
			return path, nil, int64(countLines(in.OldString)), removed, nil
		}
		lr, _ := LocateNewString(path, in.NewString)
		if lr == nil {
			return path, nil, CountAddedLines(in.OldString, in.NewString), removed, nil
		}
		ranges, suggested := narrowToChangedLines(in.OldString, in.NewString, *lr)
		return path, ranges, suggested, removed, nil
	}

	// Otherwise it is a whole-file write.
	body := in.Content
	if body == "" {
		body = in.NewString
	}
	if body == "" {
		// A path with no content at all is not a write we can reason about
		// (a `read`, or a tool shape we don't know). Returning &body here would
		// tell ResolveWholeFileWrite the file was emptied and mark every prior
		// line as removed, so bail out instead.
		return path, nil, 0, nil, nil
	}
	suggested := int64(countLines(body))
	lr, _ := LineRangeForWholeFile(path)
	if lr == nil {
		return path, nil, suggested, nil, &body
	}
	return path, lr, suggested, nil, &body
}

// devinToolFailed reports whether PostToolUse is describing a tool that errored.
// Absent/unparseable responses are treated as success: PreToolUse carries no
// response at all, and refusing to record on a missing field would drop every
// edit.
func devinToolFailed(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var resp devinToolResponse
	if json.Unmarshal(raw, &resp) != nil {
		return false
	}
	return resp.Success != nil && !*resp.Success
}

// devinCwd resolves the working directory for a Devin hook. Devin's payload has
// no `cwd`, so we fall back to the hook process's own working directory —
// correct in practice because Devin spawns hooks inside the project it is
// editing.
func devinCwd(p devinHookPayload) string {
	if p.Cwd != "" {
		return p.Cwd
	}
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return wd
}

// devinAbsPath resolves a possibly-relative tool path against the session cwd.
// Needed because, without a `cwd` field, a relative path is otherwise
// unresolvable.
func devinAbsPath(path, cwd string) string {
	if path == "" || filepath.IsAbs(path) || cwd == "" {
		return path
	}
	return filepath.Join(cwd, path)
}

// IsDevinHookPayload reports whether a payload delivered to a DIFFERENT tool's
// record command actually came from Devin CLI.
//
// This is not hypothetical. Devin reads user-level hooks out of
// ~/.claude/settings.json and ~/.claude.json as well as its own config, so on a
// machine with both installed, `blamely install` wires a `record claude` hook
// that Devin ALSO fires — silently filing Devin's edits under claude.
//
// The discriminator is `prompt_id`: Devin sends it on every tool event and
// Claude Code has no such field. It is required alongside the absence of
// Claude's own `transcript_path` and Cursor's `cursor_version`, so a future
// Claude Code that adds a prompt_id cannot be misread as Devin.
func IsDevinHookPayload(raw []byte) bool {
	var probe struct {
		PromptID       string          `json:"prompt_id"`
		TranscriptPath string          `json:"transcript_path"`
		CursorVersion  string          `json:"cursor_version"`
		ToolName       string          `json:"tool_name"`
		ToolInput      json.RawMessage `json:"tool_input"`
	}
	if json.Unmarshal(raw, &probe) != nil {
		return false
	}
	if probe.PromptID == "" || probe.TranscriptPath != "" || probe.CursorVersion != "" {
		return false
	}
	// Devin's built-in tools are lowercase; Claude Code's are CamelCase
	// (Write/Edit/MultiEdit/Bash). Requiring a known lowercase Devin tool name
	// keeps an unrelated payload that happens to carry a prompt_id from being
	// rerouted.
	switch probe.ToolName {
	case "write", "edit", "apply_patch", "notebook_edit", "exec", "shell":
		return true
	}
	return false
}
