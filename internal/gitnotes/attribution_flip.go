package gitnotes

import (
	"strings"

	"github.com/blamely/blamely/internal/authorship"
	"github.com/blamely/blamely/internal/store"
)

// This file rewrites a note's ADDED-line attribution from the attribution working
// log (the diff-based truth) instead of a content-hash guess, recomputing the
// AI/Human totals, by_gen_type, and by_tool line counts to match. A file with no
// working log keeps its prior attribution (degraded, not wrong). Deletions are
// handled in attribution_deletions.go; the content_sha reconciliation in
// attribution_gutter.go then recovers AI lines the working-log fold missed.

// gcWorkingLogsIfEnabled prunes dangling-base working logs after a commit
// (flag-gated, best-effort) so history-rewrite churn doesn't accumulate on disk.
func gcWorkingLogsIfEnabled(repoPath string) {
	if !authorship.Enabled() {
		return
	}
	_, _ = authorship.GCWorkingLogs(repoPath)
}

// migrateWorkingLogsOnCommit finalizes working logs after a commit: files NOT in
// this commit migrate forward to the new HEAD (so uncommitted attribution survives a
// partial commit), and files IN this commit have their working logs DELETED (their
// attribution now lives in the note + SQLite). Must run AFTER the flip has read the
// parent-base logs. Flag-gated.
func migrateWorkingLogsOnCommit(repoPath string, note *Note) {
	if note == nil || !authorship.Enabled() {
		return
	}
	parent := commitParentSHA(repoPath, note.Commit)
	if parent == "" {
		return
	}
	// Files included in THIS commit have their working logs deleted (note + SQLite
	// hold the attribution); only uncommitted files migrate forward to the new HEAD.
	committed := make(map[string]bool, len(note.Files))
	for _, f := range note.Files {
		committed[f.Path] = true
	}
	_ = authorship.MigrateWorkingLogs(repoPath, note.Branch, parent, note.Commit, committed)
}

func flipNoteToWorkingLog(repoPath string, note *Note) {
	if note == nil || !authorship.Enabled() {
		return
	}
	parent := commitParentSHA(repoPath, note.Commit)
	if parent == "" {
		return
	}
	// Branching does not move HEAD, so `git checkout -b feature` leaves this
	// commit's working logs filed under the branch the work was DONE on while the
	// flip looks under the branch it is being COMMITTED on. Same base commit, same
	// working tree, different key — and the flip silently found nothing, shipping
	// the session's AI work as Human. Claim those logs before reading. No-op in the
	// normal case where the branch never changed.
	authorship.AdoptWorkingLogsAtBase(repoPath, note.Branch, parent)
	flipAddsAtBase(repoPath, note.Branch, parent, note, func(path string) (string, bool) {
		return showFileAt(repoPath, note.Commit, path)
	})
}

// flipAddsAtBase rewrites the note's added-line attribution from the working logs at
// `base` (the commit's parent for a committed note, or HEAD for the uncommitted
// working tree), then recomputes the added-line aggregates. contentOf returns a
// file's content as the note counts it (the committed blob, or the working tree),
// onto which a log written for other content is carried (AuthorsForContent).
func flipAddsAtBase(repoPath, branch, base string, note *Note, contentOf func(path string) (string, bool)) {
	flipped := false
	for fi := range note.Files {
		fe := &note.Files[fi]
		authors, ok := authorship.AuthorsForContent(repoPath, branch, base, fe.Path, func() (string, bool) {
			return contentOf(fe.Path)
		})
		if !ok {
			continue // not tracked → keep this file's prior attribution
		}
		var rewritten []RangeEntry
		for _, r := range fe.Lines {
			if r.Type != "add" {
				rewritten = append(rewritten, r) // deletions handled in attribution_deletions.go
				continue
			}
			for ln := r.Start; ln <= r.End; ln++ {
				rewritten = append(rewritten, rangeForWorkingLogLine(ln, authors[ln]))
			}
		}
		fe.Lines = collapseAddRanges(rewritten)
		flipped = true
	}
	if flipped {
		recomputeAddedAggregates(note)
	}
}

