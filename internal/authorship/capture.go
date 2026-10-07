package authorship

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/blamely/blamely/internal/gitutil"
)

// capture.go is the high-level entry every observed edit funnels through (CLI
// hooks, daemon watchers, editor plugin via the daemon). It resolves the
// working-log key from git and routes to the engine in store.go / attribute.go.
// Cross-platform: only git + path/filepath, no OS-specific calls.

// Context is the working-log key for a file: which repo/branch/base_sha it belongs
// to, plus the forward-slashed repo-relative path.
type Context struct {
	RepoRoot string
	Branch   string
	BaseSHA  string
	RelPath  string
}

// ResolveContext derives the Context for an absolute file path using git, or
// ok=false if the path is not inside a work tree. Detached HEAD and the
// pre-first-commit state get stable sentinel keys so the working log still has a
// home (and rotates when a real branch/commit appears).
func ResolveContext(absPath string) (Context, bool) {
	// Canonicalize symlinks on both sides before computing the relative path:
	// `git rev-parse --show-toplevel` returns the real path, so if the repo lives
	// under a symlinked prefix (e.g. macOS /var → /private/var, or any symlinked
	// checkout on Linux/Windows) an un-resolved absPath would make filepath.Rel
	// produce a "../.." path and wrongly look outside the work tree.
	if resolved, err := filepath.EvalSymlinks(absPath); err == nil {
		absPath = resolved
	}
	return ContextAt(gitutil.Locate(absPath), absPath)
}

// ContextAt is ResolveContext for a caller that already holds the path's
// gitutil.Location (a hook that needed its RepoID anyway), so the top level and
// HEAD aren't asked of git a second time; only the branch is.
func ContextAt(loc gitutil.Location, absPath string) (Context, bool) {
	if resolved, err := filepath.EvalSymlinks(absPath); err == nil {
		absPath = resolved
	}
	top := loc.Toplevel
	if top == "" {
		return Context{}, false
	}
	if resolved, err := filepath.EvalSymlinks(top); err == nil {
		top = resolved
	}
	rel, err := filepath.Rel(top, absPath)
	if err != nil || strings.HasPrefix(rel, "..") {
		return Context{}, false
	}
	branch := gitutil.BranchName(top)
	if branch == "" {
		branch = "DETACHED"
	}
	base := loc.HeadSHA
	if base == "" {
		base = "INITIAL" // repo has no commits yet
	}
	return Context{RepoRoot: top, Branch: branch, BaseSHA: base, RelPath: filepath.ToSlash(rel)}, true
}

// SeedHook, if set, is invoked before a file's first recorded edit to seed its
// working log from COMMITTED authorship (so unchanged committed lines keep their
// real author across a commit instead of defaulting to Human, I5). It is a hook —
// not a direct call — so the engine stays free of any git-notes dependency; a higher
// layer that can read notes registers it. Best-effort: it already no-ops when a log
// exists, and any error is ignored. Runs OUTSIDE Update's lock to avoid re-entry.
var SeedHook func(repoRoot, branch, baseSHA, relPath string)

// RecordEdit captures an observed POST-edit: it reads the file's current content
// and updates its working log, attributing the genuinely-changed lines to author.
// The first observed edit (no stored baseline) diffs against the file's HEAD
// content, so pre-existing committed lines are not mis-credited to the author.
func RecordEdit(absPath string, author Author) (*WorkingLog, error) {
	ctx, ok := ResolveContext(absPath)
	if !ok {
		return nil, fmt.Errorf("authorship: %q is not inside a git work tree", absPath)
	}
	return RecordEditIn(ctx, absPath, author)
}

// RecordEditIn is RecordEdit for a caller that already resolved the Context.
// The HEAD content (a git process) is read only on the file's first observed
// edit, the one case it is used.
func RecordEditIn(ctx Context, absPath string, author Author) (*WorkingLog, error) {
	if SeedHook != nil {
		SeedHook(ctx.RepoRoot, ctx.Branch, ctx.BaseSHA, ctx.RelPath)
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, err
	}
	fallback := func() string { return headContent(ctx.RepoRoot, ctx.BaseSHA, ctx.RelPath) }
	return update(ctx.RepoRoot, ctx.Branch, ctx.BaseSHA, ctx.RelPath, string(data), fallback, author, 0)
}

// PutBaselinesIfUntracked snapshots the current content of each repo-relative path
// under repoRoot as its pre-edit baseline, skipping any file Attribution already
// tracks (whose stored baseline is what its line attributions describe).
//
// The working-log key (branch + base SHA) is resolved ONCE for the whole batch
// rather than per file. That matters because the caller is a tool hook that runs
// on every single tool call: ResolveContext costs three git invocations, so a
// per-file resolve would spawn dozens of processes per call — worse than the extra
// hook process this whole path exists to avoid.
//
// Best-effort and silent: a baseline we fail to write only costs precision on the
// next edit, and this runs inside a hook that must never fail the host tool.
func PutBaselinesIfUntracked(repoRoot string, relPaths []string) {
	if !Enabled() || repoRoot == "" || len(relPaths) == 0 {
		return
	}
	top := repoRoot
	if resolved, err := filepath.EvalSymlinks(top); err == nil {
		top = resolved
	}
	branch := gitutil.BranchName(top)
	if branch == "" {
		branch = "DETACHED"
	}
	base := gitutil.HeadSHA(top)
	if base == "" {
		base = "INITIAL"
	}
	for _, rel := range relPaths {
		rel = filepath.ToSlash(rel)
		if wl, err := loadWorkingLogFile(WorkingLogPath(top, branch, base, rel)); err == nil && wl != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join(top, filepath.FromSlash(rel)))
		if err != nil {
			continue
		}
		_ = PutBaseline(top, branch, base, rel, string(data))
	}
}

// CaptureBaseline records the file's CURRENT content as the pre-edit baseline —
// the `record --pre` fallback (Decision B). Call it before a terminal agent writes
// a file the editor isn't tracking live; the next RecordEdit diffs against it.
func CaptureBaseline(absPath string) error {
	ctx, ok := ResolveContext(absPath)
	if !ok {
		return fmt.Errorf("authorship: %q is not inside a git work tree", absPath)
	}
	return CaptureBaselineIn(ctx, absPath)
}

// CaptureBaselineIn is CaptureBaseline for a caller that already resolved the
// Context.
func CaptureBaselineIn(ctx Context, absPath string) error {
	data, err := os.ReadFile(absPath)
	if err != nil {
		return err
	}
	return PutBaseline(ctx.RepoRoot, ctx.Branch, ctx.BaseSHA, ctx.RelPath, string(data))
}

// headContent returns relPath's content at baseSHA, or "" if the file is new
// (not in HEAD) or there is no commit yet. Used only as the first-edit fallback
// baseline; a real stored baseline always takes precedence in Update.
func headContent(repoRoot, baseSHA, relPath string) string {
	if baseSHA == "" || baseSHA == "INITIAL" {
		return ""
	}
	// git uses forward slashes in tree paths on every OS.
	out, err := gitutil.Output(repoRoot, "show", baseSHA+":"+relPath)
	if err != nil {
		return "" // file not present at HEAD → treat as new
	}
	return string(out)
}
