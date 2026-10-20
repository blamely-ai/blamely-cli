package authorship

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"regexp"
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
	// Runs of lines a formatter split or joined (see detectReflows).
	reflows := detectReflows(oldLines, newLines, matchedOld, movedFrom)

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
	reflowed := make([]bool, len(newLines))
	for _, r := range reflows {
		// A reflowed run keeps its author only when the old lines had ONE author;
		// a split/join across authors can't say whose each new line is, so it
		// stays with the editor, as before.
		shared := priorAuthorOr(prior, r.old[0]+1)
		uniform := true
		for _, oi := range r.old[1:] {
			if priorAuthorOr(prior, oi+1) != shared {
				uniform = false
				break
			}
		}
		if !uniform {
			continue
		}
		for _, ni := range r.new {
			perLine[ni] = shared
			reflowed[ni] = true
		}
	}

	// overrode[i] records the author a CHANGED line replaced, when its type differs
	// from the new author (a human rewriting AI code, or vice-versa) — an audit
	// marker, not a change to who owns the line now.
	overrode := detectOverrode(prior, matchedOld, movedFrom, len(oldLines), author)
	for i := range overrode {
		if reflowed[i] {
			overrode[i] = nil // a reflow replaced nothing
		}
	}

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
	for _, r := range detectReflows(oldLines, newLines, matchedOld, movedFrom) {
		for _, oi := range r.old {
			keptOld[oi] = true
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

// reflowRun is a run of OLD lines a formatter rewrote as a run of NEW lines.
type reflowRun struct {
	old, new []int
}

// detectReflows finds the runs of the LCS alignment's gaps that a formatter only
// reshaped: old lines left in a gap (not matched, not moved away) and new lines in
// it (not matched, not moved in) that read the same once joined and compared with
// reflowLineKey. Prettier and `eslint --fix` split a long call or object literal
// over several lines, join short ones, and add or drop separators doing it — no
// one authored anything, so the new lines keep the old lines' author (Attribute).
// One gap often holds several such runs (every function of a file reformatted at
// once), and a genuine change in one of them must not cost the others their
// author, so reflowChunks cuts the gap at the line boundaries the two sides share;
// what it can't pair is left alone. Walks the gaps exactly like detectOverrode, so
// it is deterministic and identical across the Go, TS, and Kotlin ports.
func detectReflows(oldLines, newLines []string, matchedOld, movedFrom []int) []reflowRun {
	consumedOld := make([]bool, len(oldLines))
	for _, mf := range movedFrom {
		if mf >= 0 {
			consumedOld[mf] = true
		}
	}
	var runs []reflowRun
	m := len(matchedOld)
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
		gapOldEnd := len(oldLines)
		if gapNewEnd < m {
			gapOldEnd = matchedOld[gapNewEnd]
		}
		var gapOld, gapNew []int
		for ni := i; ni < gapNewEnd; ni++ {
			if movedFrom[ni] < 0 {
				gapNew = append(gapNew, ni)
			}
		}
		for oi := oldCursor; oi < gapOldEnd; oi++ {
			if !consumedOld[oi] {
				gapOld = append(gapOld, oi)
			}
		}
		if len(gapNew) > 0 && len(gapOld) > 0 {
			runs = append(runs, reflowChunks(oldLines, newLines, gapOld, gapNew)...)
		}
		oldCursor = gapOldEnd
		i = gapNewEnd
	}
	return runs
}

// reflowChunks splits one gap (old lines gapOld, new lines gapNew) into the runs a
// formatter only reshaped. Joined, the two sides' reflowLineKeys read the same up
// to their longest common prefix and from their longest common suffix; a line
// boundary both sides share inside the prefix (walking forward) or the suffix
// (walking back from the end, over what the prefix left) closes a run. Whatever
// is left between the two holds a genuine change and stays out. A whole gap that
// reads the same yields its runs from the front alone. Lines whose key is empty
// (blank lines) join the run next to them, never form one. Offsets are in each
// port's own string units; a boundary only depends on the text up to it being
// equal, so the three ports cut the same runs.
func reflowChunks(oldLines, newLines []string, gapOld, gapNew []int) []reflowRun {
	oldOff, oldText := reflowOffsets(oldLines, gapOld)
	newOff, newText := reflowOffsets(newLines, gapNew)
	var runs []reflowRun

	prefix := 0
	for prefix < len(oldText) && prefix < len(newText) && oldText[prefix] == newText[prefix] {
		prefix++
	}
	a, b := 0, 0 // where the open run starts
	for i, j := 1, 1; i <= len(gapOld) && j <= len(gapNew); {
		v, w := oldOff[i], newOff[j]
		if v < w {
			i++
			continue
		}
		if w < v {
			j++
			continue
		}
		if v == 0 {
			i, j = i+1, j+1
			continue
		}
		if v > prefix {
			break
		}
		for i < len(gapOld) && oldOff[i+1] == v {
			i++
		}
		for j < len(gapNew) && newOff[j+1] == v {
			j++
		}
		runs = append(runs, reflowRun{old: gapOld[a:i], new: gapNew[b:j]})
		a, b = i, j
		i, j = i+1, j+1
	}

	// From the back, over what the front left. dist(k) is how far line k starts
	// from the end of its side.
	oldEnd, newEnd := oldOff[len(gapOld)], newOff[len(gapNew)]
	suffix := 0
	for suffix < oldEnd-oldOff[a] && suffix < newEnd-newOff[b] &&
		oldText[len(oldText)-1-suffix] == newText[len(newText)-1-suffix] {
		suffix++
	}
	ea, eb := len(gapOld), len(gapNew) // where the open run ends
	for i, j := len(gapOld)-1, len(gapNew)-1; i >= a && j >= b; {
		v, w := oldEnd-oldOff[i], newEnd-newOff[j]
		if v < w {
			i--
			continue
		}
		if w < v {
			j--
			continue
		}
		if v == 0 {
			i, j = i-1, j-1
			continue
		}
		if v > suffix {
			break
		}
		for i > a && oldOff[i-1] == oldOff[i] {
			i--
		}
		for j > b && newOff[j-1] == newOff[j] {
			j--
		}
		runs = append(runs, reflowRun{old: gapOld[i:ea], new: gapNew[j:eb]})
		ea, eb = i, j
		i, j = i-1, j-1
	}
	return runs
}

// reflowOffsets joins the reflowLineKeys of lines[idx...]; off[k] is where line
// idx[k] starts in the joined text (off[len(idx)] is its length).
func reflowOffsets(lines []string, idx []int) ([]int, string) {
	off := make([]int, len(idx)+1)
	var b strings.Builder
	for k, i := range idx {
		b.WriteString(reflowLineKey(lines[i]))
		off[k+1] = b.Len()
	}
	return off, b.String()
}

// reflowLineKey is a line as a formatter sees it, for comparing joined runs:
// normalized (normalizeLineForMatch), then every `,` `;` `(` `)` dropped, since
// splitting or joining lines adds and removes separators between items, and
// Prettier wraps an expression it breaks over lines in parentheses
// (`return (`…`);`, `=> (`…`)`) or drops ones it finds redundant.
// Cross-language contract like normalizeLineForMatch.
func reflowLineKey(line string) string {
	return reflowSeparators.Replace(normalizeLineForMatch(line))
}

var reflowSeparators = strings.NewReplacer(",", "", ";", "", "(", "", ")", "")

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

// maxAlignCells is the largest middle region (between the common prefix and
// suffix) whose LCS table alignLines keeps whole: int32 cells, 64 MB. An edit to
// a large file only touches a small middle, so this covers nearly every edit.
// Larger middles — thousands of lines changed at once, or two far-apart edits in
// one recorded change — go through alignMiddleCheckpointed: the same result in
// O(sqrt(rows)·cols) memory, for about twice the time. (Without either, the table
// was (n+1)×(m+1) cells over the WHOLE file — tens of GB for a 50k-line file.)
//
// A variable only so tests can force the checkpointed path on small inputs.
var maxAlignCells = 16_000_000

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
			i, j = alignMiddleCheckpointed(oldN, newN, p, oldEnd, newEnd, matched)
		} else {
			// dp[(a-p)*w + (b-p)] = LCS length of oldN[a:oldEnd] and newN[b:newEnd].
			w := cols + 1
			dp := make([]int32, (rows+1)*w)
			for a := rows - 1; a >= 0; a-- {
				fillLCSRow(oldN[p+a], newN[p:newEnd], dp[a*w:(a+1)*w], dp[(a+1)*w:(a+2)*w], 0)
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

// fillLCSRow computes one row of the suffix-LCS table: row[b] = LCS length of
// (oldLine + the old lines below) and newMid[b:], for b >= from, given the row
// below (next). row and next span all of newMid plus one trailing zero.
func fillLCSRow(oldLine string, newMid []string, row, next []int32, from int) {
	cols := len(newMid)
	row[cols] = 0
	for b := cols - 1; b >= from; b-- {
		if oldLine == newMid[b] {
			row[b] = next[b+1] + 1
		} else if next[b] >= row[b+1] {
			row[b] = next[b]
		} else {
			row[b] = row[b+1]
		}
	}
}

// alignMiddleCheckpointed is alignLines' DP + backtrack over the middle
// oldN[p:oldEnd] × newN[p:newEnd] without holding the whole table. It keeps
// every step-th row (step ≈ sqrt(rows)), then walks the backtrack block by block,
// rebuilding each block's rows from the checkpoint below it. A block is rebuilt
// only from the backtrack's current column rightward: a row's values there
// depend on nothing to their left. The backtrack sees exactly the values the
// whole table holds, so the matches — duplicates included — are identical.
// Returns where the backtrack stopped.
func alignMiddleCheckpointed(oldN, newN []string, p, oldEnd, newEnd int, matched []int) (int, int) {
	rows, cols := oldEnd-p, newEnd-p
	newMid := newN[p:newEnd]
	w := cols + 1
	step := int(math.Ceil(math.Sqrt(float64(rows))))

	// checkpoint[k] = table row k*step; the row past the last (rows) is all zeros.
	checkpoint := make([][]int32, rows/step+1)
	cur, below := make([]int32, w), make([]int32, w)
	for a := rows - 1; a >= 0; a-- {
		fillLCSRow(oldN[p+a], newMid, cur, below, 0)
		if a%step == 0 {
			checkpoint[a/step] = append([]int32(nil), cur...)
		}
		cur, below = below, cur
	}
	zero := make([]int32, w)
	rowAt := func(a int) []int32 {
		if a == rows {
			return zero
		}
		return checkpoint[a/step]
	}

	block := make([][]int32, step+1)
	for k := range block {
		block[k] = make([]int32, w)
	}
	i, j := p, p
	for i < oldEnd && j < newEnd {
		top := (i - p) / step * step
		bottom := min(top+step, rows)
		from := j - p
		// block[a-top] = table row a, for top <= a <= bottom, columns >= from.
		copy(block[bottom-top][from:], rowAt(bottom)[from:])
		for a := bottom - 1; a >= top; a-- {
			fillLCSRow(oldN[p+a], newMid, block[a-top], block[a-top+1], from)
		}
		for i-p < bottom && j < newEnd {
			a, b := i-p, j-p
			if oldN[i] == newN[j] {
				matched[j] = i
				i++
				j++
			} else if block[a+1-top][b] >= block[a-top][b+1] {
				i++
			} else {
				j++
			}
		}
	}
	return i, j
}

// normalizeLineForMatch reduces a line to the form a code formatter can't change,
// so a line that changed only in formatting matches and keeps its prior author:
// reformatting is not authorship. In order:
//   - REMOVE all whitespace (git diff -w semantics): indentation, trailing, and
//     between tokens (operator spacing like `x=1` ↔ `x = 1`);
//   - treat `"` and the backtick as `'`: Prettier/ESLint rewrite quote style;
//   - `(x)=>` as `x=>`: Prettier's arrowParens adds or drops those parentheses;
//   - drop trailing `;` and `,`: semicolons and trailing commas come and go.
//
// Trade-off: such an edit made by hand (or inside a string literal) also reads as
// formatting and keeps the prior author; that is rare and defensible. This MUST
// produce identical output in the Go, TypeScript, and Kotlin ports (the golden
// vectors enforce it) so the three agree on what counts as a reflow.
func normalizeLineForMatch(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		if r == '"' || r == '`' {
			return '\''
		}
		return r
	}, s)
	s = arrowParamParens.ReplaceAllString(s, "$1=>")
	return strings.TrimRight(s, ";,")
}

// arrowParamParens matches a lone parenthesized arrow parameter, `(x)=>`.
// ASCII identifier classes only, so the three regex engines agree.
var arrowParamParens = regexp.MustCompile(`\(([A-Za-z_$][A-Za-z0-9_$]*)\)=>`)

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
