package gitutil

import (
	"path/filepath"
	"runtime"
	"strings"
)

// Rel returns p's path relative to the work tree rooted at wt — the path git
// itself uses for the file, which is what commit-time attribution matches
// against (`git diff` names files relative to the top level).
//
// filepath.Rel is enough when both paths spell the same prefix. They don't when
// the work tree is reached through two aliases: git reports the top level as
// the volume's real path while the editor hands us the path the user opened.
// Seen on Windows profiles that live on a mounted volume (VMware App Volumes:
// `C:\SnapVolumesTemp\MountPoints\{…}\SVROOT\Users\<u>\…` from git against
// `C:\Users\<u>\…` from VS Code), where Go's EvalSymlinks does not follow the
// mount's reparse point. Every lexical attempt then yields `..`, and the old
// code fell back to the ABSOLUTE path — which never matched the committed
// relative path, so every edit in the repo was attributed to Human.
//
// So after the lexical attempts, Rel asks git: `rev-parse --show-prefix` from
// the nearest existing ancestor of p gives the repo-relative directory however
// the caller spelled it, and the top level reported alongside confirms it is
// the same work tree as wt. The git process is spawned only on that last
// resort, so the common case costs nothing extra.
//
// ok is false when p is not inside wt's work tree (or wt/p is empty). The
// result uses native separators, like filepath.Rel.
func Rel(wt, p string) (string, bool) {
	if wt == "" || p == "" {
		return "", false
	}
	if r, ok := lexicalRel(wt, p); ok {
		return r, true
	}
	if r, ok := lexicalRel(resolveExisting(wt), resolveExisting(p)); ok {
		return r, true
	}
	anc, rest := existingAncestor(p)
	if anc == "" {
		return "", false
	}
	out, err := Output(anc, "rev-parse", "--path-format=absolute", "--show-toplevel", "--show-prefix")
	if err != nil {
		return "", false
	}
	lines := strings.SplitN(strings.TrimRight(string(out), "\r\n"), "\n", 2)
	if len(lines) != 2 {
		return "", false
	}
	top := toplevelFromOutput(strings.TrimSpace(lines[0]))
	if top == "" || !sameDir(top, wt) {
		return "", false
	}
	return relFromPrefix(strings.TrimSpace(lines[1]), rest), true
}

// Rel is Rel(l.Toplevel, p) that first tries the prefix the Locate call
// already fetched, so a hook needs no extra git process even on an aliased
// work tree. Only p's leaf can differ from the directory Locate was asked
// about (Locate resolves a file through its parent), so the stored prefix is
// reused when p is that directory or an entry directly inside it; anything
// else goes through Rel.
func (l Location) Rel(p string) (string, bool) {
	if l.Toplevel == "" || p == "" {
		return "", false
	}
	if r, ok := lexicalRel(l.Toplevel, p); ok {
		return r, true
	}
	if l.PrefixKnown && l.PrefixDir != "" {
		if fi, err := pathStat(p); err == nil && fi.IsDir() {
			if sameDir(p, l.PrefixDir) {
				return relFromPrefix(l.Prefix, ""), true
			}
		} else if sameDir(filepath.Dir(p), l.PrefixDir) {
			return relFromPrefix(l.Prefix, filepath.Base(p)), true
		}
	}
	return Rel(l.Toplevel, p)
}

// lexicalRel is filepath.Rel restricted to paths inside base.
func lexicalRel(base, p string) (string, bool) {
	r, err := filepath.Rel(base, p)
	if err != nil || filepath.IsAbs(r) || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", false
	}
	return r, true
}

// relFromPrefix joins git's `--show-prefix` output (slash-separated, with a
// trailing slash, empty at the top level) with the remainder below the
// directory the prefix describes. "." for the top level itself.
func relFromPrefix(prefix, rest string) string {
	r := filepath.Join(filepath.FromSlash(prefix), rest)
	if r == "" {
		return "."
	}
	return r
}

// existingAncestor returns the nearest existing directory at or above p, and
// p's path below it (empty when p itself is an existing directory). A file
// not yet written, or a whole new subtree, still resolves through the first
// ancestor git can be asked about.
func existingAncestor(p string) (anc, rest string) {
	p = filepath.Clean(p)
	if fi, err := pathStat(p); err == nil && fi.IsDir() {
		return p, ""
	}
	for cur, parent := p, filepath.Dir(p); parent != cur; cur, parent = parent, filepath.Dir(parent) {
		if fi, err := pathStat(parent); err == nil && fi.IsDir() {
			r, err := filepath.Rel(parent, p)
			if err != nil {
				return "", ""
			}
			return parent, r
		}
	}
	return "", ""
}

// resolveExisting is EvalSymlinks that also works for a path whose leaf does
// not exist yet: the nearest existing ancestor is resolved and the remainder
// re-attached, so a not-yet-written file under a symlinked checkout still
// compares equal to its real-path spelling.
func resolveExisting(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	anc, rest := existingAncestor(p)
	if anc == "" {
		return p
	}
	if r, err := filepath.EvalSymlinks(anc); err == nil {
		anc = r
	}
	return filepath.Join(anc, rest)
}

// sameDir reports whether a and b name the same directory, after cleaning and
// symlink resolution; Windows paths compare case-insensitively, as the file
// system does.
func sameDir(a, b string) bool {
	a, b = filepath.Clean(resolveExisting(a)), filepath.Clean(resolveExisting(b))
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// showPrefix returns `git rev-parse --show-prefix` for dir (an existing
// directory): the repo-relative path of dir with a trailing slash, or "" at
// the top level. ok is false outside a work tree.
func showPrefix(dir string) (string, bool) {
	out, err := Output(dir, "rev-parse", "--show-prefix")
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}
