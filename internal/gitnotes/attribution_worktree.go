package gitnotes

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"

	"github.com/blamely/blamely/internal/config"
	"github.com/blamely/blamely/internal/gitutil"
	"github.com/blamely/blamely/internal/store"

	"github.com/blamely/blamely/internal/procattr"
)

// DiffWorkingTree diffs the working tree against HEAD (tracked changes, staged +
// unstaged) and parses it like DiffCommit, so the CURRENT uncommitted change can be
// attributed and shown by `blamely stats` before a commit exists.
func DiffWorkingTree(repoPath string) (*CommitChange, error) {
	cmd := procattr.Hide(exec.Command("git", "-C", repoPath, "diff", "--unified=0", "--no-color", "-M", "HEAD"))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("git diff: %w", err)
	}
	excl, _ := config.LoadExcludeListForRepo(repoPath)
	out, perr := parseDiff(stdout, excl)
	_ = cmd.Wait()
	if perr != nil {
		return nil, perr
	}
	return out, nil
}

// AttributeWorkingTree builds a Note for the current UNCOMMITTED change: a Human
// skeleton from the working-tree diff, flipped to AI from the working logs + deletions
// log keyed at HEAD (the same sources the commit-time flip reads). Used by
// `blamely stats` with no argument.
func AttributeWorkingTree(repoPath string) (*Note, error) {
	if wt, ok := gitutil.Toplevel(repoPath); ok {
		repoPath = wt
	}
	change, err := DiffWorkingTree(repoPath)
	if err != nil {
		return nil, err
	}
	branch := BranchName(repoPath)
	base := gitutil.HeadSHA(repoPath)
	note := buildWorkingTreeNote(change, branch)
	flipAddsAtBase(repoPath, branch, base, note, func(path string) (string, bool) {
		content, err := os.ReadFile(filepath.Join(repoPath, filepath.FromSlash(path)))
		return string(content), err == nil
	})
	flipDeletesAtBase(repoPath, branch, base, note, change)

	// Same lossless content_sha cross-check the commit-time path runs, so `blamely
	// stats` (pre-commit) matches the committed note: upgrade Human add/delete lines
	// the working-log + deletions-log fold couldn't resolve to the AI tool that
	// recorded their content in SQLite. Passing now as the "commit" time makes the
	// window (HEAD, now] — i.e. exactly this session's uncommitted edits, since
	// PreviousCommitTimestampNanos(now) resolves to HEAD. Best-effort: a missing DB
	// just leaves the fold's result unchanged.
	if db, derr := store.Open(); derr == nil {
		defer db.Close()
		repoID, _ := gitutil.RepoID(repoPath)
		if repoID == "" {
			repoID = repoPath
		}
		nowNanos := time.Now().UnixNano()
		reconcileAddsFromEdits(db, repoID, nowNanos, note, change.Added)
		reconcileDeletesFromEdits(db, repoID, nowNanos, note, change)
	}
	// Fold deletions into the generation breakdown so `blamely stats` bars cover all
	// changed lines, not just additions (mirrors AttributeAndWrite).
	recomputeByGenType(note)
	// Per-file AI/Human added+deleted split, from the same settled ranges.
	recomputeFileLineSplits(note)
	return note, nil
}

// buildWorkingTreeNote lays down the Human skeleton (added + deleted ranges, totals)
// for the working-tree diff; the flips then set AI and recompute the split.
func buildWorkingTreeNote(change *CommitChange, branch string) *Note {
	note := &Note{Schema: 2, Branch: branch, ByTool: map[string]Tool{}}

	addByFile := map[string][]int{}
	for _, a := range change.Added {
		addByFile[a.File] = append(addByFile[a.File], a.LineNum)
	}
	paths := map[string]bool{}
	for f := range addByFile {
		paths[f] = true
	}
	for f, dl := range change.Deleted {
		if len(dl) > 0 {
			paths[f] = true
		}
	}
	ordered := make([]string, 0, len(paths))
	for f := range paths {
		ordered = append(ordered, f)
	}
	sort.Strings(ordered)

	delCount := 0
	for _, path := range ordered {
		fe := FileEntry{Path: path}
		var acc []LineEntry
		adds := addByFile[path]
		sort.Ints(adds)
		for _, ln := range adds {
			acc = append(acc, LineEntry{Line: ln, Type: "add", AuthorType: "Human", GenType: strPtr("human")})
		}
		fe.Added = len(adds)
		dl := change.Deleted[path]
		for _, d := range dl {
			acc = append(acc, LineEntry{Line: d.LineNum, Type: "delete", AuthorType: "Human"})
		}
		fe.Deleted = len(dl)
		delCount += len(dl)
		if kind, ok := change.FileChanges[path]; ok {
			fe.Type = string(kind)
		}
		fe.Lines = collapseToRanges(acc)
		note.Files = append(note.Files, fe)
	}

	note.Totals.AddedLines = len(change.Added)
	note.Totals.HumanLines = len(change.Added)
	note.Totals.DeletedLines = delCount
	note.ByGenType.Human = len(change.Added)
	return note
}
