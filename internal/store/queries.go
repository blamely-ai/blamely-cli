package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

type Tool string

const (
	ToolClaude  Tool = "claude"
	ToolCursor  Tool = "cursor"
	ToolCodex   Tool = "codex"
	ToolCopilot Tool = "copilot"
	ToolGemini  Tool = "gemini"
	// ToolDevin is Devin CLI — Cognition's local terminal agent, recorded via
	// its Claude-Code-compatible PostToolUse hook. Note this covers the LOCAL
	// CLI only: Devin Cloud sessions edit files in a remote sandbox that never
	// touches this machine, so those arrive as an ordinary `git pull` and are
	// invisible to the daemon.
	ToolDevin Tool = "devin"
	// ToolHuman is retained ONLY to recognise legacy rows written before the
	// human/tool split. New code never writes this value — human-typed code
	// is represented by tool="" + gen_type=GenTypeHuman. Readers normalise
	// legacy ToolHuman rows on the fly.
	ToolHuman Tool = "human"
	// ToolCopyPaste marks content that arrived via a clipboard paste rather
	// than being typed. We don't claim AI origin — the source could be a
	// web AI chat, another project, Stack Overflow, etc. The signal is
	// "this code was pasted, not typed", which is itself useful in reports
	// and stops blamely from confidently labelling pasted code as human-typed.
	ToolCopyPaste Tool = "copypaste"
)

type Confidence string

const (
	ConfidenceHigh   Confidence = "high"
	ConfidenceMedium Confidence = "medium"
	ConfidenceLow    Confidence = "low"
)

// GenType describes how an AI edit was produced.
type GenType string

const (
	GenTypeChat       GenType = "chat"       // Conversational AI session (Claude Code, Cursor Composer, Copilot Chat)
	GenTypeCLI        GenType = "cli"        // Command-line tool (Codex CLI, claude CLI)
	GenTypeCompletion GenType = "completion" // Inline/tab completion (Copilot Tab, Cursor Tab)
	// GenTypeHuman is for code typed by the user. Paired with an empty tool
	// field — humans aren't a tool. This is what HumanEditWatcher emits and
	// what attribute.go falls back to when no AI signal claims a line.
	GenTypeHuman   GenType = "human"
	GenTypeUnknown GenType = "unknown"
)

type Edit struct {
	ID               int64
	TimestampNanos   int64
	RepoPath         string
	FilePath         string
	Tool             Tool
	Confidence       Confidence
	GenType          GenType
	Model            sql.NullString
	InputTokens      sql.NullInt64
	OutputTokens     sql.NullInt64
	CacheReadTokens  sql.NullInt64
	CacheWriteTokens sql.NullInt64
	HashBefore       sql.NullString
	HashAfter        sql.NullString
	RawMeta          sql.NullString
	// SuggestedLines is the AI's original suggestion size at watcher time.
	// AcceptedLines() returns the sum of (EndLine-StartLine+1) across Lines,
	// i.e. what actually stuck after any partial-acceptance/user-editing.
	SuggestedLines int64
	Lines          []EditLine
	// Branch is the checked-out branch when the edit was recorded ("" = detached
	// or unknown). Denormalized onto the row so the live gutter can scope by
	// branch with a single indexed lookup. SessionID is a UUID string for the
	// work session row in sessions (distinct from AI-tool session_id in raw_meta).
	Branch    string
	SessionID sql.NullString
	// RemovedLines holds content hashes of lines this edit DELETED (from
	// old_string / unified-diff "-" lines), used to attribute committed
	// `type:"delete"` ranges back to this edit. Unlike Lines, these carry no
	// position — deleted lines have no stable post-edit location.
	RemovedLines []RemovedLineHash
}

// AcceptedLines returns the total number of lines covered by this edit's
// post-acceptance line ranges. Compare to SuggestedLines for an acceptance
// ratio: e.g. SuggestedLines=10, AcceptedLines()=6 means the user kept 6.
func (e *Edit) AcceptedLines() int64 {
	var n int64
	for _, l := range e.Lines {
		if l.EndLine >= l.StartLine {
			n += int64(l.EndLine - l.StartLine + 1)
		}
	}
	return n
}

