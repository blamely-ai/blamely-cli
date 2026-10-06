package daemon

import (
	"encoding/json"
	"log"
	"path"
	"strings"

	"github.com/blamely/blamely/internal/store"
)

// copilotDupWindowNanos bounds how far apart two recorders' reports of the same
// Copilot edit can land. The hook fires right after the tool runs; the transcript
// watcher polls every few seconds; VS Code flushes chatSessions lazily, sometimes
// minutes later.
const copilotDupWindowNanos = int64(5 * 60 * 1e9)

// copilotSources are the raw_meta "source" tags of the recorders that can each
// report the SAME Copilot chat edit in VS Code: the agent hook, the extension's
// transcript stream, and VS Code's chatSessions store.
var copilotSources = map[string]bool{
	"copilot_hook":          true,
	"copilot_transcript":    true,
	"copilot_chat_session":  true,
	"copilot_chat_textedit": true,
}

// mergeCopilotCrossSourceDuplicate reports whether e is a second report of a
// Copilot edit another recorder already stored — same repo and file, within
// copilotDupWindowNanos, from a DIFFERENT source, in the SAME VS Code chat
// session, with the same net added and removed lines. If so it folds e's
// model/tokens into the stored row and the caller skips the insert.
//
// The session check is what keeps two real edits apart: file, line hashes and
// time alone matched a Copilot CLI edit and a VS Code Chat edit that happened to
// add the same line, and stored one row for both. See copilotSessionKey.
//
// Storing both would credit the lines correctly but count the turn's tokens
// twice (they are deduped per tool+timestamp, and the two rows differ in time)
// and double each line's consume-once match budget, so a human copy of an AI line
// could be claimed by the spare record. Reports from the same source are never
// merged: two identical edits from one recorder are two real edits.
func mergeCopilotCrossSourceDuplicate(db *store.DB, e *store.Edit) bool {
	if db == nil || e.Tool != store.ToolCopilot || (len(e.Lines) == 0 && len(e.RemovedLines) == 0) {
		return false
	}
	src := editSource(e)
	if !copilotSources[src] {
		return false
	}
	session := copilotSessionKey(e)
	if session == "" {
		return false // not provably a VS Code chat edit: never merge
	}
	cands, err := db.EditsForFileSince(e.RepoPath, e.FilePath, e.TimestampNanos-copilotDupWindowNanos)
	if err != nil {
		return false
	}
	for i := range cands {
		c := &cands[i]
		if c.Tool != store.ToolCopilot || c.TimestampNanos > e.TimestampNanos+copilotDupWindowNanos {
			continue
		}
		if cs := editSource(c); cs == src || !copilotSources[cs] {
			continue
		}
		if copilotSessionKey(c) != session {
			continue
		}
		if !sameNetLines(c, e) {
			continue
		}
		if err := db.FillEditUsage(c.ID, *e); err != nil {
			log.Printf("copilot dedupe: %v", err)
			return false
		}
		log.Printf("copilot dedupe: %s edit for %q already recorded by %s (edit %d) — merged",
			src, e.FilePath, editSource(c), c.ID)
		return true
	}
	return false
}

// copilotRawMeta is the part of a Copilot edit's raw_meta dedupe reads.
type copilotRawMeta struct {
	Source          string `json:"source"`
	TranscriptPath  string `json:"transcript_path"`   // copilot_hook, copilot_transcript
	ChatSessionPath string `json:"chat_session_path"` // copilot_chat_session, copilot_chat_textedit
}

func parseCopilotRawMeta(e *store.Edit) copilotRawMeta {
	var meta copilotRawMeta
	if e.RawMeta.Valid && e.RawMeta.String != "" {
		_ = json.Unmarshal([]byte(e.RawMeta.String), &meta)
	}
	return meta
}

// editSource returns raw_meta's "source" tag ("" when absent or unparsable).
func editSource(e *store.Edit) string {
	return parseCopilotRawMeta(e).Source
}

// copilotSessionKey identifies the VS Code chat session an edit came from as
// "<workspaceStorage hash dir>/<session id>", or "" when the edit is not
// provably from one.
//
// Every recorder that can report the same VS Code edit points into one
// workspace's storage, under the same session id:
//
//	hook, transcript watcher: <hash>/GitHub.copilot-chat/transcripts/<id>.jsonl
//	chatSessions watcher:     <hash>/chatSessions/<id>.jsonl
//
// The Copilot CLI's hook points into ~/.copilot instead, so its edits get no key
// and are never merged — the CLI has no second recorder that could double them.
// Paths are compared case-insensitively with forward slashes (Windows paths).
func copilotSessionKey(e *store.Edit) string {
	meta := parseCopilotRawMeta(e)
	p := meta.ChatSessionPath
	if p == "" {
		p = meta.TranscriptPath
	}
	p = strings.ToLower(strings.ReplaceAll(p, `\`, "/"))
	dir, file := path.Split(p)
	id := strings.TrimSuffix(file, path.Ext(file))
	if id == "" {
		return ""
	}
	dir = strings.TrimSuffix(dir, "/")
	switch {
	case strings.HasSuffix(dir, "/github.copilot-chat/transcripts"):
		dir = strings.TrimSuffix(dir, "/github.copilot-chat/transcripts")
	case strings.HasSuffix(dir, "/chatsessions"):
		dir = strings.TrimSuffix(dir, "/chatsessions")
	default:
		return ""
	}
	if dir == "" {
		return ""
	}
	return dir + "/" + id
}

// sameNetLines reports whether a and b added and removed exactly the same lines,
// compared as content_sha multisets. Positions are ignored (chat recorders use
// placeholders) and so are blank lines, which carry no content_sha. Both must have
// at least one hashed line: two edits with nothing comparable are not "the same".
func sameNetLines(a, b *store.Edit) bool {
	addA, addB := map[string]int{}, map[string]int{}
	for _, l := range a.Lines {
		if l.ContentSHA != "" {
			addA[l.ContentSHA]++
		}
	}
	for _, l := range b.Lines {
		if l.ContentSHA != "" {
			addB[l.ContentSHA]++
		}
	}
	remA, remB := map[string]int{}, map[string]int{}
	for _, r := range a.RemovedLines {
		if r.ContentSHA != "" {
			remA[r.ContentSHA]++
		}
	}
	for _, r := range b.RemovedLines {
		if r.ContentSHA != "" {
			remB[r.ContentSHA]++
		}
	}
	if len(addA)+len(remA) == 0 {
		return false
	}
	return sameCounts(addA, addB) && sameCounts(remA, remB)
}

func sameCounts(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, n := range a {
		if b[k] != n {
			return false
		}
	}
	return true
}
