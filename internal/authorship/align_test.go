package authorship

import (
	"math/rand"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// alignLinesFullDP is the original whole-file LCS DP + backtrack — the reference
// alignLines must reproduce exactly.
func alignLinesFullDP(oldLines, newLines []string) []int {
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
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if oldN[i] == newN[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	i, j := 0, 0
	for i < n && j < m {
		if oldN[i] == newN[j] {
			matched[j] = i
			i++
			j++
		} else if dp[i+1][j] >= dp[i][j+1] {
			i++
		} else {
			j++
		}
	}
	return matched
}

// The prefix/suffix-trimmed alignment must match the whole-file DP on every input,
// including the duplicate-line cases where a different-but-valid LCS would move
// authorship between identical lines. A tiny alphabet makes duplicates (and so
// ambiguous suffix boundaries) the common case.
func TestAlignLinesMatchesFullDP(t *testing.T) {
	checkAlignLinesMatchesFullDP(t, 200000)
}

// Above maxAlignCells the middle goes through the checkpointed table; it must
// give the very same matches. A tiny cap forces that path on every input.
func TestAlignLinesCheckpointedMatchesFullDP(t *testing.T) {
	defer func(c int) { maxAlignCells = c }(maxAlignCells)
	maxAlignCells = 1
	checkAlignLinesMatchesFullDP(t, 200000)
	rng := rand.New(rand.NewSource(2))
	alphabet := []string{"a", "b", " a", "c", ""}
	for iter := 0; iter < 300; iter++ { // larger middles: several checkpoint blocks
		oldLines := make([]string, 50+rng.Intn(150))
		for k := range oldLines {
			oldLines[k] = alphabet[rng.Intn(len(alphabet))]
		}
		newLines := append([]string{}, oldLines...)
		for e := rng.Intn(20); e > 0; e-- {
			k := rng.Intn(len(newLines))
			switch rng.Intn(3) {
			case 0:
				newLines = append(newLines[:k], newLines[k+1:]...)
			case 1:
				newLines = append(newLines[:k], append([]string{alphabet[rng.Intn(len(alphabet))]}, newLines[k:]...)...)
			default:
				newLines[k] = alphabet[rng.Intn(len(alphabet))]
			}
		}
		got, want := alignLines(oldLines, newLines), alignLinesFullDP(oldLines, newLines)
		for k := range want {
			if got[k] != want[k] {
				t.Fatalf("old=%q new=%q\n got %v\nwant %v", oldLines, newLines, got, want)
			}
		}
	}
}

func checkAlignLinesMatchesFullDP(t *testing.T, iters int) {
	t.Helper()
	rng := rand.New(rand.NewSource(1))
	alphabet := []string{"a", "b", " a", "c", ""}
	gen := func(k int) []string {
		out := make([]string, k)
		for i := range out {
			out[i] = alphabet[rng.Intn(len(alphabet))]
		}
		return out
	}
	for iter := 0; iter < iters; iter++ {
		oldLines := gen(rng.Intn(9))
		var newLines []string
		switch rng.Intn(3) {
		case 0:
			newLines = gen(rng.Intn(9))
		default: // an edit of old: shares a prefix and/or suffix
			cut1 := rng.Intn(len(oldLines) + 1)
			cut2 := cut1 + rng.Intn(len(oldLines)-cut1+1)
			newLines = append(append(append([]string{}, oldLines[:cut1]...), gen(rng.Intn(4))...), oldLines[cut2:]...)
		}
		got, want := alignLines(oldLines, newLines), alignLinesFullDP(oldLines, newLines)
		for k := range want {
			if got[k] != want[k] {
				t.Fatalf("old=%q new=%q\n got %v\nwant %v", oldLines, newLines, got, want)
			}
		}
	}
}

// A one-line edit in a 50k-line file must not allocate a whole-file LCS table
// (that was ~20 GB per call and froze machines).
func TestAlignLinesLargeFileOneLineEdit(t *testing.T) {
	const n = 50000
	oldLines := make([]string, n)
	for i := range oldLines {
		oldLines[i] = "line " + strconv.Itoa(i%97) // plenty of duplicates
	}
	newLines := append([]string{}, oldLines...)
	newLines[n/2] = "changed"
	newLines = append(newLines[:n/3], append([]string{"inserted"}, newLines[n/3:]...)...)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	start := time.Now()
	matched := alignLines(oldLines, newLines)
	elapsed := time.Since(start)
	runtime.ReadMemStats(&after)

	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 200<<20 {
		t.Errorf("allocated %d MB, want < 200 MB", alloc>>20)
	}
	if elapsed > 5*time.Second {
		t.Errorf("took %v", elapsed)
	}
	if matched[n/3] != -1 || matched[n/2+1] != -1 {
		t.Errorf("inserted/changed lines must be unmatched: %d %d", matched[n/3], matched[n/2+1])
	}
	if matched[0] != 0 || matched[len(newLines)-1] != n-1 {
		t.Errorf("unchanged lines must match in place")
	}
}

// Two far-apart edits in a 4,500-line file put a ~4,500-line middle above the
// cap. Duplicate lines there have different prior owners; every unchanged line —
// the Copilot-written duplicate among them — must keep its owner, as it does
// below the cap.
func TestAttributeAboveCapKeepsDuplicateOwners(t *testing.T) {
	const n = 4500
	old := make([]string, n)
	for k := range old {
		old[k] = "stmt" + strconv.Itoa(k)
		if k%10 == 0 {
			old[k] = "}" // duplicates throughout
		}
	}
	copilot := Author{Type: AI, Tool: "copilot", GenType: "chat"}
	// Line 2001 (1-based; old[2000] is a "}") was written by Copilot, the rest by a human.
	prior := &WorkingLog{Lines: []LineAttribution{
		{Start: 1, End: 2000, Author: HumanAuthor()},
		{Start: 2001, End: 2001, Author: copilot},
		{Start: 2002, End: n, Author: HumanAuthor()},
	}}
	// In one change the human edits line 2 and the second-to-last line, and deletes
	// a "}" above the Copilot line. Pairing duplicates by content order instead of
	// by position would then hand each later "}" its predecessor's owner.
	cur := append([]string{}, old...)
	cur[1] = "edited"
	cur[n-2] = "edited too"
	cur = append(cur[:500], cur[501:]...) // old[500] is a "}"
	if (n-1)*(n-1) <= maxAlignCells {
		t.Fatalf("test must exceed the cap: middle %d² <= %d", n-1, maxAlignCells)
	}
	wl := Attribute(prior, strings.Join(old, "\n")+"\n", strings.Join(cur, "\n")+"\n", HumanAuthor(), 1)
	owner := map[int]AuthorType{}
	for _, r := range wl.Lines {
		for ln := r.Start; ln <= r.End; ln++ {
			owner[ln] = r.Author.Type
		}
	}
	if owner[2000] != AI { // old line 2001, one up after the deletion
		t.Errorf("unchanged Copilot line (now 2000) became %v", owner[2000])
	}
	ai := 0
	for _, a := range owner {
		if a == AI {
			ai++
		}
	}
	if ai != 1 {
		t.Errorf("want exactly 1 AI line, got %d", ai)
	}
}

// A middle far above the cap stays within bounded memory.
func TestAlignLinesAboveCapBoundedMemory(t *testing.T) {
	const n = 20000
	oldLines := make([]string, n)
	for i := range oldLines {
		oldLines[i] = "line " + strconv.Itoa(i%97)
	}
	newLines := append([]string{}, oldLines...)
	newLines[1] = "first"
	newLines[n-2] = "last" // middle ≈ n×n = 400M cells, 1.6 GB as a whole table

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	matched := alignLines(oldLines, newLines)
	runtime.ReadMemStats(&after)
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 100<<20 {
		t.Errorf("allocated %d MB, want < 100 MB", alloc>>20)
	}
	for k := range newLines {
		want := k
		if k == 1 || k == n-2 {
			want = -1
		}
		if matched[k] != want {
			t.Fatalf("matched[%d] = %d, want %d", k, matched[k], want)
		}
	}
}
