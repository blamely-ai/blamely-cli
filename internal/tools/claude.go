package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/blamely/blamely/internal/config"
	"github.com/blamely/blamely/internal/daemon"
	"github.com/blamely/blamely/internal/gitutil"
)

// Claude Code's PostToolUse hook pipes a payload like:
// {
//   "session_id": "...",
//   "transcript_path": "/Users/.../.claude/projects/.../<uuid>.jsonl",
//   "cwd": "/path/to/repo",
//   "tool_name": "Edit",   // or Write, MultiEdit, NotebookEdit
//   "tool_input": { ... }
// }
//
// We extract file_path, locate the new content's line range, then enrich
// with model+tokens from the transcript before POSTing to the daemon.

// claudeHookPayload is the JSON body that both Claude Code AND Cursor send
// to a PostToolUse hook. Cursor includes `cursor_version`; Claude Code does not.
type claudeHookPayload struct {
	SessionID      string          `json:"session_id"`
	ConversationID string          `json:"conversation_id"` // Cursor alias for session_id
	TranscriptPath string          `json:"transcript_path"`
	Cwd            string          `json:"cwd"`
	ToolName       string          `json:"tool_name"`
	ToolInput      json.RawMessage `json:"tool_input"`
	// Cursor-specific fields
	CursorVersion string `json:"cursor_version"`
	Model         string `json:"model"` // Cursor puts the model in the top-level payload
	// GenType may be set by Cursor to "completion" for Tab accepts vs "chat"
	// for Composer edits. When empty we infer from the session context.
	GenType string `json:"gen_type"`
}

// The input structs accept both `file_path` (Claude's field) and `path`
// (Cursor's native field, e.g. StrReplace/Write in agent transcripts and some
// hook versions) — see target().
type editInput struct {
	FilePath  string `json:"file_path"`
	Path      string `json:"path"`
	OldString string `json:"old_string"`
	NewString string `json:"new_string"`
}

func (in editInput) target() string { return firstNonEmpty(in.FilePath, in.Path) }

type writeInput struct {
	FilePath string `json:"file_path"`
	Path     string `json:"path"`
	Content  string `json:"content"`
}

func (in writeInput) target() string { return firstNonEmpty(in.FilePath, in.Path) }

type multiEditInput struct {
	FilePath string `json:"file_path"`
	Path     string `json:"path"`
	Edits    []struct {
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
	} `json:"edits"`
}

func (in multiEditInput) target() string { return firstNonEmpty(in.FilePath, in.Path) }

// deletePathFromInput pulls the target path out of a Cursor `Delete` tool's
// input, accepting either `path` (Cursor's field) or `file_path` (the
// Claude-shaped alias) so the parser is tolerant of version drift.
func deletePathFromInput(raw json.RawMessage) string {
	var in struct {
		Path     string `json:"path"`
		FilePath string `json:"file_path"`
	}
	if json.Unmarshal(raw, &in) != nil {
		return ""
	}
	if in.Path != "" {
		return in.Path
	}
	return in.FilePath
}

// cursorGenType resolves a Cursor edit/delete's generation type: an explicit
// gen_type if the payload carries one, "completion" for a Tab accept (no
// conversation context), otherwise a Composer/chat edit.
func cursorGenType(p claudeHookPayload) string {
	if p.GenType != "" {
		return p.GenType
	}
	if p.SessionID == "" && p.ConversationID == "" {
		return "completion"
	}
	return "chat"
}

