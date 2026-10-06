package authorship

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
	"unicode"
)

// Attribute is THE attribution engine (docs/attribution-v2-design.md §7). Given a
// file's prior working log + the baseline content those attributions describe, and
// the new content produced by `author`'s edit, it returns the updated working log.
//
// The rule, with no content-hash guessing:
//   - A new line that is UNCHANGED vs the baseline (matched by an LCS alignment)
//     keeps its prior author — so a human line an AI edit merely re-emitted stays
//     Human (I3), even if identical content appears elsewhere (I2).
//   - A new line that is added or changed is attributed to `author`.
//   - Lines with no prior coverage default to Human (I5).
//
// Add (empty baseline → all `author`), edit (diff), and delete (lines vanish) all
// fall out of the same alignment. Reflow (whitespace-only changes) is handled by
// the normalized comparison in alignLines, and MOVE detection carries a
// relocated line's prior author to its new position: a block moved by an AI edit
// stays Human (moving code is not authoring it). Genuine adds/changes still go to
// `author`.
//
// nowMS is injected (not time.Now()) so callers/tests are deterministic; pass 0
// to stamp with the wall clock.
func Attribute(prior *WorkingLog, baseline, newContent string, author Author, nowMS int64) *WorkingLog {
	oldLines := splitLines(baseline)
	newLines := splitLines(newContent)

	// matchedOld[i] = index into oldLines that new line i is unchanged from, or -1.
	matchedOld := alignLines(oldLines, newLines)
	// movedFrom[i] = index into oldLines that new line i was MOVED from (an
	// unmatched old line of identical content relocated), or -1.
	movedFrom := detectMoves(oldLines, newLines, matchedOld)

	perLine := make([]Author, len(newLines))
	for i := range newLines {
		if j := matchedOld[i]; j >= 0 {
			// Unchanged line: carry the prior author (old line j+1, 1-based).
			perLine[i] = priorAuthorOr(prior, j+1)
		} else if mf := movedFrom[i]; mf >= 0 {
			// Moved line: carry the prior author from its old position.
			perLine[i] = priorAuthorOr(prior, mf+1)
		} else {
			perLine[i] = author
		}
	}

	// overrode[i] records the author a CHANGED line replaced, when its type differs
	// from the new author (a human rewriting AI code, or vice-versa) — an audit
	// marker, not a change to who owns the line now.
	overrode := detectOverrode(prior, matchedOld, movedFrom, len(oldLines), author)

	if nowMS == 0 {
		nowMS = time.Now().UnixMilli()
	}
	file := ""
	base := ""
	if prior != nil {
		file, base = prior.File, prior.BaseSHA
	}
	return &WorkingLog{
		Schema:    WorkingLogSchema,
		File:      file,
		BaseSHA:   base,
		BlobSHA:   sha256Hex(newContent),
		UpdatedMS: nowMS,
		Lines:     coalesce(perLine, overrode),
	}
}

// DeletedBaselineLines returns the contents of baseline lines that this edit REMOVED
// — present in baseline but neither LCS-matched nor moved into newContent. Used to
// attribute deletions to the editing author (the working log itself only describes
// surviving content). Uses the same alignment + move detection as Attribute, so a
// moved or reflowed line is NOT reported as a deletion.
func DeletedBaselineLines(baseline, newContent string) []string {
	oldLines := splitLines(baseline)
	newLines := splitLines(newContent)
	matchedOld := alignLines(oldLines, newLines)
	movedFrom := detectMoves(oldLines, newLines, matchedOld)

	keptOld := make([]bool, len(oldLines))
	for _, j := range matchedOld {
		if j >= 0 {
			keptOld[j] = true
		}
	}
	for _, mf := range movedFrom {
		if mf >= 0 {
			keptOld[mf] = true
		}
	}
	var deleted []string
	for i, line := range oldLines {
		if !keptOld[i] {
			deleted = append(deleted, line)
		}
	}
	return deleted
}