// reconcileAddsFromEdits is the lossless cross-check the working-log flip can't
// do. Chat edits record their content_sha with PLACEHOLDER line positions (see
// tools.copilotAddedRangesFromContent: "positions are just stable placeholders"),
// because the apply-time line numbers drift before commit. The whole-file
// working-log fold therefore can't tell which AI-authored line is which when the
// AI emits content that duplicates lines already in the file — e.g. a new CSS
// button block byte-identical (bar its selector/colour) to a sibling block. The
// fold collapses that ambiguity to a single guess and the genuinely AI-authored
// lines fall through to Human, while the duplicate (unchanged) lines get the AI
// mark.
//
// Scoped to THIS commit's ADDED lines the match is unambiguous: unchanged lines
// aren't candidates, so they can't steal the attribution. For every added line
// the flip left as Human whose exact committed content matches a content_sha an
// AI tool recorded within this commit's window (consume-once, confidence-then-
// recency ordered — the same matcher the deletion path uses), credit it to that
// tool. Human→AI ONLY: a line the working log already attributed to an AI tool is
// never downgraded, and a content_sha the AI never recorded never flips a human
// line. The committed diff + the recorded edits are both exact artifacts, so this
// is a deterministic set operation, not a heuristic.
func reconcileAddsFromEdits(db *store.DB, repoID string, commitNanos int64, note *Note, added []AddedLine) {
	reconcileAddsFromEditsWindowed(db, repoID, commitNanos, note, added, nil)
}

// fileHasHumanAddLines reports whether any added range in the file is still
// attributed to a human — i.e. whether the per-file window widening (a git
// spawn) can possibly change anything for this file.
func fileHasHumanAddLines(fe *FileEntry) bool {
	for i := range fe.Lines {
		if fe.Lines[i].Type == "add" && fe.Lines[i].AuthorType != "AI" {
			return true
		}
	}
	return false
}

// reconcileAddsFromEditsWindowed is reconcileAddsFromEdits with an optional
// PER-FILE lower-bound resolver. When fileTouchNanos is non-nil, each file's
// window opens at min(repo-wide previous-commit bound, last commit that touched
// THIS file) — so edits that predate the repo's last commit (stash pop, long
// work sessions interleaved with commits to other files) stay eligible, while
// content the file already committed remains excluded. nil keeps the legacy
// repo-wide bound exactly.
func reconcileAddsFromEditsWindowed(db *store.DB, repoID string, commitNanos int64, note *Note, added []AddedLine, fileTouchNanos func(relPath string) int64) {
	if db == nil || note == nil || !authorship.Enabled() || len(added) == 0 {
		return
	}
	// Same window as the working-log flip and deletion attribution: the lower
	// bound excludes edits a previous commit on this branch already consumed; the
	// 5s post-commit slack covers git's second-precision commit timestamp against
	// nanosecond edit timestamps.
	sinceNanos := db.PreviousCommitTimestampNanos(repoID, commitNanos)
	const sameSecondSlackNanos = int64(5 * 1e9)
	maxNanos := commitNanos + sameSecondSlackNanos

	reconcileAddsWithMatchers(note, added, func(fe *FileEntry) (m, cp, ct *editMatcher) {
		// Per-file window: widen the lower bound to the last commit that touched
		// this file (never raise it). Only resolved when the file still has
		// Human-attributed added lines — the git spawn is skipped otherwise. ALL
		// THREE matchers share the same bound so copypaste/cursor-tab pins keep
		// their precedence over generic AI matches across the widened window.
		fileSince := sinceNanos
		if fileTouchNanos != nil && fileHasHumanAddLines(fe) {
			if ft := fileTouchNanos(fe.Path); ft < fileSince {
				fileSince = ft
			}
		}
		return newEditMatcher(db, repoID, fe.Path, fileSince, maxNanos, aiEligible),
			newEditMatcher(db, repoID, fe.Path, fileSince, maxNanos, copyPasteEligible),
			newEditMatcher(db, repoID, fe.Path, fileSince, maxNanos, cursorTabEligible)
	})
}

