package tools

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// LineRange is a 1-based, inclusive line range together with the SHA of the
// new text. Used as an anchor for re-locating the range after later edits.
type LineRange struct {
	Start          int
	End            int
	ContentSHA     string
	ContentSHANorm string
}

// DeletedLineHash is the content hash of a single line removed by an edit —
// the deletion-side counterpart to LineRange's ContentSHA/ContentSHANorm.
// Blank/whitespace-only lines are never hashed (both fields stay "").
type DeletedLineHash struct {
	ContentSHA     string
	ContentSHANorm string
}

// LocateNewString finds where `newString` lives in `filePath` and returns
// its post-edit line range. If newString is empty (pure deletion) or not
// found, returns (nil, nil). Multi-occurrence matches return the first one.
//
// The match ignores line endings: CRLF is folded to LF on both sides before
// searching, so the result is the same whether the file on disk is LF
// (macOS/Linux) or CRLF (Windows, a core.autocrlf=true checkout, eol=crlf) and
// whichever ending the tool's payload carries. Agents typically send an LF
// new_string while keeping the file's CRLF on disk; a raw byte search missed
// that, so the edit was stored with no line hashes and — once its working log
// was gone — its lines were committed as Human. Folding drops only the \r of
// each \r\n, never a \n, so the line numbers counted below are unchanged, and
// ContentSHA covers the folded text, like the per-line hashes that strip a
// trailing \r.
func LocateNewString(filePath, newString string) (*LineRange, error) {
	if newString == "" {
		return nil, nil
	}
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", filePath, err)
	}
	data = foldCRLF(data)
	newString = strings.ReplaceAll(newString, "\r\n", "\n")
	idx := bytes.Index(data, []byte(newString))
	if idx < 0 {
		return nil, nil
	}
	startLine := 1 + bytes.Count(data[:idx], []byte("\n"))
	end := idx + len(newString)
	// Trailing newline on newString means its last line is the one BEFORE the next newline.
	trimmed := strings.TrimRight(newString, "\n")
	endLine := startLine + strings.Count(trimmed, "\n")
	_ = end
	return &LineRange{
		Start:          startLine,
		End:            endLine,
		ContentSHA:     sha256Hex([]byte(trimmed)),
		ContentSHANorm: sha256HexNorm(trimmed),
	}, nil
}