// detectMoves pairs each unmatched NEW line with an unmatched OLD line of identical
// (whitespace-normalized) content — a relocated line. Matching is FIFO by content
// (first available old line of that content), which is deterministic and identical
// across the Go, TS, and Kotlin ports. LCS-matched lines are never moves; a new
// line with no surviving deleted twin is a genuine add (movedFrom = -1).
func detectMoves(oldLines, newLines []string, matchedOld []int) []int {
	moved := make([]int, len(newLines))
	for i := range moved {
		moved[i] = -1
	}
	oldMatched := make([]bool, len(oldLines))
	for _, j := range matchedOld {
		if j >= 0 {
			oldMatched[j] = true
		}
	}
	oldN := normalizeLinesForMatch(oldLines)
	newN := normalizeLinesForMatch(newLines)
	// queues[content] = unmatched old indices with that normalized content, in order.
	queues := make(map[string][]int)
	for oi := range oldLines {
		if !oldMatched[oi] {
			queues[oldN[oi]] = append(queues[oldN[oi]], oi)
		}
	}
	for ni := range newLines {
		if matchedOld[ni] >= 0 {
			continue
		}
		q := queues[newN[ni]]
		if len(q) > 0 {
			moved[ni] = q[0]
			queues[newN[ni]] = q[1:]
		}
	}
	return moved
}

// detectOverrode finds replace pairs and, for each changed NEW line whose replaced
// OLD line had a different-type author, records that prior author. It walks the LCS
// alignment gap by gap and pairs, positionally, the NEW lines that are neither
// matched nor moved against the OLD lines not consumed by a move — deterministic
// and identical across the Go, TS, and Kotlin ports. Pure inserts, pure deletes,
// and moves never produce an override.
func detectOverrode(prior *WorkingLog, matchedOld, movedFrom []int, nOld int, author Author) []*Author {
	m := len(matchedOld)
	overrode := make([]*Author, m)
	consumedOld := make([]bool, nOld)
	for _, mf := range movedFrom {
		if mf >= 0 {
			consumedOld[mf] = true
		}
	}
	oldCursor := 0
	i := 0
	for i < m {
		if matchedOld[i] >= 0 {
			oldCursor = matchedOld[i] + 1
			i++
			continue
		}
		gapNewEnd := i
		for gapNewEnd < m && matchedOld[gapNewEnd] < 0 {
			gapNewEnd++
		}
		gapOldEnd := nOld
		if gapNewEnd < m {
			gapOldEnd = matchedOld[gapNewEnd]
		}
		// Pair the gap's non-moved new lines with its non-consumed old lines.
		var newAvail, oldAvail []int
		for ni := i; ni < gapNewEnd; ni++ {
			if movedFrom[ni] < 0 {
				newAvail = append(newAvail, ni)
			}
		}
		for oi := oldCursor; oi < gapOldEnd; oi++ {
			if !consumedOld[oi] {
				oldAvail = append(oldAvail, oi)
			}
		}
		for k := 0; k < len(newAvail) && k < len(oldAvail); k++ {
			replaced := priorAuthorOr(prior, oldAvail[k]+1)
			if replaced.Type != author.Type {
				p := replaced
				overrode[newAvail[k]] = &p
			}
		}
		oldCursor = gapOldEnd
		i = gapNewEnd
	}
	return overrode
}

// priorAuthorOr returns the prior author of 1-based old line n, defaulting to
// Human when there is no prior log or the line is uncovered (I5).
func priorAuthorOr(prior *WorkingLog, n int) Author {
	if prior == nil {
		return HumanAuthor()
	}
	return prior.authorAtLine(n)
}

// splitLines splits content into lines WITHOUT a trailing empty element from a
// final newline, and strips a trailing CR so CRLF (Windows) and LF compare equal.
// This keeps attribution stable across line-ending differences on the 3 OSes.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, "\n")
	if n := len(parts); n > 0 && parts[n-1] == "" {
		parts = parts[:n-1] // drop the empty piece after a final '\n'
	}
	for i := range parts {
		parts[i] = strings.TrimSuffix(parts[i], "\r")
	}
	return parts
}

// maxAlignCells caps the LCS table alignLines allocates for the region between the
// common prefix and suffix (int32 cells: 64 MB). An edit to a large file only
// touches a small middle, so this is reached only when thousands of lines change at
// once; that middle is then left unmatched and detectMoves pairs identical lines back
// to their prior authors. Without a cap the table was (n+1)×(m+1) cells over the
// WHOLE file — tens of GB for a 50k-line file on every recorded edit.
const maxAlignCells = 16_000_000