// RecordClaudeFromStdin handles the PostToolUse hook payload sent by both
// Claude Code AND Cursor — they share the same hooks framework. The payload
// is distinguished by the presence of `cursor_version`: Cursor payloads
// include it, Claude Code payloads do not.
//
// It also fields payloads from Devin CLI. Devin reads user-level hooks out of
// ~/.claude/settings.json and ~/.claude.json in addition to its own config, so
// on a machine running both, the `record claude` hook blamely installs is fired
// by Devin too. Left alone that files every Devin edit under claude; the
// IsDevinHookPayload check below reroutes them to the Devin recorder.
func RecordClaudeFromStdin(r io.Reader) error {
	raw, err := readHookPayload(r)
	if err != nil {
		return err
	}
	if IsDevinHookPayload(raw) {
		return recordDevinPayload(raw)
	}
	var p claudeHookPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return fmt.Errorf("parse hook payload: %w", err)
	}
	// Cursor sends session_id under conversation_id in some versions.
	if p.SessionID == "" && p.ConversationID != "" {
		p.SessionID = p.ConversationID
	}

	isCursor := p.CursorVersion != ""

	// Cursor often omits transcript_path from the hook payload. Derive it from
	// cwd + session_id using the known storage layout:
	//   ~/.cursor/projects/<cwd-encoded>/agent-transcripts/<uuid>/<uuid>.jsonl
	// where <cwd-encoded> is the cwd with leading slash removed and / → -.
	if isCursor && p.TranscriptPath == "" && p.SessionID != "" && p.Cwd != "" {
		p.TranscriptPath = cursorTranscriptPath(p.Cwd, p.SessionID)
	}

	// Claude CLI also sometimes omits transcript_path. Derive it from cwd +
	// session_id using the Claude storage layout:
	//   ~/.claude/projects/<cwd-encoded>/<session-id>.jsonl
	// where <cwd-encoded> replaces ALL slashes (including the leading /) with -.
	if !isCursor && p.TranscriptPath == "" && p.SessionID != "" && p.Cwd != "" {
		p.TranscriptPath = claudeTranscriptPath(p.Cwd, p.SessionID)
	}

	filePath, ranges, suggested, removed, newFullContent, err := extractClaudeRanges(p)
	if err != nil {
		return err
	}
	if filePath == "" {
		// Cursor deletes a file with a dedicated `Delete` tool (payload: a bare
		// `path`/`file_path`), and runs shell via `Shell`. Neither produces an
		// edit range, so without this an AI-deleted file falls through to Human
		// at commit time. Handle both so Cursor deletions are credited to it.
		if isCursor {
			switch p.ToolName {
			case "Delete":
				if path := deletePathFromInput(p.ToolInput); path != "" {
					return recordToolDeletionPath(path, p.Cwd, "cursor", cursorGenType(p), p.Model, p.SessionID, p.TranscriptPath, "cursor_delete")
				}
			case "Shell":
				return recordShellDeletionsFrom(p.Cwd, shellCommandFromInput(p.ToolInput), "cursor", cursorGenType(p), p.Model, p.SessionID, p.TranscriptPath, "cursor_shell_delete")
			}
			return nil
		}
		// No file-edit tool produced a path. A Bash command, however, may have
		// created or modified files directly (e.g. `printf > f`, `cat > f`,
		// heredocs, a script) — bypassing Write/Edit entirely. We can't parse
		// arbitrary shell (paths are often dynamic, e.g. `> "$fname"`), so we
		// ask git which source files in the repo just changed and attribute
		// those. Claude only (Cursor Tab has no Bash tool).
		if p.ToolName == "Bash" {
			gt := ReadTranscriptGenType(p.TranscriptPath)
			if gt == "" {
				gt = "chat"
			}
			return recordClaudeBashWrites(p.Cwd, p.SessionID, p.TranscriptPath, gt, shellCommandFromInput(p.ToolInput))
		}
		return nil
	}

	resolvedFile := resolveSymlinks(filePath)
	repoPath, _ := gitutil.RepoID(resolvedFile)
	if repoPath == "" && p.Cwd != "" {
		repoPath, _ = gitutil.RepoID(resolveSymlinks(p.Cwd))
	}
	wt, _ := gitutil.Toplevel(resolvedFile)
	rel := resolvedFile
	if wt != "" {
		if r, err := filepath.Rel(wt, resolvedFile); err == nil && !strings.HasPrefix(r, "..") {
			rel = r
		}
	}

	// Write overwrites the whole file with no "before" content of its own —
	// fetch the daemon's cached snapshot (the file's content as of its last
	// recorded edit) so we can still detect what this Write changed.
	if newFullContent != nil {
		var wfRemoved []DeletedLineHash
		ranges, wfRemoved = ResolveWholeFileWrite(repoPath, rel, *newFullContent, ranges)
		removed = append(removed, wfRemoved...)
	}

	tool := "claude"
	if isCursor {
		tool = "cursor"
	}

	// Determine generation type.
	//
	// For Claude CLI/Code: read the "entrypoint" field from the first user turn
	// of the transcript. "cli" and "sdk-ts" entrypoints map to GenTypeCLI;
	// interactive entrypoints (e.g. "claude-vscode") map to GenTypeChat.
	//
	// For Cursor we distinguish two sources:
	//   - Composer (chat): fires from within a conversation → session_id or
	//     conversation_id is set in the payload.
	//   - Cursor Tab (inline completion): no conversation context → both IDs
	//     are empty. When Cursor sets gen_type explicitly in the payload we
	//     honour that directly; otherwise we use the session-presence heuristic.
	genType := "chat"
	switch {
	case isCursor && p.GenType != "":
		genType = p.GenType
	case isCursor && p.SessionID == "" && p.ConversationID == "":
		genType = "completion"
	case !isCursor:
		genType = ReadTranscriptGenType(p.TranscriptPath)
	}

	payload := daemon.EditPayload{
		Tool:           tool,
		Confidence:     "high",
		GenType:        genType,
		RepoPath:       repoPath,
		FilePath:       rel,
		SuggestedLines: suggested,
		Lines:          toDaemonRanges(ranges),
		RemovedLines:   toDaemonRemovedLines(removed),
		RawMeta: fmt.Sprintf(`{"session_id":%q,"tool":%q,"cursor_version":%q,"transcript_path":%q}`,
			p.SessionID, p.ToolName, p.CursorVersion, p.TranscriptPath),
	}

	if isCursor && p.Model != "" {
		payload.Model = p.Model
	}
	applyHookUsage(&payload, hookUsageOptions{
		transcriptPath: p.TranscriptPath,
		sessionID:      p.SessionID,
		tool:           tool,
	})

	// Attribution: mirror this edit into the working log
	// BEFORE the daemon POST, so capture is daemon-independent. No-op when the flag
	// is off; never affects the recording below.
	captureAuthorship(repoPath, rel, tool, genType, payload.Model)

	return postToDaemon(payload)
}

