package gitnotes

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blamely/blamely/internal/authorship"
)

// flipNoteToWorkingLog must rewrite a note's added-line attribution from the working
// log and recompute the totals. Scenario: v1 mislabeled all 3 added lines AI; the
// working log says L1/L2 Human + L3 AI; after the flip the note must match.
func TestFlipNoteToWorkingLog(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	git := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "core.hooksPath="}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	git("config", "user.email", "t@t")
	git("config", "user.name", "t")
	git("checkout", "-q", "-b", "main")
	os.WriteFile(filepath.Join(repo, "seed"), []byte("x\n"), 0o644)
	git("add", ".")
	git("commit", "-q", "-m", "c1")
	parent := git("rev-parse", "HEAD")

	// Working log at the parent base: human types two lines, AI appends one.
	const rel = "f.txt"
	abs := filepath.Join(repo, rel)
	os.WriteFile(abs, []byte("h1\nh2\n"), 0o644)
	authorship.Update(repo, "main", parent, rel, "h1\nh2\n", "", authorship.HumanAuthor(), 1)
	os.WriteFile(abs, []byte("h1\nh2\nai3\n"), 0o644)
	authorship.Update(repo, "main", parent, rel, "h1\nh2\nai3\n", "",
		authorship.Author{Type: authorship.AI, Tool: "claude", GenType: "chat"}, 2)

	git("add", ".")
	git("commit", "-q", "-m", "c2")
	commit := git("rev-parse", "HEAD")

	// v1 note: the OLD engine mislabeled all three added lines AI.
	model := "claude-opus-4-8"
	note := &Note{
		Commit:    commit,
		Branch:    "main",
		Totals:    Totals{AddedLines: 3, AILines: 3, HumanLines: 0, Models: map[string]int{model: 3}},
		ByGenType: ByGenType{Chat: 3},
		ByTool:    map[string]Tool{"claude": {Lines: 3, AcceptedLines: 3, Model: &model}},
		Files: []FileEntry{{
			Path: rel, Type: "ADDED", Added: 3,
			Lines: []RangeEntry{{Start: 1, End: 3, Type: "add", AuthorType: "AI", Tool: "claude"}},
		}},
	}

	flipNoteToWorkingLog(repo, note)

	// Per-line: L1-2 Human, L3 AI.
	lines := note.Files[0].Lines
	if len(lines) != 2 {
		t.Fatalf("want 2 collapsed ranges, got %d: %+v", len(lines), lines)
	}
	if lines[0].AuthorType != "Human" || lines[0].Start != 1 || lines[0].End != 2 {
		t.Errorf("range 0: want Human 1-2, got %+v", lines[0])
	}
	if lines[1].AuthorType != "AI" || lines[1].Start != 3 || lines[1].Tool != "claude" {
		t.Errorf("range 1: want AI claude L3, got %+v", lines[1])
	}
	// Totals + aggregates recomputed.
	if note.Totals.AILines != 1 || note.Totals.HumanLines != 2 || note.Totals.AddedLines != 3 {
		t.Errorf("totals: want ai=1 human=2 added=3, got %+v", note.Totals)
	}
	if note.ByGenType.Human != 2 || note.ByGenType.Chat != 1 {
		t.Errorf("by_gen_type: want human=2 chat=1, got %+v", note.ByGenType)
	}
	if note.ByTool["claude"].Lines != 1 {
		t.Errorf("by_tool claude lines: want 1, got %d", note.ByTool["claude"].Lines)
	}
}

// A formatter run outside the editor (`prettier --write`, lint-staged) rewrites a
// file after the last recorded edit and nobody records it, so the log describes
// content the commit doesn't have. The flip must carry the log onto the committed
// blob instead of reading its line numbers off it: every line Prettier split stays
// AI. Real Prettier 3 output from authorship/testdata/prettier.
func TestFlipNoteToWorkingLog_UnrecordedReformat(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join("..", "authorship", "testdata", "prettier", name))
		if err != nil {
			t.Fatal(err)
		}
		return strings.ReplaceAll(string(b), "\r\n", "\n")
	}
	before, after := read("return-parens-before.ts"), read("return-parens-after.ts")

	repo := t.TempDir()
	git := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "core.hooksPath=", "-c", "core.autocrlf=false"}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	git("checkout", "-q", "-b", "main")
	os.WriteFile(filepath.Join(repo, "seed"), []byte("x\n"), 0o644)
	git("add", ".")
	git("commit", "-q", "-m", "c1")
	parent := git("rev-parse", "HEAD")

	const rel = "a.ts"
	abs := filepath.Join(repo, rel)
	os.WriteFile(abs, []byte(before), 0o644)
	if _, err := authorship.Update(repo, "main", parent, rel, before, "",
		authorship.Author{Type: authorship.AI, Tool: "copilot", GenType: "cli"}, 1); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(abs, []byte(after), 0o644) // prettier --write: not recorded
	git("add", ".")
	git("commit", "-q", "-m", "c2")

	n := strings.Count(strings.TrimSuffix(after, "\n"), "\n") + 1
	note := &Note{
		Commit: git("rev-parse", "HEAD"),
		Branch: "main",
		Files: []FileEntry{{
			Path: rel, Type: "ADDED", Added: n,
			Lines: []RangeEntry{{Start: 1, End: n, Type: "add", AuthorType: "Human"}},
		}},
	}
	flipNoteToWorkingLog(repo, note)

	lines := note.Files[0].Lines
	if len(lines) != 1 || lines[0].AuthorType != "AI" || lines[0].Start != 1 || lines[0].End != n {
		t.Errorf("want one AI range 1-%d, got %+v", n, lines)
	}
}
