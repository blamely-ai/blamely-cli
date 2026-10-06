package gitnotes

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// tempHome points ~/.blamely at a temp dir so tests never touch the real log.
func tempHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

func readSyncLog(t *testing.T, home string) string {
	t.Helper()
	data, _ := os.ReadFile(filepath.Join(home, ".blamely", "sync-notes.log"))
	return string(data)
}

// A failed background sync is recorded and printed by the next push, once; a
// later successful sync clears the record. Every attempt lands in sync-notes.log.
func TestSyncNotesAndRecord_ReportsFailureOnNextPush(t *testing.T) {
	home := tempHome(t)
	f := newSyncFixture(t, false)
	repo := f.clone()
	commitWithNote(t, repo, "a.txt", "no key here", `{"n":"a"}`)
	pushCode(t, repo)
	f.enableRule()

	if err := SyncNotesAndRecord(repo, "origin", "", nil); err == nil {
		t.Fatal("expected the rule to reject the notes push")
	}
	var out bytes.Buffer
	ReportPreviousSyncFailure(repo, &out)
	if !strings.Contains(out.String(), "no Jira issue key") || !strings.Contains(out.String(), "failed") {
		t.Errorf("report should carry the server's reason, got: %q", out.String())
	}
	out.Reset()
	ReportPreviousSyncFailure(repo, &out)
	if out.Len() != 0 {
		t.Errorf("a failure is reported once, got again: %q", out.String())
	}

	// A failure followed by a success leaves nothing to report.
	if err := SyncNotesAndRecord(repo, "origin", "", nil); err == nil {
		t.Fatal("expected the rule to reject the notes push again")
	}
	if err := os.Remove(filepath.Join(f.remote, "hooks", "pre-receive")); err != nil {
		t.Fatal(err)
	}
	if err := SyncNotesAndRecord(repo, "origin", "", nil); err != nil {
		t.Fatalf("sync without the rule: %v", err)
	}
	ReportPreviousSyncFailure(repo, &out)
	if out.Len() != 0 {
		t.Errorf("a successful sync must clear the recorded failure, got: %q", out.String())
	}

	lines := strings.Split(strings.TrimSpace(readSyncLog(t, home)), "\n")
	if len(lines) != 3 {
		t.Fatalf("sync-notes.log: got %d lines, want 3 (failed, failed, ok):\n%s", len(lines), strings.Join(lines, "\n"))
	}
	for i, want := range []string{"sync failed: ", "sync failed: ", "sync ok: "} {
		if !strings.Contains(lines[i], want) || !strings.Contains(lines[i], "-> origin") {
			t.Errorf("line %d = %q, want %q ... -> origin", i, lines[i], want)
		}
	}
	if !strings.Contains(lines[0], "no Jira issue key") {
		t.Errorf("failure line should carry the server's reason: %q", lines[0])
	}
}

// The log stays ASCII, one line per attempt, and bounded in size.
func TestSyncLog_OneASCIILineAndBounded(t *testing.T) {
	home := tempHome(t)
	LogSyncOutcome("/r", "origin", errors.New("rejected —\n  remote: über"))
	got := readSyncLog(t, home)
	if strings.Count(got, "\n") != 1 {
		t.Errorf("want exactly one line, got %q", got)
	}
	for _, r := range got {
		if r > 0x7e {
			t.Fatalf("non-ASCII in log line: %q", got)
		}
	}
	for i := 0; i < maxSyncLogSize/40+100; i++ {
		LogSyncOutcome("/a/fairly/long/repository/path", "origin", nil)
	}
	st, err := os.Stat(filepath.Join(home, ".blamely", "sync-notes.log"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() > maxSyncLogSize+maxSyncLogLine+64 {
		t.Errorf("log grew to %d bytes, cap is %d", st.Size(), maxSyncLogSize)
	}
	if !strings.HasPrefix(readSyncLog(t, home), "20") {
		t.Error("trimmed log must start at a line boundary")
	}
}

// The lock serializes syncs and is taken over once it is stale.
func TestLockSync(t *testing.T) {
	f := newSyncFixture(t, false)
	repo := f.clone()
	unlock, err := lockSync(repo)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan time.Time, 1)
	go func() {
		u, err := lockSync(repo)
		if err == nil {
			got <- time.Now()
			u()
		}
	}()
	time.Sleep(700 * time.Millisecond)
	released := time.Now()
	unlock()
	select {
	case at := <-got:
		if at.Before(released) {
			t.Error("second sync took the lock while the first held it")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second sync never got the lock")
	}

	// A lock left by a killed sync is taken over.
	p := gitPath(repo, syncLockFile)
	if err := os.WriteFile(p, []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-syncLockStale - time.Minute)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	u, err := lockSync(repo)
	if err != nil {
		t.Fatalf("stale lock not taken over: %v", err)
	}
	u()
}

// End to end through the real binary: the hook's `sync-notes` call returns
// without waiting for the network, and the detached run still publishes.
func TestSyncNotesCommand_ReturnsBeforeSyncAndPublishesInBackground(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the blamely binary")
	}
	// Built before HOME moves: go's module and build caches live under it.
	exe := filepath.Join(t.TempDir(), "blamely")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", exe, "../../cmd/blamely").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	home := tempHome(t)
	f := newSyncFixture(t, false)
	repo := f.clone()
	sha := commitWithNote(t, repo, "a.txt", "PROJ-1 first", `{"n":"a"}`)
	pushCode(t, repo)

	// Slow every remote call down; the hook's call must not wait for it.
	slowGit := filepath.Join(t.TempDir(), "bin")
	if runtime.GOOS != "windows" {
		if err := os.MkdirAll(slowGit, 0o755); err != nil {
			t.Fatal(err)
		}
		realGit, _ := exec.LookPath("git")
		script := "#!/bin/sh\ncase \"$*\" in *fetch*|*push*) sleep 3 ;; esac\nexec " + realGit + " \"$@\"\n"
		if err := os.WriteFile(filepath.Join(slowGit, "git"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", slowGit+string(os.PathListSeparator)+os.Getenv("PATH"))
	}

	cmd := exec.Command(exe, "sync-notes", repo, "origin", f.remote)
	cmd.Stdin = strings.NewReader("refs/heads/main " + sha + " refs/heads/main " + strings.Repeat("0", 40) + "\n")
	start := time.Now()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sync-notes: %v\n%s", err, out)
	}
	if runtime.GOOS != "windows" && time.Since(start) > 2*time.Second {
		t.Errorf("hook call took %v; it must not wait for the network", time.Since(start))
	}

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := noteIn(t, f.remote, sha); ok && strings.Contains(readSyncLog(t, home), "sync ok: ") {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("the background sync never published the note; sync-notes.log:\n%s", readSyncLog(t, home))
}