const (
	// bashWriteWindow bounds how recently a file must have been modified to be
	// credited to the Bash command that just ran. The PostToolUse hook fires
	// immediately after the command, so a generous window absorbs hook latency
	// while still excluding files the user edited minutes earlier.
	bashWriteWindow = 15 * time.Second
	// maxBashWriteFiles caps how many changed files we attribute to one command.
	// A larger change set is almost certainly a bulk operation (build, codegen,
	// `git checkout`, `npm install`) rather than authored content — we skip
	// rather than guess, to avoid stealing credit for non-authored files.
	maxBashWriteFiles = 30
)

// recordClaudeBashWrites attributes source files that changed while a Claude
// Bash command ran. It uses `git status` (not a filesystem walk) so the scan is
// bounded and automatically respects .gitignore — build output and node_modules
// never appear. Each changed source file is recorded as a medium-confidence
// claude edit covering the whole file, matching how a Write is stored, so the
// existing commit-time attribution credits it without any further changes.
// A cwd ABOVE the repos (a workspace dir holding separate `backend/` and
// `frontend/` clones) resolves to every repo nested beneath it, so an agent
// started there still gets its shell writes attributed — see DiscoverRepos.
func recordClaudeBashWrites(cwd, sessionID, transcriptPath, genType, command string) error {
	for _, root := range gitutil.DiscoverRepos(cwd) {
		if err := recordClaudeBashWritesIn(root, sessionID, transcriptPath, genType, command); err != nil {
			return err
		}
	}
	return nil
}

// recordClaudeBashWritesIn is recordClaudeBashWrites scoped to one repo root.
func recordClaudeBashWritesIn(root, sessionID, transcriptPath, genType, command string) error {
	// File deletions the command performed (e.g. `rm foo.html`), scoped to the
	// command's actual rm/del targets so a file the USER deleted by hand isn't
	// swept in. Done first (and unconditionally) so a command that ONLY deletes
	// — leaving recentlyChangedFiles empty — still records its removals.
	if err := recordShellDeletions(root, command, "claude", genType, "", sessionID, transcriptPath, "claude_bash_delete"); err != nil {
		return err
	}

	for _, payload := range claudeBashWritePayloads(root, sessionID, transcriptPath, genType) {
		if err := postToDaemon(payload); err != nil {
			return err
		}
	}
	return nil
}

