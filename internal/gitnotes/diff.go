package gitnotes

import (
	"bufio"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/blamely/blamely/internal/config"
	"github.com/blamely/blamely/internal/tools"

	"github.com/blamely/blamely/internal/procattr"
)

// linesSimilar reports whether a removed line (del) and an added line (add) at
// the same position in a hunk are a line MODIFICATION — the same line reworked —
// rather than a real deletion plus an unrelated addition. True when they're
// whitespace/format-equal, or share enough text (common prefix + suffix ≥ ~half
// their combined length) that the add is clearly an edit of the del. This lets a
// hunk that deletes lines and adds DIFFERENT ones still report the deletions
// (matching `git --stat`), while a genuine reword stays collapsed to one
// authored line so simple edits aren't double-counted as +1/-1.
func linesSimilar(del, add string) bool {
	d := tools.NormalizeLineText(strings.TrimRight(del, "\r"))
	a := tools.NormalizeLineText(strings.TrimRight(add, "\r"))
	if d == a {
		return true // identical or whitespace/format-only change
	}
	if d == "" || a == "" {
		return false // one blank, the other not — not a modification
	}
	affix := commonAffixLen(d, a)
	// shared fraction = 2*affix / (len(d)+len(a)) ≥ 0.5  ⇔  4*affix ≥ len(d)+len(a)
	return 4*affix >= len(d)+len(a)
}

// commonAffixLen returns the combined length of the longest common prefix and
// the longest common suffix of a and b (bytewise; the two never overlap).
func commonAffixLen(a, b string) int {
	p := 0
	for p < len(a) && p < len(b) && a[p] == b[p] {
		p++
	}
	s := 0
	for s < len(a)-p && s < len(b)-p && a[len(a)-1-s] == b[len(b)-1-s] {
		s++
	}
	return p + s
}

// AddedLine is one line that was added (or modified) by the commit.
// LineNum is the 1-based line number in the POST-commit file.
// Content is the raw text of the line (without the leading '+'), stripped of
// trailing carriage returns. Used as a ContentSHA fallback in attribution so
// that Copilot/chat edits survive line-number drift caused by human edits made
// to the file after the AI applied them but before the commit.
type AddedLine struct {
	File    string
	LineNum int
	Content string
}

// DeletedLine is one line that was removed (and not replaced in-place) by the
// commit. LineNum is the 1-based line number in the PRE-commit file. Content
// is the raw text of the line (without the leading '-'), stripped of trailing
// carriage returns. Used by attribution to hash the removed line and match it
// against an AI edit's recorded edit_removed_lines, so AI-caused deletions can
// be distinguished from human ones.
type DeletedLine struct {
	LineNum int
	Content string
}

// FileChangeType is the file-level change kind: ADDED, DELETED, MODIFIED,
// RENAMED, or COPIED. Surfaced in the git note so consumers can render a
// per-file status without re-running `git diff`. Move is a special case of
// renamed (same name, different directory) — git doesn't expose it
// separately, so we report it as RENAMED.
type FileChangeType string

const (
	FileAdded    FileChangeType = "ADDED"
	FileDeleted  FileChangeType = "DELETED"
	FileModified FileChangeType = "MODIFIED"
	FileRenamed  FileChangeType = "RENAMED"
	FileCopied   FileChangeType = "COPIED"
)

// CommitChange is the diff result of a single commit: every added line, the
// per-file pre-image line numbers that were deleted, a post→pre rename map
// so the attribute step can look up edits recorded under the old filename,
// the file-level change kind keyed by post-commit path, and an analogous
// copy map for files git detected as copy-with-modifications.
type CommitChange struct {
	Added       []AddedLine
	Deleted     map[string][]DeletedLine  // pre-commit path → deleted lines (pre-image)
	Renames     map[string]string         // post-commit path → pre-commit path (renamed)
	Copies      map[string]string         // post-commit path → pre-commit path (copied)
	FileChanges map[string]FileChangeType // post-commit path → file-level change kind
}

// DeletedCount returns the per-file count derived from Deleted line numbers.
// Used by call sites that historically consumed map[string]int.
func (c *CommitChange) DeletedCount(file string) int {
	return len(c.Deleted[file])
}

// ChangedLines returns every added/modified line of `sha` in `repoPath`,
// using the post-commit line numbers.
func ChangedLines(repoPath, sha string) ([]AddedLine, error) {
	c, err := DiffCommit(repoPath, sha)
	if err != nil {
		return nil, err
	}
	return c.Added, nil
}

