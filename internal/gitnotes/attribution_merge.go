package gitnotes

import (
	"bytes"
	"os/exec"
	"strings"

	"github.com/blamely/blamely/internal/authorship"
	"github.com/blamely/blamely/internal/config"

	"github.com/blamely/blamely/internal/procattr"
)

// restrictMergeToResolution narrows a MERGE commit's added lines to the conflict
// RESOLUTION — the lines added relative to BOTH parents. DiffCommit uses the first
// parent only, so a merge's added set also contains the merged-in branch's lines;
// those were authored on that branch (and attributed in its own commits), not in the
// merge, so crediting the merge with them inflates its report. A line added vs both
// parents is genuinely new in the merge (a conflict resolution), and that is what we
// keep. No-op for non-merge commits and when attribution is off (preserves legacy behavior).
//
// Line numbers from both diffs are post-image (positions in `sha`), so intersecting
// by (file, line) is valid. Deletions/renames are left as the first-parent diff
// computed them.
func restrictMergeToResolution(repoPath, sha string, change *CommitChange) {
	if change == nil || !authorship.Enabled() {
		return
	}
	out, err := procattr.Hide(exec.Command("git", "-C", repoPath, "rev-parse", "-q", "--verify", sha+"^2")).Output()
	if err != nil {
		return // not a merge (no second parent)
	}
	secondParent := strings.TrimSpace(string(out))
	if secondParent == "" {
		return
	}
	diff, err := procattr.Hide(exec.Command("git", "-C", repoPath, "diff", "--unified=0", "--no-color", "-M",
		secondParent+".."+sha)).Output()
	if err != nil {
		return
	}
	excl, _ := config.LoadExcludeListForRepo(repoPath)
	vsP2, perr := parseDiff(bytes.NewReader(diff), excl)
	if perr != nil {
		return
	}
	addedVsP2 := make(map[string]map[int]bool, len(vsP2.Added))
	for _, a := range vsP2.Added {
		m := addedVsP2[a.File]
		if m == nil {
			m = make(map[int]bool)
			addedVsP2[a.File] = m
		}
		m[a.LineNum] = true
	}
	kept := change.Added[:0]
	for _, a := range change.Added {
		if addedVsP2[a.File][a.LineNum] {
			kept = append(kept, a)
		}
	}
	change.Added = kept
}
