package gitnotes

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/blamely/blamely/internal/config"
	"github.com/blamely/blamely/internal/daemon"
	"github.com/blamely/blamely/internal/gitutil"
	"github.com/blamely/blamely/internal/store"
	"github.com/blamely/blamely/internal/tools"
)

const NotesRef = "refs/notes/blamely"

// Version is the blamely version stamped into a note's generated_by field. It
// defaults to "dev" and is overwritten at startup by cmd/blamely with the real
// resolved version, so a note records exactly which blamely wrote it.
var Version = "dev"

// ConvTurn is one user or assistant turn from the AI tool's conversation
// transcript that produced edits for this commit.
type ConvTurn struct {
	Role string `json:"role"` // "user" or "assistant"
	Text string `json:"text"`
}

// filterConversation applies the user's conversation config to the built turns.
// The master `Conversation` toggle gates everything; within it, user and
// assistant turns are kept independently so a team can, e.g., store the model's
// replies but omit the developer's prompts (or vice versa). Returns nil when
// nothing survives so the `conversation` field is omitted from the note.
func filterConversation(turns []ConvTurn, nc config.NoteConfig) []ConvTurn {
	if !nc.Conversation {
		return nil
	}
	if nc.ConversationUser && nc.ConversationAssistant {
		return turns
	}
	out := make([]ConvTurn, 0, len(turns))
	for _, t := range turns {
		switch strings.ToLower(t.Role) {
		case "user":
			if !nc.ConversationUser {
				continue
			}
		case "assistant":
			if !nc.ConversationAssistant {
				continue
			}
		}
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Note is the JSON payload written to refs/notes/blamely for each commit.
type Note struct {
	Schema int    `json:"schema"`
	Commit string `json:"commit"`
	// Branch is the currently-checked-out branch when the commit was made,
	// or "" if HEAD was detached. Useful when the same commit lands on
	// multiple branches (the note is attached to the commit, not the branch).
	Branch string `json:"branch,omitempty"`
	// BaseSHA is the work session's divergence point from the default branch
	// (merge-base), or "" on the trunk. Together with Branch it identifies the
	// session whose edits were scoped into this note.
	BaseSHA string `json:"base_sha,omitempty"`
	// Message is the full commit message (subject + body) as recorded by git.
	Message string `json:"message,omitempty"`
	// CodingTimeNanos is the wall-clock time from the earliest edit observed
	// for this repo (up to the commit) to the commit timestamp. Zero if no
	// edits were observed in the lookback window (default 8h, see
	// store.SessionDurationNanos).
	CodingTimeNanos int64           `json:"coding_time_nanos,omitempty"`
	Totals          Totals          `json:"totals"`
	ByTool          map[string]Tool `json:"by_tool"`
	ByGenType       ByGenType       `json:"by_gen_type"`
	Files           []FileEntry     `json:"files"`
	GeneratedBy     string          `json:"generated_by"`
	// Conversation holds the last few user/assistant turns from the
	// AI transcript that drove the edits in this commit. Populated only
	// when a transcript_path was captured in the edit's raw_meta.
	Conversation []ConvTurn `json:"conversation,omitempty"`
}

// ByGenType shows how many lines in the commit were produced by each
// generation method: a chat/panel session, a CLI command, an inline
// completion, or typed by the human author.
type ByGenType struct {
	Chat       int `json:"chat"`
	CLI        int `json:"cli"`
	Completion int `json:"completion"`
	// Human is the count of human-typed lines. Lives here (not in ByTool)
	// because humans aren't a tool.
	Human   int `json:"human"`
	Unknown int `json:"unknown,omitempty"`
}

type Totals struct {
	// Added lines, split by author. AddedLines is their sum (ai + human). The Go
	// field names stay AILines/HumanLines (used throughout the codebase) but they
	// serialize under the clearly-scoped JSON keys so a reader never has to guess
	// that a bare "lines" meant *added*.
	AddedLines int `json:"added_lines"`
	AILines    int `json:"ai_added_lines"`
	HumanLines int `json:"human_added_lines"`

	// Deleted lines, split by author. DeletedLines is the total; AIDeletedLines
	// is the subset whose removal matched an AI edit's recorded
	// edit_removed_lines hashes, and HumanDeletedLines is the rest.
	DeletedLines      int `json:"deleted_lines"`
	AIDeletedLines    int `json:"ai_deleted_lines"`
	HumanDeletedLines int `json:"human_deleted_lines"`

	Files  int     `json:"files"`
	Tokens *Tokens `json:"tokens,omitempty"`
	// Models is a per-model line-count rollup. Keys are concrete model
	// identifiers (e.g. "claude-opus-4-7", "gpt-4o"), not provider names.
	// Lines whose source AI didn't expose a model name aren't counted here.
	Models map[string]int `json:"models,omitempty"`
}

// UnmarshalJSON reads a Totals, accepting notes written by older blamely
// versions that used the pre-1.x keys ai_lines / human_lines (now
// ai_added_lines / human_added_lines) and deriving added_lines when it's absent,
// so `blamely report` on an old commit still shows the right numbers.
func (t *Totals) UnmarshalJSON(data []byte) error {
	type alias Totals
	aux := struct {
		*alias
		LegacyAI    *int `json:"ai_lines"`
		LegacyHuman *int `json:"human_lines"`
	}{alias: (*alias)(t)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if t.AILines == 0 && aux.LegacyAI != nil {
		t.AILines = *aux.LegacyAI
	}
	if t.HumanLines == 0 && aux.LegacyHuman != nil {
		t.HumanLines = *aux.LegacyHuman
	}
	if t.AddedLines == 0 {
		t.AddedLines = t.AILines + t.HumanLines
	}
	return nil
}

type Tool struct {
	// Lines is the tool's ADDED (authored) line count.
	Lines int `json:"lines"`
	// DeletedLines is the tool's removed-line count. A deletion-only commit
	// still records the tool here (with Lines == 0) so by_tool isn't empty when
	// an AI tool deleted code but authored nothing.
	DeletedLines int `json:"deleted_lines,omitempty"`
	// SuggestedLines and AcceptedLines are only set for AI tools — they
	// describe the model's original proposal vs. what the user kept. They
	// don't apply to human edits.
	SuggestedLines int64   `json:"suggested_lines,omitempty"`
	AcceptedLines  int     `json:"accepted_lines,omitempty"`
	Model          *string `json:"model,omitempty"`
	Tokens         *Tokens `json:"tokens,omitempty"`
}

type Tokens struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
}

type FileEntry struct {
	Path string `json:"path"`
	// Type is the file-level change kind: ADDED, DELETED, MODIFIED, RENAMED,
	// or COPIED. Surfaced so consumers can render per-file status (e.g. a
	// "+ added" / "- deleted" tag) without re-parsing the diff.
	Type string `json:"type,omitempty"`
	// RenamedFrom (resp. CopiedFrom) is set for RENAMED / COPIED files and
	// names the source pre-commit path.
	RenamedFrom string `json:"renamed_from,omitempty"`
	CopiedFrom  string `json:"copied_from,omitempty"`
	// Added/Deleted are the file's line-change totals, serialized under the
	// same key names the Totals block uses (added_lines / deleted_lines; the
	// pre-1.x keys were the bare added / deleted, still accepted on read).
	// The four *_lines splits mirror the Totals keys at file scope
	// (ai_added_lines + human_added_lines == added_lines, likewise for
	// deletes) and are filled by recomputeFileLineSplits from the settled
	// ranges after every flip/reconcile pass, so consumers get the split
	// without re-summing Lines.
	Added        int `json:"added_lines"`
	Deleted      int `json:"deleted_lines"`
	AIAdded      int `json:"ai_added_lines,omitempty"`
	HumanAdded   int `json:"human_added_lines,omitempty"`
	AIDeleted    int `json:"ai_deleted_lines,omitempty"`
	HumanDeleted int `json:"human_deleted_lines,omitempty"`
	// Lines holds the attribution as collapsed ranges (schema 2): consecutive
	// lines sharing the same (type, author_type, tool, model, gen_type) are
	// merged into one RangeEntry. Added/Deleted above stay line-accurate.
	Lines []RangeEntry `json:"lines,omitempty"`
	// acc is the per-line accumulation buffer used while building the note; it
	// is collapsed into Lines at flush time and never serialized.
	acc []LineEntry
}

// UnmarshalJSON reads a FileEntry, accepting notes written by older blamely
// versions that used the bare keys added / deleted (now added_lines /
// deleted_lines), so `blamely report` on an old commit still shows the right
// per-file counts.
func (f *FileEntry) UnmarshalJSON(data []byte) error {
	type alias FileEntry
	aux := struct {
		*alias
		LegacyAdded   *int `json:"added"`
		LegacyDeleted *int `json:"deleted"`
	}{alias: (*alias)(f)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if f.Added == 0 && aux.LegacyAdded != nil {
		f.Added = *aux.LegacyAdded
	}
	if f.Deleted == 0 && aux.LegacyDeleted != nil {
		f.Deleted = *aux.LegacyDeleted
	}
	return nil
}

// RangeEntry is a contiguous run of added or deleted lines that share the same
// attribution. For a single line Start == End. Replaces the schema-1 per-line
// LineEntry to keep notes small on large commits (one object per range, not
// per line). Readers of older notes should treat a bare `line` as start==end.
type RangeEntry struct {
	Start int    `json:"start"` // post-image for adds, pre-image for deletes
	End   int    `json:"end"`   // inclusive; == Start for a single line
	Type  string `json:"type"`  // "add" | "delete"
	// AuthorType is the binary AI-or-human classification. "AI" for any AI
	// tool (claude/cursor/codex/copilot/gemini), "Human" for typed-by-user,
	// pasted, or otherwise-unmatched lines (including most deletions: a
	// deletion is "AI" only when it matches a hash an AI tool recorded as
	// removed at edit time).
	AuthorType string `json:"author_type,omitempty"`
	// Tool is the attributed AI tool. Omitted for human-typed lines (humans
	// aren't a tool) and for deletions that couldn't be matched to an AI edit.
	Tool    string  `json:"tool,omitempty"`
	Model   *string `json:"model,omitempty"`
	GenType *string `json:"gen_type,omitempty"`
}

// NumLines returns the number of lines covered by the range (inclusive).
func (r RangeEntry) NumLines() int {
	if r.End < r.Start {
		return 0
	}
	return r.End - r.Start + 1
}

// LineEntry is the intermediate per-line attribution accumulated while building
// a file's entry, before collapsing into RangeEntry ranges. Internal only.
type LineEntry struct {
	Line       int
	Type       string
	AuthorType string
	Tool       string
	Model      *string
	GenType    *string
}

// collapseToRanges sorts per-line entries and merges consecutive lines that
// share the same attribution into ranges. Adds and deletes never merge (Type
// differs); a gap in line numbers always starts a new range.
func collapseToRanges(lines []LineEntry) []RangeEntry {
	if len(lines) == 0 {
		return nil
	}
	sort.SliceStable(lines, func(i, j int) bool {
		if lines[i].Line != lines[j].Line {
			return lines[i].Line < lines[j].Line
		}
		return lines[i].Type < lines[j].Type
	})
	var out []RangeEntry
	for _, le := range lines {
		if n := len(out); n > 0 {
			last := &out[n-1]
			if last.Type == le.Type &&
				last.AuthorType == le.AuthorType &&
				last.Tool == le.Tool &&
				equalStrPtr(last.Model, le.Model) &&
				equalStrPtr(last.GenType, le.GenType) &&
				le.Line == last.End+1 {
				last.End = le.Line
				continue
			}
		}
		out = append(out, RangeEntry{
			Start:      le.Line,
			End:        le.Line,
			Type:       le.Type,
			AuthorType: le.AuthorType,
			Tool:       le.Tool,
			Model:      le.Model,
			GenType:    le.GenType,
		})
	}
	return out
}

// backfillFromClaudeTranscript credits whole-file creates/deletes the AI made
// and committed in a single bash command. Such files attribute as Human because
// the commit (and this note) ran before the PostToolUse hook recorded the edit,
// and afterwards the working tree is clean — but the Claude transcript records
// the `cat > f` / `rm f` / Write op. We only touch ADDED files the AI wrote and
// DELETED files the AI removed (whole-file ops), so existing per-line matching
// for MODIFIED files is never overridden.
func backfillFromClaudeTranscript(db *store.DB, note *Note, repoRoot, repoID string, commitNanos int64) {
	if note == nil || len(note.Files) == 0 {
		return
	}
	since := db.PreviousCommitTimestampNanos(repoID, commitNanos)
	until := commitNanos + int64(5*time.Second)
	written, deleted := tools.ClaudeCommitFileOps(repoRoot, since, until)
	if len(written) == 0 && len(deleted) == 0 {
		return
	}
	chat := string(store.GenTypeChat)
	if note.ByTool == nil {
		note.ByTool = map[string]Tool{}
	}
	for i := range note.Files {
		f := &note.Files[i]
		switch f.Type {
		case "ADDED":
			if tools.MatchesFileOp(f.Path, written) {
				flipFileToAI(note, f, "add", "claude", &chat)
			}
		case "DELETED":
			if tools.MatchesFileOp(f.Path, deleted) {
				flipFileToAI(note, f, "delete", "claude", &chat)
			}
		case "MODIFIED":
			// A whole-file overwrite (`cat > f` / Write) the AI committed in one
			// command: it replaced the whole file, so every added AND removed
			// line is the AI's. (A partial Edit is NOT in `written`, so a
			// genuinely-mixed file still relies on per-line matching.)
			if tools.MatchesFileOp(f.Path, written) {
				flipFileToAI(note, f, "add", "claude", &chat)
				flipFileToAI(note, f, "delete", "claude", &chat)
			}
		}
	}
	note.Totals.AddedLines = note.Totals.AILines + note.Totals.HumanLines
}

// backfillFromCursorTranscript credits whole-file creates/deletes a Cursor agent
// made and committed in a single Shell command (e.g. `git rm f && git commit`).
// Those land here as Human because the commit — and this note — preceded (or ran
// entirely without) the PostToolUse Shell hook that would have recorded the edit,
// and afterwards the tree is clean. Cursor's agent transcript still records the
// Shell `rm`/`git rm`, `Delete`, or `Write` tool call. This is the Cursor analogue
// of backfillFromClaudeTranscript; we only touch ADDED files the AI wrote and
// DELETED files the AI removed, so per-line MODIFIED attribution is never overridden.
func backfillFromCursorTranscript(db *store.DB, note *Note, repoRoot, repoID string, commitNanos int64) {
	if note == nil || len(note.Files) == 0 {
		return
	}
	// Cursor transcript lines have no per-line timestamp, so CursorCommitFileOps
	// windows by transcript-file mtime: any session touched since the previous
	// commit is a candidate. The path-exact match against THIS commit's files below
	// keeps that safe.
	since := db.PreviousCommitTimestampNanos(repoID, commitNanos)
	written, deleted := tools.CursorCommitFileOps(repoRoot, since)
	if len(written) == 0 && len(deleted) == 0 {
		return
	}
	chat := string(store.GenTypeChat)
	if note.ByTool == nil {
		note.ByTool = map[string]Tool{}
	}
	for i := range note.Files {
		f := &note.Files[i]
		switch f.Type {
		case "ADDED":
			if tools.MatchesFileOp(f.Path, written) {
				flipFileToAI(note, f, "add", "cursor", &chat)
			}
		case "DELETED":
			if tools.MatchesFileOp(f.Path, deleted) {
				flipFileToAI(note, f, "delete", "cursor", &chat)
			}
		case "MODIFIED":
			// A whole-file overwrite (`cat > f` / Write) the agent committed in one
			// command: it replaced the whole file, so every added AND removed line is
			// the AI's. A partial edit is NOT in `written`, so mixed files still rely
			// on per-line matching.
			if tools.MatchesFileOp(f.Path, written) {
				flipFileToAI(note, f, "add", "cursor", &chat)
				flipFileToAI(note, f, "delete", "cursor", &chat)
			}
		}
	}
	note.Totals.AddedLines = note.Totals.AILines + note.Totals.HumanLines
	note.Totals.HumanDeletedLines = note.Totals.DeletedLines - note.Totals.AIDeletedLines
}

// backfillFromChatSessions credits whole-file deletions a Copilot or Cursor AGENT
// performed via an apply_patch `*** Delete File:` tool call. Such a deletion
// produces no edit (no textEditGroup to record), and the editor plugin only
// attributes a deletion to AI within a short window after the last AI edit — so an
// agent that removes a file minutes after its last edit falls to Human. The chat
// session JSONL still records the delete directive; we match it against this
// commit's DELETED files and flip them to the owning AI tool. Only DELETED files
// are touched, so per-line add/modify attribution is never overridden.
func backfillFromChatSessions(db *store.DB, note *Note, repoRoot, repoID string, commitNanos int64) {
	if note == nil || len(note.Files) == 0 {
		return
	}
	hasDeleted := false
	for i := range note.Files {
		if note.Files[i].Type == string(FileDeleted) || note.Files[i].Type == "DELETED" {
			hasDeleted = true
			break
		}
	}
	if !hasDeleted {
		return
	}
	// Discover candidate chat sessions over a WIDE lookback, not just since the
	// previous commit: a pure `*** Delete File:` leaves no edit row in the
	// [prevCommit, commit] window, so the session that performed it is only
	// findable via its EARLIER edits (the agent edited, the human committed, then
	// the agent deleted in a later turn that produced no edit). This is safe — the
	// flip below only applies to files DELETED in THIS commit whose exact path the
	// session named, so an unrelated old session can't claim a deletion.
	since := commitNanos - int64(2*time.Hour)
	until := commitNanos + int64(5*time.Second)
	refs, err := db.ChatSessionPathsForPeriod(repoID, since, until)
	if err != nil || len(refs) == 0 {
		return
	}
	chat := string(store.GenTypeChat)
	if note.ByTool == nil {
		note.ByTool = map[string]Tool{}
	}
	for _, ref := range refs {
		tool := string(ref.Tool)
		if tool == "" {
			continue
		}
		raw := tools.ChatSessionDeletedFiles(ref.Path)
		if len(raw) == 0 {
			continue
		}
		// Prefer repo-relative paths so MatchesFileOp can match exactly; absolute
		// paths still match by basename inside MatchesFileOp.
		targets := make([]string, 0, len(raw))
		for _, d := range raw {
			if rel, err := filepath.Rel(repoRoot, d); err == nil && !strings.HasPrefix(rel, "..") {
				targets = append(targets, filepath.ToSlash(rel))
			} else {
				targets = append(targets, d)
			}
		}
		for i := range note.Files {
			f := &note.Files[i]
			if f.Type != string(FileDeleted) && f.Type != "DELETED" {
				continue
			}
			if tools.MatchesFileOp(f.Path, targets) {
				flipFileToAI(note, f, "delete", tool, &chat)
			}
		}
	}
	note.Totals.HumanDeletedLines = note.Totals.DeletedLines - note.Totals.AIDeletedLines
}

// flipFileToAI reassigns a whole file's Human-attributed ranges of the given
// kind ("add"/"delete") to the given AI tool (chat gen_type) and adjusts the
// note's running totals.
func flipFileToAI(note *Note, f *FileEntry, kind, tool string, gen *string) {
	for j := range f.Lines {
		r := &f.Lines[j]
		if r.Type != kind || r.AuthorType == "AI" {
			continue
		}
		n := r.NumLines()
		r.AuthorType = "AI"
		r.Tool = tool
		r.GenType = gen
		note.ByGenType.Human -= n
		note.ByGenType.Chat += n
		t := note.ByTool[tool]
		if kind == "add" {
			t.Lines += n
			t.AcceptedLines += n
			t.SuggestedLines += int64(n)
			note.Totals.HumanLines -= n
			note.Totals.AILines += n
		} else {
			t.DeletedLines += n
			note.Totals.HumanDeletedLines -= n
			note.Totals.AIDeletedLines += n
		}
		note.ByTool[tool] = t
	}
}

// AttributeAndWrite is the main entry point invoked by the post-commit hook.
// It returns the freshly-written Note so callers (like cmd/blamely) can render
// a summary bar to stdout.
//
// repoPath comes from the hook as `git rev-parse --show-toplevel` (the
// WORKTREE root). We pass it to git for diff/notes operations. For DB
// lookups we use the canonical repo identity from `git rev-parse
// --git-common-dir`, so worktrees of the same repo share attribution.
func AttributeAndWrite(repoPath, sha string) (*Note, error) {
	return attributeAndWriteEx(repoPath, sha, nil)
}

// attributeAndWriteEx is AttributeAndWrite with an optional replay override.
// The post-rewrite hook passes an explicit {rebaseSquash, oldSHAs} context —
// the rebase markers are gone by then, so detection can't see the fold — which
// routes the squashed commit through the same source-note + wide reconciles a
// detected cherry-pick/squash-merge gets. When the override is set the
// in-progress skip is bypassed too (the rewrite has already finished; the
// caller explicitly wants a rebuild).
func attributeAndWriteEx(repoPath, sha string, replayOverride *replayCtx) (*Note, error) {
	if wt, ok := gitutil.Toplevel(repoPath); ok {
		repoPath = wt
	}

	// Attribution git-op robustness: make blamely notes follow history rewrites,
	// and skip recomputing while one is in progress so the inherited (correct) note
	// isn't clobbered by a v1 fallback. No-ops when attribution is off.
	ensureNotesFollowRewrites(repoPath)
	if replayOverride == nil && attributionShouldSkip(repoPath) {
		return nil, nil
	}

	repoID, _ := gitutil.RepoID(repoPath)
	if repoID == "" {
		repoID = repoPath
	}

	// Capture any note already on this SHA before we recompute. On a plain `git commit
	// --amend` git copies the pre-amend note here (notes.rewriteRef), but an amend isn't
	// an in-progress op so we don't skip — reconcileAddsFromInheritedNote below restores
	// the inherited AI attribution the recompute would otherwise clobber. nil on a fresh
	// commit (no note yet).
	inheritedNote := loadNoteForSeed(repoPath, sha)

	change, err := DiffCommit(repoPath, sha)
	if err != nil {
		return nil, fmt.Errorf("diff commit: %w", err)
	}
	// Attribution: a merge commit's note should credit only the conflict
	// resolution (lines added vs BOTH parents), not the merged-in branch's lines
	// (authored elsewhere). No-op for non-merges / when attribution is off.
	restrictMergeToResolution(repoPath, sha, change)
	commitNanos, err := CommitTimestampNanos(repoPath, sha)
	if err != nil {
		return nil, err
	}
	db, err := store.Open()
	if err != nil {
		return nil, err
	}
	defer db.Close()

	note, err := buildNote(db, repoID, sha, commitNanos, change.Added, change.Deleted, change.Renames, change.FileChanges)
	if err != nil {
		return nil, err
	}
	// Backfill whole-file creates/deletes the AI committed in one bash command
	// (e.g. `cat > f && git commit`, `rm f && git commit`). Those land here as
	// Human because the commit — and this note — preceded the PostToolUse hook
	// that would have recorded the edit; the AI's transcript still proves it.
	backfillFromClaudeTranscript(db, note, repoPath, repoID, commitNanos)
	// Same idea for a Cursor agent: a `git rm f && git commit` run through its Shell
	// tool commits with no surviving edit row, so the deletion would attribute to
	// Human. Cursor's agent transcript records the Shell/Delete/Write op; credit it.
	backfillFromCursorTranscript(db, note, repoPath, repoID, commitNanos)
	// Same idea for Copilot/Cursor agents: a `*** Delete File:` tool call in the
	// chat session removes a file with no edit record, so the deletion would
	// otherwise attribute to Human. Credit it to the owning AI tool.
	backfillFromChatSessions(db, note, repoPath, repoID, commitNanos)
	// Branch + commit message are commit-scoped metadata; capture from git
	// while we still have the working tree handy.
	note.Branch = BranchName(repoPath)
	note.Message = CommitMessage(repoPath, sha)
	// Last resort, and only when nothing was observed locally: credit the agent
	// named in a Co-Authored-By trailer. This is what rescues work done off this
	// machine — a Devin Cloud session edits a remote sandbox and arrives as a
	// finished branch, so no hook ever fired and every line would read as Human.
	// Runs after note.Message is populated, and after every observed-edit
	// backfill above, so it can never outrank real line-level evidence.
	backfillFromCommitTrailer(note, note.Message)
	// Coding time: earliest observed edit (within an 8h lookback) → commit
	// timestamp. Falls back to 0 if no edits were recorded for this repo.
	note.CodingTimeNanos = db.SessionDurationNanos(repoID, commitNanos)
	// CopiedFrom is set per-file from change.Copies (the rename half is
	// handled inside flushFile via the renames map).
	for i := range note.Files {
		if from, ok := change.Copies[note.Files[i].Path]; ok {
			note.Files[i].CopiedFrom = from
		}
	}
	// Attach conversation from the AI SESSIONS that produced this repo's edits in
	// this commit's window. Three scopes combine so a commit's note shows exactly
	// its own conversation and nothing else:
	//   1. repo_path — SessionTranscriptsForPeriod only returns sessions whose
	//      edits are in THIS repo, so working in many repos never cross-bleeds.
	//   2. session_id — sessions are keyed/deduped by AI session_id, so each
	//      distinct conversation that contributed code is included exactly once.
	//   3. time window (prevCommit, commit] — within each session, only the turns
	//      made after the previous commit, so a long session spanning several
	//      commits doesn't repeat its whole history on every commit.
	// transcript_path is written into raw_meta by the Claude hook as of blamely
	// 0.2; older edits silently produce no conversation. Must run BEFORE
	// json.Marshal/writeNote so it ends up in the persisted note.
	//
	// Guard on AILines: a commit with zero AI-attributed lines had no AI tool
	// involved, so any "matching" session/transcript found in the time window
	// is necessarily a different (possibly cross-repo) conversation that
	// happens to overlap — attaching it would mislabel a human-only commit.
	sinceConv := db.PreviousCommitTimestampNanos(repoID, commitNanos)
	const slackNanos = int64(5 * 1e9)
	const maxConvTurns = 12
	if note.Totals.AILines > 0 {
		if sessions, err := db.SessionTranscriptsForPeriod(repoID, sinceConv, commitNanos+slackNanos); err == nil {
			for _, s := range sessions {
				if len(note.Conversation) >= maxConvTurns {
					break
				}
				turns, err := tools.ReadTranscriptConversationWindow(s.TranscriptPath, maxConvTurns, 300, sinceConv, commitNanos+slackNanos)
				if err != nil {
					continue
				}
				for _, t := range turns {
					if len(note.Conversation) >= maxConvTurns {
						break
					}
					note.Conversation = append(note.Conversation, ConvTurn{Role: t.Role, Text: t.Text})
				}
			}
		}

		// Persist the user prompts of each session in this commit window into the
		// prompts table (keyed by session_id), so the conversation survives transcript
		// rotation/deletion and can be shown without re-reading the transcript. Best
		// effort: failures here never block note writing.
		if sessions, err := db.SessionTranscriptsForPeriod(repoID, sinceConv, commitNanos+slackNanos); err == nil {
			for _, s := range sessions {
				turns, err := tools.ReadTranscriptConversation(s.TranscriptPath, 200, 4000)
				if err != nil || len(turns) == 0 {
					continue
				}
				userPrompts := make([]string, 0, len(turns))
				for _, t := range turns {
					if t.Role == "user" && t.Text != "" {
						userPrompts = append(userPrompts, t.Text)
					}
				}
				if len(userPrompts) > 0 {
					_ = db.UpsertUserPrompts(s.SessionID, repoID, s.Tool, userPrompts, commitNanos)
				}
			}
		}

		// Copilot / Cursor chat panels persist a delta-encoded chat JSONL rather
		// than a Claude-style transcript. Pull its conversation (if no Claude
		// transcript already supplied one) and its per-session token usage, which
		// is the only place Copilot/Cursor token counts are observable locally.
		until := commitNanos + slackNanos
		if refs, err := db.ChatSessionPathsForPeriod(repoID, sinceConv, until); err == nil {
			for _, ref := range refs {
				if len(note.Conversation) == 0 {
					if turns, err := tools.ReadChatSessionConversation(ref.Path, 10, 300, sinceConv, until); err == nil {
						for _, t := range turns {
							note.Conversation = append(note.Conversation, ConvTurn{Role: t.Role, Text: t.Text})
						}
					}
				}
				if usage, err := tools.ReadChatSessionUsage(ref.Path, sinceConv, until); err == nil {
					applyChatUsage(note, ref.Tool, usage)
				}
			}
		}
	}

	// Apply the user's note-content config (~/.blamely/config.json). The note is
	// built in full above; here we strip whatever the user disabled (or hasn't
	// opted into), in one auditable place, just before it is persisted.
	// Conversation defaults off; everything else defaults on. Attribution
	// itself is never affected — only what gets written into the note.
	cfg := config.LoadConfig()
	if !cfg.Note.Message {
		note.Message = ""
	}
	if !cfg.Note.CodingTime {
		note.CodingTimeNanos = 0
	}
	note.Conversation = filterConversation(note.Conversation, cfg.Note)
	if !cfg.Note.FileLines {
		for i := range note.Files {
			note.Files[i].Lines = nil
		}
	}
	if !cfg.Note.Tokens {
		note.Totals.Tokens = nil
		for k, t := range note.ByTool {
			t.Tokens = nil
			note.ByTool[k] = t
		}
	}

	// Standardize by_tool so every AI tool reports the same shape. by_tool only
	// ever holds AI tools (human lines live in Totals.HumanLines), so a tool's
	// committed Lines ARE the lines the user kept — i.e. its accepted_lines.
	// Watchers don't set accepted_lines (only the human→AI flip path did), which
	// left it missing for codex/cursor/claude-desktop entries. Backfill it here,
	// in one place, so consumers see accepted_lines for every tool. suggested_lines
	// is deliberately left untouched: it's the model's REAL original proposal as
	// reported by each watcher (0 where the watcher can't measure it), and
	// inflating it to accepted would destroy the proposed-vs-kept signal.
	for k, t := range note.ByTool {
		if t.AcceptedLines == 0 && t.Lines > 0 {
			t.AcceptedLines = t.Lines
			note.ByTool[k] = t
		}
	}

	// Attribution: rewrite the note's added-line attribution FROM the working log
	// (diff-based truth, no hash guessing), then the deletions, GC, and migrate the
	// logs forward on commit.
	flipNoteToWorkingLog(repoPath, note)
	// Lossless cross-check: the working-log fold can't disambiguate AI-authored
	// added lines that duplicate existing content (chat edits carry placeholder
	// positions), so it drops them to Human. Scoped to this commit's added lines,
	// the recorded content_shas reattribute them deterministically. Human→AI only.
	// The window is per-file: it opens at the last commit that touched the file
	// (not the repo-wide previous commit), so AI edits that predate an unrelated
	// commit — the stash-pop→commit shape — remain eligible while content the
	// file already committed stays excluded.
	reconcileParent := commitParentSHA(repoPath, sha)
	reconcileAddsFromEditsWindowed(db, repoID, commitNanos, note, change.Added, func(relPath string) int64 {
		return lastFileTouchNanos(repoPath, reconcileParent, relPath)
	})
	// Amend recovery: restore AI added-line attribution from the note git inherited onto
	// this SHA when the working-log flip and edit reconcile couldn't (the original commit
	// deleted the working log and the edits are out of window). Human→AI only, no-op
	// without an inherited note.
	reconcileAddsFromInheritedNote(inheritedNote, note)
	// Replay recovery (cherry-pick / squash): this commit re-commits content that
	// was AUTHORED earlier, possibly on another branch — the working log and the
	// windowed reconcile can't see it. Carry the SOURCE commits' settled notes
	// over by content first (no guessing), then fall back to the unbounded edit
	// match, hard-gated to detected replays. Human→AI only, consume-once.
	replay := replayCtx{}
	if replayOverride != nil {
		replay = *replayOverride
	} else {
		replay = detectReplayOp(repoPath, sha)
	}
	if replay.Kind != replayNone {
		reconcileAddsFromSourceNotes(sourceNoteContentIndex(repoPath, replay.SourceSHAs), note, change.Added)
		reconcileAddsFromEditsWide(db, repoID, note, change.Added, replay)
	}
	flipDeletionsToWorkingLog(repoPath, note, change)
	// Lossless cross-check for deletions: the deletions log misses chat/cli tools
	// that removed lines (recorded only in SQLite edit_removed_lines), so reattribute
	// those Human deletions to the AI tool by removed-line content_sha. Human→AI only.
	reconcileDeletesFromEdits(db, repoID, commitNanos, note, change)
	// Fold deletions into the generation breakdown (the add-recompute above reset it
	// to additions only), so the Generation bars cover all changed lines.
	recomputeByGenType(note)
	// Per-file AI/Human added+deleted split, from the same settled ranges.
	recomputeFileLineSplits(note)
	// Token usage: the Human skeleton attaches no edit per line, so buildNote's
	// token aggregation never runs for AI tools (leaving tokens nil). Now that the
	// flip has settled which tools contributed, sum their recorded usage from the
	// in-window edits. Gated on the same config as the strip above.
	if cfg.Note.Tokens {
		applyEditTokens(db, repoID, commitNanos, note)
	}
	// Same story for suggested_lines: the flip records WHICH tool authored each
	// line but not the model's original proposal size, so the "kept X of Y"
	// denominator would stay 0 for every tool. Restore it from the in-window
	// edits now that the flip has settled which tools contributed.
	applyEditSuggested(db, repoID, commitNanos, note)
	gcWorkingLogsIfEnabled(repoPath)
	migrateWorkingLogsOnCommit(repoPath, note)

	body, err := json.Marshal(note)
	if err != nil {
		return nil, fmt.Errorf("marshal note: %w", err)
	}
	if err := writeNote(repoPath, sha, body); err != nil {
		return nil, err
	}
	if err := db.MarkCommitNoted(sha, repoID, commitNanos); err != nil {
		return nil, err
	}

	// HEAD advanced — invalidate cached session identity so post-commit edits
	// resolve to a new work session on this branch.
	daemon.InvalidateSessionCache(repoID)
	daemon.InvalidateSessionCache(repoPath)

	return note, nil
}

// applyChatUsage folds a chat session's summed token usage into the note for
// the owning tool. It only applies when that tool actually has attributed lines
// in this commit (a chat session that produced no committed code shouldn't add
// tokens). The chat JSONL exposes only prompt/completion counts, mapped to
// input/output; there is no cache-token split. Also backfills the tool's model
// from the session when the edit rows didn't carry one.
func applyChatUsage(note *Note, tool store.Tool, u *tools.TranscriptUsage) {
	if u == nil {
		return
	}
	key := string(tool)
	t, ok := note.ByTool[key]
	if !ok {
		return
	}
	if t.Tokens == nil {
		t.Tokens = &Tokens{}
	}
	t.Tokens.Input += u.InputTokens
	t.Tokens.Output += u.OutputTokens
	if t.Model == nil && u.Model != "" {
		m := u.Model
		t.Model = &m
	}
	note.ByTool[key] = t

	if note.Totals.Tokens == nil {
		note.Totals.Tokens = &Tokens{}
	}
	note.Totals.Tokens.Input += u.InputTokens
	note.Totals.Tokens.Output += u.OutputTokens
}

// applyEditTokens fills per-tool and total token usage from the SQLite edits in
// this commit's window. The v2 note builder lays a Human skeleton with no edit
// attached per line, so buildNote's resolved-loop token aggregation never runs for
// AI tools (tokens stay nil); this restores them after the flip determined which
// tools contributed. Only tools that actually authored committed lines are
// credited, and only edits that recorded usage count.
//
// Tokens are deduped PER TURN (tool + timestamp), not per edit row: Codex stamps
// the same turn-usage on every file it edited in a turn (identical ts), so a
// per-row sum would multiply by the file count. Claude/Copilot use distinct
// per-edit timestamps, so each genuine edit still counts once.
func applyEditTokens(db *store.DB, repoID string, commitNanos int64, note *Note) {
	if db == nil || note == nil {
		return
	}
	contributing := map[string]bool{}
	for name := range note.ByTool {
		if authorTypeFor(store.Tool(name)) == "AI" {
			contributing[name] = true
		}
	}
	if len(contributing) == 0 {
		return
	}
	sinceNanos := db.PreviousCommitTimestampNanos(repoID, commitNanos)
	const slack = int64(5 * 1e9)
	maxNanos := commitNanos + slack

	perTool := map[string]*Tokens{}
	var total Tokens
	seenTurn := map[string]bool{}
	any := false
	for fi := range note.Files {
		edits, err := db.EditsForFileSince(repoID, note.Files[fi].Path, sinceNanos)
		if err != nil {
			continue
		}
		for i := range edits {
			e := &edits[i]
			if e.TimestampNanos <= sinceNanos || e.TimestampNanos > maxNanos {
				continue
			}
			tool := string(e.Tool)
			if !contributing[tool] || !hasUsage(e) {
				continue
			}
			turnKey := fmt.Sprintf("%s:%d", tool, e.TimestampNanos)
			if seenTurn[turnKey] {
				continue
			}
			seenTurn[turnKey] = true
			t := perTool[tool]
			if t == nil {
				t = &Tokens{}
				perTool[tool] = t
			}
			in, out := nullInt64(e.InputTokens), nullInt64(e.OutputTokens)
			cr, cw := nullInt64(e.CacheReadTokens), nullInt64(e.CacheWriteTokens)
			t.Input += in
			t.Output += out
			t.CacheRead += cr
			t.CacheWrite += cw
			total.Input += in
			total.Output += out
			total.CacheRead += cr
			total.CacheWrite += cw
			any = true
		}
	}
	if !any {
		return
	}
	for tool, t := range perTool {
		bt := note.ByTool[tool]
		bt.Tokens = t
		note.ByTool[tool] = bt
	}
	tot := total
	note.Totals.Tokens = &tot
}

// applyEditSuggested backfills each AI tool's suggested_lines from its SQLite
// edits after the working-log flip set by_tool. The flip records WHICH tool
// authored each committed line but not the model's original proposal size, so
// suggested_lines — the denominator of the "kept X of Y" acceptance metric —
// would otherwise stay 0 for every tool (e.g. a Claude chat Edit records
// SuggestedLines at hook time, but it was lost between the dead SQLite matcher
// and the working-log flip). Mirrors applyEditTokens: only tools that authored
// committed lines are credited, and each edit row counts its suggestion once
// (deduped by edit ID) so an edit spanning several committed lines isn't summed
// per line.
func applyEditSuggested(db *store.DB, repoID string, commitNanos int64, note *Note) {
	if db == nil || note == nil {
		return
	}
	contributing := map[string]bool{}
	for name := range note.ByTool {
		if authorTypeFor(store.Tool(name)) == "AI" {
			contributing[name] = true
		}
	}
	if len(contributing) == 0 {
		return
	}
	sinceNanos := db.PreviousCommitTimestampNanos(repoID, commitNanos)
	const slack = int64(5 * 1e9)
	maxNanos := commitNanos + slack

	perTool := map[string]int64{}
	seenEdit := map[int64]bool{}
	for fi := range note.Files {
		edits, err := db.EditsForFileSince(repoID, note.Files[fi].Path, sinceNanos)
		if err != nil {
			continue
		}
		for i := range edits {
			e := &edits[i]
			if e.TimestampNanos <= sinceNanos || e.TimestampNanos > maxNanos {
				continue
			}
			tool := string(e.Tool)
			if !contributing[tool] || e.SuggestedLines <= 0 || seenEdit[e.ID] {
				continue
			}
			seenEdit[e.ID] = true
			perTool[tool] += e.SuggestedLines
		}
	}
	for tool, n := range perTool {
		bt := note.ByTool[tool]
		bt.SuggestedLines = n
		note.ByTool[tool] = bt
	}
}

// inheritBlankLineAttribution reassigns blank (empty/whitespace-only) lines to
// the author of the nearest non-blank line in the SAME file, preferring the
// line above. A blank line has no stable identity — its content_sha isn't
// unique, so once the file drifts it can't be re-located by content and falls
// through to Human even when the AI generated it as part of a block. Inheriting
// from the surrounding block keeps AI-generated blank lines AI, while blank
// lines the user added between human lines stay Human. Only blanks that
// defaulted to Human are touched, and only an AI neighbour promotes them — a
// human neighbour leaves the blank human. `resolved` must already be sorted by
// (file, line).
func inheritBlankLineAttribution(resolved []perLine) {
	for i := range resolved {
		if strings.TrimSpace(resolved[i].content) != "" {
			continue // non-blank lines keep their own (content-matched) attribution
		}
		if resolved[i].tool != "" || resolved[i].genType != store.GenTypeHuman {
			continue // a blank that already matched an AI edit — leave it
		}
		src := -1
		for j := i - 1; j >= 0 && resolved[j].file == resolved[i].file; j-- {
			if strings.TrimSpace(resolved[j].content) != "" {
				src = j
				break
			}
		}
		if src == -1 {
			for j := i + 1; j < len(resolved) && resolved[j].file == resolved[i].file; j++ {
				if strings.TrimSpace(resolved[j].content) != "" {
					src = j
					break
				}
			}
		}
		if src == -1 || resolved[src].tool == "" {
			continue // no neighbour, or the neighbour is human — keep blank human
		}
		resolved[i].tool = resolved[src].tool
		resolved[i].genType = resolved[src].genType
		resolved[i].model = resolved[src].model
	}
}

// coalesceDuplicateBlocks makes a copy-pasted block coherent. Per-line
// content matching splits a human's exact duplicate of an AI block jaggedly:
// some lines fall to Human (the AI made fewer copies than now exist) while
// others — lines that also repeat elsewhere in the file — stay AI, so the paste
// shows up as a mix at confusing line numbers. This pass finds a contiguous run
// of added lines that exactly duplicates an EARLIER run in the same file and,
// when the AI did NOT author that many copies (signalled by at least one Human
// line in either run), attributes the whole LATER run to Human (the paste) and
// the EARLIER run to AI (the original). It only fires on runs of >=
// dupBlockMinLines, so a single repeated line or a genuine AI-authored duplicate
// block (where the budget already kept both copies AI, leaving no Human line) is
// untouched. `resolved` must be sorted by (file, line).
const dupBlockMinLines = 3

func coalesceDuplicateBlocks(resolved []perLine) {
	for s := 0; s < len(resolved); {
		e := s
		for e < len(resolved) && resolved[e].file == resolved[s].file {
			e++
		}
		coalesceFileDuplicateBlocks(resolved[s:e])
		s = e
	}
}

func coalesceFileDuplicateBlocks(lines []perLine) {
	n := len(lines)
	posByContent := map[string][]int{} // earlier line indices per content
	for i := 0; i < n; {
		// Longest contiguous run starting at i that equals an earlier run.
		bestLen, bestK := 0, -1
		for _, k := range posByContent[lines[i].content] {
			l := 0
			for i+l < n && k+l < i && lines[i+l].content == lines[k+l].content {
				l++
			}
			if l > bestLen {
				bestLen, bestK = l, k
			}
		}
		if bestLen >= dupBlockMinLines {
			later := lines[i : i+bestLen]
			earlier := lines[bestK : bestK+bestLen]
			// Only act when the AI didn't author this many copies — i.e. the
			// budget left a Human line somewhere in one of the two runs.
			if blockHasHuman(later) || blockHasHuman(earlier) {
				if ai := firstAILine(earlier, later); ai != nil {
					for j := range earlier {
						earlier[j].tool, earlier[j].genType, earlier[j].model = ai.tool, ai.genType, ai.model
					}
				}
				for j := range later {
					later[j].tool, later[j].genType, later[j].model = "", store.GenTypeHuman, ""
				}
			}
			for j := i; j < i+bestLen; j++ {
				posByContent[lines[j].content] = append(posByContent[lines[j].content], j)
			}
			i += bestLen
			continue
		}
		posByContent[lines[i].content] = append(posByContent[lines[i].content], i)
		i++
	}
}

func blockHasHuman(block []perLine) bool {
	for i := range block {
		if isNonAITool(block[i].tool) {
			return true
		}
	}
	return false
}

// firstAILine returns the first AI-attributed line across the two runs, whose
// tool/model/gen_type is applied to the original block. nil if neither run has
// an AI line (then we leave attributions as-is rather than guess).
func firstAILine(a, b []perLine) *perLine {
	for i := range a {
		if !isNonAITool(a[i].tool) {
			return &a[i]
		}
	}
	for i := range b {
		if !isNonAITool(b[i].tool) {
			return &b[i]
		}
	}
	return nil
}

// perLine is the intermediate per-line attribution before collapsing into ranges.
type perLine struct {
	file    string
	line    int
	content string // raw line text from the diff (for ContentSHA fallback)
	tool    store.Tool
	model   string
	genType store.GenType
	edit    *store.Edit
}

// moveAttr carries the AI attribution of a line that was ADDED in this commit,
// used to credit a same-commit DELETION of identical content (a line move) to
// that tool. remaining is a consume-once budget: one AI add credits at most one
// moved deletion.
type moveAttr struct {
	tool      store.Tool
	model     string
	genType   store.GenType
	remaining int
}

func buildNote(db *store.DB, repoPath, sha string, commitNanos int64, added []AddedLine, deleted map[string][]DeletedLine, renames map[string]string, fileChanges map[string]FileChangeType) (*Note, error) {
	// baseSHA = the commit's parent (HEAD while the edits were made) — the key the
	// working-log flip reads.
	baseSHA := gitutil.ParentCommitSHA(repoPath, sha)

	// Intra-session lower bound: an edit already consumed by an earlier commit on
	// this branch must not re-claim a line in a later commit (sessions can span
	// many commits). Returns 0 for the first commit → includes all prior edits.
	// Both the primary AND content_sha-drift fallback are bounded below by this:
	// an AI edit from a PREVIOUS commit (this session or another) must not
	// re-attribute a line a human copy-pastes into a LATER commit.
	sinceNanos := db.PreviousCommitTimestampNanos(repoPath, commitNanos)

	// git commit timestamps are second-precision; edit timestamps are
	// nanosecond-precision. Allow up to 5s of post-commit slack so an
	// edit recorded in the same wall-clock second as the commit still
	// counts. (The post-commit hook fires immediately after the commit.)
	const sameSecondSlackNanos = int64(5 * 1e9)
	maxNanos := commitNanos + sameSecondSlackNanos

	// The legacy content_sha matcher fed off SQLite edits; with Attribution the
	// only engine it never runs. deletionEditsForFile is kept (returns nil) because
	// the deletion-skeleton path below still calls it; flipDeletionsToWorkingLog sets
	// the AI deletions.
	deletionEditsForFile := func(file string) []store.Edit { return nil }

	// Lay down a Human skeleton for every added line. flipNoteToWorkingLog
	// rewrites AI lines from the working log afterward and recomputes the added-line
	// totals; deletions default Human and flipDeletionsToWorkingLog sets their AI.
	resolved := make([]perLine, 0, len(added))
	for _, a := range added {
		resolved = append(resolved, perLine{file: a.File, line: a.LineNum, content: a.Content, tool: "", genType: store.GenTypeHuman})
	}
	sort.Slice(resolved, func(i, j int) bool {
		if resolved[i].file != resolved[j].file {
			return resolved[i].file < resolved[j].file
		}
		return resolved[i].line < resolved[j].line
	})

	inheritBlankLineAttribution(resolved)
	coalesceDuplicateBlocks(resolved)

	// Move detection: when the same content is ADDED by an AI tool and DELETED in
	// the SAME commit, git scores it as -old/+new but it is a line MOVE — the AI
	// that re-added the content also removed it from its old position. Credit such
	// deletions to that tool instead of Human. Keyed by exact line content with a
	// consume-once budget; blank/whitespace lines are excluded (too ambiguous —
	// they appear everywhere). attributeDeletedLine consults this below.
	movedAI := map[string]*moveAttr{}
	for i := range resolved {
		p := &resolved[i]
		if p.tool == "" || authorTypeFor(p.tool) != "AI" || strings.TrimSpace(p.content) == "" {
			continue
		}
		if m, ok := movedAI[p.content]; ok {
			m.remaining++
		} else {
			movedAI[p.content] = &moveAttr{tool: p.tool, model: p.model, genType: p.genType, remaining: 1}
		}
	}

	note := &Note{
		Schema:      2,
		Commit:      sha,
		BaseSHA:     baseSHA,
		ByTool:      map[string]Tool{},
		GeneratedBy: "blamely " + Version,
	}

	// Aggregate per-tool and build files/lines.
	type toolAgg struct {
		lines int
		// acceptedLines counts only lines whose source edit reported a suggestion
		// (SuggestedLines > 0 — i.e. inline completions). It's the "accepted"
		// half of the suggested/accepted acceptance metric, so it must NOT include
		// chat/cli lines (which are applied directly, never "suggested"): counting
		// those made accepted_lines exceed suggested_lines, which is impossible.
		acceptedLines int
		confidence    store.Confidence
		model         string
		tokens        Tokens
		hasTokens     bool
	}
	agg := map[store.Tool]*toolAgg{}
	seenEditTokens := map[int64]bool{}

	var totalTokens Tokens
	var hasTotalTokens bool

	var curFile string
	var curFileEntry *FileEntry
	var fileCount int

	// Helper: count added lines per file from the resolved list.
	addedPerFile := map[string]int{}
	for _, p := range resolved {
		addedPerFile[p.file]++
	}

	// suggestedPerTool sums each edit's SuggestedLines once per edit so the
	// same edit doesn't double-count when it touches multiple lines.
	suggestedPerTool := map[store.Tool]int64{}
	seenEditSuggested := map[int64]bool{}

	flushFile := func() {
		if curFileEntry == nil {
			return
		}
		curFileEntry.Added = addedPerFile[curFileEntry.Path]
		// Deleted line numbers: look up by post-commit path (direct match), then
		// by the pre-commit name for renames (e.g. `git mv old.go new.go`). The
		// diff parser already filters out modification deletes (paired -/+
		// inside the same hunk) so everything left here is a genuine deletion.
		var delLines []DeletedLine
		if d, ok := deleted[curFileEntry.Path]; ok {
			delLines = d
		} else if old, ok := renames[curFileEntry.Path]; ok {
			delLines = deleted[old]
		}
		if len(delLines) > 0 {
			delEdits := deletionEditsForFile(curFileEntry.Path)
			remSHA, remNorm := removedHashMultisets(delEdits)
			// Credit a whole-file AI Write that produced the committed content with
			// this file's deletions, even when it recorded no removed lines (its
			// record-time "before" snapshot was stale). See synthesizeWriteRemovals.
			synthesizeWriteRemovals(repoPath, sha, curFileEntry.Path, delEdits, delLines, remSHA, remNorm)
			curFileEntry.acc = append(curFileEntry.acc, attributeDeletedLines(delLines, delEdits, sinceNanos, maxNanos, remSHA, remNorm, &note.Totals, movedAI)...)
		}
		curFileEntry.Deleted = len(delLines)
		note.Totals.DeletedLines += curFileEntry.Deleted
		// File-level change kind from the diff parser.
		if kind, ok := fileChanges[curFileEntry.Path]; ok {
			curFileEntry.Type = string(kind)
		}
		if from, ok := renames[curFileEntry.Path]; ok {
			curFileEntry.RenamedFrom = from
		}
		// Collapse per-line accumulation (adds + deletes) into ranges.
		curFileEntry.Lines = collapseToRanges(curFileEntry.acc)
		note.Files = append(note.Files, *curFileEntry)
		fileCount++
	}

	for _, p := range resolved {
		if p.file != curFile {
			flushFile()
			curFile = p.file
			curFileEntry = &FileEntry{Path: p.file}
		}

		a, ok := agg[p.tool]
		if !ok {
			a = &toolAgg{confidence: confidenceFor(p.tool, p.edit)}
			agg[p.tool] = a
		}
		a.lines++
		if p.model != "" && a.model == "" {
			a.model = p.model
		}

		if p.edit != nil {
			editID := p.edit.ID
			if !seenEditTokens[editID] {
				seenEditTokens[editID] = true
				if hasUsage(p.edit) {
					t := Tokens{
						Input:      nullInt64(p.edit.InputTokens),
						Output:     nullInt64(p.edit.OutputTokens),
						CacheRead:  nullInt64(p.edit.CacheReadTokens),
						CacheWrite: nullInt64(p.edit.CacheWriteTokens),
					}
					a.tokens.Input += t.Input
					a.tokens.Output += t.Output
					a.tokens.CacheRead += t.CacheRead
					a.tokens.CacheWrite += t.CacheWrite
					a.hasTokens = true
					totalTokens.Input += t.Input
					totalTokens.Output += t.Output
					totalTokens.CacheRead += t.CacheRead
					totalTokens.CacheWrite += t.CacheWrite
					hasTotalTokens = true
				}
			}
			if !seenEditSuggested[editID] && p.edit.SuggestedLines > 0 {
				seenEditSuggested[editID] = true
				suggestedPerTool[p.tool] += p.edit.SuggestedLines
			}
			// "Accepted" counts only lines from a suggesting (completion) edit, so
			// it stays ≤ suggested_lines. Per edit, lines-in-commit ≤ its
			// SuggestedLines, so the summed accepted ≤ summed suggested.
			if p.edit.SuggestedLines > 0 {
				a.acceptedLines++
			}
		}

		entry := LineEntry{
			Line:       p.line,
			Type:       "add",
			AuthorType: authorTypeFor(p.tool),
			Tool:       string(p.tool),
			Model:      strPtr(p.model),
		}
		if gt := string(p.genType); gt != "" && gt != string(store.GenTypeUnknown) {
			entry.GenType = strPtr(gt)
		}
		curFileEntry.acc = append(curFileEntry.acc, entry)
	}
	flushFile()

	// Files with ONLY deletions (no added lines): the loop above iterates
	// `resolved`, which is built from added lines, so a pure-deletion file
	// would never get a FileEntry. Walk the deleted map and add entries for
	// any file we haven't already emitted. Pre-rename paths are skipped
	// because the post-rename path picks up their deletions via flushFile's
	// renames lookup.
	emitted := map[string]bool{}
	for _, f := range note.Files {
		emitted[f.Path] = true
	}
	renamedFrom := map[string]bool{}
	for _, from := range renames {
		renamedFrom[from] = true
	}
	for path, delLines := range deleted {
		if emitted[path] || renamedFrom[path] || len(delLines) == 0 {
			continue
		}
		// Surface the file-level kind so reports can render `[DELETED]`
		// next to the path. Fall back to "DELETED" when the diff parser
		// didn't put the file in fileChanges (defensive — diff sections
		// for removed files normally do).
		kind := fileChanges[path]
		if kind == "" {
			kind = FileDeleted
		}
		fe := FileEntry{Path: path, Type: string(kind), Deleted: len(delLines)}
		delEdits := deletionEditsForFile(path)
		remSHA, remNorm := removedHashMultisets(delEdits)
		// Deletion-only files (an AI Write that just removed lines, no additions)
		// reach this path instead of flushFile — apply the same Write-removal
		// synthesis so those deletions attribute to the AI, not Human.
		synthesizeWriteRemovals(repoPath, sha, path, delEdits, delLines, remSHA, remNorm)
		acc := attributeDeletedLines(delLines, delEdits, sinceNanos, maxNanos, remSHA, remNorm, &note.Totals, movedAI)
		fe.Lines = collapseToRanges(acc)
		note.Files = append(note.Files, fe)
		note.Totals.DeletedLines += fe.Deleted
		fileCount++
		emitted[path] = true
	}
	// Files in fileChanges that have neither additions nor deletions (pure
	// rename / pure copy). Without this pass the note would omit them
	// entirely and `git mv foo bar` would leave no trace.
	for path, kind := range fileChanges {
		if emitted[path] || renamedFrom[path] {
			continue
		}
		fe := FileEntry{Path: path, Type: string(kind)}
		if from, ok := renames[path]; ok {
			fe.RenamedFrom = from
		}
		note.Files = append(note.Files, fe)
		fileCount++
	}
	// Keep the files list stable for downstream readers.
	sort.SliceStable(note.Files, func(i, j int) bool {
		return note.Files[i].Path < note.Files[j].Path
	})

	// Populate by_tool and totals. Tools with zero attributed lines are omitted
	// unless they have a non-zero suggested-lines count (still useful signal).
	for tool, a := range agg {
		if a.lines == 0 && suggestedPerTool[tool] == 0 {
			continue
		}
		// AI/Human bar: empty tool = human-typed (always); copypaste lands
		// on the Human side because we can't prove the clipboard content
		// came from an AI.
		if isNonAITool(tool) {
			note.Totals.HumanLines += a.lines
		} else {
			note.Totals.AILines += a.lines
		}
		// by_tool lists AI tools (and copypaste, which is a distinct
		// not-typed-not-AI bucket). Humans aren't a tool, so we never emit
		// an entry for tool="" — those lines surface via
		// totals.human_lines and by_gen_type.human instead.
		if tool == "" {
			continue
		}
		t := Tool{
			Lines: a.lines,
			Model: strPtr(a.model),
		}
		// suggested/accepted are AI-only metrics. Clipboard-paste lines
		// have no model proposal to compare against. accepted counts only
		// completion lines (a.acceptedLines), not chat/cli, so it never exceeds
		// suggested_lines.
		if !isNonAITool(tool) {
			t.AcceptedLines = a.acceptedLines
			t.SuggestedLines = suggestedPerTool[tool]
		}
		if a.hasTokens {
			tk := a.tokens
			t.Tokens = &tk
		}
		note.ByTool[string(tool)] = t
	}

	// Fold per-tool DELETED line counts into by_tool from the resolved per-line
	// delete ranges. by_tool above is built only from ADDED lines, so without
	// this a deletion-only commit — or the deletion side of a mixed one — leaves
	// by_tool empty even though an AI tool removed the code (the gutter/leaderboard
	// would then show no contributor). Each AI delete range credits its tool's
	// DeletedLines and backfills the tool's model when it has none yet.
	for i := range note.Files {
		for _, r := range note.Files[i].Lines {
			if r.Type != "delete" || r.AuthorType != "AI" || r.Tool == "" {
				continue
			}
			tl := note.ByTool[r.Tool]
			tl.DeletedLines += r.NumLines()
			if tl.Model == nil && r.Model != nil && *r.Model != "" {
				tl.Model = r.Model
			}
			note.ByTool[r.Tool] = tl
		}
	}

	note.Totals.Files = fileCount
	if hasTotalTokens {
		tk := totalTokens
		note.Totals.Tokens = &tk
	}

	// Models rollup: per-model line counts derived from the resolved per-line
	// data. Keys are concrete model identifiers ("claude-opus-4-7", etc.),
	// not provider names. Lines whose source AI didn't expose a model aren't
	// counted here.
	for _, p := range resolved {
		if p.model == "" {
			continue
		}
		if note.Totals.Models == nil {
			note.Totals.Models = map[string]int{}
		}
		note.Totals.Models[p.model]++
	}

	// Populate by_gen_type from the resolved per-line data. Every resolved
	// line is counted exactly once. `copypaste` lines carry gen_type=human
	// (set as the per-line default and never overwritten for a paste), so
	// they roll up to by_gen_type.human — consistent with totals.human_lines,
	// which also counts them. This is NOT a double count with by_tool: by_tool
	// and by_gen_type are independent breakdowns of the same lines, and a
	// pasted line belongs in by_tool["copypaste"] AND by_gen_type.human, the
	// same way an AI line is in both by_tool[<tool>] and by_gen_type[<kind>].
	// Skipping copypaste here is what made by_gen_type undercount the human
	// share while totals stayed correct.
	for _, p := range resolved {
		bumpGenType(&note.ByGenType, p.genType)
	}
	// Deleted lines were already bucketed into by_gen_type per-line above
	// (attributeDeletedLine), using the matched AI edit's gen_type for
	// AI-attributed deletions and "human" otherwise.

	// Derive the totals from the working counters. AILines / HumanLines are
	// added-line counts; DeletedLines is the total, so the human share is
	// whatever wasn't matched to an AI deletion.
	note.Totals.AddedLines = note.Totals.AILines + note.Totals.HumanLines
	note.Totals.HumanDeletedLines = note.Totals.DeletedLines - note.Totals.AIDeletedLines

	return note, nil
}

// removedHashMultisets builds, per edit, "remaining" multisets of the
// content_sha / content_sha_norm hashes that edit recorded as removed
// (edit_removed_lines). pickEditForRemovedLine consumes (decrements) entries
// from these multisets as deleted lines in this commit are matched, so each
// recorded removal can attribute at most one deleted line — mirroring how
// claimedSHA prevents an added line's content_sha from over-attributing.
// committedLineSHAs returns the multiset of non-blank line content_shas for the
// file as it exists at sha, hashed the same way edits record added lines
// (TrimRight "\r", blank lines skipped). nil on any git error.
func committedLineSHAs(repoPath, sha, path string) map[string]int {
	out, err := exec.Command("git", "-C", repoPath, "show", sha+":"+path).Output()
	if err != nil {
		return nil
	}
	lines := strings.Split(string(out), "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	m := map[string]int{}
	for _, ln := range lines {
		text := strings.TrimRight(ln, "\r")
		if strings.TrimSpace(text) == "" {
			continue
		}
		m[sha256HexStr([]byte(text))]++
	}
	return m
}

// editContentSHAs is the multiset of non-blank content_shas an edit recorded as
// added lines.
func editContentSHAs(e *store.Edit) map[string]int {
	m := map[string]int{}
	for _, l := range e.Lines {
		if l.ContentSHA != "" {
			m[l.ContentSHA]++
		}
	}
	return m
}

func sameSHAMultiset(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// synthesizeWriteRemovals credits a whole-file AI Write with this file's
// committed deletions when it recorded none of its own. A Write overwrites the
// whole file, so removed lines are detected by diffing the new content against a
// cached "before" snapshot at record time — but that snapshot can be stale (an
// intermediate write already advanced it), so a Write that truly removed lines
// may have recorded zero. When an AI edit's recorded content EXACTLY equals the
// committed file, that edit definitively produced the commit, so the commit's
// deletions (already known from the git diff) are its removals. We add them to
// the edit's removed-line budget; the consume-once matcher then attributes each
// deleted line to that tool instead of letting it fall to Human.
//
// The exact full-content match is the safety gate: a narrowed/partial edit, or a
// human edit made after the Write, won't match, so we never over-credit.
func synthesizeWriteRemovals(repoPath, sha, path string, delEdits []store.Edit, delLines []DeletedLine, remSHA, remNorm map[int64]map[string]int) {
	if len(delLines) == 0 {
		return
	}
	committed := committedLineSHAs(repoPath, sha, path)
	if len(committed) == 0 {
		return
	}
	for i := range delEdits {
		e := &delEdits[i]
		if isNonAITool(e.Tool) || len(e.RemovedLines) > 0 || len(e.Lines) == 0 {
			continue
		}
		if !sameSHAMultiset(editContentSHAs(e), committed) {
			continue
		}
		if remSHA[e.ID] == nil {
			remSHA[e.ID] = map[string]int{}
		}
		if remNorm[e.ID] == nil {
			remNorm[e.ID] = map[string]int{}
		}
		for _, d := range delLines {
			if strings.TrimSpace(d.Content) == "" {
				continue
			}
			remSHA[e.ID][sha256HexStr([]byte(d.Content))]++
			remNorm[e.ID][sha256HexNormStr(d.Content)]++
		}
	}
}

func removedHashMultisets(edits []store.Edit) (remainingSHA, remainingNorm map[int64]map[string]int) {
	remainingSHA = map[int64]map[string]int{}
	remainingNorm = map[int64]map[string]int{}
	for i := range edits {
		e := &edits[i]
		if len(e.RemovedLines) == 0 {
			continue
		}
		shaSet := map[string]int{}
		normSet := map[string]int{}
		for _, r := range e.RemovedLines {
			if r.ContentSHA != "" {
				shaSet[r.ContentSHA]++
			}
			if r.ContentSHANorm != "" {
				normSet[r.ContentSHANorm]++
			}
		}
		remainingSHA[e.ID] = shaSet
		remainingNorm[e.ID] = normSet
	}
	return remainingSHA, remainingNorm
}

// pickEditForRemovedLine returns the edit (confidence/recency-ordered, like
// pickEdit) recorded within (minNanos, maxNanos] whose edit_removed_lines
// include a hash matching content — exact content_sha first, then the
// whitespace-normalized content_sha_norm. A match consumes (decrements) the
// corresponding entry in remainingSHA/remainingNorm so it can't also
// attribute another deleted line. Returns nil for blank/whitespace-only
// content (never recorded at edit time, see tools.RemovedLineHashes) or when
// no edit's removed-lines include this hash, leaving the line "Human".
func pickEditForRemovedLine(edits []store.Edit, content string, minNanos, maxNanos int64, remainingSHA, remainingNorm map[int64]map[string]int) *store.Edit {
	if strings.TrimSpace(content) == "" {
		return nil
	}
	want := sha256HexStr([]byte(content))
	for i := range edits {
		e := &edits[i]
		if e.TimestampNanos <= minNanos || e.TimestampNanos > maxNanos {
			continue
		}
		if set := remainingSHA[e.ID]; set[want] > 0 {
			set[want]--
			return e
		}
	}
	wantNorm := sha256HexNormStr(content)
	if wantNorm == "" {
		return nil
	}
	for i := range edits {
		e := &edits[i]
		if e.TimestampNanos <= minNanos || e.TimestampNanos > maxNanos {
			continue
		}
		if set := remainingNorm[e.ID]; set[wantNorm] > 0 {
			set[wantNorm]--
			return e
		}
	}
	return nil
}

// attributeDeletedLine builds the LineEntry for one removed line, matching it
// against sessionEdits' recorded edit_removed_lines hashes via
// pickEditForRemovedLine. On a match to an AI tool's edit, it sets
// AuthorType/Tool/Model/GenType (mirroring an added line's attribution) and
// increments totals.AIDeletedLines; otherwise the line stays
// AuthorType:"Human" with no Tool/Model/GenType set, identical to the
// pre-AI-deletion-attribution output. The resolved gen_type (the matched
// edit's, or "human") is returned alongside so the caller can bump
// by_gen_type after inheritBlankDeletedLineAttribution has had a chance to
// reassign blank lines.
func attributeDeletedLine(d DeletedLine, sessionEdits []store.Edit, sinceNanos, maxNanos int64, remainingSHA, remainingNorm map[int64]map[string]int, totals *Totals, movedAI map[string]*moveAttr) (LineEntry, store.GenType) {
	le := LineEntry{Line: d.LineNum, Type: "delete", AuthorType: "Human"}
	gt := store.GenTypeHuman
	matched := false
	if e := pickEditForRemovedLine(sessionEdits, d.Content, sinceNanos, maxNanos, remainingSHA, remainingNorm); e != nil {
		if at := authorTypeFor(e.Tool); at == "AI" {
			le.AuthorType = at
			le.Tool = string(e.Tool)
			if e.Model.Valid {
				le.Model = strPtr(e.Model.String)
			}
			gt = e.GenType
			if gtStr := string(gt); gtStr != "" && gtStr != string(store.GenTypeUnknown) {
				le.GenType = strPtr(gtStr)
			}
			totals.AIDeletedLines++
			matched = true
		}
	}
	// Move detection: the deleted content was re-added by an AI tool in this same
	// commit (a line move/reorder), so the AI caused this deletion too. Only when
	// the line wasn't already matched to a recorded AI removal above.
	if !matched && strings.TrimSpace(d.Content) != "" {
		if m, ok := movedAI[d.Content]; ok && m.remaining > 0 {
			m.remaining--
			le.AuthorType = "AI"
			le.Tool = string(m.tool)
			if m.model != "" {
				le.Model = strPtr(m.model)
			}
			gt = m.genType
			if gtStr := string(gt); gtStr != "" && gtStr != string(store.GenTypeUnknown) {
				le.GenType = strPtr(gtStr)
			}
			totals.AIDeletedLines++
		}
	}
	return le, gt
}

// attributeDeletedLines builds the LineEntry for every line in delLines (see
// attributeDeletedLine), runs inheritBlankDeletedLineAttribution so blank
// deleted lines pick up an adjacent AI-attributed deletion's attribution, and
// finally bumps by_gen_type for each line's (possibly reassigned) gen_type.
// attributeDeletedLines attributes each deleted line to its tool/gen_type and folds
// the per-line deletion totals. It deliberately does NOT touch by_gen_type: deletions
// are folded into the generation breakdown exactly once, by recomputeByGenType as
// the final step. Counting them here too double-counts pure-deletion commits, because
// recomputeAddedAggregates (which would have reset by_gen_type) only runs when the
// commit has added lines — a delete-only commit leaves this fold in place, then the
// final fold adds it again (observed as 24 deleted lines for a 12-line deletion).
func attributeDeletedLines(delLines []DeletedLine, sessionEdits []store.Edit, sinceNanos, maxNanos int64, remainingSHA, remainingNorm map[int64]map[string]int, totals *Totals, movedAI map[string]*moveAttr) []LineEntry {
	entries := make([]LineEntry, len(delLines))
	gts := make([]store.GenType, len(delLines))
	for i, d := range delLines {
		entries[i], gts[i] = attributeDeletedLine(d, sessionEdits, sinceNanos, maxNanos, remainingSHA, remainingNorm, totals, movedAI)
	}
	inheritBlankDeletedLineAttribution(delLines, entries, gts, totals)
	return entries
}

// inheritBlankDeletedLineAttribution gives a blank deleted line the
// AuthorType/Tool/Model/GenType of an adjacent non-blank deleted line from the
// same removal, when that neighbour was AI-attributed. tools.RemovedLineHashes
// never records a content_sha for blank/whitespace-only lines, so
// pickEditForRemovedLine can never match one directly and
// attributeDeletedLine leaves it "Human" by default — even when an AI deleted
// the whole surrounding block, blank lines included. Mirrors
// inheritBlankLineAttribution, which does the same for added lines: look
// backward for the nearest non-blank neighbour, falling back to forward only
// if there is no non-blank neighbour at all; a non-AI neighbour leaves the
// blank line "Human".
func inheritBlankDeletedLineAttribution(delLines []DeletedLine, entries []LineEntry, gts []store.GenType, totals *Totals) {
	for i := range entries {
		if strings.TrimSpace(delLines[i].Content) != "" {
			continue // non-blank lines keep their own (content-matched) attribution
		}
		if entries[i].AuthorType == "AI" {
			continue // a blank that already matched an AI edit — leave it
		}
		src := -1
		for j := i - 1; j >= 0; j-- {
			if strings.TrimSpace(delLines[j].Content) != "" {
				src = j
				break
			}
		}
		if src == -1 {
			for j := i + 1; j < len(entries); j++ {
				if strings.TrimSpace(delLines[j].Content) != "" {
					src = j
					break
				}
			}
		}
		if src == -1 || entries[src].AuthorType != "AI" {
			continue // no neighbour, or the neighbour is human — keep blank human
		}
		entries[i].AuthorType = entries[src].AuthorType
		entries[i].Tool = entries[src].Tool
		entries[i].Model = entries[src].Model
		entries[i].GenType = entries[src].GenType
		gts[i] = gts[src]
		totals.AIDeletedLines++
	}
}

// bumpGenType increments the by_gen_type bucket matching gt. Shared by added
// lines (resolved) and deleted lines (attributeDeletedLine) so an
// AI-attributed deletion counts toward that edit's gen_type (chat/cli) rather
// than always landing in "human".
func bumpGenType(by *ByGenType, gt store.GenType) {
	switch gt {
	case store.GenTypeChat:
		by.Chat++
	case store.GenTypeCLI:
		by.CLI++
	case store.GenTypeCompletion:
		by.Completion++
	case store.GenTypeHuman:
		by.Human++
	default:
		by.Unknown++
	}
}

// pickEdit returns the first edit (the slice is confidence-then-recency ordered)
// recorded within (minNanos, maxNanos] that claims the given line. A content_sha
// match always counts; a line-number range match counts only when allowLineMatch
// is true.
//
// The primary (session-scoped) call passes minNanos = previous-commit timestamp
// so an edit already consumed by an earlier commit on this branch can't re-claim
// a line in a later commit. The cross-session fallback passes minNanos = 0 and
// allowLineMatch = false: it relies on content alone (line numbers are unreliable
// across cherry-pick/squash) and must NOT be time-bounded, because the original
// AI edit predates the rewritten commit's timestamp.
//
// useNorm switches the content match from the exact content_sha to the
// whitespace-normalized content_sha_norm — the autoformatter-drift fallback.
// When useNorm is true, coversLine is never consulted: position-only matches
// (edits without a content_sha at all) are already covered by the non-norm
// primary call, so re-checking them here would just duplicate that result.

// pickDriftEdit chooses the AI edit for a line whose content matches a recorded
// AI line but at a DIFFERENT position (drift). Unlike pickEdit it ignores line
// numbers (drift moved them) and, crucially, respects each edit's recorded
// occurrence count: among the AI edits that recorded this content, it returns the
// newest one that still has an unconsumed occurrence (decrementing it), so when
// several edits authored identical content the committed lines are distributed by
// recorded count instead of all going to the newest edit. Falls back to the
// newest match when every edit's budget is spent (so a line is still attributed
// rather than dropped). rec/used are keyed by edit ID then content hash.

// coversLine reports whether any edit line covers post-image line n BY POSITION.
// Lines that carry a ContentSHA are skipped here on purpose: their content is
// the authoritative signal, so they must match via coversContentSHA, not by
// line number. Matching a content_sha line by position would re-introduce the
// drift bug — an inserted line landing inside the AI's original range would be
// mislabelled AI, and the AI line pushed out of the range mislabelled human.
// Only ranges WITHOUT a content_sha (inline completions, log/velocity edits)
// are matched positionally. Mirrors the editor gutter's resolve logic.
func coversLine(lines []store.EditLine, n int) bool {
	for _, l := range lines {
		if l.ContentSHA != "" {
			continue
		}
		if n >= l.StartLine && n <= l.EndLine {
			return true
		}
	}
	return false
}

// coversContentSHA returns true when any edit line's stored ContentSHA matches
// the SHA-256 of the committed line's content. When ln > 0, the matching edit
// line must also have StartLine == ln, so a human copy-paste of AI-generated
// content at a different position is not mis-attributed. Pass ln = 0 for the
// cross-session fallback (cherry-pick/squash) where line numbers may differ.

// coversContentSHANorm is coversContentSHA's fallback for the
// whitespace-normalized hash: it matches a line whose exact bytes changed
// (e.g. an autoformatter reindented an AI-written line, changed its trailing
// commas, or adjusted spacing) but whose collapsed-whitespace text still
// matches the AI-recorded line. Same ln=0 cross-session/drift convention as
// coversContentSHA.

// editIsNarrowed reports whether an edit was recorded as a NARROWED delta
// (it captured only the lines that differ from a pre-chat snapshot, rather than
// the whole applied text). The recorder stamps `"narrowed":true` into raw_meta
// (see tools.recordTextEditGroup). Such edits record distinct new lines, so the
// drift budget sums them across edits instead of taking the max. A missing or
// false flag (every non-chat tool, and chat applies with no fresh snapshot)
// means a full recording, which is max'd.

func confidenceFor(tool store.Tool, e *store.Edit) store.Confidence {
	if e != nil {
		return e.Confidence
	}
	return defaultConf(tool)
}

func defaultConf(t store.Tool) store.Confidence {
	if t == store.ToolCopilot {
		return store.ConfidenceLow
	}
	return store.ConfidenceHigh
}

func hasUsage(e *store.Edit) bool {
	return e.InputTokens.Valid || e.OutputTokens.Valid ||
		e.CacheReadTokens.Valid || e.CacheWriteTokens.Valid
}

func nullInt64(n sql.NullInt64) int64 {
	if n.Valid {
		return n.Int64
	}
	return 0
}

func writeNote(repoPath, sha string, body []byte) error {
	cmd := exec.Command("git", "-C", repoPath, "notes", "--ref="+NotesRef, "add", "-f", "-F", "-", sha)
	cmd.Stdin = strings.NewReader(string(body))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git notes add: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// authorTypeFor maps an internal tool identifier to the binary author bucket
// rendered in the note's per-line entries. Empty tool means human-typed
// (the canonical form for human edits). Legacy ToolHuman maps to Human too
// so commits made before the human/tool split still render correctly.
// `copypaste` rolls up to "Human" because we can't prove the clipboard
// source was an AI; the distinct tool name in by_tool keeps the nuance.
func authorTypeFor(t store.Tool) string {
	switch t {
	case "", store.ToolHuman, store.ToolCopyPaste:
		return "Human"
	default:
		return "AI"
	}
}

// isNonAITool returns true for tool identifiers that should NOT count toward
// the AI side of the binary AI-vs-Human bar (and shouldn't get
// suggested/accepted-line metrics). Currently: human-typed lines (tool="")
// — plus the legacy ToolHuman value — and clipboard-paste lines of unknown
// origin.
func isNonAITool(t store.Tool) bool {
	return t == "" || t == store.ToolHuman || t == store.ToolCopyPaste
}
func equalStrPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func sha256HexStr(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// sha256HexNormStr is sha256HexStr over the whitespace-normalized line text
// (see tools.NormalizeLineText), returning "" for blank/whitespace-only
// content — mirroring content_sha_norm's record-time convention so blank
// lines never spuriously match each other.
func sha256HexNormStr(content string) string {
	norm := tools.NormalizeLineText(content)
	if norm == "" {
		return ""
	}
	return sha256HexStr([]byte(norm))
}
