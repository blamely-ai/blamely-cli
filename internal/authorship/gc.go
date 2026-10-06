package authorship

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/blamely/blamely/internal/gitutil"
)

// workingLogRetainDepth bounds how many commits behind HEAD a base-SHA working-log
// dir is kept. Committed files' logs are retained at their parent base as the durable
// re-attribution source (amend/rebase, or a divergent sibling on the same base); this
// caps the disk that retention costs. The bound is deliberately generous — far beyond
// any realistic amend/rebase/sibling reach — so attribution recovery is never the
// thing GC breaks. Bases that are NOT ancestors of HEAD (divergent siblings) have no
// "depth" and are never pruned by this rule.
const workingLogRetainDepth = 200

// GCWorkingLogs prunes working-log trees whose base commit git no longer has —
// i.e. base_sha directories whose object is gone after an amend / rebase / `git gc`
// removed the dangling commit. This is provably safe: a log is removed ONLY once
// its base object no longer exists, so it can never describe content reachable from
// any ref. It bounds the disk growth from history-rewriting churn.
//
// It does NOT prune logs whose base is still a valid object (even if unreachable):
// those may back uncommitted edits in another worktree/branch. The broader
// lifecycle — carrying committed authorship across a commit as the next log's prior
// (note-seeding) — is separate; without it, GC here never risks a regression.
//
// Best-effort and cross-platform (path/filepath + git plumbing only). Returns the
// number of base_sha directories removed.
func GCWorkingLogs(repoRoot string) (pruned int, err error) {
	root := filepath.Join(gitutil.GitDir(repoRoot), "blamely", "working_logs")
	branchDirs, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	for _, bd := range branchDirs {
		if !bd.IsDir() {
			continue
		}
		branchPath := filepath.Join(root, bd.Name())
		baseDirs, derr := os.ReadDir(branchPath)
		if derr != nil {
			continue
		}
		for _, sd := range baseDirs {
			if !sd.IsDir() || !looksLikeSHA(sd.Name()) {
				continue // keep non-SHA bases (INITIAL/DETACHED) and stray files
			}
			if objectExists(repoRoot, sd.Name()) {
				// Base object still present. Keep it unless it's an ANCESTOR of HEAD that
				// sits deeper than the retain depth — that far back, no amend/rebase or
				// sibling re-attribution targets it, so its retained committed-file logs
				// can go. Non-ancestors (divergent siblings) and shallow bases are kept.
				if d, ok := baseDepthBehindHead(repoRoot, sd.Name()); ok && d > workingLogRetainDepth {
					if os.RemoveAll(filepath.Join(branchPath, sd.Name())) == nil {
						pruned++
					}
				}
				continue
			}
			if os.RemoveAll(filepath.Join(branchPath, sd.Name())) == nil {
				pruned++
			}
		}
		// Drop the branch dir if GC emptied it.
		if entries, rerr := os.ReadDir(branchPath); rerr == nil && len(entries) == 0 {
			_ = os.Remove(branchPath)
		}
	}
	return pruned, nil
}

// looksLikeSHA reports whether s is a 40- or 64-hex-char object id (SHA-1 / SHA-256),
// so GC only ever considers real base_sha directories — never INITIAL / DETACHED.
func looksLikeSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// baseDepthBehindHead returns how many commits HEAD is ahead of sha (i.e. sha's
// distance behind HEAD) and true, but ONLY when sha is an ancestor of HEAD. For a
// non-ancestor (a divergent sibling base, or when HEAD is unresolvable) it returns
// false so the caller keeps the dir — a sibling base may still back live work.
func baseDepthBehindHead(repoRoot, sha string) (int, bool) {
	if _, aerr := gitutil.Output(repoRoot, "merge-base", "--is-ancestor", sha, "HEAD"); aerr != nil {
		return 0, false // not an ancestor of HEAD (or no HEAD) → no meaningful depth
	}
	out, err := gitutil.Output(repoRoot, "rev-list", "--count", sha+"..HEAD")
	if err != nil {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, false
	}
	return n, true
}

// objectExists reports whether git still has the object (any type). Existence, not
// reachability, is the liveness signal: an unreachable-but-present commit may still
// back uncommitted work, whereas a missing object can back nothing.
func objectExists(repoRoot, sha string) bool {
	_, err := gitutil.Output(repoRoot, "cat-file", "-e", sha)
	return err == nil
}
