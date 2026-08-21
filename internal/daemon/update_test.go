package daemon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blamely/blamely/internal/config"
	"github.com/blamely/blamely/internal/updatehint"
)

// pinUpdateHooks installs check/apply stubs for one test and restores whatever
// was there, so the package-level vars can't leak between tests.
func pinUpdateHooks(t *testing.T,
	check func(ctx context.Context) (updatehint.Hint, bool, error),
	apply func(ctx context.Context) error,
) {
	t.Helper()
	prevCheck, prevApply, prevDelay := CheckUpdate, ApplyUpdate, updateCheckInitialDelay
	prevRetry := updateRetryInterval
	CheckUpdate, ApplyUpdate, updateCheckInitialDelay = check, apply, time.Millisecond
	updateRetryInterval = time.Millisecond
	t.Cleanup(func() {
		CheckUpdate, ApplyUpdate, updateCheckInitialDelay = prevCheck, prevApply, prevDelay
		updateRetryInterval = prevRetry
	})
}

// TestWatchForUpdates_OfflineIsSilent is the corp-network case: api.github.com is
// blocked, so every check errors. Nothing must be recorded and nothing must
// break — an unreachable releases API has to look exactly like "up to date".
func TestWatchForUpdates_OfflineIsSilent(t *testing.T) {
	fakeHome(t)
	checked := make(chan struct{}, 1)
	pinUpdateHooks(t,
		func(ctx context.Context) (updatehint.Hint, bool, error) {
			select {
			case checked <- struct{}{}:
			default:
			}
			return updatehint.Hint{}, false, errors.New("dial tcp: connection refused")
		},
		func(ctx context.Context) error {
			t.Error("apply must never run when the check failed")
			return nil
		},
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { watchForUpdates(ctx); close(done) }()

	select {
	case <-checked:
	case <-time.After(2 * time.Second):
		t.Fatal("the check never ran")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchForUpdates did not exit on context cancel")
	}
	if _, ok := updatehint.Read(); ok {
		t.Error("a failed check must not write a hint")
	}
}

// The shipped default is auto-apply, so a machine with no config file of its own
// installs the update it just found.
func TestRunUpdateCheck_AppliesByDefault(t *testing.T) {
	fakeHome(t)
	applied := false
	pinUpdateHooks(t,
		func(ctx context.Context) (updatehint.Hint, bool, error) {
			return updatehint.Hint{Version: "1.9.0", Tag: "v1.9.0"}, true, nil
		},
		func(ctx context.Context) error { applied = true; return nil },
	)

	runUpdateCheck(context.Background())

	if !applied {
		t.Error("update.auto defaults to on, so an available update must be installed")
	}
	// Once installed, the hint describes the version now running — it must not
	// keep telling the user to update.
	if _, ok := updatehint.Read(); ok {
		t.Error("hint must be cleared after a successful auto-install")
	}
}

// Opting out keeps the notice without the install: this is what a
// change-controlled fleet sets.
func TestRunUpdateCheck_AutoOffOnlyRecordsHint(t *testing.T) {
	fakeHome(t)
	cfg := config.DefaultConfig()
	cfg.Update.Auto = false
	if _, err := config.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	applied := false
	pinUpdateHooks(t,
		func(ctx context.Context) (updatehint.Hint, bool, error) {
			return updatehint.Hint{Version: "1.9.0", Tag: "v1.9.0"}, true, nil
		},
		func(ctx context.Context) error { applied = true; return nil },
	)

	runUpdateCheck(context.Background())

	if applied {
		t.Error("update was applied with update.auto off")
	}
	h, ok := updatehint.Read()
	if !ok {
		t.Fatal("no hint recorded for an available update")
	}
	if h.Version != "1.9.0" || h.CheckedAt.IsZero() {
		t.Errorf("hint = %+v, want version 1.9.0 with a CheckedAt stamp", h)
	}
}

// A failed install must leave the hint in place: the user still needs to know an
// update is waiting, and the previous version is still what's running.
func TestRunUpdateCheck_FailedAutoInstallKeepsHint(t *testing.T) {
	fakeHome(t)
	pinUpdateHooks(t,
		func(ctx context.Context) (updatehint.Hint, bool, error) {
			return updatehint.Hint{Version: "1.9.0", Tag: "v1.9.0"}, true, nil
		},
		func(ctx context.Context) error { return errors.New("checksum mismatch") },
	)

	runUpdateCheck(context.Background())

	if _, ok := updatehint.Read(); !ok {
		t.Error("hint must survive a failed auto-install")
	}
}

func TestRunUpdateCheck_UpToDateClearsStaleHint(t *testing.T) {
	fakeHome(t)
	if err := updatehint.Write(updatehint.Hint{Version: "1.9.0", Tag: "v1.9.0"}); err != nil {
		t.Fatal(err)
	}
	pinUpdateHooks(t,
		func(ctx context.Context) (updatehint.Hint, bool, error) {
			return updatehint.Hint{Version: "1.9.0"}, false, nil
		},
		nil,
	)

	runUpdateCheck(context.Background())

	if _, ok := updatehint.Read(); ok {
		t.Error("hint must be cleared once we are up to date (e.g. after a manual update)")
	}
}

func TestRunUpdateCheck_DisabledByEnvKillSwitch(t *testing.T) {
	fakeHome(t)
	t.Setenv("BLAMELY_NO_UPDATE_CHECK", "1")
	pinUpdateHooks(t,
		func(ctx context.Context) (updatehint.Hint, bool, error) {
			t.Error("check ran despite BLAMELY_NO_UPDATE_CHECK=1")
			return updatehint.Hint{}, false, nil
		},
		nil,
	)
	runUpdateCheck(context.Background())
}

// stopWatcher cancels the watcher and waits for it to actually return. Leaving
// it running past the test is not harmless: t.Cleanup then restores CheckUpdate
// to nil underneath a loop that calls it, panicking the whole package.
func stopWatcher(t *testing.T, cancel context.CancelFunc, done <-chan struct{}) {
	t.Helper()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchForUpdates did not stop after its context was cancelled")
	}
}

