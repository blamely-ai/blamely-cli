package authorship

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
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
// Bases of commits that have not reached any remote yet are never pruned by depth:
// a push that failed and is retried later must still find its logs to re-attribute
// from (see unpushedBases).
//
// It runs on every commit (post-commit hook), so it costs a fixed handful of git
// processes regardless of how many base dirs exist: one batched existence check, one
// unpushed-commit listing, and one streamed history walk. Spawning git per dir made
// commits take tens of seconds on Windows once a repo had accumulated logs.
//
// Best-effort and cross-platform (path/filepath + git plumbing only). Returns the
// number of base_sha directories removed. When git can't answer, nothing is pruned.
func GCWorkingLogs(repoRoot string) (pruned int, err error) {
	root := filepath.Join(gitutil.GitDir(repoRoot), "blamely", "working_logs")
	branchDirs, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}

	type baseDir struct{ path, sha string }
	var bases []baseDir
	var branchPaths []string
	for _, bd := range branchDirs {
		if !bd.IsDir() {
			continue
		}
		branchPath := filepath.Join(root, bd.Name())
		branchPaths = append(branchPaths, branchPath)
		baseDirs, derr := os.ReadDir(branchPath)
		if derr != nil {
			continue
		}
		for _, sd := range baseDirs {
			if !sd.IsDir() || !looksLikeSHA(sd.Name()) {
				continue // keep non-SHA bases (INITIAL/DETACHED) and stray files
			}
			bases = append(bases, baseDir{filepath.Join(branchPath, sd.Name()), sd.Name()})
		}
	}
	if len(bases) == 0 {
		return 0, nil
	}

	shas := make([]string, 0, len(bases))
	seen := map[string]bool{}
	for _, b := range bases {
		if !seen[b.sha] {
			seen[b.sha] = true
			shas = append(shas, b.sha)
		}
	}
	present, err := presentObjects(repoRoot, shas)
	if err != nil {
		return 0, nil // can't tell what exists — prune nothing
	}
	// Depth pruning only runs when we know which bases back unpushed commits; if
	// that listing fails, keep every live base rather than risk dropping one.
	unpushed, uerr := unpushedBases(repoRoot)
	var depth map[string]int
	if uerr == nil {
		var live []string
		for _, sha := range shas {
			if present[sha] && !unpushed[sha] {
				live = append(live, sha)
			}
		}
		depth = depthsBehindHead(repoRoot, live)
	}

	for _, b := range bases {
		if present[b.sha] {
			// Base object still present. Keep it unless it's an ANCESTOR of HEAD that
			// sits deeper than the retain depth — that far back, no amend/rebase or
			// sibling re-attribution targets it, so its retained committed-file logs
			// can go. Non-ancestors (divergent siblings), shallow bases and bases of
			// unpushed commits are kept.
			if d, ok := depth[b.sha]; !ok || d <= workingLogRetainDepth {
				continue
			}
		}
		if os.RemoveAll(b.path) == nil {
			pruned++
		}
	}
	// Drop branch dirs GC emptied.
	for _, branchPath := range branchPaths {
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

// presentObjects reports which of shas git still has (any type), in one
// `cat-file --batch-check`. Existence, not reachability, is the liveness signal: an
// unreachable-but-present commit may still back uncommitted work, whereas a missing
// object can back nothing.
func presentObjects(repoRoot string, shas []string) (map[string]bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitutil.DefaultTimeout)
	defer cancel()
	cmd := gitutil.Command(ctx, repoRoot, "cat-file", "--batch-check")
	cmd.Stdin = strings.NewReader(strings.Join(shas, "\n") + "\n")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	present := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[1] != "missing" {
			present[f[0]] = true
		}
	}
	return present, nil
}

// unpushedBases returns every commit that has not reached a remote-tracking ref yet,
// plus each such commit's parents — a commit's working logs live under its parent's
// base dir. A push that failed (rejected, offline, hook error) leaves its commits here
// until a later push lands, so their logs survive until then. A repo with no remotes
// has nothing to push to, so it gets an empty set and plain depth pruning.
func unpushedBases(repoRoot string) (map[string]bool, error) {
	remotes, err := gitutil.Output(repoRoot, "remote")
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	if strings.TrimSpace(string(remotes)) == "" {
		return set, nil
	}
	out, err := gitutil.Output(repoRoot, "rev-list", "--parents", "HEAD", "--branches", "--not", "--remotes")
	if err != nil {
		return nil, err
	}
	for _, sha := range strings.Fields(string(out)) {
		set[sha] = true
	}
	return set, nil
}

// depthsBehindHead returns, for each sha that is an ancestor of HEAD, a lower bound on
// how many commits HEAD is ahead of it. Non-ancestors (divergent siblings) and an
// unresolvable HEAD are absent from the map, so the caller keeps them.
//
// It walks `rev-list --topo-order HEAD` once and records each sha's position. Topo
// order never lists a commit before its descendants, so every commit listed before
// sha is not one of its ancestors and therefore counts toward sha..HEAD: the position
// never overstates the depth (and equals it on linear history). The walk stops as
// soon as every sha has been seen.
func depthsBehindHead(repoRoot string, shas []string) map[string]int {
	depth := map[string]int{}
	if len(shas) == 0 {
		return depth
	}
	want := make(map[string]bool, len(shas))
	for _, sha := range shas {
		want[sha] = true
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitutil.DefaultTimeout)
	defer cancel()
	cmd := gitutil.Command(ctx, repoRoot, "rev-list", "--topo-order", "HEAD")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return depth
	}
	if cmd.Start() != nil {
		return depth
	}
	sc := bufio.NewScanner(stdout)
	for pos := 0; sc.Scan(); pos++ {
		if sha := strings.TrimSpace(sc.Text()); want[sha] {
			depth[sha] = pos
			if len(depth) == len(want) {
				break
			}
		}
	}
	cancel() // stop the walk early; the resulting kill error is expected
	_ = cmd.Wait()
	if ctx.Err() == context.DeadlineExceeded {
		return map[string]int{} // walk timed out: positions may be incomplete, prune nothing
	}
	return depth
}