// claudeBashWritePayloads builds the edit payloads for the files a Bash command
// just wrote inside `root`. Split out from the recording so the resolution can be
// asserted in tests without a running daemon.
func claudeBashWritePayloads(root, sessionID, transcriptPath, genType string) []daemon.EditPayload {
	files := recentlyChangedFiles(root, bashWriteWindow)
	// none → read-only command (ls/grep/test); too many → bulk op we won't guess.
	if len(files) == 0 || len(files) > maxBashWriteFiles {
		return nil
	}
	repoID, _ := gitutil.RepoID(resolveSymlinks(root))
	if repoID == "" {
		repoID = root
	}
	out := make([]daemon.EditPayload, 0, len(files))
	for _, rel := range files {
		abs := filepath.Join(root, rel)
		ranges := perLineShaRanges(abs)
		if len(ranges) == 0 {
			continue
		}
		payload := daemon.EditPayload{
			Tool:           "claude",
			Confidence:     "medium",
			GenType:        genType,
			RepoPath:       repoID,
			FilePath:       rel,
			SuggestedLines: int64(len(ranges)),
			Lines:          toDaemonRanges(ranges),
			RawMeta: fmt.Sprintf(`{"session_id":%q,"tool":"claude","source":"claude_bash_fswrite","transcript_path":%q}`,
				sessionID, transcriptPath),
		}
		// Backfill model + token usage from the session transcript, exactly like
		// the Write/Edit hook path — otherwise the row has an empty model.
		applyHookUsage(&payload, hookUsageOptions{
			transcriptPath: transcriptPath,
			sessionID:      sessionID,
			tool:           "claude",
		})
		out = append(out, payload)
	}
	return out
}

// gitDeletedFiles returns repo-relative source-file paths git reports as
// deleted in the working tree (a 'D' in either status column). The files are
// gone from disk, so their content must come from HEAD.
func gitDeletedFiles(root string) []string {
	out, err := gitutil.Output(root, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return nil
	}
	var files []string
	for _, line := range strings.Split(string(out), "\n") {
		if len(line) < 4 {
			continue
		}
		if line[0] != 'D' && line[1] != 'D' {
			continue
		}
		path := strings.TrimSpace(line[3:])
		if i := strings.Index(path, " -> "); i >= 0 {
			path = path[i+len(" -> "):]
		}
		path = strings.Trim(path, `"`)
		if path != "" && looksLikeSourceFile(path) {
			files = append(files, path)
		}
	}
	return files
}

// headFileRemovedHashes fingerprints every non-blank line of rel as it exists
// at HEAD — the removed-line content hashes for a deleted file, so commit-time
// attribution can credit the AI that deleted it.
func headFileRemovedHashes(root, rel string) []DeletedLineHash {
	out, err := gitutil.Output(root, "show", "HEAD:"+filepath.ToSlash(rel))
	if err != nil {
		return nil
	}
	return RemovedLineHashes(string(out), "")
}

// recordHeadDeletion records that `tool` removed the repo-relative file `rel`
// (gone from the working tree but still present at HEAD). It fingerprints the
// file's HEAD content as removed lines and posts an edit carrying only those
// hashes, so commit-time attribution credits `tool` for the deletion — the
// same machinery that already handles a Claude `rm`. It is a no-op when the
// file isn't at HEAD (nothing to fingerprint), so a file the AI both created
// and deleted before any commit is never recorded (it never reaches a diff).
//
// Deletion edits carry no token usage: the session's tokens already land on the
// tool's add/edit rows, so attaching them here too would double-count. `model`
// is set only when the caller knows it (e.g. Cursor's payload carries one).
func recordHeadDeletion(root, rel, tool, genType, model, sessionID, transcriptPath, source string) error {
	payload, ok := buildHeadDeletionPayload(root, rel, tool, genType, model, sessionID, transcriptPath, source)
	if !ok {
		return nil
	}
	return postToDaemon(payload)
}

// buildHeadDeletionPayload assembles the daemon edit for a deleted file by
// fingerprinting its HEAD content as removed lines. Returns ok=false when the
// file isn't at HEAD (nothing to fingerprint). Separated from recordHeadDeletion
// so the payload can be asserted in tests without a running daemon.
func buildHeadDeletionPayload(root, rel, tool, genType, model, sessionID, transcriptPath, source string) (daemon.EditPayload, bool) {
	removed := headFileRemovedHashes(root, rel)
	if len(removed) == 0 {
		return daemon.EditPayload{}, false
	}
	repoID, _ := gitutil.RepoID(resolveSymlinks(root))
	if repoID == "" {
		repoID = root
	}
	payload := daemon.EditPayload{
		Tool:           tool,
		Confidence:     "high",
		GenType:        genType,
		RepoPath:       repoID,
		FilePath:       rel,
		SuggestedLines: int64(len(removed)),
		RemovedLines:   toDaemonRemovedLines(removed),
		RawMeta: fmt.Sprintf(`{"session_id":%q,"tool":%q,"source":%q,"transcript_path":%q}`,
			sessionID, tool, source, transcriptPath),
	}
	if model != "" {
		payload.Model = model
	}
	return payload, true
}