// DiffCommit is ChangedLines + rename tracking. Uses `git diff -M` so
// renames are surfaced as `rename from` / `rename to` headers instead of
// add+delete pairs.
//
// Files matching ~/.blamely/exclude or the repo's .gitignore are filtered
// out at parse time — they never appear in CommitChange and therefore
// never enter attribution or reports.
func DiffCommit(repoPath, sha string) (*CommitChange, error) {
	parent, hasParent, err := parentSHA(repoPath, sha)
	if err != nil {
		return nil, err
	}
	// -M rename detection only. We deliberately do NOT pass -C (copy detection):
	// when an AI Writes a brand-new file that happens to resemble an existing one
	// (e.g. contact.html created from register.html), -C makes git report it as a
	// COPY and emit only the handful of differing lines as "added" — so a 146-line
	// AI-authored file shows added=43 and the rest of the AI's work vanishes from
	// the note. Without -C the file is a normal new file with all 146 lines added,
	// matching `git show --stat` and attributing every line the AI wrote.
	// The first argument "-C repoPath" is git's own -C (change-directory) flag and
	// is unrelated to copy detection.
	var args []string
	if hasParent {
		args = []string{"-C", repoPath, "diff", "--unified=0", "--no-color", "-M", parent + ".." + sha}
	} else {
		emptyTree, err := emptyTreeSHA(repoPath)
		if err != nil {
			return nil, err
		}
		args = []string{"-C", repoPath, "diff", "--unified=0", "--no-color", "-M", emptyTree, sha}
	}
	cmd := procattr.Hide(exec.Command("git", args...))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("git diff: %w", err)
	}
	// Lenient exclude-list load: any error (file missing, permission denied)
	// yields a nil filter, which the parser treats as "exclude nothing". A
	// broken exclude config must never break attribution.
	excl, _ := config.LoadExcludeListForRepo(repoPath)
	out, perr := parseDiff(stdout, excl)
	_ = cmd.Wait()
	if perr != nil {
		return nil, perr
	}
	return out, nil
}

// trimDiffHeaderPathTab strips git's path-terminating TAB from a `--- a/` /
// `+++ b/` header path. Git appends a TAB after the filename in these headers
// when the name contains a space (to mark where the name ends), e.g.
// `+++ b/login page.html\t`. Left in, the trailing tab makes the post-commit
// path ("login page.html\t") fail to match the recorded AI edit's path
// ("login page.html"), so every line of a space-named file falls to Human even
// though an AI tool authored it. A filename containing a literal tab is
// git-quoted instead, so cutting at the first tab is safe.
func trimDiffHeaderPathTab(s string) string {
	if i := strings.IndexByte(s, '\t'); i >= 0 {
		return s[:i]
	}
	return s
}

