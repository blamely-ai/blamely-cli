package authorship

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func ai(tool string) Author { return Author{Type: AI, Tool: tool, GenType: "chat"} }
func human() Author         { return HumanAuthor() }

// typesByLine expands a working log into a per-line AuthorType slice (1-based →
// index 0). Uncovered lines report "" so gaps are visible in failures.
func typesByLine(wl *WorkingLog, n int) []AuthorType {
	out := make([]AuthorType, n)
	for _, r := range wl.Lines {
		for ln := r.Start; ln <= r.End && ln <= n; ln++ {
			out[ln-1] = r.Author.Type
		}
	}
	return out
}

// overrodeTypesByLine expands a working log into a per-line overrode-author-type
// slice (nil = no override on that line).
func overrodeTypesByLine(wl *WorkingLog, n int) []*AuthorType {
	out := make([]*AuthorType, n)
	for _, r := range wl.Lines {
		if r.Overrode == nil {
			continue
		}
		t := r.Overrode.Type
		for ln := r.Start; ln <= r.End && ln <= n; ln++ {
			tt := t
			out[ln-1] = &tt
		}
	}
	return out
}

func deref(t *AuthorType) AuthorType {
	if t == nil {
		return ""
	}
	return *t
}

func joinLines(ls ...string) string { return strings.Join(ls, "\n") }

func TestAttribute_NewFileAllAI(t *testing.T) {
	// Empty baseline → every line is the editing author (a brand-new AI file).
	wl := Attribute(nil, "", joinLines("a", "b", "c"), ai("claude"), 1)
	got := typesByLine(wl, 3)
	for i, ty := range got {
		if ty != AI {
			t.Errorf("line %d: want AI, got %q", i+1, ty)
		}
	}
}

// I2 + I3: user types lines, THEN AI edits re-emitting them. The human lines must
// stay Human (this is the field repro that kept failing under hash matching).
func TestAttribute_TypeThenAI(t *testing.T) {
	// 1) Human types two lines.
	log1 := Attribute(nil, "", joinLines("human one", "human two"), human(), 1)
	// 2) AI rewrites the file, re-including the human lines and appending its own.
	base := joinLines("human one", "human two")
	newC := joinLines("human one", "human two", "ai line a", "ai line b")
	log2 := Attribute(log1, base, newC, ai("claude"), 2)

	got := typesByLine(log2, 4)
	want := []AuthorType{Human, Human, AI, AI}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d: want %q, got %q (human-typed line must not become AI)", i+1, want[i], got[i])
		}
	}
}

// Duplicate identical content on different lines: the unchanged occurrence stays
// its prior author; an AI-added copy elsewhere is AI. No hash ambiguity.
func TestAttribute_DuplicateLines(t *testing.T) {
	log1 := Attribute(nil, "", joinLines("dup", "mid"), human(), 1) // L1 dup(Human), L2 mid(Human)
	base := joinLines("dup", "mid")
	newC := joinLines("dup", "mid", "dup") // AI appends a second "dup"
	log2 := Attribute(log1, base, newC, ai("codex"), 2)

	got := typesByLine(log2, 3)
	want := []AuthorType{Human, Human, AI}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d: want %q, got %q", i+1, want[i], got[i])
		}
	}
}

// Whole-file rewrite that re-emits unchanged lines must leave them as-is (I3) and
// credit only the genuinely-new line.
func TestAttribute_WholeFileRewriteOnlyNewIsAI(t *testing.T) {
	log1 := Attribute(nil, "", joinLines("a", "b", "c"), human(), 1)
	base := joinLines("a", "b", "c")
	newC := joinLines("a", "b", "c", "d") // agent rewrote, only +d is new
	log2 := Attribute(log1, base, newC, ai("copilot"), 2)
	got := typesByLine(log2, 4)
	want := []AuthorType{Human, Human, Human, AI}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d: want %q, got %q", i+1, want[i], got[i])
		}
	}
}

// A genuine in-place edit by AI: changed line → AI, surrounding unchanged → prior.
func TestAttribute_EditMiddleLine(t *testing.T) {
	log1 := Attribute(nil, "", joinLines("a", "b", "c"), human(), 1)
	log2 := Attribute(log1, joinLines("a", "b", "c"), joinLines("a", "B", "c"), ai("claude"), 2)
	got := typesByLine(log2, 3)
	want := []AuthorType{Human, AI, Human}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d: want %q, got %q", i+1, want[i], got[i])
		}
	}
}

// Deletion: removed lines just vanish; survivors keep their author.
func TestAttribute_Delete(t *testing.T) {
	log1 := Attribute(nil, "", joinLines("a", "b", "c"), human(), 1)
	log2 := Attribute(log1, joinLines("a", "b", "c"), joinLines("a", "c"), ai("claude"), 2)
	got := typesByLine(log2, 2)
	want := []AuthorType{Human, Human}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d: want %q, got %q", i+1, want[i], got[i])
		}
	}
}

// CRLF (Windows) vs LF must compare equal so attribution is line-ending stable.
func TestAttribute_CRLFStable(t *testing.T) {
	log1 := Attribute(nil, "", "a\r\nb", human(), 1)
	log2 := Attribute(log1, "a\r\nb", "a\r\nb\r\nc", ai("claude"), 2)
	got := typesByLine(log2, 3)
	want := []AuthorType{Human, Human, AI}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d: want %q, got %q", i+1, want[i], got[i])
		}
	}
}

// Real Prettier 3 output (testdata/prettier): an AI-written file reformatted on
// save — quotes, semicolons, arrow parens, a signature and an object literal split
// over several lines, a one-line `if` expanded. Nobody authored anything, so every
// line stays AI, under both the default and a singleQuote/no-semi config.
// return-parens is two one-line functions Prettier split in one go (one alignment
// gap), wrapping the second's broken-up return in `return (`…`);`.
func TestAttribute_PrettierReformatKeepsAuthor(t *testing.T) {
	for _, pair := range [][2]string{
		{"before.ts", "after-default.ts"},
		{"before.ts", "after-single-quote-no-semi.ts"},
		{"return-parens-before.ts", "return-parens-after.ts"},
	} {
		before, err := os.ReadFile(filepath.Join("testdata", "prettier", pair[0]))
		if err != nil {
			t.Fatal(err)
		}
		name := pair[1]
		after, err := os.ReadFile(filepath.Join("testdata", "prettier", name))
		if err != nil {
			t.Fatal(err)
		}
		prior := Attribute(nil, "", string(before), ai("copilot"), 1)
		wl := Attribute(prior, string(before), string(after), HumanAuthor(), 2)
		n := len(splitLines(string(after)))
		for i, got := range typesByLine(wl, n) {
			if got != AI {
				t.Errorf("%s line %d (%q): got %s, want ai", name, i+1, splitLines(string(after))[i], got)
			}
		}
	}
}
