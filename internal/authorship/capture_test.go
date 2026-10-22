package authorship

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// gitRepo creates a throwaway repo with one committed file and returns the repo
// root. Skips (not fails) if git is unavailable so the unit tests above still run.
func gitRepo(t *testing.T, file, content string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("checkout", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-q", "-m", "seed")
	return dir
}

// End-to-end through git: a committed (human) file, then an AI edit that appends a
// line. The first RecordEdit has no stored baseline, so it diffs against HEAD —
// the committed lines must stay Human, only the appended line is AI.
func TestRecordEdit_FirstEditDiffsAgainstHEAD(t *testing.T) {
	repo := gitRepo(t, "app.go", joinLines("package main", "func main() {}"))
	abs := filepath.Join(repo, "app.go")

	// AI appends a line.
	if err := os.WriteFile(abs, []byte(joinLines("package main", "func main() {}", "// added by ai")), 0o644); err != nil {
		t.Fatal(err)
	}
	wl, err := RecordEdit(abs, ai("claude"))
	if err != nil {
		t.Fatal(err)
	}
	got := typesByLine(wl, 3)
	want := []AuthorType{Human, Human, AI}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d: want %q, got %q (HEAD lines must stay Human)", i+1, want[i], got[i])
		}
	}
}

// The type-then-AI sequence, end to end: human edits the file (recorded), then AI
// re-emits + appends. The stored baseline from the first RecordEdit carries the
// human lines so they survive the AI re-emit.
func TestRecordEdit_TypeThenAISequence(t *testing.T) {
	repo := gitRepo(t, "f.txt", "seed")
	abs := filepath.Join(repo, "f.txt")

	// 1) Human edit: replace seed with two typed lines.
	os.WriteFile(abs, []byte(joinLines("typed one", "typed two")), 0o644)
	if _, err := RecordEdit(abs, human()); err != nil {
		t.Fatal(err)
	}
	// 2) AI rewrites, re-including the human lines and adding its own.
	os.WriteFile(abs, []byte(joinLines("typed one", "typed two", "ai three")), 0o644)
	wl, err := RecordEdit(abs, ai("codex"))
	if err != nil {
		t.Fatal(err)
	}
	got := typesByLine(wl, 3)
	want := []AuthorType{Human, Human, AI}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d: want %q, got %q", i+1, want[i], got[i])
		}
	}
}

// CaptureBaseline (the pre-edit fallback) lets a terminal agent's edit diff against
// the human state the editor never saw, instead of HEAD.
func TestCaptureBaseline_ThenRecordEdit(t *testing.T) {
	repo := gitRepo(t, "f.txt", "v0")
	abs := filepath.Join(repo, "f.txt")

	// Human changes the file outside the editor (not recorded), then a pre-edit
	// baseline is captured just before the agent runs.
	os.WriteFile(abs, []byte(joinLines("human a", "human b")), 0o644)
	if err := CaptureBaseline(abs); err != nil {
		t.Fatal(err)
	}
	// Agent appends.
	os.WriteFile(abs, []byte(joinLines("human a", "human b", "agent c")), 0o644)
	wl, err := RecordEdit(abs, ai("claude"))
	if err != nil {
		t.Fatal(err)
	}
	got := typesByLine(wl, 3)
	want := []AuthorType{Human, Human, AI}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d: want %q, got %q (captured baseline should protect human lines)", i+1, want[i], got[i])
		}
	}
}

func TestResolveContext_OutsideRepo(t *testing.T) {
	if _, ok := ResolveContext(filepath.Join(t.TempDir(), "x.txt")); ok {
		t.Errorf("expected ok=false outside a git work tree")
	}
}

// A shell command that runs right after a recorded edit (an agent checking its
// work with `git diff`) must not be credited with the file that edit wrote:
// only files changed since their log was written, or with no log, remain.
func TestChangedSinceRecorded(t *testing.T) {
	repo := gitRepo(t, "recorded.go", joinLines("package main"))
	recorded := filepath.Join(repo, "recorded.go")
	if err := os.WriteFile(recorded, []byte(joinLines("package main", "// by agent")), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := RecordEdit(recorded, ai("copilot")); err != nil {
		t.Fatal(err)
	}
	if got := ChangedSinceRecorded(repo, []string{"recorded.go"}); len(got) != 0 {
		t.Fatalf("file unchanged since its recorded edit: got %v, want none", got)
	}

	if err := os.WriteFile(recorded, []byte(joinLines("package main", "// by agent", "// by command")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "untracked.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := ChangedSinceRecorded(repo, []string{"recorded.go", "untracked.go"})
	if len(got) != 2 {
		t.Errorf("changed after its record, and never recorded: got %v, want both", got)
	}
}