// parseDiff reads `git diff --unified=0 -M -C` output and produces a
// CommitChange. Extracted from DiffCommit so the hunk-pairing logic can be
// exercised with hand-crafted diff strings in tests.
//
// Pairing semantics: within each hunk we buffer the `-` and `+` lines, then
// pair them positionally. The first min(len(-), len(+)) of each are treated
// as MODIFICATIONS — the `-` side is discarded (it would otherwise be
// reported as a "deletion treated as human" even though the change is an
// in-place replacement) and the `+` side is emitted as an added line subject
// to the whitespace filter. Excess `-` lines are pure deletes; excess `+`
// lines are pure adds. This is the only correct way to recognize modifications
// when an earlier hunk in the same file has shifted line numbers — comparing
// pre-image delete line numbers against post-image add line numbers
// produces false negatives the moment the file's net line count drifts.
func parseDiff(r io.Reader, excl *config.ExcludeList) (*CommitChange, error) {
	out := &CommitChange{
		Deleted:     map[string][]DeletedLine{},
		Renames:     map[string]string{},
		Copies:      map[string]string{},
		FileChanges: map[string]FileChangeType{},
	}
	curFile := ""    // post-commit path (from +++ b/...)
	curDelFile := "" // pre-commit path (from --- a/...)
	curLine := 0     // post-image line number for the next + or context line
	curDelLine := 0  // pre-image line number for the next - or context line
	// diffHeaderPath is the post-commit path inferred from `diff --git a/X b/Y`.
	// We use it as the FileChanges key for new/deleted/renamed/copied files
	// before the +++/--- lines arrive (or when one of them is /dev/null).
	diffHeaderPath := ""
	pendingRenameFrom := ""
	pendingCopyFrom := ""
	// skipFile is set when the current file matched an exclude rule. While
	// true, every subsequent line (hunk header, +/-, file-mode lines) is
	// ignored until the next `diff --git` header resets the flag. Excluded
	// files leave no trace in CommitChange.
	skipFile := false

	// Per-hunk buffers — see function comment for pairing semantics.
	type hunkAdd struct {
		line    int
		content string
	}
	type hunkDel struct {
		line    int
		content string
	}
	var hunkDels []hunkDel
	var hunkAdds []hunkAdd

	flushHunk := func() {
		n := len(hunkDels)
		if len(hunkAdds) < n {
			n = len(hunkAdds)
		}
		// Positionally pair the first n deletes with the first n adds. The add
		// side is ALWAYS counted (content the author wrote, blank lines included —
		// keeps totals consistent with `git show --stat`). The delete side is
		// dropped ONLY when the pair is a genuine MODIFICATION — the same line
		// reworked — judged by content similarity (linesSimilar). When the two are
		// dissimilar, the old line was really removed and a DIFFERENT one added at
		// that spot, so the delete is counted too. This stops an unrelated
		// delete+add in one hunk from hiding the deletion (it used to read as a
		// modification and silently drop every paired delete).
		for i := 0; i < n; i++ {
			a := hunkAdds[i]
			d := hunkDels[i]
			aContent := strings.TrimRight(a.content, "\r")
			dContent := strings.TrimRight(d.content, "\r")
			// Nothing was authored or removed here, so count NEITHER side;
			// otherwise the unchanged line is falsely reported as an addition.
			if eolOnlyChange(d.content, a.content) {
				continue
			}
			if curFile != "" {
				out.Added = append(out.Added, AddedLine{File: curFile, LineNum: a.line, Content: aContent})
			}
			// Dissimilar → a real delete + add; similar → modification (drop delete).
			if curDelFile != "" && !linesSimilar(d.content, a.content) {
				out.Deleted[curDelFile] = append(out.Deleted[curDelFile], DeletedLine{LineNum: d.line, Content: dContent})
			}
		}
		// Excess deletes — lines removed without a same-position replacement.
		for i := n; i < len(hunkDels); i++ {
			d := hunkDels[i]
			if curDelFile != "" {
				out.Deleted[curDelFile] = append(out.Deleted[curDelFile], DeletedLine{LineNum: d.line, Content: strings.TrimRight(d.content, "\r")})
			}
		}
		// Excess adds — lines added without a same-position predecessor.
		for i := n; i < len(hunkAdds); i++ {
			a := hunkAdds[i]
			if curFile != "" {
				out.Added = append(out.Added, AddedLine{File: curFile, LineNum: a.line, Content: strings.TrimRight(a.content, "\r")})
			}
		}
		hunkDels = hunkDels[:0]
		hunkAdds = hunkAdds[:0]
	}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<16), 1<<22)
	setFileChange := func(path string, kind FileChangeType) {
		if path == "" {
			return
		}
		// Don't downgrade a more-specific kind (ADDED/RENAMED) to MODIFIED.
		if existing, ok := out.FileChanges[path]; ok && existing != FileModified {
			return
		}
		out.FileChanges[path] = kind
	}
	for sc.Scan() {
		line := sc.Text()
		// File boundary: handle the `diff --git` header BEFORE the skip
		// gate so an excluded file can't carry skipFile into the next file.
		if strings.HasPrefix(line, "diff --git ") {
			// Flush the previous file's last hunk before its curFile /
			// curDelFile get overwritten.
			flushHunk()
			diffHeaderPath = postPathFromDiffHeader(line)
			pendingRenameFrom = ""
			pendingCopyFrom = ""
			// Reset the per-file skip flag and re-evaluate against the new
			// path. Excluded files leave no trace: no FileChanges entry,
			// no Added, no Deleted.
			skipFile = excl.Match(diffHeaderPath)
			if skipFile {
				// Also clear the working state so any trailing tokens from
				// the previous file (e.g. an unflushed +/- buffer slot)
				// don't get attributed to this excluded file.
				curFile = ""
				curDelFile = ""
				curLine = 0
				curDelLine = 0
				continue
			}
			if diffHeaderPath != "" {
				if _, ok := out.FileChanges[diffHeaderPath]; !ok {
					out.FileChanges[diffHeaderPath] = FileModified
				}
			}
			continue
		}
		if skipFile {
			continue
		}
		switch {
		case strings.HasPrefix(line, "new file mode"):
			setFileChange(diffHeaderPath, FileAdded)
		case strings.HasPrefix(line, "deleted file mode"):
			// Deleted files have no post-commit path; key by pre-commit path
			// which `diff --git a/X b/X` repeats on both sides.
			setFileChange(diffHeaderPath, FileDeleted)
		case strings.HasPrefix(line, "rename from "):
			pendingRenameFrom = strings.TrimPrefix(line, "rename from ")
		case strings.HasPrefix(line, "rename to "):
			to := strings.TrimPrefix(line, "rename to ")
			if pendingRenameFrom != "" {
				out.Renames[to] = pendingRenameFrom
				pendingRenameFrom = ""
			}
			setFileChange(to, FileRenamed)
		case strings.HasPrefix(line, "copy from "):
			pendingCopyFrom = strings.TrimPrefix(line, "copy from ")
		case strings.HasPrefix(line, "copy to "):
			to := strings.TrimPrefix(line, "copy to ")
			if pendingCopyFrom != "" {
				out.Copies[to] = pendingCopyFrom
				pendingCopyFrom = ""
			}
			setFileChange(to, FileCopied)
		case strings.HasPrefix(line, "--- a/"):
			curDelFile = trimDiffHeaderPathTab(strings.TrimPrefix(line, "--- a/"))
		case strings.HasPrefix(line, "--- /dev/null"):
			curDelFile = "" // new file — no pre-commit path
		case strings.HasPrefix(line, "+++ b/"):
			curFile = trimDiffHeaderPathTab(strings.TrimPrefix(line, "+++ b/"))
		case strings.HasPrefix(line, "+++ /dev/null"):
			curFile = "" // deleted file — no post-commit path
		case strings.HasPrefix(line, "@@"):
			// New hunk: flush the previous hunk's buffered lines before
			// resetting line counters.
			flushHunk()
			delStart, addStart, ok := parseHunkHeaderBothSides(line)
			if !ok {
				continue
			}
			curLine = addStart
			curDelLine = delStart
		case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
			if curLine > 0 {
				hunkAdds = append(hunkAdds, hunkAdd{line: curLine, content: line[1:]})
				curLine++
			}
		case strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---"):
			if curDelLine > 0 {
				hunkDels = append(hunkDels, hunkDel{line: curDelLine, content: line[1:]})
			}
			curDelLine++
		case strings.HasPrefix(line, " "):
			curLine++
			curDelLine++
		}
	}
	flushHunk()
	return out, sc.Err()
}

