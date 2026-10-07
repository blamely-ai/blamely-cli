package gitutil

import (
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/blamely/blamely/internal/procattr"
)

// RepoID returns the canonical repository identifier for the file/dir at `p`.
//
// Why not just `git rev-parse --show-toplevel`? With linked worktrees, every
// worktree has its OWN top-level dir, so edits recorded under worktree A
// can't be joined against commits in worktree B even though they share the
// same logical repository.
//
// `git rev-parse --git-common-dir` returns the .git directory of the MAIN
// worktree (the same for every linked worktree). Stripping the trailing
// `/.git` gives us a stable repo path. Symlinks are resolved so macOS
// `/tmp → /private/tmp` doesn't sneak in here.
//
// Returns ("", false) if the path isn't inside a git repo (or git failed).
func RepoID(p string) (string, bool) {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	dir := p
	if fi, err := pathStat(p); err != nil || !fi.IsDir() {
		// A regular file, OR a path that doesn't exist yet — e.g. an AI `create_file`
		// whose transcript event is tailed before the file is flushed to disk. Resolve
		// from the parent directory (which exists and is in the same work tree); without
		// this `git -C <missing-file>` fails, RepoID returns "", and the brand-new file's
		// AI edit is silently dropped → it falls to Human at commit.
		dir = filepath.Dir(p)
	}
	out, err := procattr.Hide(exec.Command("git", "-C", dir, "rev-parse", "--path-format=absolute", "--git-common-dir")).Output()
	if err != nil {
		return "", false
	}
	return repoIDFromCommonDir(strings.TrimSpace(string(out)))
}

// repoIDFromCommonDir turns `rev-parse --git-common-dir` output into RepoID's
// result.
func repoIDFromCommonDir(gitDir string) (string, bool) {
	if gitDir == "" {
		return "", false
	}
	if r, err := filepath.EvalSymlinks(gitDir); err == nil {
		gitDir = r
	}
	// Strip the trailing "/.git" segment if present (the common dir for a
	// regular clone is .../<repo>/.git; for a bare repo it's just .../<repo>).
	if filepath.Base(gitDir) == ".git" {
		return filepath.Dir(gitDir), true
	}
	return gitDir, true
}

// Toplevel returns the working-tree root for `p`. Useful when callers
// genuinely want the worktree-local path (e.g. computing relative file
// paths for the diff parser).
func Toplevel(p string) (string, bool) {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	dir := p
	if fi, err := pathStat(p); err != nil || !fi.IsDir() {
		// See RepoID: descend to the parent dir for files and not-yet-written paths so a
		// streamed `create_file` resolves a correct repo-relative path instead of an
		// absolute one (which wouldn't match `git diff` at commit → Human).
		dir = filepath.Dir(p)
	}
	out, err := procattr.Hide(exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel")).Output()
	if err != nil {
		return "", false
	}
	return toplevelFromOutput(strings.TrimSpace(string(out))), true
}

func toplevelFromOutput(root string) string {
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	return root
}

// Location is what a hook needs to know about the repo holding a path:
// RepoID, Toplevel and HeadSHA, each exactly as those functions return it
// ("" where one fails).
type Location struct {
	RepoID   string
	Toplevel string
	HeadSHA  string
}

// Locate resolves RepoID, Toplevel and HeadSHA for p with ONE git process
// instead of three. Hooks run on every agent tool call and a git spawn is the
// dominant cost of one (worse on machines whose antivirus scans each process),
// so this matters. If the combined call fails — no commit yet, a bare repo, a
// file literally named HEAD — it falls back to the three separate calls, so
// the result is always the same as calling them one by one.
func Locate(p string) Location {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	dir := p
	if fi, err := pathStat(p); err != nil || !fi.IsDir() {
		dir = filepath.Dir(p) // see RepoID
	}
	out, err := procattr.Hide(exec.Command("git", "-C", dir, "rev-parse",
		"--path-format=absolute", "--git-common-dir", "--show-toplevel", "HEAD")).Output()
	if err == nil {
		if lines := strings.Split(strings.TrimSpace(string(out)), "\n"); len(lines) == 3 {
			id, ok := repoIDFromCommonDir(strings.TrimSpace(lines[0]))
			top := strings.TrimSpace(lines[1])
			head := strings.TrimSpace(lines[2])
			if ok && top != "" && head != "" {
				return Location{RepoID: id, Toplevel: toplevelFromOutput(top), HeadSHA: head}
			}
		}
	}
	var loc Location
	loc.RepoID, _ = RepoID(p)
	if top, ok := Toplevel(p); ok {
		loc.Toplevel = top
		loc.HeadSHA = HeadSHA(top)
	}
	return loc
}

// GitDir returns the checkout-local git directory. Unlike CommonDir, this is
// different for each linked worktree; .git may be a file rather than a directory.
// git prints the path with forward slashes on Windows too; Clean converts it to
// the native form the rest of this package returns.
func GitDir(root string) string {
	out, err := Output(root, "rev-parse", "--absolute-git-dir")
	if err == nil && strings.TrimSpace(string(out)) != "" {
		return filepath.Clean(strings.TrimSpace(string(out)))
	}
	return filepath.Join(root, ".git")
}