// alignLines returns, for each NEW line index, the index of the OLD line it is
// unchanged from (an LCS match), or -1 if it is added/changed. Standard LCS DP
// with backtrack; positional matching is what makes duplicate identical lines
// resolve correctly (the unchanged occurrence matches in place; an extra copy is
// reported as added) without any content-hash disambiguation.
//
// Lines are compared WHITESPACE-NORMALIZED (reflow): a line that changed
// only in indentation / trailing or collapsed whitespace counts as unchanged and
// keeps its prior author — reformatting is not authorship ("formatting
// non-substantial"). A genuine content change still mismatches → the editor.
//
// The DP only covers the lines between the common prefix and the common suffix,
// yet the result is identical to running it over the whole file:
//   - the backtrack matches equal lines at (i, i) immediately, so the common prefix
//     is matched in place;
//   - for a common last line x, LCS(A+x, B+x) = LCS(A, B) + 1 at every cell that
//     still contains x, so the backtrack takes the same steps as on (A, B) until
//     one side is exhausted. Then the full walk matches x against the FIRST x left
//     on the other side, and ends exhausted on the same side. The suffix loop below
//     replays exactly that, one suffix line at a time, in linear time.
//
// MUST match the TS and Kotlin ports exactly (the golden vectors enforce it).
func alignLines(oldLines, newLines []string) []int {
	n, m := len(oldLines), len(newLines)
	matched := make([]int, m)
	for i := range matched {
		matched[i] = -1
	}
	if n == 0 || m == 0 {
		return matched
	}
	oldN := normalizeLinesForMatch(oldLines)
	newN := normalizeLinesForMatch(newLines)

	p := 0
	for p < n && p < m && oldN[p] == newN[p] {
		matched[p] = p
		p++
	}
	s := 0
	for s < n-p && s < m-p && oldN[n-1-s] == newN[m-1-s] {
		s++
	}
	oldEnd, newEnd := n-s, m-s // the middle is oldN[p:oldEnd] vs newN[p:newEnd]

	i, j := p, p
	rows, cols := oldEnd-p, newEnd-p
	if rows > 0 && cols > 0 {
		if (rows+1)*(cols+1) > maxAlignCells {
			i, j = oldEnd, newEnd // too large: leave the middle unmatched
		} else {
			// dp[(a-p)*w + (b-p)] = LCS length of oldN[a:oldEnd] and newN[b:newEnd].
			w := cols + 1
			dp := make([]int32, (rows+1)*w)
			for a := rows - 1; a >= 0; a-- {
				for b := cols - 1; b >= 0; b-- {
					if oldN[p+a] == newN[p+b] {
						dp[a*w+b] = dp[(a+1)*w+b+1] + 1
					} else if dp[(a+1)*w+b] >= dp[a*w+b+1] {
						dp[a*w+b] = dp[(a+1)*w+b]
					} else {
						dp[a*w+b] = dp[a*w+b+1]
					}
				}
			}
			// Backtrack to recover the matched (unchanged) pairs.
			for i < oldEnd && j < newEnd {
				a, b := i-p, j-p
				if oldN[i] == newN[j] {
					matched[j] = i
					i++
					j++
				} else if dp[(a+1)*w+b] >= dp[a*w+b+1] {
					i++
				} else {
					j++
				}
			}
		}
	}
	// Replay the common suffix (see above): one side is exhausted here.
	for k := 0; k < s; k++ {
		oi, nj := oldEnd+k, newEnd+k
		x := oldN[oi]
		if i == oi {
			for newN[j] != x {
				j++
			}
			matched[j] = oi
			i, j = oi+1, j+1
		} else {
			for oldN[i] != x {
				i++
			}
			matched[nj] = i
			i, j = i+1, nj+1
		}
	}
	return matched
}

// normalizeLineForMatch reduces a line to its whitespace-insensitive form by
// REMOVING all whitespace (git diff -w semantics). A line that changed only in
// whitespace — indentation, trailing, or between tokens (operator spacing like
// `x=1` ↔ `x = 1`) — therefore matches and keeps its prior author: reformatting is
// not authorship. Trade-off: a whitespace edit INSIDE a string literal also reads
// as reflow (prior author kept); that is rare and defensible. This MUST produce
// identical output in the Go, TypeScript, and Kotlin ports (the golden vectors
// enforce it) so the three agree on what counts as a reflow.
func normalizeLineForMatch(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}

func normalizeLinesForMatch(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = normalizeLineForMatch(l)
	}
	return out
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