// recordToolDeletionPath records `tool`'s deletion of a specific absolute file
// path. The file is already gone from disk, so the repo root is resolved from
// its still-existing parent directory (falling back to cwd) and the path is
// rebuilt in the root's symlink-space for a clean repo-relative name. Used by
// the tools that name the deleted file explicitly (Cursor's `Delete`, Codex's
// `*** Delete File:`), so attribution is scoped to exactly that file rather
// than every deletion in the working tree.
func recordToolDeletionPath(absPath, cwd, tool, genType, model, sessionID, transcriptPath, source string) error {
	if absPath == "" {
		return nil
	}
	root := findRepoRoot(absPath, cwd)
	if root == "" {
		return nil
	}
	dir := filepath.Dir(absPath)
	if rd, err := filepath.EvalSymlinks(dir); err == nil {
		dir = rd
	}
	rootResolved := root
	if rr, err := filepath.EvalSymlinks(root); err == nil {
		rootResolved = rr
	}
	rel, err := filepath.Rel(rootResolved, filepath.Join(dir, filepath.Base(absPath)))
	if err != nil || strings.HasPrefix(rel, "..") {
		return nil
	}
	return recordHeadDeletion(root, rel, tool, genType, model, sessionID, transcriptPath, source)
}

// recordShellDeletions credits a tool for the files a shell command actually
// removed. It intersects git's deleted-file list with the rm/del targets parsed
// from `command` (via shellDeleteTargets), so a file the USER deleted by hand
// around the same time is NOT mis-credited to the AI just because it happens to
// be gone from the working tree. Used after a shell command an AI ran (Claude
// `Bash`, Cursor `Shell`, Gemini `run_shell_command`, Copilot `run_in_terminal`)
// where the deletion isn't a structured per-file op. When the command can't be
// parsed for delete targets (dynamic paths, scripts), nothing is recorded —
// under-attributing is safer than stealing credit for a human's deletion.
func recordShellDeletions(root, command, tool, genType, model, sessionID, transcriptPath, source string) error {
	targets := shellDeleteTargets(command)
	if len(targets) == 0 {
		return nil
	}
	for _, rel := range gitDeletedFiles(root) {
		if !MatchesFileOp(rel, targets) {
			continue
		}
		if err := recordHeadDeletion(root, rel, tool, genType, model, sessionID, transcriptPath, source); err != nil {
			return err
		}
	}
	return nil
}

// recordShellDeletionsFrom is recordShellDeletions for a command whose only
// location hint is the agent's cwd. It runs the scan for EVERY repo that cwd
// resolves to: normally the single repo containing cwd, but — when the agent was
// started ABOVE its repos, e.g. a workspace dir holding separate `backend/` and
// `frontend/` clones — each repo nested beneath it. `git rev-parse` cannot find
// those (it only searches upward), so without this every shell deletion from such
// a session is dropped and the removed lines fall back to Human at commit.
func recordShellDeletionsFrom(cwd, command, tool, genType, model, sessionID, transcriptPath, source string) error {
	return recordShellDeletionsIn(gitutil.DiscoverRepos(cwd), command, tool, genType, model, sessionID, transcriptPath, source)
}

// recordShellDeletionsIn runs recordShellDeletions over an already-resolved set
// of repo roots, for callers that need to know whether any repo was found at all.
func recordShellDeletionsIn(roots []string, command, tool, genType, model, sessionID, transcriptPath, source string) error {
	for _, root := range roots {
		if err := recordShellDeletions(root, command, tool, genType, model, sessionID, transcriptPath, source); err != nil {
			return err
		}
	}
	return nil
}

// shellCommandFromInput pulls the shell command out of a tool's hook input,
// accepting both `command` (Bash/Cursor Shell/Gemini/Copilot) and `cmd`.
func shellCommandFromInput(raw json.RawMessage) string {
	var in struct {
		Command string `json:"command"`
		Cmd     string `json:"cmd"`
	}
	if json.Unmarshal(raw, &in) != nil {
		return ""
	}
	if in.Command != "" {
		return in.Command
	}
	return in.Cmd
}