// reconcileAddsFromEditsWide is the LAST-RESORT reconcile for replay-shaped
// commits (cherry-pick / squash): the same Human→AI, consume-once content match,
// but over EVERY recorded edit for the file (EditsForFileAny) instead of a time
// window — a replayed line's original edit can be arbitrarily old and from
// another branch. HARD-GATED to detected replays: on an ordinary commit the
// unbounded window would let a human's retype of old AI content re-attribute.
// Runs after the source-note reconcile, so it only claims lines the settled
// notes couldn't explain (never-noted source commits, rewritten squash message).
func reconcileAddsFromEditsWide(db *store.DB, repoID string, note *Note, added []AddedLine, replay replayCtx) {
	if db == nil || note == nil || !authorship.Enabled() || len(added) == 0 || replay.Kind == replayNone {
		return
	}
	reconcileAddsWithMatchers(note, added, func(fe *FileEntry) (m, cp, ct *editMatcher) {
		return newEditMatcherAll(db, repoID, fe.Path, aiEligible),
			newEditMatcherAll(db, repoID, fe.Path, copyPasteEligible),
			newEditMatcherAll(db, repoID, fe.Path, cursorTabEligible)
	})
}

// reconcileAddsWithMatchers is the shared expand→upgrade→re-collapse loop over
// the note's added ranges. matchersFor supplies the (AI, copypaste, cursorTab)
// matchers per file — windowed or unbounded — so precedence and blank-line
// inheritance behave identically for every reconcile variant.
func reconcileAddsWithMatchers(note *Note, added []AddedLine, matchersFor func(fe *FileEntry) (m, cp, ct *editMatcher)) {
	// Committed content of each added line, keyed by file then line number.
	contentByLine := map[string]map[int]string{}
	for _, a := range added {
		if contentByLine[a.File] == nil {
			contentByLine[a.File] = map[int]string{}
		}
		contentByLine[a.File][a.LineNum] = a.Content
	}

	changedAny := false
	for fi := range note.Files {
		fe := &note.Files[fi]
		lineContent := contentByLine[fe.Path]
		if len(lineContent) == 0 {
			continue
		}
		// AI edits for this file, each with a consume-once budget of the exact
		// and whitespace-normalized content_shas it recorded as ADDED lines.
		// A second matcher tags copy-pasted lines: author_type stays Human, but the
		// distinct copypaste tool is preserved (matching v1's by_tool nuance).
		// Cursor Tab-log edits get their OWN matcher, tried before copy-paste: an
		// accepted Cursor Tab (inline) suggestion is recorded both by the editor
		// plugin (often mis-tagged copypaste — its content matches the clipboard
		// during repetitive edits) AND by Cursor's own Tab log. The Tab log is
		// authoritative that the line was a live completion, not a paste, so it wins.
		m, cp, ct := matchersFor(fe)
		if m.empty() && cp.empty() && ct.empty() {
			continue
		}

		// Expand add ranges to single lines, upgrade Human→AI where content matches
		// a recorded AI edit (or tag copy-paste), then re-collapse (mirrors
		// flipAddsAtBase). Deletion ranges pass through untouched.
		var rewritten []RangeEntry
		changed := false
		for _, r := range fe.Lines {
			if r.Type != "add" {
				rewritten = append(rewritten, r)
				continue
			}
			for ln := r.Start; ln <= r.End; ln++ {
				single := RangeEntry{Start: ln, End: ln, Type: "add", AuthorType: r.AuthorType, Tool: r.Tool, Model: r.Model, GenType: r.GenType}
				if single.AuthorType != "AI" {
					// Precedence: Cursor Tab log → copy-paste → generic AI.
					//
					// A Cursor Tab-log edit is tried FIRST: Cursor's own log proves
					// the line was an accepted inline (Tab) completion, which overrides
					// the plugin's heuristic copypaste record for the same content
					// (the plugin sees the insert first and mis-reads it as a paste
					// when the clipboard happens to match during repetitive edits —
					// repro: 7e1f1912, Tab-completed <option>/label lines shown Human).
					//
					// Copy-paste is tried next: a line the human explicitly pasted
					// stays Human (tagged), even though its content also matches an AI
					// content_sha — copying AI-generated code and pasting it is the
					// COMMON case, and the AI matcher would otherwise re-claim the paste
					// as AI (repro: 9a73263c — a codex line copied+pasted showed codex).
					// cp.match only fires when a real copypaste edit recorded this
					// content (consume-once), so a genuinely-AI line is never withheld.
					if e := ct.match(lineContent[ln]); e != nil {
						single = aiAddRange(ln, e)
						changed = true
					} else if e := cp.match(lineContent[ln]); e != nil {
						// Copy-paste: keep author_type Human, tag the tool.
						single.Tool = string(e.Tool)
						changed = true
					} else if e := m.match(lineContent[ln]); e != nil {
						single = aiAddRange(ln, e)
						changed = true
					}
				}
				rewritten = append(rewritten, single)
			}
		}
		if changed {
			// Blank separator lines the AI inserted inside its block (e.g. between
			// two CSS rules) carry no content_sha, so pickAddEdit can't match them.
			// buildNote's inheritBlankLineAttribution would have caught them, but it
			// ran BEFORE this reconcile when the whole block was still Human. Re-run
			// the same backward-then-forward inheritance now that the surrounding
			// lines are AI.
			inheritBlankAddRanges(rewritten, lineContent)
			fe.Lines = collapseAddRanges(rewritten)
			changedAny = true
		}
	}
	if changedAny {
		recomputeAddedAggregates(note)
	}
}

