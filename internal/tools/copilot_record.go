package tools

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"strings"

	"github.com/blamely/blamely/internal/daemon"
	"github.com/blamely/blamely/internal/gitutil"
)

// copilotHookPayload is a tolerant view of the JSON the GitHub Copilot CLI
// hook pipes into `blamely record copilot`. The Copilot framework uses the
// same Anthropic-style PostToolUse shape as Claude / Cursor (tool_name +
// tool_input with file_path), so we share the editInput/writeInput/multiEdit
// structs from claude.go.
//
// Fields we don't recognise are ignored. When the payload doesn't carry a
// file path we still emit a low-confidence session-active marker so the
// fold-in heuristic in attribute.go can credit Copilot at all rather than
// leaving the lines as human.
type copilotHookPayload struct {
	SessionID      string          `json:"session_id"`
	ConversationID string          `json:"conversation_id"`
	TranscriptPath string          `json:"transcript_path"`
	Cwd            string          `json:"cwd"`
	ToolName       string          `json:"tool_name"`
	ToolInput      json.RawMessage `json:"tool_input"`
	Model          string          `json:"model"`
}

// RecordCopilotFromStdin handles the PostToolUse hook payload Copilot pipes
// to `blamely record copilot`. It mirrors RecordClaudeFromStdin but:
//   - records tool="copilot"
//   - uses "completion" as the default gen_type (Copilot is mostly inline/tab),
//     downgrades to "chat" when the payload's tool_name suggests a chat panel
//   - falls back to a session-active marker (no file/lines) for payload shapes
//     we don't recognise, so the attribute fold-in can still credit Copilot
func RecordCopilotFromStdin(r io.Reader) error {
	raw, err := readHookPayload(r)
	if err != nil {
		return err
	}
	var p copilotHookPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		// Malformed JSON: never fail the host tool. Log and emit a marker.
		log.Printf("blamely record copilot: parse hook payload: %v", err)
		return emitCopilotMarker("")
	}
	if p.SessionID == "" && p.ConversationID != "" {
		p.SessionID = p.ConversationID
	}

	filePath, ranges, suggested, removed, newFullContent := extractCopilotRanges(p)
	if filePath == "" {
		// Copilot removes files either with a dedicated delete tool (a bare
		// path) or via its terminal tool (`rm`). Neither produces an edit
		// range, so credit the removal here — otherwise an AI-deleted file
		// falls through to Human at commit time.
		gen := copilotGenType(p.ToolName)
		switch p.ToolName {
		case "delete_file", "remove_file", "delete", "Delete":
			if path := deletePathFromInput(p.ToolInput); path != "" {
				return recordToolDeletionPath(path, p.Cwd, "copilot", gen, p.Model, p.SessionID, p.TranscriptPath, "copilot_delete")
			}
		case "run_in_terminal", "Bash", "shell", "Shell":
			// The terminal tool both WRITES and deletes: a script, a heredoc or a
			// formatter run through it produces no edit range at all, so without the
			// write half everything Copilot authors through the terminal commits as
			// Human. recordShellWritesInRoots covers both (deletions first).
			// Resolved explicitly (rather than via recordShellWrites) so a cwd in no
			// repo at all still falls through to the session marker below.
			if roots := gitutil.DiscoverRepos(p.Cwd); len(roots) > 0 {
				return recordShellWritesInRoots(roots, shellCommandFromInput(p.ToolInput), shellWriteOpts{
					Tool:           "copilot",
					GenType:        gen,
					Model:          p.Model,
					SessionID:      p.SessionID,
					TranscriptPath: p.TranscriptPath,
					WriteSource:    "copilot_shell_fswrite",
					DeleteSource:   "copilot_shell_delete",
				})
			}
		}
		// Payload didn't carry a file path: keep the session-marker fallback.
		return emitCopilotMarker(p.SessionID)
	}

	resolved := resolveSymlinks(filePath)
	repoPath, _ := gitutil.RepoID(resolved)
	if repoPath == "" && p.Cwd != "" {
		repoPath, _ = gitutil.RepoID(resolveSymlinks(p.Cwd))
	}
	wt, _ := gitutil.Toplevel(resolved)
	rel := resolved
	if wt != "" {
		if r, err := filepath.Rel(wt, resolved); err == nil && !strings.HasPrefix(r, "..") {
			rel = r
		}
	}

	// Whole-file overwrite (Write): it carries no "before" content, so diff the
	// new content against the daemon's cached snapshot to detect removed lines —
	// otherwise a Copilot CLI overwrite that drops lines loses the deletion.
	if newFullContent != nil {
		var wfRemoved []DeletedLineHash
		ranges, wfRemoved = ResolveWholeFileWrite(wt, rel, *newFullContent, ranges)
		removed = append(removed, wfRemoved...)
	}

	gen := copilotGenType(p.ToolName)
	payload := daemon.EditPayload{
		Tool:           "copilot",
		Confidence:     "high", // we have a real file+lines, not a session guess
		GenType:        gen,
		RepoPath:       repoPath,
		WorktreePath:   wt,
		FilePath:       rel,
		Model:          p.Model,
		SuggestedLines: suggested,
		Lines:          toDaemonRanges(ranges),
		RemovedLines:   toDaemonRemovedLines(removed),
		RawMeta: fmt.Sprintf(`{"session_id":%q,"tool":%q,"transcript_path":%q,"source":"copilot_hook"}`,
			p.SessionID, p.ToolName, p.TranscriptPath),
	}
	applyHookUsage(&payload, hookUsageOptions{
		transcriptPath: p.TranscriptPath,
		sessionID:      p.SessionID,
		tool:           "copilot",
	})
	// Attribution: mirror into the working log before the
	// daemon POST so capture is daemon-independent. No-op when the flag is off.
	captureAuthorship(wt, rel, "copilot", gen, payload.Model)
	return postToDaemon(payload)
}