// perLineShaRanges returns one range per non-blank line of the file, each
// carrying that line's content_sha (sha256 of the line text, sans trailing \r).
// The editor gutter attributes chat/cli rows by hashing the CURRENT line text
// and matching it against these shas, so a single whole-file hash would never
// paint a line — per-line shas are what the live gutter needs, and they also let
// commit-time attribution survive line-number drift. Capped so a huge generated
// file can't blow past the daemon's request size limit.
func perLineShaRanges(absPath string) []LineRange {
	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil
	}
	const maxLines = 4000
	var out []LineRange
	for i, ln := range strings.Split(string(data), "\n") {
		text := strings.TrimRight(ln, "\r")
		if strings.TrimSpace(text) == "" {
			continue
		}
		n := i + 1
		out = append(out, LineRange{Start: n, End: n, ContentSHA: sha256Hex([]byte(text)), ContentSHANorm: sha256HexNorm(text)})
		if len(out) >= maxLines {
			break
		}
	}
	return out
}

// perLineShaRangesFromContent is perLineShaRanges for an in-memory string (the
// content an AI Write just produced) rather than a file on disk. It returns one
// {n,n} range per line. Non-blank lines carry a content_sha so attribution
// follows the line text across later insertions; blank lines carry no sha (a
// blank line's hash isn't unique) but still get a range so they're counted as
// authored. The trailing empty element from a final newline is dropped. Capped
// for request size, matching perLineShaRanges.
func perLineShaRangesFromContent(content string) []LineRange {
	if content == "" {
		return nil
	}
	lines := strings.Split(content, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1] // drop trailing empty from a final newline
	}
	const maxLines = 4000
	out := make([]LineRange, 0, len(lines))
	for i, ln := range lines {
		text := strings.TrimRight(ln, "\r")
		sha := ""
		shaNorm := ""
		if strings.TrimSpace(text) != "" {
			sha = sha256Hex([]byte(text))
			shaNorm = sha256HexNorm(text)
		}
		n := i + 1
		out = append(out, LineRange{Start: n, End: n, ContentSHA: sha, ContentSHANorm: shaNorm})
		if len(out) >= maxLines {
			break
		}
	}
	return out
}

// recentlyChangedFiles returns repo-relative paths that git reports as
// changed/untracked and whose on-disk mtime is within `window` of now. Using
// git keeps the result bounded (usually a handful of paths) and gitignore-aware.
func recentlyChangedFiles(root string, window time.Duration) []string {
	out, err := gitutil.Output(root, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return nil
	}
	cutoff := time.Now().Add(-window)
	var files []string
	for _, line := range strings.Split(string(out), "\n") {
		// Porcelain v1 lines are "XY <path>" (3-char status prefix). Renames
		// and copies use "XY <old> -> <new>"; we want the new path.
		if len(line) < 4 {
			continue
		}
		path := strings.TrimSpace(line[3:])
		if i := strings.Index(path, " -> "); i >= 0 {
			path = path[i+len(" -> "):]
		}
		path = strings.Trim(path, `"`)
		if path == "" {
			continue
		}
		abs := filepath.Join(root, path)
		info, err := os.Stat(abs)
		if err != nil || info.IsDir() || info.Size() == 0 {
			continue
		}
		if info.ModTime().Before(cutoff) {
			continue
		}
		if !looksLikeSourceFile(abs) {
			continue
		}
		files = append(files, path)
	}
	return files
}