// TestWatchForUpdates_FirstCheckConvergesFreshInstall pins down the behaviour a
// fresh machine depends on: whatever release the installer script fetched, the
// daemon reaches the current one on its own shortly after startup — no user
// action, no waiting out the 24h cadence.
func TestWatchForUpdates_FirstCheckConvergesFreshInstall(t *testing.T) {
	fakeHome(t)
	applied := make(chan struct{}, 1)
	pinUpdateHooks(t,
		func(ctx context.Context) (updatehint.Hint, bool, error) {
			return updatehint.Hint{Version: "1.8.1", Tag: "v1.8.1"}, true, nil
		},
		func(ctx context.Context) error {
			select {
			case applied <- struct{}{}:
			default:
			}
			return nil
		},
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { watchForUpdates(ctx); close(done) }()
	defer stopWatcher(t, cancel, done)

	select {
	case <-applied:
	case <-time.After(2 * time.Second):
		t.Fatal("a newer release was available and never installed")
	}
}

// TestWatchForUpdates_RetriesAfterFailedInstall is the regression guard for the
// gap this exposed: the check found a newer release, the install failed, and the
// loop then slept the FULL interval — so one transient failure on a fresh
// machine left it on the old version for a day.
func TestWatchForUpdates_RetriesAfterFailedInstall(t *testing.T) {
	fakeHome(t)
	attempts := make(chan int, maxUpdateRetries+2)
	var n int
	var mu sync.Mutex
	pinUpdateHooks(t,
		func(ctx context.Context) (updatehint.Hint, bool, error) {
			return updatehint.Hint{Version: "1.8.1", Tag: "v1.8.1"}, true, nil
		},
		func(ctx context.Context) error {
			mu.Lock()
			n++
			cur := n
			mu.Unlock()
			select {
			case attempts <- cur:
			default:
			}
			return errors.New("fork/exec: the request is not supported")
		},
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { watchForUpdates(ctx); close(done) }()
	defer stopWatcher(t, cancel, done)

	// A second attempt proves the failure shortened the wait: the configured
	// interval is 24h, so without the retry path nothing else would run.
	deadline := time.After(2 * time.Second)
	seen := 0
	for seen < 2 {
		select {
		case <-attempts:
			seen++
		case <-deadline:
			t.Fatalf("only %d install attempt(s); a failed install must be retried", seen)
		}
	}
}

// TestWatchForUpdates_RetriesAreBounded keeps a permanently broken install from
// re-downloading the release every retry interval forever: after maxUpdateRetries
// consecutive failures the loop must fall back to the normal cadence.
func TestWatchForUpdates_RetriesAreBounded(t *testing.T) {
	fakeHome(t)
	var mu sync.Mutex
	var n int
	pinUpdateHooks(t,
		func(ctx context.Context) (updatehint.Hint, bool, error) {
			return updatehint.Hint{Version: "1.8.1", Tag: "v1.8.1"}, true, nil
		},
		func(ctx context.Context) error {
			mu.Lock()
			n++
			mu.Unlock()
			return errors.New("still broken")
		},
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { watchForUpdates(ctx); close(done) }()
	defer stopWatcher(t, cancel, done)

	// With a 1ms retry interval, an unbounded loop would run away; the bound
	// caps it at the initial attempt plus maxUpdateRetries before the 24h sleep.
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	got := n
	mu.Unlock()
	if got > 1+maxUpdateRetries {
		t.Errorf("%d install attempts; want at most %d before falling back to the normal interval", got, 1+maxUpdateRetries)
	}
	if got < 2 {
		t.Errorf("%d install attempts; the failure should have been retried at least once", got)
	}
}

// TestWatchForUpdates_LogsTheSchedule is the answer to "did it ever check?".
// Before these lines a machine that was already up to date wrote NOTHING about
// updates to daemon.log, so a failed auto-update and a check that never ran
// looked identical from the outside.
func TestWatchForUpdates_LogsTheSchedule(t *testing.T) {
	fakeHome(t)
	var buf syncBuffer
	restore := pinLogOutput(t, &buf)
	defer restore()

	CurrentVersion = "1.8.0"
	t.Cleanup(func() { CurrentVersion = "" })

	checked := make(chan struct{}, 1)
	pinUpdateHooks(t,
		func(ctx context.Context) (updatehint.Hint, bool, error) {
			select {
			case checked <- struct{}{}:
			default:
			}
			return updatehint.Hint{}, false, nil // up to date
		},
		func(ctx context.Context) error {
			t.Error("nothing to install when already up to date")
			return nil
		},
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { watchForUpdates(ctx); close(done) }()
	select {
	case <-checked:
	case <-time.After(2 * time.Second):
		t.Fatal("the check never ran")
	}
	// Let the post-check lines land before cancelling.
	time.Sleep(50 * time.Millisecond)
	stopWatcher(t, cancel, done)

	got := buf.String()
	for _, want := range []string{
		"update: first check in",
		"update: checking now (current 1.8.0, channel latest)",
		"update: up to date (1.8.0)",
		"update: next check in",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("daemon log missing %q:\n%s", want, got)
		}
	}
}

// syncBuffer is a bytes.Buffer safe for the logger's goroutine and the test's.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// pinLogOutput captures log output for one test.
func pinLogOutput(t *testing.T, w io.Writer) func() {
	t.Helper()
	prev := log.Writer()
	log.SetOutput(w)
	return func() { log.SetOutput(prev) }
}
