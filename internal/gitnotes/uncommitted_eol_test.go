package gitnotes

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestChangedNewSideLines(t *testing.T) {
	diff := "" +
		"diff --git a/a.ts b/a.ts\n" +
		"--- a/a.ts\n" +
		"+++ b/a.ts\n" +
		// The old last line gains a newline as a line is appended after it.
		"@@ -3 +3,2 @@\n" +
		"-}\n" +
		"\\ No newline at end of file\n" +
		"+}\n" +
		"+// by hand\n" +
		"diff --git a/b.ts b/b.ts\n" +
		"--- a/b.ts\n" +
		"+++ b/b.ts\n" +
		// A real modification, then a pure insertion further down.
		"@@ -1 +1 @@\n" +
		"-old\n" +
		"+new\n" +
		"@@ -5,0 +6,2 @@\n" +
		"+x\n" +
		"+y\n" +
		"diff --git a/gone.ts b/gone.ts\n" +
		"--- a/gone.ts\n" +
		"+++ /dev/null\n" +
		"@@ -1 +0,0 @@\n" +
		"-bye\n"
	got := changedNewSideLines([]byte(diff))
	check := func(file string, want ...int) {
		t.Helper()
		lines := got[file]
		if len(lines) != len(want) {
			t.Errorf("%s: got %v, want %v", file, lines, want)
			return
		}
		for _, ln := range want {
			if !lines[ln] {
				t.Errorf("%s: got %v, want %v", file, lines, want)
				return
			}
		}
	}
	check("a.ts", 4) // line 3 only gained its newline
	check("b.ts", 1, 6, 7)
	if _, ok := got["gone.ts"]; ok {
		t.Errorf("a deleted file has no post-image lines: got %v", got["gone.ts"])
	}
}

// End to end through git: a file committed without a trailing newline, then a
// line typed after it. Only the new line is a change; the old last line must not
// be marked, or the gutter paints it with its committed author (an AI tool when
// an agent wrote it) although nobody touched it.
func TestUncommittedAddedLines_EOFNewlineIsNotAChange(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "core.hooksPath=", "-c", "core.autocrlf=false"}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	abs := filepath.Join(repo, "a.ts")
	if err := os.WriteFile(abs, []byte("export function f() {\r\n  return 1;\r\n}"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-q", "-m", "c1")
	if err := os.WriteFile(abs, []byte("export function f() {\r\n  return 1;\r\n}\r\n// by hand"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := UncommittedAddedLines(repo, "a.ts"); len(got) != 1 || !got[4] {
		t.Errorf("UncommittedAddedLines = %v, want only line 4", got)
	}
	if got := UncommittedAddedLinesAll(repo)["a.ts"]; len(got) != 1 || !got[4] {
		t.Errorf("UncommittedAddedLinesAll[a.ts] = %v, want only line 4", got)
	}
}
