package gitnotes

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/blamely/blamely/internal/config"
	"github.com/blamely/blamely/internal/filelock"
	"github.com/blamely/blamely/internal/gitutil"
	"github.com/blamely/blamely/internal/procattr"
)

// The pre-push hook used to run SyncNotes in the foreground, so every `git push`
// also waited for a notes fetch and a notes push to the same server — two more
// round trips, each authenticating again (a Git Credential Manager start on
// Windows), with networkTimeout as the only bound. Now the hook hands the work
// to a detached `blamely sync-notes --wait` and returns at once.
//
// Running detached, nobody is watching its output, so a failure is kept in
// syncErrorFile and printed by the next push's hook instead: a rejected notes
// push must not go unnoticed for good. Every attempt is also appended to
// ~/.blamely/sync-notes.log (config.SyncLogFile), the history to look at when a
// push reported a failure or notes never showed up on the server.

// syncLockFile serializes syncs in one repo. A sync clears every scratch ref
// under syncScratchBase when it starts, so two overlapping background runs (two
// quick pushes) would break each other's half-built chains.
//
// It is an OS lock (internal/filelock), not a marker file: the OS releases it
// when its holder exits, so a killed sync never leaves it behind, and no one can
// ever remove a lock another process still holds — there is no "stale" lock to
// guess at, however long a sync's network retries take.
const syncLockFile = "blamely-sync.lock"

// syncErrorFile holds the last background sync's failure until a push reports it.
// Like the lock it lives in the common git directory: the notes ref, and so a
// failed sync, belongs to the repository, not to the worktree that pushed.
const syncErrorFile = "blamely-sync.error"

// syncLockWait bounds how long a sync waits for an earlier one to finish: longer
// than a sync can take (a ref-race retry makes two attempts, each a fetch and a
// push bounded by networkTimeout).
const syncLockWait = 5 * networkTimeout

// StartSyncNotesInBackground launches `exe sync-notes --wait` for this push,
// detached, and returns without waiting for it. tips are the commits the push
// sends (from the pre-push stdin, which the child cannot read).
func StartSyncNotesInBackground(exe, repo, remote, url string, tips []string) error {
	args := []string{"sync-notes", "--wait"}
	for _, t := range tips {
		args = append(args, "--tip", t)
	}
	args = append(args, repo, remote)
	if url != "" {
		args = append(args, url)
	}
	cmd := procattr.Detach(exec.Command(exe, args...))
	// No terminal is attached any more: a credential prompt would hang until the
	// timeout instead of reaching the user. The user's own push has just
	// authenticated, so a credential helper normally answers without asking.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// SyncNotesAndRecord is SyncNotes for a background run: it waits its turn behind
// any sync already running in repo, then records a failure for the next push to
// report (and clears the record on success).
func SyncNotesAndRecord(repo, remote, url string, pushedTips []string) error {
	unlock, err := lockSync(repo)
	if err != nil {
		LogSyncOutcome(repo, remote, err)
		recordSyncError(repo, remote, err)
		return err
	}
	defer unlock()
	err = SyncNotes(repo, remote, url, pushedTips)
	LogSyncOutcome(repo, remote, err)
	if err != nil {
		recordSyncError(repo, remote, err)
	} else if p := syncStatePath(repo, syncErrorFile); p != "" {
		_ = os.Remove(p)
	}
	return err
}

// ReportPreviousSyncFailure prints, and forgets, the failure a background sync
// recorded since the last push.
func ReportPreviousSyncFailure(repo string, w io.Writer) {
	p := syncStatePath(repo, syncErrorFile)
	if p == "" {
		return
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return
	}
	_ = os.Remove(p)
	if msg := strings.TrimSpace(string(data)); msg != "" {
		fmt.Fprintf(w, "blamely: %s\n", strings.ReplaceAll(msg, "\n", "\n  "))
	}
}

func recordSyncError(repo, remote string, err error) {
	p := syncStatePath(repo, syncErrorFile)
	if p == "" {
		return
	}
	msg := fmt.Sprintf("the last sync of %s to %s failed - attribution stays local until a push succeeds\n%s",
		NotesRef, remote, err.Error())
	if lp, lerr := config.SyncLogFile(); lerr == nil {
		msg += "\n(history: " + lp + ")"
	}
	_ = os.WriteFile(p, []byte(msg+"\n"), 0o644)
}

// syncStatePath returns name inside the repository's COMMON git directory.
//
// Not gitPath: in a linked worktree that resolves to the worktree's private
// .git/worktrees/<name>/, while the notes ref and the refs/blamely-sync scratch
// refs a sync works in are shared by every worktree. Per-worktree locks let two
// worktrees' pushes sync at once and clear each other's scratch refs.
func syncStatePath(repo, name string) string {
	out, err := runGit(gitutil.DefaultTimeout, repo, nil, nil, "rev-parse", "--git-common-dir")
	dir := strings.TrimSpace(out)
	if err != nil || dir == "" {
		return ""
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(repo, dir) // relative to -C repo
	}
	return filepath.Join(dir, name)
}

// lockSync takes the repo's sync lock, waiting up to syncLockWait for a running
// sync to finish. The returned func releases it.
func lockSync(repo string) (func(), error) {
	p := syncStatePath(repo, syncLockFile)
	if p == "" {
		return nil, errors.New("cannot locate the git directory")
	}
	deadline := time.Now().Add(syncLockWait)
	for {
		f, ok, err := filelock.TryLock(p)
		if err != nil {
			return nil, err
		}
		if ok {
			return func() { _ = f.Close() }, nil
		}
		if time.Now().After(deadline) {
			return nil, errors.New("another notes sync is still running")
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// maxSyncLogLine caps how much of an error goes on a line; a server's rejection
// can carry a long hook output. ASCII-only lines, for the same reason as
// update.log: Windows PowerShell 5.1 reads a BOM-less file in the ANSI codepage.
const maxSyncLogLine = 400

// maxSyncLogSize bounds sync-notes.log. Unlike update.log it gets a line per
// push, so it is trimmed to its newer half once it grows past this.
const maxSyncLogSize = 256 << 10

// LogSyncOutcome appends one line to ~/.blamely/sync-notes.log for a sync
// attempt. Best-effort: a sync must never fail because its history couldn't be
// recorded.
func LogSyncOutcome(repo, remote string, err error) {
	line := fmt.Sprintf("sync ok: %s -> %s", repo, remote)
	if err != nil {
		line = fmt.Sprintf("sync failed: %s -> %s: %s", repo, remote, err.Error())
	}
	appendSyncLog(line)
}

func appendSyncLog(line string) {
	path, err := config.SyncLogFile()
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	trimSyncLog(path)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = fmt.Fprintf(f, "%s %s\n", time.Now().Format("2006-01-02 15:04:05"), asciiLine(line))
}

// trimSyncLog keeps the newer half of the log once it is past maxSyncLogSize,
// cut at a line boundary.
func trimSyncLog(path string) {
	st, err := os.Stat(path)
	if err != nil || st.Size() <= maxSyncLogSize {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	data = data[len(data)-maxSyncLogSize/2:]
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		data = data[i+1:]
	}
	_ = os.WriteFile(path, data, 0o644)
}

// asciiLine flattens s to one line of printable ASCII, truncated.
func asciiLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e {
			return '?'
		}
		return r
	}, s)
	if len(s) > maxSyncLogLine {
		s = s[:maxSyncLogLine] + "..."
	}
	return s
}
