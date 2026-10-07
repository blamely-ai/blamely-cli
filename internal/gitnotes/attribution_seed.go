package gitnotes

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os/exec"
	"strconv"
	"strings"

	"github.com/blamely/blamely/internal/authorship"

	"github.com/blamely/blamely/internal/procattr"
)

// SeedCommittedWorkingLog writes a working log for relPath at (branch, baseSHA) that
// reflects COMMITTED per-line authorship: `git blame` finds the commit that last
// touched each line, and that commit's blamely note says whether the line was AI
// (and which tool/model/gen_type) or human. Seeding this before the first post-commit
// edit means unchanged committed lines keep their real author instead of defaulting
// to Human (I5). Registered as authorship.SeedHook (see internal/tools).
//
// Flag-gated; no-op when a log already exists or the file isn't present at baseSHA.
// Best-effort and cross-platform (git plumbing + path-agnostic string parsing).
func SeedCommittedWorkingLog(repoPath, branch, baseSHA, relPath string) error {
	if !authorship.Enabled() {
		return nil
	}
	if wl, err := authorship.LoadWorkingLog(repoPath, branch, baseSHA, relPath); err != nil || wl != nil {
		return err // already tracking (or read error) → don't seed
	}
	content, ok := showFileAt(repoPath, baseSHA, relPath)
	if !ok {
		return nil // not present at base (e.g. a brand-new file) → nothing to seed
	}
	authors := committedAuthorsByLine(repoPath, baseSHA, relPath)
	if authors == nil {
		return nil
	}
	return authorship.SeedWorkingLog(repoPath, branch, baseSHA, relPath, content, collapseAuthors(authors), 0)
}

// committedAuthorsByLine returns the committed author of each line of relPath at
// baseSHA, in order, via `git blame --porcelain` + the per-commit note. The notes for
// every distinct blame commit are fetched in ONE batch (see prefetchSeedNotes) rather
// than one `git notes show` per commit — the per-commit spawns were the dominant cost
// for files with long history, slow enough on Windows to trip the editor's timeout.
func committedAuthorsByLine(repoPath, baseSHA, relPath string) []authorship.Author {
	out, err := procattr.Hide(exec.Command("git", "-C", repoPath, "blame", "--porcelain", baseSHA, "--", relPath)).Output()
	if err != nil {
		return nil
	}
	// Pass 1: parse the blame into a per-output-line (commit sha, origin line) list and
	// collect the distinct commits.
	type lineRef struct {
		sha    string
		origin int
	}
	var refs []lineRef
	shas := map[string]bool{}
	var curSHA string
	var curOrigin int
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		if line[0] == '\t' { // the line's content; curSHA/curOrigin describe it
			refs = append(refs, lineRef{curSHA, curOrigin})
			shas[curSHA] = true
			curOrigin++
			continue
		}
		fields := strings.SplitN(line, " ", 4)
		if len(fields[0]) == 40 && isHex40(fields[0]) { // header: "<sha> <orig> <final> [count]"
			curSHA = fields[0]
			if len(fields) > 1 {
				curOrigin, _ = strconv.Atoi(fields[1])
			}
		}
	}
	// Pass 2: batch-fetch the notes, then resolve each line's author from them.
	notes := prefetchSeedNotes(repoPath, shas)
	authors := make([]authorship.Author, len(refs))
	for i, r := range refs {
		authors[i] = authorFromNote(notes[r.sha], r.origin, relPath)
	}
	return authors
}

// authorFromNote returns the author of originLine in relPath as recorded by a commit's
// (already-fetched) note: an AI range → that tool; otherwise Human.
func authorFromNote(note *Note, originLine int, relPath string) authorship.Author {
	if note != nil {
		for _, f := range note.Files {
			if f.Path != relPath {
				continue
			}
			for i := range f.Lines {
				r := &f.Lines[i]
				if r.Type != "add" || r.Tool == "" { // AI lines carry a tool
					continue
				}
				if originLine >= r.Start && originLine <= r.End {
					a := authorship.Author{Type: authorship.AI, Tool: r.Tool}
					if r.Model != nil {
						a.Model = *r.Model
					}
					if r.GenType != nil {
						a.GenType = *r.GenType
					}
					return a
				}
			}
		}
	}
	return authorship.HumanAuthor()
}