// copilotAddedRanges returns PER-LINE content_sha ranges for newStr — the form
// commit-time attribution needs to match added lines (a single block range
// without per-line shas never matches, so the lines fall to Human). If oldStr is
// present it narrows to the genuinely-changed lines; otherwise every non-blank
// new line is credited. Positions are placeholders — matching is by content_sha,
// so we don't need to locate the text in the file (LocateNewString can fail when
// the on-disk file already moved on).
func copilotAddedRanges(oldStr, newStr string) ([]LineRange, int64) {
	if strings.TrimSpace(newStr) == "" {
		return nil, 0
	}
	if strings.TrimSpace(oldStr) == "" {
		r := perLineShaRangesFromContent(newStr)
		return r, int64(countLines(newStr))
	}
	return narrowToChangedLines(oldStr, newStr, LineRange{Start: 1, End: countLines(newStr)})
}

// extractCopilotRanges is intentionally permissive: it accepts Copilot's native
// agent tool shapes (str_replace_editor, create_file, insert_edit_into_file) as
// well as Claude-compatible shapes (Edit/Write/MultiEdit) and a generic fallback
// that tries any payload with a recognisable file path + content field. Every add
// path emits per-line content_sha (via copilotAddedRanges / perLineSha) so an
// AI-added line attributes to copilot instead of falling to Human.
func extractCopilotRanges(p copilotHookPayload) (string, []LineRange, int64, []DeletedLineHash, *string) {
	switch p.ToolName {
	// ── GitHub Copilot agent / chat tools ────────────────────────────────────
	// These are the tool names Copilot sends from its chat panel in VS Code and
	// Cursor. The payloads use "path" not "file_path", and field names differ
	// from Claude's conventions.

	case "str_replace_editor":
		// Payload: {command, path, old_str, new_str} for replacements, or
		//          {command, path, content} for "create" operations.
		var in struct {
			Command string `json:"command"`
			Path    string `json:"path"`
			OldStr  string `json:"old_str"`
			NewStr  string `json:"new_str"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(p.ToolInput, &in); err != nil || in.Path == "" {
			return "", nil, 0, nil, nil
		}
		body := in.NewStr
		if body == "" {
			body = in.Content
		}
		removed := RemovedLineHashes(in.OldStr, body)
		if strings.TrimSpace(body) == "" && in.OldStr != "" {
			return in.Path, nil, int64(countLines(in.OldStr)), removed, nil
		}
		ranges, suggested := copilotAddedRanges(in.OldStr, body)
		return in.Path, ranges, suggested, removed, nil

	case "create_file":
		// Payload: {path, content} — new file, nothing removed.
		var in struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(p.ToolInput, &in); err != nil || in.Path == "" {
			return "", nil, 0, nil, nil
		}
		return in.Path, perLineShaRangesFromContent(in.Content), int64(countLines(in.Content)), nil, nil

	case "insert_edit_into_file":
		// Payload: {path, code, explanation?} — pure insertion, nothing removed.
		var in struct {
			Path string `json:"path"`
			Code string `json:"code"`
		}
		if err := json.Unmarshal(p.ToolInput, &in); err != nil || in.Path == "" {
			return "", nil, 0, nil, nil
		}
		return in.Path, perLineShaRangesFromContent(in.Code), int64(countLines(in.Code)), nil, nil

	// ── Claude-compatible shapes (also used by some Copilot variants) ─────────

	case "Edit":
		var in editInput
		if err := json.Unmarshal(p.ToolInput, &in); err != nil || in.FilePath == "" {
			return "", nil, 0, nil, nil
		}
		removed := RemovedLineHashes(in.OldString, in.NewString)
		if strings.TrimSpace(in.NewString) == "" && in.OldString != "" {
			return in.FilePath, nil, int64(countLines(in.OldString)), removed, nil
		}
		ranges, suggested := copilotAddedRanges(in.OldString, in.NewString)
		return in.FilePath, ranges, suggested, removed, nil

	case "Write":
		var in writeInput
		if err := json.Unmarshal(p.ToolInput, &in); err != nil || in.FilePath == "" {
			return "", nil, 0, nil, nil
		}
		// Whole-file overwrite: per-line shas for the new content; removed lines
		// are computed by the caller against the cached snapshot (newFullContent).
		return in.FilePath, perLineShaRangesFromContent(in.Content), int64(countLines(in.Content)), nil, &in.Content

	case "MultiEdit":
		var in multiEditInput
		if err := json.Unmarshal(p.ToolInput, &in); err != nil || in.FilePath == "" {
			return "", nil, 0, nil, nil
		}
		var suggested int64
		var out []LineRange
		var removed []DeletedLineHash
		for _, ed := range in.Edits {
			removed = append(removed, RemovedLineHashes(ed.OldString, ed.NewString)...)
			if strings.TrimSpace(ed.NewString) == "" && ed.OldString != "" {
				suggested += int64(countLines(ed.OldString))
				continue
			}
			narrowed, narrowSuggest := copilotAddedRanges(ed.OldString, ed.NewString)
			out = append(out, narrowed...)
			suggested += narrowSuggest
		}
		return in.FilePath, out, suggested, removed, nil

	default:
		// Generic fallback: try multiple field-name conventions. Copilot uses
		// "path"; Claude/older hooks use "file_path". Content body may be in
		// "new_string", "new_str", "content", or "code"; an old body (for
		// removed-line detection) in "old_string" or "old_str".
		var generic struct {
			FilePath  string `json:"file_path"`
			Path      string `json:"path"`
			NewString string `json:"new_string"`
			NewStr    string `json:"new_str"`
			Content   string `json:"content"`
			Code      string `json:"code"`
			OldString string `json:"old_string"`
			OldStr    string `json:"old_str"`
		}
		if err := json.Unmarshal(p.ToolInput, &generic); err != nil {
			return "", nil, 0, nil, nil
		}
		fp := generic.FilePath
		if fp == "" {
			fp = generic.Path
		}
		if fp == "" {
			return "", nil, 0, nil, nil
		}
		body := generic.NewString
		if body == "" {
			body = generic.NewStr
		}
		if body == "" {
			body = generic.Content
		}
		if body == "" {
			body = generic.Code
		}
		old := generic.OldString
		if old == "" {
			old = generic.OldStr
		}
		removed := RemovedLineHashes(old, body)
		ranges, suggested := copilotAddedRanges(old, body)
		return fp, ranges, suggested, removed, nil
	}
}

func copilotGenType(toolName string) string {
	// RecordCopilotFromStdin is the GitHub Copilot *CLI's* PostToolUse hook, so
	// every edit it sees is a command-line agent action → "cli" (same as Codex),
	// NOT inline tab-completion. Inline completions never fire this hook (the
	// editor plugin records those), and VS Code's Copilot Chat panel is handled by
	// the transcript watcher — so "completion" is never correct here.
	t := strings.ToLower(toolName)
	if strings.Contains(t, "chat") || strings.Contains(t, "ask") || strings.Contains(t, "panel") {
		return "chat"
	}
	return "cli"
}

// emitCopilotMarker is a fallback for payloads where we couldn't extract a
// file path. We log and return nil rather than POSTing — the daemon's
// CopilotWatcher already produces session-active markers via its in-process
// sink (which bypasses the HTTP validation that requires repo_path /
// file_path), so a parallel HTTP marker would just get rejected.
func emitCopilotMarker(sessionID string) error {
	log.Printf("blamely record copilot: payload missing file_path (session=%q) — relying on watcher's session marker", sessionID)
	_ = daemon.EditPayload{} // keep daemon import in case the policy changes
	return nil
}