// eolOnlyChange reports whether a positionally paired -/+ line is the same line
// whose trailing newline changed: byte-identical on both sides, it appears only
// because of the "\ No newline at end of file" transition git emits when a line
// is appended after the old last line (which then gains a newline). Shared by
// the commit note (parseDiff) and the gutter (changedNewSideLines) so both agree.
func eolOnlyChange(del, add string) bool {
	return strings.TrimRight(del, "\r") == strings.TrimRight(add, "\r")
}

// postPathFromDiffHeader extracts POST from "diff --git a/PRE b/POST". POST
// may equal PRE for a simple modification; for renames the two differ and
// the dedicated `rename from`/`rename to` headers carry authoritative info.
// Returns "" on a malformed line.
func postPathFromDiffHeader(s string) string {
	const prefix = "diff --git "
	if !strings.HasPrefix(s, prefix) {
		return ""
	}
	rest := s[len(prefix):]
	i := strings.Index(rest, " b/")
	if i < 0 {
		return ""
	}
	return rest[i+len(" b/"):]
}

// parseHunkHeader extracts the "+start,len" from "@@ -a,b +start,len @@ ..."
// Length may be omitted (defaults to 1).
func parseHunkHeader(s string) (start, length int, ok bool) {
	// "@@ -1,2 +3,4 @@"
	const plus = " +"
	i := strings.Index(s, plus)
	if i < 0 {
		return 0, 0, false
	}
	rest := s[i+2:]
	j := strings.IndexAny(rest, " @")
	if j < 0 {
		return 0, 0, false
	}
	spec := rest[:j]
	if k := strings.IndexByte(spec, ','); k >= 0 {
		if _, err := fmt.Sscanf(spec, "%d,%d", &start, &length); err != nil {
			return 0, 0, false
		}
	} else {
		length = 1
		if _, err := fmt.Sscanf(spec, "%d", &start); err != nil {
			return 0, 0, false
		}
	}
	return start, length, true
}