// collapseAuthors turns a per-line author slice into contiguous LineAttribution
// ranges (1-based), merging adjacent lines with the identical author.
func collapseAuthors(perLine []authorship.Author) []authorship.LineAttribution {
	var out []authorship.LineAttribution
	for i, a := range perLine {
		ln := i + 1
		if n := len(out); n > 0 && out[n-1].End == ln-1 && out[n-1].Author == a {
			out[n-1].End = ln
			continue
		}
		out = append(out, authorship.LineAttribution{Start: ln, End: ln, Author: a})
	}
	return out
}

// ShowFileAt is the exported wrapper used by the record-deletion command to read a
// file's committed content (the baseline that deletions are computed against).
func ShowFileAt(repoPath, sha, relPath string) (string, bool) {
	return showFileAt(repoPath, sha, relPath)
}

func showFileAt(repoPath, sha, relPath string) (string, bool) {
	out, err := procattr.Hide(exec.Command("git", "-C", repoPath, "show", sha+":"+relPath)).Output()
	if err != nil {
		return "", false
	}
	return string(out), true
}

// loadNoteForSeed reads and parses a single commit's blamely note (one `git notes
// show`). Used where exactly one note is needed (e.g. the inherited note on amend);
// the seeding path uses prefetchSeedNotes to batch many.
func loadNoteForSeed(repoPath, sha string) *Note {
	out, err := procattr.Hide(exec.Command("git", "-C", repoPath, "notes", "--ref="+NotesRef, "show", sha)).Output()
	if err != nil {
		return nil
	}
	var note Note
	if json.Unmarshal(out, &note) != nil {
		return nil
	}
	return &note
}

// prefetchSeedNotes fetches the blamely note for every commit in `shas` using TWO git
// calls total — `git notes list` (commit → note-blob map) and one `git cat-file
// --batch` for all needed note blobs — instead of a `git notes show` per commit. On
// Windows, collapsing N process spawns to 2 is what keeps the single-file seed under
// the editor's timeout. Returns commit sha → parsed note (commits without a note or
// with unparsable notes are simply absent → resolved as Human).
func prefetchSeedNotes(repoPath string, shas map[string]bool) map[string]*Note {
	notes := map[string]*Note{}
	if len(shas) == 0 {
		return notes
	}
	listOut, err := procattr.Hide(exec.Command("git", "-C", repoPath, "notes", "--ref="+NotesRef, "list")).Output()
	if err != nil {
		return notes // no notes ref / error → everything resolves to Human
	}
	commitToNoteObj := map[string]string{}
	var noteObjs []string
	sc := bufio.NewScanner(bytes.NewReader(listOut))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		f := strings.Fields(sc.Text()) // "<note-blob-sha> <commit-sha>"
		if len(f) == 2 && shas[f[1]] {
			commitToNoteObj[f[1]] = f[0]
			noteObjs = append(noteObjs, f[0])
		}
	}
	blobs := batchCatFileBlobs(repoPath, noteObjs)
	for commit, obj := range commitToNoteObj {
		data, ok := blobs[obj]
		if !ok {
			continue
		}
		var n Note
		if json.Unmarshal(data, &n) == nil {
			nn := n
			notes[commit] = &nn
		}
	}
	return notes
}

// batchCatFileBlobs reads the contents of the given git objects in ONE
// `git cat-file --batch` invocation, returning object sha → raw content. Missing
// objects are skipped. The --batch stream is `<sha> <type> <size>\n<content>\n`
// repeated; `<sha> missing\n` for absent objects.
func batchCatFileBlobs(repoPath string, objs []string) map[string][]byte {
	res := map[string][]byte{}
	if len(objs) == 0 {
		return res
	}
	cmd := procattr.Hide(exec.Command("git", "-C", repoPath, "cat-file", "--batch"))
	cmd.Stdin = strings.NewReader(strings.Join(objs, "\n") + "\n")
	out, err := cmd.Output()
	if err != nil && len(out) == 0 {
		return res
	}
	i := 0
	for i < len(out) {
		nl := bytes.IndexByte(out[i:], '\n')
		if nl < 0 {
			break
		}
		header := string(out[i : i+nl])
		i += nl + 1
		parts := strings.Fields(header)
		if len(parts) < 3 { // "<sha> missing" → no content follows
			continue
		}
		size, perr := strconv.Atoi(parts[2])
		if perr != nil || i+size > len(out) {
			break
		}
		res[parts[0]] = append([]byte(nil), out[i:i+size]...)
		i += size
		if i < len(out) && out[i] == '\n' { // trailing newline after content
			i++
		}
	}
	return res
}