// LineRangeForWholeFile returns one 1-based, single-line LineRange per line of
// a full-file Write, each carrying that line's own content SHA — the same
// per-line CONTENT convention UnifiedDiffAddedRanges uses (sha256 of the line
// text with any trailing \r stripped). A single combined whole-file hash can
// never match a per-line lookup, which left every "create a new file" event
// unattributable (skipped by the line-based path because it carried a
// content_sha, but invisible to the content_sha path because its hash covered
// the whole blob, not one line) — see e.g. Codex `patch_apply: "add"` events.
// Per-line hashes also survive later partial edits: lines that remain
// untouched keep matching by hash even after the file's line numbers shift.
func LineRangeForWholeFile(filePath string) ([]LineRange, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", filePath, err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<16), 1<<24)
	var out []LineRange
	ln := 0
	for sc.Scan() {
		ln++
		text := strings.TrimRight(sc.Text(), "\r")
		out = append(out, LineRange{Start: ln, End: ln, ContentSHA: sha256Hex([]byte(text)), ContentSHANorm: sha256HexNorm(text)})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan %s: %w", filePath, err)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// foldCRLF returns b with every CRLF folded to LF. Only the \r of a \r\n pair is
// dropped, so line boundaries — and every line number counted by \n — are the
// same before and after. A lone \r is left alone. Returns b itself when it holds
// no CRLF.
func foldCRLF(b []byte) []byte {
	if !bytes.Contains(b, []byte("\r\n")) {
		return b
	}
	return bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// NormalizeLineText collapses a line's leading/trailing and internal
// whitespace runs to single spaces. Used to compute a fallback content hash
// that survives autoformatter reflows (reindentation, trailing-comma /
// quote-style changes that don't alter whitespace runs, line wrapping) which
// would otherwise change a line's exact bytes and break the primary
// content_sha match, falsely demoting AI-written lines to "Human".
func NormalizeLineText(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// sha256HexNorm returns the hex SHA-256 of the normalized line text, or ""
// for blank/whitespace-only lines — mirroring the content_sha convention so
// blank lines don't spuriously collide across the whole file.
func sha256HexNorm(text string) string {
	norm := NormalizeLineText(text)
	if norm == "" {
		return ""
	}
	return sha256Hex([]byte(norm))
}

// unifiedDiffHunkHeaderRe matches a unified-diff hunk header and captures the
// new file's starting line number, e.g. "@@ -135,6 +135,16 @@" -> "135".
var unifiedDiffHunkHeaderRe = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// UnifiedDiffAddedRanges walks a unified diff body and returns one per-line
// LineRange (with its content's SHA) for every `+` line, anchored at its true
// post-patch line number — derived by tracking each hunk's starting line and
// advancing through context (` `) and added (`+`) lines (removed `-` lines
// don't exist in the new file, so they don't advance the counter). suggested
// is the count of added lines, i.e. len(ranges).
//
// Only `+` lines are attributed — context and removed lines carry no new
// AI-written content. Per-line ranges (vs. one coarse span) are what let
// attribution survive later edits: each line is re-located by its content
// hash rather than by position.
func UnifiedDiffAddedRanges(diff string) (ranges []LineRange, suggested int64) {
	newLine := 0
	for _, raw := range strings.Split(diff, "\n") {
		if hm := unifiedDiffHunkHeaderRe.FindStringSubmatch(raw); hm != nil {
			if n, err := strconv.Atoi(hm[1]); err == nil {
				newLine = n
			}
			continue
		}
		if newLine == 0 {
			continue // narrative text before the first hunk header
		}
		switch {
		case strings.HasPrefix(raw, "+"):
			text := strings.TrimRight(raw[1:], "\r")
			ranges = append(ranges, LineRange{
				Start: newLine, End: newLine,
				ContentSHA:     sha256Hex([]byte(text)),
				ContentSHANorm: sha256HexNorm(text),
			})
			suggested++
			newLine++
		case strings.HasPrefix(raw, "-"):
			// removed from the old file — doesn't advance the new-file counter
		default:
			newLine++ // context line (unified diff prefixes these with a space)
		}
	}
	return ranges, suggested
}

// UnifiedDiffRemovedLineHashes walks a unified diff and returns a content hash
// for every line genuinely removed by the patch — i.e. "-" lines not paired
// with identical-content "+"/context lines elsewhere in the same hunk. Per
// hunk, it reconstructs the "old" text (context + "-" lines, in order) and the
// "new" text (context + "+" lines, in order) and delegates to
// RemovedLineHashes: shared context lines cover each other and only the truly
// removed lines are reported, mirroring the pairing semantics
// gitnotes/diff.go's flushHunk uses to classify excess "-" lines as deletions.
func UnifiedDiffRemovedLineHashes(diff string) []DeletedLineHash {
	var out []DeletedLineHash
	var oldBuf, newBuf []string
	inHunk := false
	flush := func() {
		if inHunk && len(oldBuf) > 0 {
			out = append(out, RemovedLineHashes(strings.Join(oldBuf, "\n"), strings.Join(newBuf, "\n"))...)
		}
		oldBuf, newBuf = nil, nil
	}
	for _, raw := range strings.Split(diff, "\n") {
		if strings.HasPrefix(raw, "@@") {
			flush()
			inHunk = true
			continue
		}
		if !inHunk {
			continue
		}
		switch {
		case strings.HasPrefix(raw, "+"):
			newBuf = append(newBuf, strings.TrimRight(raw[1:], "\r"))
		case strings.HasPrefix(raw, "-"):
			oldBuf = append(oldBuf, strings.TrimRight(raw[1:], "\r"))
		default:
			text := strings.TrimRight(strings.TrimPrefix(raw, " "), "\r")
			oldBuf = append(oldBuf, text)
			newBuf = append(newBuf, text)
		}
	}
	flush()
	return out
}

// narrowToChangedLines diffs oldStr against newStr, finds the line ranges
// within newStr whose content didn't exist in oldStr, and translates those
// (1-based, relative to newStr) into absolute file-line ranges using the
// already-located range of newStr in the file (`full`).
//
// Use case: an AI Edit's new_string usually carries a few unchanged context
// lines around the actual change so the patch matches uniquely. We don't
// want those context lines credited to the AI — only the genuinely new
// lines. The returned `suggested` count is the total number of lines marked
// new (sum of the returned ranges' sizes). Falls back to crediting the whole
// located span when the strings share no new lines (no-op / reorder).
//
// Crucially the output is emitted ONE RANGE PER LINE, each non-blank line
// carrying its own content_sha (sha256 of the line text, sans trailing \r) —
// exactly like the Write path's perLineShaRangesFromContent. This is what makes
// attribution survive line drift: when the user later inserts a line inside the
// AI-edited block, every AI line is re-located by hashing the CURRENT text and
// matching the stored sha at its new position, while the user's freshly-typed
// line (no matching sha) is correctly human. A coarse multi-line range with no
// sha matches purely by position, so an inserted line lands inside the range and
// is mislabelled AI. The note still stores collapsed ranges (collapseToRanges),
// but the recorded edit — and thus attribution — is line-by-line.
func narrowToChangedLines(oldStr, newStr string, full LineRange) ([]LineRange, int64) {
	newLines := strings.Split(newStr, "\n")
	if n := len(newLines); n > 0 && newLines[n-1] == "" {
		newLines = newLines[:n-1] // drop trailing empty from a final newline
	}
	offset := full.Start - 1

	// emit expands a 1-based [relStart, relEnd] span (relative to newStr) into
	// per-line absolute ranges, each carrying the line's content_sha.
	emit := func(relStart, relEnd int) []LineRange {
		var rs []LineRange
		for rel := relStart; rel <= relEnd; rel++ {
			abs := offset + rel
			if abs > full.End {
				break
			}
			text := ""
			if rel-1 >= 0 && rel-1 < len(newLines) {
				text = strings.TrimRight(newLines[rel-1], "\r")
			}
			sha := ""
			shaNorm := ""
			if strings.TrimSpace(text) != "" {
				sha = sha256Hex([]byte(text))
				shaNorm = sha256HexNorm(text)
			}
			rs = append(rs, LineRange{Start: abs, End: abs, ContentSHA: sha, ContentSHANorm: shaNorm})
		}
		return rs
	}

	// LCS diff (not multiset) so an AI move/reorder within the replaced region is
	// credited at the line's new position rather than dropped as "unchanged".
	changed := addedOrMovedLineRanges([]byte(oldStr), []byte(newStr))
	if len(changed) == 0 {
		// No genuinely-new lines vs old_string. Credit the whole located span,
		// still line-by-line so it can't be matched positionally.
		out := emit(1, full.End-offset)
		return out, int64(len(out))
	}

	var out []LineRange
	var suggested int64
	for _, r := range changed {
		rs := emit(r.Start, r.End)
		out = append(out, rs...)
		suggested += int64(len(rs))
	}
	return out, suggested
}

// ResolveWholeFileWrite is the single, tool-agnostic resolution for a whole-file
// overwrite (Write / write_file / Copilot CLI write): given the repo+file and the
// new full content, it fetches the pre-write snapshot and returns the added lines
// narrowed to what genuinely changed plus the removed-line hashes. When no
// snapshot exists it returns `fallback` (the caller's whole-file ranges) and no
// removals, since there is no baseline to narrow against.
//
// Every whole-file parser MUST route through here so the "unchanged/human lines an
// overwrite re-emitted are not AI" rule is applied identically — instead of each
// parser re-deriving (and, as happened, some omitting) it.
func ResolveWholeFileWrite(repoPath, filePath, newContent string, fallback []LineRange) (added []LineRange, removed []DeletedLineHash) {
	snapshot, ok := fetchSnapshot(repoPath, filePath)
	if !ok {
		return fallback, nil
	}
	return narrowWholeFileAddedLines(snapshot, newContent), RemovedLineHashes(snapshot, newContent)
}

// narrowWholeFileAddedLines narrows a WHOLE-FILE write (newStr is the entire new
// file content) down to the lines genuinely new or moved vs oldStr (the pre-write
// content), each carrying its content_sha so attribution survives drift. Unlike
// narrowToChangedLines it has NO "credit the whole span when nothing changed"
// fallback: a write whose content equals the prior file yields zero AI lines.
//
// Shared by every parser that receives whole-file content (Claude Write, Copilot
// CLI record, …): without it those parsers credit EVERY line — including unchanged
// and human-typed lines the agent merely re-emitted — to the AI. Returns line
// numbers as positions within newStr.
func narrowWholeFileAddedLines(oldStr, newStr string) []LineRange {
	newLines := strings.Split(newStr, "\n")
	if n := len(newLines); n > 0 && newLines[n-1] == "" {
		newLines = newLines[:n-1] // drop trailing empty from a final newline
	}
	var out []LineRange
	for _, r := range addedOrMovedLineRanges([]byte(oldStr), []byte(newStr)) {
		for rel := r.Start; rel <= r.End; rel++ {
			if rel-1 < 0 || rel-1 >= len(newLines) {
				continue
			}
			text := strings.TrimRight(newLines[rel-1], "\r")
			if strings.TrimSpace(text) == "" {
				continue
			}
			out = append(out, LineRange{
				Start: rel, End: rel,
				ContentSHA:     sha256Hex([]byte(text)),
				ContentSHANorm: sha256HexNorm(text),
			})
		}
	}
	return out
}

// CountAddedLines returns the number of net-new lines in newStr vs oldStr —
// i.e. the same metric narrowToChangedLines yields when an absolute file
// location is unavailable. Used by Edit/MultiEdit handlers to still credit
// the AI with `suggested_lines` even when LocateNewString fails (file not
// on disk yet, etc.).
func CountAddedLines(oldStr, newStr string) int64 {
	var n int64
	for _, r := range AddedOrChangedRanges([]byte(oldStr), []byte(newStr)) {
		n += int64(r.End - r.Start + 1)
	}
	return n
}

// AddedOrChangedRanges returns the 1-based line ranges in `newContent` that
// were NOT present (by exact content) in `oldContent`. Used by the human-edit
// watcher to detect lines the user typed/modified between two file snapshots.
//
// Semantics: each occurrence of a line in `oldContent` "covers" exactly one
// occurrence of the same content in `newContent` (left-to-right, greedy).
// Any new occurrence beyond the old count is treated as changed/added.
// Consecutive changed lines are collapsed into a single LineRange.
//
// This multiset approach correctly detects duplicate lines: if the old file
// has two closing braces and the new file has three, the extra brace is
// flagged. The old set-membership approach missed this and silently dropped
// human-typed lines whose content already existed elsewhere in the file.
func AddedOrChangedRanges(oldContent, newContent []byte) []LineRange {
	oldLines := bytes.Split(oldContent, []byte{'\n'})
	newLines := bytes.Split(newContent, []byte{'\n'})
	// Drop a trailing empty element produced by a final newline.
	if n := len(oldLines); n > 0 && len(oldLines[n-1]) == 0 {
		oldLines = oldLines[:n-1]
	}
	if n := len(newLines); n > 0 && len(newLines[n-1]) == 0 {
		newLines = newLines[:n-1]
	}
	// Count how many times each line content appears in the old file.
	remaining := make(map[string]int, len(oldLines))
	for _, l := range oldLines {
		remaining[string(l)]++
	}
	var out []LineRange
	curStart := 0
	for i, l := range newLines {
		lineNum := i + 1
		key := string(l)
		if remaining[key] > 0 {
			// This occurrence is "covered" by an old-file occurrence.
			remaining[key]--
			if curStart != 0 {
				out = append(out, LineRange{Start: curStart, End: lineNum - 1})
				curStart = 0
			}
		} else {
			// Extra occurrence — this line is new or duplicated beyond old count.
			if curStart == 0 {
				curStart = lineNum
			}
		}
	}
	if curStart != 0 {
		out = append(out, LineRange{Start: curStart, End: len(newLines)})
	}
	return out
}

// addedOrMovedLineRanges returns the 1-based line ranges in newContent that an
// LCS (longest-common-subsequence) line diff does NOT match against oldContent —
// i.e. genuinely new lines AND lines the AI MOVED/reordered. This is the
// narrowing primitive for whole-file/region rewrites.
//
// It differs from AddedOrChangedRanges (multiset) in exactly the move case: the
// multiset diff counts content occurrences, so a line that merely changed
// position (same content, count unchanged) is treated as "covered" and dropped —
// which silently drops AI moves (e.g. the agent relocates an <h1> down the file),
// leaving the moved line to fall through to Human at commit time. LCS preserves
// order, so a relocated line breaks the common subsequence and is correctly
// reported at its new position, while lines that only SHIFTED (same content, same
// relative order, due to inserts/deletes above) stay in the subsequence and are
// NOT over-credited.
//
// Falls back to the multiset diff for very large inputs to bound the O(n*m) DP.
func addedOrMovedLineRanges(oldContent, newContent []byte) []LineRange {
	oldLines := bytes.Split(oldContent, []byte{'\n'})
	newLines := bytes.Split(newContent, []byte{'\n'})
	if n := len(oldLines); n > 0 && len(oldLines[n-1]) == 0 {
		oldLines = oldLines[:n-1]
	}
	if n := len(newLines); n > 0 && len(newLines[n-1]) == 0 {
		newLines = newLines[:n-1]
	}
	n, m := len(oldLines), len(newLines)
	// Bound memory/time: above ~3k×3k lines fall back to the multiset diff.
	if n == 0 || m == 0 || n*m > 9_000_000 {
		return AddedOrChangedRanges(oldContent, newContent)
	}

	// dp[i][j] = LCS length of oldLines[i:] and newLines[j:].
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if bytes.Equal(oldLines[i], newLines[j]) {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	// Walk the DP: a newLine matched in the LCS is unchanged; the rest are
	// added/moved.
	matched := make([]bool, m)
	i, j := 0, 0
	for i < n && j < m {
		if bytes.Equal(oldLines[i], newLines[j]) {
			matched[j] = true
			i++
			j++
		} else if dp[i+1][j] >= dp[i][j+1] {
			i++
		} else {
			j++
		}
	}
	var out []LineRange
	curStart := 0
	for k := 0; k < m; k++ {
		if !matched[k] {
			if curStart == 0 {
				curStart = k + 1
			}
		} else if curStart != 0 {
			out = append(out, LineRange{Start: curStart, End: k})
			curStart = 0
		}
	}
	if curStart != 0 {
		out = append(out, LineRange{Start: curStart, End: m})
	}
	return out
}

// RemovedLineHashes returns a content hash for every line present in oldStr
// but not in newStr — the deletion-side counterpart to CountAddedLines.
//
// AddedOrChangedRanges([]byte(newStr), []byte(oldStr)) treats oldStr as the
// "new" content: it returns the ranges of oldStr lines not covered by newStr,
// i.e. exactly the lines this edit removed. Each non-blank removed line is
// hashed (both content_sha and content_sha_norm); blank/whitespace-only lines
// are skipped, mirroring the content_sha_norm convention so they never
// spuriously match other blank lines.
func RemovedLineHashes(oldStr, newStr string) []DeletedLineHash {
	oldLines := strings.Split(oldStr, "\n")
	if n := len(oldLines); n > 0 && oldLines[n-1] == "" {
		oldLines = oldLines[:n-1]
	}
	var out []DeletedLineHash
	for _, r := range AddedOrChangedRanges([]byte(newStr), []byte(oldStr)) {
		for ln := r.Start; ln <= r.End; ln++ {
			if ln < 1 || ln > len(oldLines) {
				continue
			}
			text := strings.TrimRight(oldLines[ln-1], "\r")
			if strings.TrimSpace(text) == "" {
				continue
			}
			out = append(out, DeletedLineHash{
				ContentSHA:     sha256Hex([]byte(text)),
				ContentSHANorm: sha256HexNorm(text),
			})
		}
	}
	return out
}
