package daemon

import (
	"encoding/json"
	"log"

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
// copilotDupWindowNanos, from a DIFFERENT source, with the same net added and
// removed lines. If so it folds e's model/tokens into the stored row and the
// caller skips the insert.
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

// editSource returns raw_meta's "source" tag ("" when absent or unparsable).
func editSource(e *store.Edit) string {
	if !e.RawMeta.Valid || e.RawMeta.String == "" {
		return ""
	}
	var meta struct {
		Source string `json:"source"`
	}
	if json.Unmarshal([]byte(e.RawMeta.String), &meta) != nil {
		return ""
	}
	return meta.Source
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