func isHex40(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// UncommittedAddedLines returns the post-image line numbers of relPath that differ
// from HEAD in the working tree (`git diff HEAD`), i.e. the current uncommitted
// CHANGES. The gutter intersects working-log authorship with this set so it marks
// only changed lines (not every committed line). Empty/no diff → empty set.
func UncommittedAddedLines(repoPath, relPath string) map[int]bool {
	set := map[int]bool{}
	out, err := procattr.Hide(exec.Command("git", "-C", repoPath, "diff", "HEAD", "--unified=0", "--no-color", "--", relPath)).Output()
	if err != nil {
		return set
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "@@") {
			addNewSideHunk(line, set)
		}
	}
	return set
}

// addNewSideHunk parses a unified-diff hunk header `@@ -a,b +c,d @@` and marks the
// new-side range c..c+d-1 in set.
func addNewSideHunk(line string, set map[int]bool) {
	plus := strings.Index(line, "+")
	if plus < 0 {
		return
	}
	seg := line[plus+1:]
	if sp := strings.IndexByte(seg, ' '); sp >= 0 {
		seg = seg[:sp]
	}
	start, count := 0, 1
	if comma := strings.IndexByte(seg, ','); comma >= 0 {
		start, _ = strconv.Atoi(seg[:comma])
		count, _ = strconv.Atoi(seg[comma+1:])
	} else {
		start, _ = strconv.Atoi(seg)
	}
	for i := 0; i < count; i++ {
		set[start+i] = true
	}
}

// UncommittedAddedLinesAll is the batched form of UncommittedAddedLines: ONE
// `git diff HEAD` for the whole repo, parsed into relPath → changed line set. The
// repo-wide `--all` gutter path uses this instead of diffing each working log
// separately — one git subprocess instead of one per file (the dominant cost on
// Windows). `core.quotepath=false` keeps non-ASCII paths literal so they match the
// working-log File keys. A file absent from the result simply has no current changes.
func UncommittedAddedLinesAll(repoPath string) map[string]map[int]bool {
	byFile := map[string]map[int]bool{}
	out, err := procattr.Hide(exec.Command("git", "-C", repoPath, "-c", "core.quotepath=false",
		"diff", "HEAD", "--unified=0", "--no-color")).Output()
	if err != nil {
		return byFile
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var cur map[int]bool
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "+++ "):
			// New-side file header: `+++ b/<path>` (or `+++ /dev/null` for a deletion).
			p := strings.TrimPrefix(line, "+++ ")
			if p == "/dev/null" {
				cur = nil
				continue
			}
			cur = map[int]bool{}
			byFile[strings.TrimPrefix(p, "b/")] = cur
		case strings.HasPrefix(line, "@@") && cur != nil:
			addNewSideHunk(line, cur)
		}
	}
	return byFile
}

// UntrackedFiles is the batched form of IsUntracked: ONE
// `git ls-files --others --exclude-standard` for the whole repo, returning the set of
// untracked-and-not-ignored paths. The `--all` path resolves this once instead of
// probing each file separately.
func UntrackedFiles(repoPath string) map[string]bool {
	set := map[string]bool{}
	out, err := procattr.Hide(exec.Command("git", "-C", repoPath, "-c", "core.quotepath=false",
		"ls-files", "--others", "--exclude-standard")).Output()
	if err != nil {
		return set
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		if p := strings.TrimSpace(sc.Text()); p != "" {
			set[p] = true
		}
	}
	return set
}

// IsUntracked reports whether relPath exists in the working tree but is NOT tracked
// by git and is not ignored — i.e. a brand-new file. Such files never appear in
// `git diff HEAD` (they're absent from both HEAD and the index until `git add`), so
// UncommittedAddedLines returns an empty set for them; callers use this to fall back
// to "every line changed" instead of showing a blank gutter. `git ls-files --others
// --exclude-standard` lists exactly the untracked-and-not-ignored paths.
func IsUntracked(repoPath, relPath string) bool {
	out, err := procattr.Hide(exec.Command("git", "-C", repoPath, "ls-files", "--others", "--exclude-standard", "--", relPath)).Output()
	if err != nil {
		return false
	}
	return len(bytes.TrimSpace(out)) > 0
}