// extractClaudeRanges returns the file path, the located line ranges after
// the edit landed, and `suggested`: the number of lines the model proposed
// before any user editing. For Edit/MultiEdit `suggested` is the new_string
// line count summed across edits; for Write it's the full content's line
// count. The watcher records this on the edit row so a later attribution
// pass can show "claude suggested 10, accepted 6" when the user overrode
// some of the AI-produced lines.
func extractClaudeRanges(p claudeHookPayload) (string, []LineRange, int64, []DeletedLineHash, *string, error) {
	switch p.ToolName {
	case "Edit", "StrReplace":
		// StrReplace is Cursor's native partial-edit tool — same
		// old_string/new_string semantics as Claude's Edit, with the target
		// under `path` instead of `file_path`. Without this case every
		// Cursor chat turn that patches (rather than rewrites) a file was
		// silently dropped and its lines fell to Human at commit time.
		var in editInput
		if err := json.Unmarshal(p.ToolInput, &in); err != nil {
			return "", nil, 0, nil, nil, fmt.Errorf("parse %s input: %w", p.ToolName, err)
		}
		file := in.target()
		if file == "" {
			return "", nil, 0, nil, nil, nil
		}
		removed := RemovedLineHashes(in.OldString, in.NewString)
		// Deletion case: new_string is empty (or whitespace-only) and
		// old_string was non-empty. We can't locate the deleted text in the
		// post-edit file (it's gone), but we still credit the AI with the
		// removal in `suggested_lines` so the report shows the AI touched
		// these many lines. Pre-image line numbers are filled in at commit
		// time by the diff parser; we record the file path here so the
		// human-edit watcher's `recent AI activity` lookup suppresses any
		// competing human-edit row for the same file.
		if strings.TrimSpace(in.NewString) == "" && in.OldString != "" {
			return file, nil, int64(countLines(in.OldString)), removed, nil, nil
		}
		// Credit the AI only for the lines that are genuinely new — not for
		// context lines that happen to be inside new_string for the match to
		// be precise. We always compute the line-count diff so suggested
		// stays accurate even when we can't locate the text in the file.
		fullRange, err := LocateNewString(file, in.NewString)
		if err != nil {
			return file, nil, CountAddedLines(in.OldString, in.NewString), removed, nil, err
		}
		if fullRange == nil {
			return file, nil, CountAddedLines(in.OldString, in.NewString), removed, nil, nil
		}
		ranges, suggested := narrowToChangedLines(in.OldString, in.NewString, *fullRange)
		return file, ranges, suggested, removed, nil, nil

	case "Write":
		var in writeInput
		if err := json.Unmarshal(p.ToolInput, &in); err != nil {
			return "", nil, 0, nil, nil, fmt.Errorf("parse Write input: %w", err)
		}
		file := in.target()
		if file == "" {
			return "", nil, 0, nil, nil, nil
		}
		suggested := int64(countLines(in.Content))
		// Emit ONE range per line, each non-blank line carrying its own
		// content_sha. This is what makes attribution survive line drift: when
		// the user later inserts a line into the AI-written file, each AI line is
		// re-located by hashing the CURRENT text and matching the stored sha at
		// its new position — so the AI lines stay AI even after they shift, and
		// the user's freshly-typed line (no matching sha) is correctly human.
		//
		// A single whole-file range [1, N] (or the old 1<<30 sentinel) can't do
		// this: it matches purely by line number, so an inserted line lands
		// inside the range and is mislabelled AI, while the AI line pushed past N
		// falls outside and is mislabelled human. Per-line shas fix both.
		ranges := perLineShaRangesFromContent(in.Content)
		if len(ranges) == 0 {
			return file, nil, suggested, nil, &in.Content, nil
		}
		return file, ranges, suggested, nil, &in.Content, nil

	case "MultiEdit":
		var in multiEditInput
		if err := json.Unmarshal(p.ToolInput, &in); err != nil {
			return "", nil, 0, nil, nil, fmt.Errorf("parse MultiEdit input: %w", err)
		}
		file := in.target()
		if file == "" {
			return "", nil, 0, nil, nil, nil
		}
		var suggested int64
		var out []LineRange
		var removed []DeletedLineHash
		for _, ed := range in.Edits {
			removed = append(removed, RemovedLineHashes(ed.OldString, ed.NewString)...)
			// Sub-edit deletion: empty NewString means this sub-edit removed
			// old_string. Credit the AI for the number of lines deleted via
			// suggested_lines, but don't try to locate the (now-missing) text.
			if strings.TrimSpace(ed.NewString) == "" && ed.OldString != "" {
				suggested += int64(countLines(ed.OldString))
				continue
			}
			lr, err := LocateNewString(file, ed.NewString)
			if err != nil || lr == nil {
				// Can't locate, but still credit the AI's net-new line count.
				suggested += CountAddedLines(ed.OldString, ed.NewString)
				continue
			}
			narrowed, narrowSuggest := narrowToChangedLines(ed.OldString, ed.NewString, *lr)
			out = append(out, narrowed...)
			suggested += narrowSuggest
		}
		return file, out, suggested, removed, nil, nil

	case "NotebookEdit":
		var in struct {
			NotebookPath string `json:"notebook_path"`
		}
		if err := json.Unmarshal(p.ToolInput, &in); err != nil {
			return "", nil, 0, nil, nil, fmt.Errorf("parse NotebookEdit input: %w", err)
		}
		// We don't attempt line-range attribution inside notebook cells in v1.
		// Recording a marker so we can attribute at least "the notebook was AI-edited".
		return in.NotebookPath, nil, 0, nil, nil, nil

	default:
		return "", nil, 0, nil, nil, nil
	}
}