// parseHunkHeaderBothSides returns both the pre-image and post-image starting
// line numbers from a "@@ -a,b +c,d @@" header. Either side's length may be
// omitted (defaults to 1). Returns ok=false if either side fails to parse.
func parseHunkHeaderBothSides(s string) (delStart, addStart int, ok bool) {
	const minus = "@@ -"
	i := strings.Index(s, minus)
	if i < 0 {
		return 0, 0, false
	}
	rest := s[i+len(minus):]
	j := strings.IndexByte(rest, ' ')
	if j < 0 {
		return 0, 0, false
	}
	delSpec := rest[:j]
	if k := strings.IndexByte(delSpec, ','); k >= 0 {
		if _, err := fmt.Sscanf(delSpec, "%d,%d", &delStart, new(int)); err != nil {
			return 0, 0, false
		}
	} else {
		if _, err := fmt.Sscanf(delSpec, "%d", &delStart); err != nil {
			return 0, 0, false
		}
	}
	add, _, addOK := parseHunkHeader(s)
	if !addOK {
		return 0, 0, false
	}
	return delStart, add, true
}

func parentSHA(repoPath, sha string) (string, bool, error) {
	out, err := procattr.Hide(exec.Command("git", "-C", repoPath, "rev-parse", sha+"^")).Output()
	if err != nil {
		// First commit has no parent.
		return "", false, nil
	}
	return strings.TrimSpace(string(out)), true, nil
}

func emptyTreeSHA(repoPath string) (string, error) {
	out, err := procattr.Hide(exec.Command("git", "-C", repoPath, "hash-object", "-t", "tree", "/dev/null")).Output()
	if err != nil {
		// Fallback to the well-known empty tree SHA-1 hash.
		return "4b825dc642cb6eb9a060e54bf8d69288fbee4904", nil
	}
	return strings.TrimSpace(string(out)), nil
}

// CommitMessage returns the full commit message body (subject + body) of sha.
// Empty string on error so callers can tolerate detached/missing state.
func CommitMessage(repoPath, sha string) string {
	out, err := procattr.Hide(exec.Command("git", "-C", repoPath, "show", "-s", "--format=%B", sha)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimRight(string(out), "\n")
}

// BranchName returns the currently checked-out branch name of repoPath. On
// detached HEAD or any failure it returns "" so the note can still be
// produced. This is called immediately after the commit, while HEAD still
// points to the new commit on the recording branch.
func BranchName(repoPath string) string {
	out, err := procattr.Hide(exec.Command("git", "-C", repoPath, "branch", "--show-current")).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func CommitTimestampNanos(repoPath, sha string) (int64, error) {
	out, err := procattr.Hide(exec.Command("git", "-C", repoPath, "show", "-s", "--format=%ct", sha)).Output()
	if err != nil {
		return 0, fmt.Errorf("git show: %w", err)
	}
	var secs int64
	if _, err := fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &secs); err != nil {
		return 0, fmt.Errorf("parse commit time: %w", err)
	}
	return secs * 1e9, nil
}

// lastFileTouchNanos returns the committer timestamp (nanoseconds) of the last
// commit reachable from ref that touched relPath, or 0 when no such commit
// exists (new file / empty ref / error). It is the PER-FILE lower bound for the
// added-line content reconcile: any content already committed necessarily
// touched the file, so a human retyping committed AI code stays out of the
// window — while AI edits recorded any time since the file last changed in
// history stay eligible. That widened (but still bounded) window is what lets a
// stash-pop→commit — whose edits predate the repo-wide previous-commit bound —
// keep its AI attribution.
func lastFileTouchNanos(repoPath, ref, relPath string) int64 {
	if ref == "" || relPath == "" {
		return 0
	}
	out, err := procattr.Hide(exec.Command("git", "-C", repoPath, "log", "-1", "--format=%ct", ref, "--", relPath)).Output()
	if err != nil {
		return 0
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return 0
	}
	var secs int64
	if _, err := fmt.Sscanf(s, "%d", &secs); err != nil {
		return 0
	}
	return secs * 1e9
}
