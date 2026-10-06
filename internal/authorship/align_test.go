package authorship

import (
	"math/rand"
	"runtime"
	"strconv"
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
	rng := rand.New(rand.NewSource(1))
	alphabet := []string{"a", "b", " a", "c", ""}
	gen := func(k int) []string {
		out := make([]string, k)
		for i := range out {
			out[i] = alphabet[rng.Intn(len(alphabet))]
		}
		return out
	}
	for iter := 0; iter < 200000; iter++ {
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