// countLines returns the number of lines in s. Empty string → 0. A non-empty
// string with no trailing newline still counts its content as one line.
func countLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

func toDaemonRanges(rs []LineRange) []daemon.Range {
	out := make([]daemon.Range, 0, len(rs))
	for _, r := range rs {
		out = append(out, daemon.Range{Start: r.Start, End: r.End, ContentSHA: r.ContentSHA, ContentSHANorm: r.ContentSHANorm})
	}
	return out
}

func toDaemonRemovedLines(rs []DeletedLineHash) []daemon.RemovedLineHash {
	out := make([]daemon.RemovedLineHash, 0, len(rs))
	for _, r := range rs {
		out = append(out, daemon.RemovedLineHash{ContentSHA: r.ContentSHA, ContentSHANorm: r.ContentSHANorm})
	}
	return out
}

func int64Ptr(v int64) *int64 { return &v }

// cursorTranscriptPath derives the Cursor agent-transcript JSONL path for a
// given project working directory and session UUID. Cursor stores transcripts at:
//
//	~/.cursor/projects/<cwd-encoded>/agent-transcripts/<uuid>/<uuid>.jsonl
//
// where <cwd-encoded> is the cwd with leading slash removed and / replaced by -.
// Returns "" if the file doesn't exist yet (e.g. session hasn't been written).
func cursorTranscriptPath(cwd, sessionID string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	proj := strings.TrimPrefix(filepath.ToSlash(cwd), "/")
	proj = strings.ReplaceAll(proj, "/", "-")
	p := filepath.Join(home, ".cursor", "projects", proj, "agent-transcripts", sessionID, sessionID+".jsonl")
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// claudeTranscriptPath derives the Claude CLI/Code transcript JSONL path for a
// given project working directory and session UUID. Claude stores transcripts at:
//
//	<claude-config-dir>/projects/<cwd-encoded>/<session-id>.jsonl
//
// where <cwd-encoded> replaces ALL slashes (including the leading /) with -, and
// <claude-config-dir> is ~/.claude by default or a custom CLAUDE_CONFIG_DIR/corp
// dir. Checks EVERY dir in the union and returns the first existing transcript, or
// "" if none exists.
func claudeTranscriptPath(cwd, sessionID string) string {
	proj := strings.ReplaceAll(filepath.ToSlash(cwd), "/", "-")
	for _, base := range config.ClaudeProjectsDirs() {
		p := filepath.Join(base, proj, sessionID+".jsonl")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// resolveSymlinks returns the canonical path with symlinks resolved.
// Falls back to the input if EvalSymlinks fails (e.g. for newly created paths).
func resolveSymlinks(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

func findRepoRoot(filePath, fallbackCwd string) string {
	// Try `git rev-parse --show-toplevel` against the file's directory.
	dir := filepath.Dir(filePath)
	if root, ok := gitToplevel(dir); ok {
		return root
	}
	if fallbackCwd != "" {
		if root, ok := gitToplevel(fallbackCwd); ok {
			return root
		}
	}
	return ""
}

func gitToplevel(dir string) (string, bool) {
	out, err := gitutil.Output(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

func postToDaemon(payload daemon.EditPayload) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	// Prefer Unix socket (bypasses network security tools like Trend Micro).
	// Fall back to TCP port on systems where AF_UNIX is unavailable (older Windows).
	var client *http.Client
	var url string
	if sock, serr := daemon.ReadSocket(); serr == nil {
		client = daemon.UnixHTTPClient(sock)
		url = "http://unix/edit"
	} else if port, perr := daemon.ReadPort(); perr == nil {
		client = &http.Client{Timeout: 2 * time.Second}
		url = fmt.Sprintf("http://127.0.0.1:%d/edit", port)
	} else {
		_ = perr
		return nil // daemon not running; best-effort, don't break Claude's flow
	}

	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("daemon rejected: %s: %s", resp.Status, string(msg))
	}
	return nil
}

func looksLikeSourceFile(p string) bool {
	// Skip hidden files, binaries, and common non-code extensions.
	base := filepath.Base(p)
	if strings.HasPrefix(base, ".") {
		return false
	}
	ext := strings.ToLower(filepath.Ext(p))
	skip := map[string]bool{
		".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".svg": true,
		".pdf": true, ".zip": true, ".tar": true, ".gz": true, ".exe": true,
		".bin": true, ".dll": true, ".so": true, ".dylib": true, ".db": true,
		".sqlite": true, ".sqlite3": true,
	}
	return !skip[ext]
}