// reconcileAddsFromInheritedNote preserves AI added-line attribution across a plain
// `git commit --amend`. git copies the pre-amend commit's note onto the amended SHA
// (notes.rewriteRef), but — unlike rebase/cherry-pick — an amend is not an in-progress
// op, so attributionShouldSkip doesn't fire and AttributeAndWrite recomputes. By then
// the committed file's working log was deleted by the original commit and its source
// edits sit outside this commit's window, so the recompute drops AI lines back to
// Human. This restores them from the note git inherited: for each added line the
// recompute left Human, if the inherited note attributed the SAME post-image line to an
// AI tool, re-apply that attribution. Human→AI ONLY, line-for-line — consistent with
// reconcileAddsFromEdits.
//
// Guard against a content-changing amend: a file is reconciled only when its added-line
// number set is identical in the inherited and freshly-built notes. If the amend altered
// the file's diff, the line numbers no longer describe the same content, so we keep the
// recompute's result rather than risk crediting a human-typed line to AI. A fresh commit
// (no inherited note) or one that inherited nothing as AI is a no-op.
func reconcileAddsFromInheritedNote(inherited, note *Note) {
	if inherited == nil || note == nil {
		return
	}
	type aiAttr struct {
		tool    string
		model   *string
		genType *string
	}
	// Inherited AI added-line attribution and the full inherited added-line set, per file.
	aiByLine := map[string]map[int]aiAttr{}
	inhAddSet := map[string]map[int]bool{}
	for _, f := range inherited.Files {
		for _, r := range f.Lines {
			if r.Type != "add" {
				continue
			}
			if inhAddSet[f.Path] == nil {
				inhAddSet[f.Path] = map[int]bool{}
			}
			for ln := r.Start; ln <= r.End; ln++ {
				inhAddSet[f.Path][ln] = true
			}
			if r.AuthorType != "AI" || r.Tool == "" {
				continue
			}
			if aiByLine[f.Path] == nil {
				aiByLine[f.Path] = map[int]aiAttr{}
			}
			for ln := r.Start; ln <= r.End; ln++ {
				aiByLine[f.Path][ln] = aiAttr{tool: r.Tool, model: r.Model, genType: r.GenType}
			}
		}
	}
	if len(aiByLine) == 0 {
		return
	}

	changedAny := false
	for fi := range note.Files {
		fe := &note.Files[fi]
		ai := aiByLine[fe.Path]
		if len(ai) == 0 {
			continue
		}
		// Same-content guard: the file's added-line set must match the inherited note's,
		// or the amend changed this file and line numbers can't be trusted.
		freshAddSet := map[int]bool{}
		for _, r := range fe.Lines {
			if r.Type != "add" {
				continue
			}
			for ln := r.Start; ln <= r.End; ln++ {
				freshAddSet[ln] = true
			}
		}
		if !sameIntSet(freshAddSet, inhAddSet[fe.Path]) {
			continue
		}

		var rewritten []RangeEntry
		changed := false
		for _, r := range fe.Lines {
			if r.Type != "add" {
				rewritten = append(rewritten, r)
				continue
			}
			for ln := r.Start; ln <= r.End; ln++ {
				single := RangeEntry{Start: ln, End: ln, Type: "add", AuthorType: r.AuthorType, Tool: r.Tool, Model: r.Model, GenType: r.GenType}
				if single.AuthorType != "AI" {
					if a, ok := ai[ln]; ok {
						single.AuthorType = "AI"
						single.Tool = a.tool
						single.Model = a.model
						single.GenType = a.genType
						changed = true
					}
				}
				rewritten = append(rewritten, single)
			}
		}
		if changed {
			fe.Lines = collapseAddRanges(rewritten)
			changedAny = true
		}
	}
	if changedAny {
		recomputeAddedAggregates(note)
	}
}

