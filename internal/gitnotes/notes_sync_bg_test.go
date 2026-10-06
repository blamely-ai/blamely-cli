package gitnotes

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/blamely/blamely/internal/filelock"
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

// The lock serializes syncs, frees itself when its holder goes away, and is
// never removed from under a live holder.
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
	// However old the lock file looks, a live holder keeps it.
	p := syncStatePath(repo, syncLockFile)
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	time.Sleep(700 * time.Millisecond)
	select {
	case <-got:
		t.Fatal("second sync took the lock while the first held it")
	default:
	}
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
}

// A sync killed mid-run leaves no lock behind: the OS drops it with the process.
func TestLockSync_ReleasedWhenHolderDies(t *testing.T) {
	f := newSyncFixture(t, false)
	repo := f.clone()
	p := syncStatePath(repo, syncLockFile)
	// Hold the lock in a child process (this test binary, see
	// TestHelperHoldSyncLock), then kill it.
	holder := exec.Command(os.Args[0], "-test.run=^TestHelperHoldSyncLock$")
	holder.Env = append(os.Environ(), "BLAMELY_HOLD_LOCK="+p)
	stdout, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 7)
	if _, err := io.ReadFull(stdout, buf); err != nil || string(buf) != "locked\n" {
		t.Fatalf("holder did not take the lock: %v %q", err, buf)
	}
	if _, ok, _ := filelock.TryLock(p); ok {
		t.Fatal("lock must be held while the holder runs")
	}
	_ = holder.Process.Kill()
	_ = holder.Wait()
	u, err := lockSync(repo)
	if err != nil {
		t.Fatalf("lock not free after its holder died: %v", err)
	}
	u()
}

// Worktrees share the notes ref and the scratch refs, so they must share the
// lock too: a sync in one worktree waits for a sync in another.
func TestLockSync_SharedAcrossWorktrees(t *testing.T) {
	f := newSyncFixture(t, false)
	repo := f.clone()
	commitWithNote(t, repo, "a.txt", "PROJ-1 a", `{"n":"a"}`)
	wt := filepath.Join(t.TempDir(), "wt")
	runIn(t, repo, "worktree", "add", "-q", "-b", "other", wt)

	real := func(p string) string { // macOS: /var is /private/var
		d, err := filepath.EvalSymlinks(filepath.Dir(p))
		if err != nil {
			t.Fatal(err)
		}
		return filepath.Join(d, filepath.Base(p))
	}
	if a, b := real(syncStatePath(repo, syncLockFile)), real(syncStatePath(wt, syncLockFile)); a != b {
		t.Fatalf("lock paths differ: %q vs %q", a, b)
	}
	unlock, err := lockSync(repo)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan struct{})
	go func() {
		if u, err := lockSync(wt); err == nil {
			close(got)
			u()
		}
	}()
	select {
	case <-got:
		t.Fatal("the other worktree took the lock while this one held it")
	case <-time.After(1500 * time.Millisecond):
	}
	unlock()
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("the other worktree never got the lock")
	}
}

// Pushes from two worktrees at once both publish their notes: the shared lock
// keeps one sync from clearing the other's scratch refs mid-build.
func TestSyncNotesAndRecord_ConcurrentWorktrees(t *testing.T) {
	tempHome(t)
	f := newSyncFixture(t, false)
	repo := f.clone()
	wt := filepath.Join(t.TempDir(), "wt")
	runIn(t, repo, "worktree", "add", "-q", "-b", "other", wt)
	shaA := commitWithNote(t, repo, "a.txt", "PROJ-1 a", `{"n":"a"}`)
	shaB := commitWithNote(t, wt, "b.txt", "PROJ-2 b", `{"n":"b"}`)
	runIn(t, repo, "push", "-q", "origin", "HEAD")
	runIn(t, wt, "push", "-q", "origin", "other")

	errs := make(chan error, 2)
	for _, dir := range []string{repo, wt} {
		go func(dir string) { errs <- SyncNotesAndRecord(dir, "origin", "", nil) }(dir)
	}
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Errorf("sync: %v", err)
		}
	}
	for _, sha := range []string{shaA, shaB} {
		if _, ok := noteIn(t, f.remote, sha); !ok {
			t.Errorf("remote is missing the note for %s", sha[:7])
		}
	}
}

// TestHelperHoldSyncLock is not a test: TestLockSync_ReleasedWhenHolderDies runs
// this binary with BLAMELY_HOLD_LOCK set to make it a process holding the lock.
func TestHelperHoldSyncLock(t *testing.T) {
	p := os.Getenv("BLAMELY_HOLD_LOCK")
	if p == "" {
		t.Skip("helper process only")
	}
	if _, ok, err := filelock.TryLock(p); err != nil || !ok {
		os.Exit(1)
	}
	os.Stdout.WriteString("locked\n")
	time.Sleep(time.Hour)
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
