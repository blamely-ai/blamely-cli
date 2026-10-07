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

	// VS Code agent tools that can touch SEVERAL files in one call
	// (multi_replace_string_in_file, apply_patch) record one edit per file.
	if edits, ok := extractCopilotMultiFileEdits(p); ok {
		if len(edits) == 0 {
			return emitCopilotMarker(p.SessionID)
		}
		for _, e := range edits {
			if err := recordCopilotFileEdit(p, e.path, e.ranges, e.suggested, e.removed, nil); err != nil {
				return err
			}
		}
		return nil
	}

	filePath, ranges, suggested, removed, newFullContent := extractCopilotRanges(p)
	if filePath == "" {
		// Copilot removes files either with a dedicated delete tool (a bare
		// path) or via its terminal tool (`rm`). Neither produces an edit
		// range, so credit the removal here — otherwise an AI-deleted file
		// falls through to Human at commit time.
		gen := copilotHookGenType(p)
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
	return recordCopilotFileEdit(p, filePath, ranges, suggested, removed, newFullContent)
}

// recordCopilotFileEdit resolves filePath's repo and posts one Copilot edit for
// it. The repo comes from the FILE, not the hook's cwd: a VS Code multi-root
// workspace reports the first root as cwd while the agent edits a sibling repo.
func recordCopilotFileEdit(p copilotHookPayload, filePath string, ranges []LineRange, suggested int64, removed []DeletedLineHash, newFullContent *string) error {
	resolved := resolveSymlinks(filePath)
	// One git process for repo, top level and HEAD; the working-log capture
	// below reuses it instead of asking git again.
	loc := gitutil.Locate(resolved)
	repoPath := loc.RepoID
	if repoPath == "" && p.Cwd != "" {
		repoPath, _ = gitutil.RepoID(resolveSymlinks(p.Cwd))
	}
	wt := loc.Toplevel
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

	gen := copilotHookGenType(p)
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
		if body == "" && in.OldStr == "" {
			// command=view (or any other read): nothing was written.
			return "", nil, 0, nil, nil
		}
		removed := RemovedLineHashes(in.OldStr, body)
		if strings.TrimSpace(body) == "" && in.OldStr != "" {
			return in.Path, nil, int64(countLines(in.OldStr)), removed, nil
		}
		ranges, suggested := copilotAddedRanges(in.OldStr, body)
		return in.Path, ranges, suggested, removed, nil

	// VS Code's agent tools name their fields in camelCase (filePath, oldString,
	// newString); older shapes used "path". Read both — a missed path drops the
	// edit entirely and its lines commit as Human.

	case "create_file":
		// Payload: {filePath | path, content} — new file, nothing removed.
		var in struct {
			FilePath string `json:"filePath"`
			Path     string `json:"path"`
			Content  string `json:"content"`
		}
		if err := json.Unmarshal(p.ToolInput, &in); err != nil {
			return "", nil, 0, nil, nil
		}
		fp := firstNonEmpty(in.FilePath, in.Path)
		if fp == "" {
			return "", nil, 0, nil, nil
		}
		return fp, perLineShaRangesFromContent(in.Content), int64(countLines(in.Content)), nil, nil

	case "insert_edit_into_file":
		// Payload: {filePath | path, code, explanation?} — pure insertion, nothing removed.
		var in struct {
			FilePath string `json:"filePath"`
			Path     string `json:"path"`
			Code     string `json:"code"`
		}
		if err := json.Unmarshal(p.ToolInput, &in); err != nil {
			return "", nil, 0, nil, nil
		}
		fp := firstNonEmpty(in.FilePath, in.Path)
		if fp == "" {
			return "", nil, 0, nil, nil
		}
		return fp, perLineShaRangesFromContent(in.Code), int64(countLines(in.Code)), nil, nil

	case "replace_string_in_file":
		// Payload: {filePath, oldString, newString, explanation?} — VS Code's
		// edit-an-existing-file tool.
		var in copilotReplacement
		if err := json.Unmarshal(p.ToolInput, &in); err != nil || in.FilePath == "" {
			return "", nil, 0, nil, nil
		}
		ranges, suggested, removed := in.ranges()
		return in.FilePath, ranges, suggested, removed, nil

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
		// "path" or (VS Code) "filePath"; Claude/older hooks use "file_path".
		// Content body may be in "new_string", "newString", "new_str", "content",
		// or "code"; an old body (for removed-line detection) in "old_string",
		// "oldString" or "old_str".
		var generic struct {
			FilePath      string `json:"file_path"`
			FilePathCamel string `json:"filePath"`
			Path          string `json:"path"`
			NewString     string `json:"new_string"`
			NewStringCam  string `json:"newString"`
			NewStr        string `json:"new_str"`
			Content       string `json:"content"`
			Code          string `json:"code"`
			OldString     string `json:"old_string"`
			OldStringCam  string `json:"oldString"`
			OldStr        string `json:"old_str"`
		}
		if err := json.Unmarshal(p.ToolInput, &generic); err != nil {
			return "", nil, 0, nil, nil
		}
		fp := firstNonEmpty(generic.FilePath, generic.FilePathCamel, generic.Path)
		if fp == "" {
			return "", nil, 0, nil, nil
		}
		body := firstNonEmpty(generic.NewString, generic.NewStringCam, generic.NewStr, generic.Content, generic.Code)
		old := firstNonEmpty(generic.OldString, generic.OldStringCam, generic.OldStr)
		if body == "" && old == "" {
			// A path with no content either way is a READ (read_file, list_dir,
			// the CLI's view), not an edit. Recording it would also run
			// captureAuthorship, which credits every uncommitted line in the
			// file — the human's included — to Copilot.
			return "", nil, 0, nil, nil
		}
		removed := RemovedLineHashes(old, body)
		ranges, suggested := copilotAddedRanges(old, body)
		return fp, ranges, suggested, removed, nil
	}
}