func sameIntSet(a, b map[int]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// inheritBlankAddRanges gives each blank Human added line the AI attribution of
// its nearest non-blank added neighbour (backward first, then forward), matching
// buildNote's inheritBlankLineAttribution. Operates on the expanded single-line
// add entries in place; deletion entries are skipped as neighbours. A blank whose
// nearest non-blank add neighbour is Human stays Human.
//
// The neighbour search walks CONTIGUOUS added line numbers only (ln±1, ln±2, …)
// and stops at the first gap — an unchanged line, or a deletion. A blank added
// line isolated by unchanged content therefore never inherits a distant AI block's
// attribution (repro: commit 095f6221 — a lone blank line at 4 wrongly inherited
// `completion` from an AI add 9 rows away at line 13).
func inheritBlankAddRanges(entries []RangeEntry, lineContent map[int]string) {
	// Index add entries by their (post-image) line number.
	addAt := make(map[int]int, len(entries)) // line → index in entries
	for i := range entries {
		if entries[i].Type == "add" {
			addAt[entries[i].Start] = i
		}
	}
	blank := func(ln int) bool { return strings.TrimSpace(lineContent[ln]) == "" }
	// nearestNonBlankAdd walks contiguous added lines outward from ln (backward
	// first, then forward), returning the first non-blank added entry it reaches
	// before a gap. Backward wins: a blank inherits the block above it, matching
	// inheritBlankLineAttribution.
	nearestNonBlankAdd := func(ln int) (RangeEntry, bool) {
		for k := ln - 1; ; k-- {
			i, ok := addAt[k]
			if !ok {
				break // gap → try forward
			}
			if !blank(k) {
				return entries[i], true
			}
		}
		for k := ln + 1; ; k++ {
			i, ok := addAt[k]
			if !ok {
				break
			}
			if !blank(k) {
				return entries[i], true
			}
		}
		return RangeEntry{}, false
	}
	for i := range entries {
		if entries[i].Type != "add" || !blank(entries[i].Start) || entries[i].AuthorType == "AI" {
			continue
		}
		src, ok := nearestNonBlankAdd(entries[i].Start)
		if !ok || src.AuthorType != "AI" {
			continue
		}
		entries[i].AuthorType = "AI"
		entries[i].Tool = src.Tool
		entries[i].Model = src.Model
		entries[i].GenType = src.GenType
	}
}

// pickAddEdit returns the AI edit (the slice is confidence-then-recency ordered,
// so the strongest/newest record wins) that recorded `content` and still has
// unconsumed budget for it, consuming one unit. Exact content_sha is preferred;
// the whitespace-normalized hash is a fallback for autoformatter drift (an
// AI-written line whose exact bytes were later reindented). A recorded line is
// ONE unit of budget shared across both hashes — when an exact match is consumed
// its normalized counterpart is decremented too — so content the AI recorded once
// can attribute at most one committed line. That strictness is deliberate:
// over-crediting a human duplicate to AI is the same class of wrong detection,
// just inverted. Blank/whitespace-only content never carries a content_sha (see
// tools.copilotAddedRangesFromContent) and never matches.
func pickAddEdit(edits []store.Edit, content string, remSHA, remNorm map[int64]map[string]int) *store.Edit {
	if strings.TrimSpace(content) == "" {
		return nil
	}
	want := sha256HexStr([]byte(content))
	for i := range edits {
		e := &edits[i]
		if remSHA[e.ID][want] > 0 {
			remSHA[e.ID][want]--
			if wn := sha256HexNormStr(content); wn != "" && remNorm[e.ID][wn] > 0 {
				remNorm[e.ID][wn]-- // keep the shared per-line budget in sync
			}
			return e
		}
	}
	wantNorm := sha256HexNormStr(content)
	if wantNorm == "" {
		return nil
	}
	for i := range edits {
		e := &edits[i]
		if remNorm[e.ID][wantNorm] > 0 {
			remNorm[e.ID][wantNorm]--
			return e
		}
	}
	return nil
}

// aiAddRange builds an AI-authored added RangeEntry for one line from the edit
// that recorded its content (mirrors rangeForWorkingLogLine's AI branch, but
// sourced from a store.Edit instead of an authorship.Author).
func aiAddRange(ln int, e *store.Edit) RangeEntry {
	re := RangeEntry{Start: ln, End: ln, Type: "add", AuthorType: "AI", Tool: string(e.Tool)}
	if e.Model.Valid && e.Model.String != "" {
		m := e.Model.String
		re.Model = &m
	}
	if gt := string(e.GenType); gt != "" && gt != string(store.GenTypeUnknown) {
		re.GenType = &gt
	}
	return re
}

func rangeForWorkingLogLine(ln int, a authorship.Author) RangeEntry {
	re := RangeEntry{Start: ln, End: ln, Type: "add", AuthorType: "Human"}
	if a.Type == authorship.AI {
		re.AuthorType = "AI"
		re.Tool = a.Tool
		if a.Model != "" {
			m := a.Model
			re.Model = &m
		}
		if a.GenType != "" {
			g := a.GenType
			re.GenType = &g
		}
	}
	return re
}

// collapseAddRanges merges adjacent single-line entries that share the full
// (type, author_type, tool, model, gen_type) identity back into spans.
func collapseAddRanges(in []RangeEntry) []RangeEntry {
	var out []RangeEntry
	for _, r := range in {
		if n := len(out); n > 0 && out[n-1].End == r.Start-1 && sameRangeIdentity(out[n-1], r) {
			out[n-1].End = r.End
			continue
		}
		out = append(out, r)
	}
	return out
}

func sameRangeIdentity(a, b RangeEntry) bool {
	return a.Type == b.Type && a.AuthorType == b.AuthorType && a.Tool == b.Tool &&
		ptrStrEq(a.Model, b.Model) && ptrStrEq(a.GenType, b.GenType)
}

func ptrStrEq(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// recomputeAddedAggregates rebuilds the ADDED-line split (Totals, by_gen_type, and
// by_tool line counts) from the now-working-log-sourced ranges. Deleted totals and
// per-tool tokens/model/suggested are preserved from v1.
func recomputeAddedAggregates(note *Note) {
	aiAdded, humanAdded := 0, 0
	var gt ByGenType
	toolLines := map[string]int{}
	models := map[string]int{}

	for _, fe := range note.Files {
		for _, r := range fe.Lines {
			if r.Type != "add" {
				continue
			}
			n := r.End - r.Start + 1
			if n <= 0 {
				continue
			}
			if r.AuthorType == "AI" {
				aiAdded += n
				if r.Tool != "" {
					toolLines[r.Tool] += n
				}
				if r.Model != nil && *r.Model != "" {
					models[*r.Model] += n
				}
				switch ptrStr(r.GenType) {
				case "chat":
					gt.Chat += n
				case "cli":
					gt.CLI += n
				case "completion":
					gt.Completion += n
				default:
					gt.Unknown += n
				}
			} else {
				humanAdded += n
				gt.Human += n
				// A Human line may still carry a non-AI tool tag (copypaste): keep it
				// in by_tool for the nuance, without affecting the AI/Human split.
				if r.Tool != "" {
					toolLines[r.Tool] += n
				}
			}
		}
	}

	note.Totals.AILines = aiAdded
	note.Totals.HumanLines = humanAdded
	note.Totals.AddedLines = aiAdded + humanAdded
	if len(models) > 0 {
		note.Totals.Models = models
	} else {
		note.Totals.Models = nil
	}
	note.ByGenType = gt

	// Rebuild by_tool line counts, preserving each tool's v1 metadata
	// (tokens/model/suggested/deleted) where present.
	rebuilt := make(map[string]Tool, len(toolLines))
	for tool, lines := range toolLines {
		t := note.ByTool[tool] // zero value if the tool is new
		t.Lines = lines
		t.AcceptedLines = lines
		rebuilt[tool] = t
	}
	// Keep deletion-only tools (authored nothing but removed lines).
	for tool, t := range note.ByTool {
		if _, ok := rebuilt[tool]; !ok && t.DeletedLines > 0 {
			t.Lines = 0
			t.AcceptedLines = 0
			rebuilt[tool] = t
		}
	}
	note.ByTool = rebuilt
}

func ptrStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