type EditLine struct {
	StartLine      int
	EndLine        int
	ContentSHA     string
	ContentSHANorm string
}

// RemovedLineHash is the content hash of a single line an edit deleted.
// ContentSHANorm mirrors EditLine's drift fallback: the whitespace-normalized
// hash, used when an autoformatter reflows a line before/around the deletion.
// Both are "" for blank/whitespace-only lines (never matched).
type RemovedLineHash struct {
	ContentSHA     string
	ContentSHANorm string
}

func (db *DB) InsertEdit(e Edit) (int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	gt := string(e.GenType)
	if gt == "" {
		gt = string(GenTypeUnknown)
	}
	res, err := tx.Exec(`
		INSERT INTO edits(ts, repo_path, file_path, tool, confidence, gen_type,
			model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
			hash_before, hash_after, raw_meta, suggested_lines, branch, session_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.TimestampNanos, e.RepoPath, e.FilePath, string(e.Tool), string(e.Confidence), gt,
		nullableString(e.Model), nullableInt(e.InputTokens), nullableInt(e.OutputTokens),
		nullableInt(e.CacheReadTokens), nullableInt(e.CacheWriteTokens),
		nullableString(e.HashBefore), nullableString(e.HashAfter), nullableString(e.RawMeta),
		e.SuggestedLines, nullableNonEmpty(e.Branch), nullableNullString(e.SessionID),
	)
	if err != nil {
		return 0, fmt.Errorf("insert edit: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("last insert id: %w", err)
	}
	for _, ln := range e.Lines {
		if _, err := tx.Exec(`
			INSERT INTO edit_lines(edit_id, start_line, end_line, content_sha, content_sha_norm)
			VALUES (?, ?, ?, ?, ?)`,
			id, ln.StartLine, ln.EndLine, ln.ContentSHA, nullableNonEmpty(ln.ContentSHANorm),
		); err != nil {
			return 0, fmt.Errorf("insert edit_line: %w", err)
		}
	}
	for _, rl := range e.RemovedLines {
		if rl.ContentSHA == "" {
			continue
		}
		if _, err := tx.Exec(`
			INSERT INTO edit_removed_lines(edit_id, content_sha, content_sha_norm)
			VALUES (?, ?, ?)`,
			id, rl.ContentSHA, nullableNonEmpty(rl.ContentSHANorm),
		); err != nil {
			return 0, fmt.Errorf("insert edit_removed_line: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return id, nil
}

// EditsForFileSince returns edits touching repo/file with ts >= sinceNanos.
// Sorted by confidence DESC then ts DESC so that high-confidence records
// (PostToolUse hooks) are always preferred over medium/low-confidence records
// (log watchers) for the same line, regardless of which
// fired last. Within the same confidence level, newest wins.
func (db *DB) EditsForFileSince(repo, file string, sinceNanos int64) ([]Edit, error) {
	return db.editsForFileWhere(
		`repo_path = ? AND file_path = ? AND ts >= ?`,
		repo, file, sinceNanos,
	)
}

// editsForFileWhere runs the standard edit SELECT with a caller-supplied WHERE
// clause (and its bind args), applies the confidence-then-recency ordering, and
// hydrates each edit's line ranges. Shared by EditsForFileSince and
// EditsForFileInSession so the column list and ordering stay in one place.
func (db *DB) editsForFileWhere(where string, args ...any) ([]Edit, error) {
	rows, err := db.Query(`
		SELECT id, ts, repo_path, file_path, tool, confidence, gen_type,
			model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
			hash_before, hash_after, raw_meta, suggested_lines, branch, session_id
		FROM edits
		WHERE `+where+`
		ORDER BY
			CASE confidence WHEN 'high' THEN 3 WHEN 'medium' THEN 2 WHEN 'low' THEN 1 ELSE 0 END DESC,
			ts DESC`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("query edits: %w", err)
	}
	defer rows.Close()

	var out []Edit
	for rows.Next() {
		var e Edit
		var tool, conf, genType string
		var branch sql.NullString
		if err := rows.Scan(&e.ID, &e.TimestampNanos, &e.RepoPath, &e.FilePath, &tool, &conf, &genType,
			&e.Model, &e.InputTokens, &e.OutputTokens, &e.CacheReadTokens, &e.CacheWriteTokens,
			&e.HashBefore, &e.HashAfter, &e.RawMeta, &e.SuggestedLines, &branch, &e.SessionID,
		); err != nil {
			return nil, fmt.Errorf("scan edit: %w", err)
		}
		e.Tool = Tool(tool)
		e.Confidence = Confidence(conf)
		e.GenType = GenType(genType)
		e.Branch = branch.String
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		lines, err := db.linesForEdit(out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Lines = lines
		removed, err := db.removedLinesForEdit(out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].RemovedLines = removed
	}
	return out, nil
}

func (db *DB) removedLinesForEdit(editID int64) ([]RemovedLineHash, error) {
	rows, err := db.Query(`SELECT content_sha, COALESCE(content_sha_norm, '')
		FROM edit_removed_lines WHERE edit_id = ?`, editID)
	if err != nil {
		return nil, fmt.Errorf("query edit_removed_lines: %w", err)
	}
	defer rows.Close()
	var out []RemovedLineHash
	for rows.Next() {
		var rl RemovedLineHash
		if err := rows.Scan(&rl.ContentSHA, &rl.ContentSHANorm); err != nil {
			return nil, fmt.Errorf("scan edit_removed_line: %w", err)
		}
		out = append(out, rl)
	}
	return out, rows.Err()
}

func (db *DB) linesForEdit(editID int64) ([]EditLine, error) {
	rows, err := db.Query(`SELECT start_line, end_line, COALESCE(content_sha, ''), COALESCE(content_sha_norm, '')
		FROM edit_lines WHERE edit_id = ? ORDER BY start_line`, editID)
	if err != nil {
		return nil, fmt.Errorf("query edit_lines: %w", err)
	}
	defer rows.Close()
	var out []EditLine
	for rows.Next() {
		var ln EditLine
		if err := rows.Scan(&ln.StartLine, &ln.EndLine, &ln.ContentSHA, &ln.ContentSHANorm); err != nil {
			return nil, fmt.Errorf("scan edit_line: %w", err)
		}
		out = append(out, ln)
	}
	return out, rows.Err()
}

// LatestChatModelNear returns the model string from the most recent row for
// the given tool + repoPath whose timestamp falls inside [ts-window, ts+window]
// AND that has a non-null model. Returns "" when no such row exists. Used by
// the ingest step to back-fill the model onto chat-attributed lines.
//
// The query also matches session markers with repo_path=” (emitted by the
// chat-session watcher, which has no file context) so that the chat-selected
// model is visible even when the edit row from the plugin/hook arrives before
// the watcher has had a chance to emit a repo-scoped row.
func (db *DB) LatestChatModelNear(tool Tool, repoPath string, tsNanos, windowNanos int64) string {
	row := db.QueryRow(`SELECT model FROM edits
		WHERE tool=? AND (repo_path=? OR repo_path='') AND model IS NOT NULL AND model != ''
		  AND ts >= ? AND ts <= ?
		ORDER BY ts DESC LIMIT 1`,
		string(tool), repoPath, tsNanos-windowNanos, tsNanos+windowNanos)
	var model sql.NullString
	if err := row.Scan(&model); err != nil {
		return ""
	}
	if !model.Valid {
		return ""
	}
	return model.String
}

// LatestChatGenTypeNear returns the gen_type to attribute a file change to when
// the given tool is active in the [ts-window, ts+window] interval.
//
// Resolution order:
//  1. If ANY chat marker exists in the window, return "chat". Chat is the
//     more specific signal — it requires either a chat-session JSONL
//     response chunk or an extension log line with "chat" in it, neither of
//     which fire for an inline Tab accept.
//  2. Otherwise return the gen_type of the most recent specific marker
//     (skipping "unknown" rows from the globalStorage-only signal).
//  3. Returns "" when no row matches; callers default accordingly
//     (the inline Tab accept is the common case when no more-specific signal
//     exists).
func (db *DB) LatestChatGenTypeNear(tool Tool, tsNanos, windowNanos int64) string {
	from, to := tsNanos-windowNanos, tsNanos+windowNanos
	// Step 1: chat-preferred.
	row := db.QueryRow(`SELECT 1 FROM edits
		WHERE tool=? AND gen_type='chat' AND ts >= ? AND ts <= ?
		LIMIT 1`, string(tool), from, to)
	var dummy int
	if err := row.Scan(&dummy); err == nil {
		return "chat"
	}
	// Step 2: latest specific marker.
	row = db.QueryRow(`SELECT gen_type FROM edits
		WHERE tool=? AND gen_type IS NOT NULL AND gen_type != '' AND gen_type != 'unknown'
		  AND ts >= ? AND ts <= ?
		ORDER BY ts DESC LIMIT 1`, string(tool), from, to)
	var gt sql.NullString
	if err := row.Scan(&gt); err != nil {
		return ""
	}
	if !gt.Valid {
		return ""
	}
	return gt.String
}

// UpgradeRecentCompletionsToChat re-stamps recent file-bearing completion edits
// of the given tool as gen_type=chat. It's called when a chat-session marker
// arrives — which often happens AFTER the editor plugin already recorded the
// apply as a completion (the chat response streams in a beat later), so the
// insert-time enrichment can't catch it. CONFIRMED inline accepts
// (confidence=high) are left alone: those genuinely are Tab completions.
func (db *DB) UpgradeRecentCompletionsToChat(tool Tool, tsNanos, windowNanos int64) error {
	// Exclude Cursor Tab-log completions: those come from Cursor's own inline
	// completion log and are authoritative Tab accepts, not chat applies, so a
	// nearby chat marker must not relabel them (repro: 7e1f1912).
	_, err := db.Exec(`
		UPDATE edits SET gen_type='chat'
		WHERE tool=? AND gen_type='completion' AND confidence != 'high'
		  AND file_path != '' AND ts >= ? AND ts <= ?
		  AND COALESCE(raw_meta,'') NOT LIKE '%cursor_tab_log%'`,
		string(tool), tsNanos-windowNanos, tsNanos+windowNanos)
	if err != nil {
		return fmt.Errorf("upgrade completions to chat: %w", err)
	}
	return nil
}

// KnownCommits returns all noted commits for the given repos, ordered by ts desc.
// If sinceNanos > 0, only commits with ts >= sinceNanos are returned.
func (db *DB) KnownCommits(repos []string, sinceNanos int64) ([]CommitRow, error) {
	if len(repos) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(repos))
	args := make([]any, len(repos))
	for i, r := range repos {
		placeholders[i] = "?"
		args[i] = r
	}
	cond := "repo_path IN (" + strings.Join(placeholders, ",") + ") AND note_written=1"
	if sinceNanos > 0 {
		cond += " AND ts >= ?"
		args = append(args, sinceNanos)
	}
	rows, err := db.Query("SELECT sha, repo_path, ts FROM commits WHERE "+cond+" ORDER BY ts DESC", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CommitRow
	for rows.Next() {
		var r CommitRow
		if err := rows.Scan(&r.SHA, &r.RepoPath, &r.TS); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type CommitRow struct {
	SHA      string
	RepoPath string
	TS       int64 // unix nanos
}

// SessionDurationNanos estimates how long the user worked on the current
// commit by finding the earliest edit in the window between the previous
// commit (in the same repo) and this commit, then returning
// commitNanos - min(ts).
//
// Coding time resets at every commit: edits older than the previous commit's
// timestamp do NOT contribute to this commit's session. If no prior commit
// exists for the repo, the window opens 8 hours before this commit. Returns
// 0 when no edits land inside the window.
func (db *DB) SessionDurationNanos(repoPath string, commitNanos int64) int64 {
	const maxLookback = int64(8 * 60 * 60 * 1e9) // 8h cap for the first-commit case

	// Floor the window at the previous commit's timestamp so each commit's
	// coding time covers only edits made AFTER the previous commit was
	// recorded. A rebase or out-of-order import that puts the previous
	// commit's ts after the current one is harmless — the SELECT below
	// ignores commits at or after commitNanos.
	var prevCommitTS int64
	_ = db.QueryRow(`
		SELECT COALESCE(MAX(ts), 0) FROM commits
		WHERE repo_path = ? AND ts < ?`,
		repoPath, commitNanos,
	).Scan(&prevCommitTS)

	lowerBound := commitNanos - maxLookback
	if prevCommitTS > lowerBound {
		lowerBound = prevCommitTS
	}

	// Strict `>` on the lower bound: we want edits made AFTER the previous
	// commit, not at the exact instant of it (which would belong to the
	// previous session by definition).
	var minTS int64
	_ = db.QueryRow(`
		SELECT COALESCE(MIN(ts), 0) FROM edits
		WHERE repo_path = ? AND ts <= ? AND ts > ? AND file_path != ''`,
		repoPath, commitNanos, lowerBound,
	).Scan(&minTS)

	if minTS == 0 {
		return 0
	}
	return commitNanos - minTS
}

// PreviousCommitTimestampNanos returns the timestamp of the most recent commit
// recorded in the DB before beforeNanos for the given repo. Returns 0 when no
// prior commit exists (i.e. this is the first commit blamely has seen for the
// repo). Used by buildNote to establish the lower bound for AI edit lookups so
// stale records from a previous coding session don't claim human-typed lines.
func (db *DB) PreviousCommitTimestampNanos(repoPath string, beforeNanos int64) int64 {
	var ts int64
	_ = db.QueryRow(
		`SELECT COALESCE(MAX(ts), 0) FROM commits WHERE repo_path = ? AND ts < ?`,
		repoPath, beforeNanos,
	).Scan(&ts)
	return ts
}

// TranscriptPathsForPeriod returns the distinct non-empty transcript_path
// values stored in raw_meta for edits in the given repo and time window.
// These are the Claude Code transcript files (.jsonl) associated with the
// edits that contributed to a commit.
func (db *DB) TranscriptPathsForPeriod(repoPath string, sinceNanos, untilNanos int64) ([]string, error) {
	rows, err := db.Query(`
		SELECT DISTINCT raw_meta FROM edits
		WHERE repo_path = ? AND ts >= ? AND ts <= ?
		  AND raw_meta LIKE '%transcript_path%'`,
		repoPath, sinceNanos, untilNanos)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	var out []string
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			continue
		}
		var m struct {
			TranscriptPath string `json:"transcript_path"`
		}
		if err := json.Unmarshal([]byte(raw), &m); err != nil || m.TranscriptPath == "" {
			continue
		}
		if !seen[m.TranscriptPath] {
			seen[m.TranscriptPath] = true
			out = append(out, m.TranscriptPath)
		}
	}
	return out, rows.Err()
}

// SessionTranscript pairs a session_id with its transcript file and owning tool,
// parsed from edit raw_meta. Used to persist user prompts keyed by session.
type SessionTranscript struct {
	SessionID      string
	TranscriptPath string
	Tool           string
}

// SessionTranscriptsForPeriod returns the distinct (session_id, transcript_path,
// tool) triples from edit raw_meta in the given repo and time window. Only rows
// that carry BOTH a session_id and a transcript_path are returned (the CLI /
// hook tools: Claude, Cursor, Codex). Deduped by session_id.
func (db *DB) SessionTranscriptsForPeriod(repoPath string, sinceNanos, untilNanos int64) ([]SessionTranscript, error) {
	rows, err := db.Query(`
		SELECT DISTINCT raw_meta FROM edits
		WHERE repo_path = ? AND ts >= ? AND ts <= ?
		  AND raw_meta LIKE '%session_id%' AND raw_meta LIKE '%transcript_path%'`,
		repoPath, sinceNanos, untilNanos)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	var out []SessionTranscript
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			continue
		}
		var m struct {
			SessionID      string `json:"session_id"`
			TranscriptPath string `json:"transcript_path"`
			Tool           string `json:"tool"`
		}
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			continue
		}
		if m.SessionID == "" || m.TranscriptPath == "" || seen[m.SessionID] {
			continue
		}
		seen[m.SessionID] = true
		out = append(out, SessionTranscript{SessionID: m.SessionID, TranscriptPath: m.TranscriptPath, Tool: m.Tool})
	}
	return out, rows.Err()
}

// UpsertUserPrompts stores the user prompts for a session, idempotently keyed by
// (session_id, seq). Re-running with the same session overwrites existing turns
// (and appends any new ones) rather than duplicating.
func (db *DB) UpsertUserPrompts(sessionID, repoPath, tool string, prompts []string, tsNanos int64) error {
	if sessionID == "" || len(prompts) == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.Prepare(`
		INSERT INTO prompts(session_id, repo_path, tool, seq, text, ts)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(session_id, seq) DO UPDATE SET
			text = excluded.text,
			repo_path = excluded.repo_path,
			tool = excluded.tool,
			ts = excluded.ts`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for seq, text := range prompts {
		if _, err := stmt.Exec(sessionID, repoPath, tool, seq, text, tsNanos); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UserPromptsForSession returns the stored user prompts for a session in turn order.
func (db *DB) UserPromptsForSession(sessionID string) ([]string, error) {
	rows, err := db.Query(
		`SELECT text FROM prompts WHERE session_id = ? ORDER BY seq ASC`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			continue
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ChatSessionRef pairs a chat-session JSONL path with the tool that owns it,
// as recorded in the raw_meta of a chat-session marker.
type ChatSessionRef struct {
	Path string
	Tool Tool
}

// ChatSessionPathsForPeriod returns the distinct chat_session_path values (with
// their owning tool) stored in raw_meta for edits in the time window. These are
// the VS Code / Cursor chat-session JSONL files the chat-session watcher tagged
// while a chat panel was producing responses.
//
// Chat-session response markers carry no file/repo context (the editor event
// isn't tied to a file), so the watcher records them with an empty repo_path.
// Matching repo_path=” rows unconditionally would leak a chat session into
// EVERY repo whose commit window overlaps it. Instead, an empty-repo_path row
// is only trusted for this repo if the SAME chat_session_path also appears on
// a textedit row that DOES carry this repo's path — i.e. that chat session
// actually produced edits here.
func (db *DB) ChatSessionPathsForPeriod(repoPath string, sinceNanos, untilNanos int64) ([]ChatSessionRef, error) {
	rows, err := db.Query(`
		SELECT repo_path, raw_meta FROM edits
		WHERE (repo_path = ? OR repo_path = '') AND ts >= ? AND ts <= ?
		  AND raw_meta LIKE '%chat_session_path%'`,
		repoPath, sinceNanos, untilNanos)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type rawRow struct {
		repoPath string
		path     string
		tool     string
	}
	var parsed []rawRow
	confirmed := map[string]bool{}
	for rows.Next() {
		var rp, raw string
		if err := rows.Scan(&rp, &raw); err != nil {
			continue
		}
		var m struct {
			ChatSessionPath string `json:"chat_session_path"`
			Tool            string `json:"tool"`
		}
		if err := json.Unmarshal([]byte(raw), &m); err != nil || m.ChatSessionPath == "" {
			continue
		}
		parsed = append(parsed, rawRow{repoPath: rp, path: m.ChatSessionPath, tool: m.Tool})
		if rp == repoPath {
			confirmed[m.ChatSessionPath] = true
		}
	}
	seen := map[string]bool{}
	var out []ChatSessionRef
	for _, r := range parsed {
		if r.repoPath != repoPath && !confirmed[r.path] {
			continue // unconfirmed cross-repo chat session — would leak
		}
		if !seen[r.path] {
			seen[r.path] = true
			out = append(out, ChatSessionRef{Path: r.path, Tool: Tool(r.tool)})
		}
	}
	return out, rows.Err()
}

// KnownRepoPaths returns all distinct repo_path values from the edits table.
// Used by the history report to know which repos to include by default.
func (db *DB) KnownRepoPaths() ([]string, error) {
	rows, err := db.Query(`SELECT DISTINCT repo_path FROM edits WHERE repo_path != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (db *DB) MarkCommitNoted(sha, repo string, tsNanos int64) error {
	_, err := db.Exec(`INSERT INTO commits(sha, repo_path, ts, note_written)
		VALUES (?, ?, ?, 1)
		ON CONFLICT(sha) DO UPDATE SET note_written = 1`,
		sha, repo, tsNanos)
	if err != nil {
		return fmt.Errorf("mark commit: %w", err)
	}
	return nil
}

// PluginEditRow is a lightweight row for the live-log poller.
type PluginEditRow struct {
	ID         int64
	Ts         int64 // nanoseconds
	Tool       string
	Confidence string
	GenType    string
	Model      string
	RepoPath   string
	FilePath   string
	RawMeta    string
	StartLine  int
	EndLine    int
}

// RecentPluginEdits returns rows from the edits table whose raw_meta source
// field matches one of the given source names and whose id > afterID.
// Used by the live-log poller to show HTTP-endpoint events (intellij_plugin,
// vscode_plugin) alongside watcher events in `blamely log copilot`.
func (db *DB) RecentPluginEdits(sources []string, afterID int64) ([]PluginEditRow, error) {
	if len(sources) == 0 {
		return nil, nil
	}
	placeholders := strings.Repeat("?,", len(sources))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, 0, len(sources)+1)
	for _, s := range sources {
		args = append(args, "%\"source\":\""+s+"%")
	}
	args = append(args, afterID)

	// Build a LIKE OR clause for each source pattern.
	like := ""
	for i := range sources {
		if i > 0 {
			like += " OR "
		}
		like += "raw_meta LIKE ?"
	}
	_ = placeholders

	rows, err := db.Query(fmt.Sprintf(`
		SELECT e.id, e.ts, e.tool, e.confidence, e.gen_type,
		       COALESCE(e.model,''), e.repo_path, e.file_path, COALESCE(e.raw_meta,''),
		       COALESCE(el.start_line,0), COALESCE(el.end_line,0)
		FROM edits e
		LEFT JOIN edit_lines el ON el.edit_id = e.id AND el.rowid = (
		    SELECT MIN(rowid) FROM edit_lines WHERE edit_id = e.id
		)
		WHERE (%s) AND e.id > ?
		ORDER BY e.id ASC`, like),
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PluginEditRow
	for rows.Next() {
		var r PluginEditRow
		if err := rows.Scan(&r.ID, &r.Ts, &r.Tool, &r.Confidence, &r.GenType,
			&r.Model, &r.RepoPath, &r.FilePath, &r.RawMeta,
			&r.StartLine, &r.EndLine); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RecentEditsByTool returns edits for the given tool with id > afterID,
// ordered oldest-first. Used by live-log tracers for hook-driven tools
// (e.g. gemini) whose events arrive solely via the HTTP /edit endpoint —
// there's no separate watcher stream to print them as they're recorded, so
// the tracer polls the table directly and shows rows as they land.
func (db *DB) RecentEditsByTool(tool Tool, afterID int64) ([]PluginEditRow, error) {
	rows, err := db.Query(`
		SELECT e.id, e.ts, e.tool, e.confidence, e.gen_type,
		       COALESCE(e.model,''), e.repo_path, e.file_path, COALESCE(e.raw_meta,''),
		       COALESCE(el.start_line,0), COALESCE(el.end_line,0)
		FROM edits e
		LEFT JOIN edit_lines el ON el.edit_id = e.id AND el.rowid = (
		    SELECT MIN(rowid) FROM edit_lines WHERE edit_id = e.id
		)
		WHERE e.tool = ? AND e.id > ?
		ORDER BY e.id ASC`,
		string(tool), afterID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PluginEditRow
	for rows.Next() {
		var r PluginEditRow
		if err := rows.Scan(&r.ID, &r.Ts, &r.Tool, &r.Confidence, &r.GenType,
			&r.Model, &r.RepoPath, &r.FilePath, &r.RawMeta,
			&r.StartLine, &r.EndLine); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func nullableString(s sql.NullString) any {
	if s.Valid {
		return s.String
	}
	return nil
}

func nullableInt(i sql.NullInt64) any {
	if i.Valid {
		return i.Int64
	}
	return nil
}

func nullableNullString(s sql.NullString) any {
	if s.Valid && s.String != "" {
		return s.String
	}
	return nil
}

// nullableNonEmpty stores "" as SQL NULL so legacy/unknown branches read back as
// NULL (the live gutter's `branch IS NULL` transition clause depends on this).
func nullableNonEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ResolveSession returns the UUID of the work session identified by
// (repoPath, branch, baseSha), creating it if absent. base_sha is the HEAD
// commit while uncommitted work accrues ("" when the repo has no commits).
func (db *DB) ResolveSession(repoPath, branch, baseSha string) (string, error) {
	var id string
	err := db.QueryRow(
		`SELECT id FROM sessions WHERE repo_path = ? AND branch = ? AND base_sha = ?`,
		repoPath, branch, baseSha,
	).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return "", fmt.Errorf("select session: %w", err)
	}
	id = uuid.New().String()
	if _, err := db.Exec(
		`INSERT OR IGNORE INTO sessions(id, repo_path, branch, base_sha) VALUES (?, ?, ?, ?)`,
		id, repoPath, branch, baseSha,
	); err != nil {
		return "", fmt.Errorf("insert session: %w", err)
	}
	if err := db.QueryRow(
		`SELECT id FROM sessions WHERE repo_path = ? AND branch = ? AND base_sha = ?`,
		repoPath, branch, baseSha,
	).Scan(&id); err != nil {
		return "", fmt.Errorf("select session after insert: %w", err)
	}
	return id, nil
}

// EditsForFileInSession returns edits touching repo/file that belong to the
// given session, newest-and-highest-confidence first (same ordering as
// EditsForFileSince). Used by commit-time attribution to scope by work session
// rather than a timestamp window.
func (db *DB) EditsForFileInSession(repo, file string, sessionID string) ([]Edit, error) {
	// Include session_id IS NULL so legacy rows (recorded before sessions
	// existed) and edits made while detached/unresolvable still attribute by
	// line within the current commit — preserving pre-sessions behavior during
	// the transition. New edits always carry a session_id and are scoped to it.
	return db.editsForFileWhere(
		`repo_path = ? AND file_path = ? AND (session_id = ? OR session_id IS NULL)`,
		repo, file, sessionID,
	)
}

// EditsForFileAny returns every recorded edit for repo/file regardless of
// session or timestamp. Used as the commit-time content_sha fallback: a
// cherry-picked or squashed commit gets a new SHA and committer timestamp, but
// the line content is unchanged, so matching by content_sha across all edits
// preserves AI authorship that a session/time-scoped query would miss.
func (db *DB) EditsForFileAny(repo, file string) ([]Edit, error) {
	return db.editsForFileWhere(`repo_path = ? AND file_path = ?`, repo, file)
}

// EditsForFileOnBranch returns every recorded edit for repo/file on the given
// branch (plus legacy rows with no branch recorded), regardless of session or
// timestamp. Used by commit-time DELETION attribution: an AI removal recorded
// in an earlier work-session is committed by the human in a later session, so
// the deletion must match removal records across sessions on the same branch —
// unlike added lines, which stay session-scoped to keep a human's paste of old
// AI code attributed Human.
func (db *DB) EditsForFileOnBranch(repo, file, branch string) ([]Edit, error) {
	return db.editsForFileWhere(
		`repo_path = ? AND file_path = ? AND (branch = ? OR branch IS NULL OR branch = '')`,
		repo, file, branch,
	)
}

// GetFileSnapshot returns the cached full content of repo/file as of the last
// recorded edit. ok=false if no snapshot has been cached yet (e.g. this file
// has never been edited while the daemon was running).
func (db *DB) GetFileSnapshot(repoPath, filePath string) (content string, ok bool, err error) {
	err = db.QueryRow(
		`SELECT content FROM file_snapshots WHERE repo_path = ? AND file_path = ?`,
		repoPath, filePath,
	).Scan(&content)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("query file_snapshots: %w", err)
	}
	return content, true, nil
}

// SetFileSnapshot caches repo/file's full content, used as the "before"
// baseline the next time this file is edited. Called after every recorded
// edit so the cache always reflects the file as of the last known edit.
func (db *DB) SetFileSnapshot(repoPath, filePath, content string, tsNanos int64) error {
	_, err := db.Exec(`
		INSERT INTO file_snapshots (repo_path, file_path, content, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(repo_path, file_path) DO UPDATE SET
			content = excluded.content, updated_at = excluded.updated_at`,
		repoPath, filePath, content, tsNanos,
	)
	if err != nil {
		return fmt.Errorf("upsert file_snapshots: %w", err)
	}
	return nil
}