// copilotReplacement is one VS Code string replacement: the whole input of
// replace_string_in_file, or one entry of multi_replace_string_in_file.
type copilotReplacement struct {
	FilePath  string `json:"filePath"`
	OldString string `json:"oldString"`
	NewString string `json:"newString"`
}

// ranges mirrors the Edit shape: a pure deletion credits the removed lines via
// suggested only; otherwise just the genuinely-changed lines are added.
func (r copilotReplacement) ranges() ([]LineRange, int64, []DeletedLineHash) {
	removed := RemovedLineHashes(r.OldString, r.NewString)
	if strings.TrimSpace(r.NewString) == "" && r.OldString != "" {
		return nil, int64(countLines(r.OldString)), removed
	}
	ranges, suggested := copilotAddedRanges(r.OldString, r.NewString)
	return ranges, suggested, removed
}

// copilotFileEdit is one file's share of a tool call that can touch several.
type copilotFileEdit struct {
	path      string
	ranges    []LineRange
	suggested int64
	removed   []DeletedLineHash
}

// extractCopilotMultiFileEdits handles the VS Code agent tools whose one call
// can edit several files — multi_replace_string_in_file and apply_patch — and
// returns one edit per file. ok is false for every other tool, which then goes
// through the single-file extractCopilotRanges.
func extractCopilotMultiFileEdits(p copilotHookPayload) (edits []copilotFileEdit, ok bool) {
	switch {
	case p.ToolName == "multi_replace_string_in_file":
		// Payload: {explanation, replacements: [{filePath, oldString, newString}, …]}
		var in struct {
			Replacements []copilotReplacement `json:"replacements"`
		}
		if err := json.Unmarshal(p.ToolInput, &in); err != nil {
			return nil, true
		}
		for _, r := range in.Replacements {
			if r.FilePath == "" {
				continue
			}
			ranges, suggested, removed := r.ranges()
			edits = append(edits, copilotFileEdit{path: r.FilePath, ranges: ranges, suggested: suggested, removed: removed})
		}
		return edits, true

	case strings.Contains(strings.ToLower(p.ToolName), "patch"):
		// Payload: {input: "*** Begin Patch …", explanation}. The paths live in
		// the patch body; a patch with none falls back to the single-file path.
		// (Not looksLikePatch: that also matches "shell", which is the terminal
		// tool handled below.)
		body, _ := patchEnvelope(p.ToolInput)
		files := parseApplyPatchPerLine(body)
		if len(files) == 0 {
			return nil, false
		}
		for _, f := range files {
			path := f.abs
			if !filepath.IsAbs(path) && p.Cwd != "" {
				path = filepath.Join(p.Cwd, path)
			}
			e := copilotFileEdit{path: path, suggested: int64(len(f.added))}
			for _, a := range f.added {
				e.ranges = append(e.ranges, LineRange{Start: a.Start, End: a.End, ContentSHA: a.ContentSHA, ContentSHANorm: a.ContentSHANorm})
			}
			for _, r := range f.removed {
				e.removed = append(e.removed, DeletedLineHash{ContentSHA: r.ContentSHA, ContentSHANorm: r.ContentSHANorm})
			}
			edits = append(edits, e)
		}
		return edits, true
	}
	return nil, false
}

// copilotHookGenType is copilotGenType plus the surface the hook came from:
// VS Code's agent hooks point transcript_path into the Copilot Chat extension's
// storage (…/GitHub.copilot-chat/transcripts/…), the CLI's into ~/.copilot.
// Chat-panel edits are "chat", the same as the transcript watcher records them.
func copilotHookGenType(p copilotHookPayload) string {
	// Backslashes replaced explicitly: filepath.ToSlash is a no-op off Windows,
	// and the path is matched the same whatever OS reads it.
	if strings.Contains(strings.ToLower(strings.ReplaceAll(p.TranscriptPath, `\`, "/")), "/github.copilot-chat/") {
		return "chat"
	}
	return copilotGenType(p.ToolName)
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
