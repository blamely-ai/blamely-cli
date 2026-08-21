package daemon

import (
	"context"
	"log"
	"os"
	"strings"
	"time"

	"github.com/blamely/blamely/internal/config"
	"github.com/blamely/blamely/internal/updatehint"
)

// CheckUpdate resolves whether a newer blamely release exists, returning the
// hint to record and whether it is actually newer than the running version.
//
// ApplyUpdate downloads and installs that release.
//
// Both are assigned by cmd/blamely at startup rather than called directly,
// because internal/daemon must NOT import internal/install — install already
// imports daemon, so the direct call would be an import cycle. This is the same
// indirection Watchers and DBWatcherFactories already use.
var (
	CheckUpdate func(ctx context.Context) (updatehint.Hint, bool, error)
	ApplyUpdate func(ctx context.Context) error
	// CurrentVersion is the running build's version, for the log lines below.
	// Assigned the same way and for the same reason: install.Version is not
	// reachable from here.
	CurrentVersion string
)

// updateCheckInitialDelay keeps the first check away from daemon startup, where
// it would compete with the watchers coming up and with the installer's health
// wait. A var so tests don't have to wait it out.
//
// This is also what converges a freshly installed machine: whatever release the
// user's installer script happened to fetch, five minutes later the daemon pulls
// the current one in the background.
var updateCheckInitialDelay = 5 * time.Minute

// updateRetryInterval is how soon a FAILED install is retried, instead of
// waiting out the full check interval. That first post-install check is the one
// that matters — a new machine should reach the current release while the user
// is still at the keyboard — and a single transient failure (a half-written
// download, a binary locked by an editor, a daemon mid-restart) used to push the
// next attempt a whole day out. A var so tests don't have to wait it out.
var updateRetryInterval = 30 * time.Minute

// maxUpdateRetries bounds those quick retries. A genuinely broken install (wrong
// arch, blocked by policy, no disk) settles back to the normal cadence instead
// of re-downloading the release every half hour forever.
const maxUpdateRetries = 2

// watchForUpdates periodically asks whether a newer blamely exists, records the
// answer as a hint for the CLI to surface, and — unless update.auto has been
// turned off — installs it.
//
// Every failure — offline, proxy, blocked api.github.com, rate limit — is a
// silent return: nothing in the attribution or git path depends on this
// goroutine, and a machine that can't reach the network must behave exactly like
// one that is up to date.
func watchForUpdates(ctx context.Context) {
	if CheckUpdate == nil {
		log.Printf("update: checks unavailable (no updater wired in)")
		return
	}
	// Say up front that the first check is coming and when. Until this line
	// existed, a machine that was already up to date produced NO update output
	// at all, so "did it ever check?" could not be answered from daemon.log —
	// which is exactly how a failing auto-update read as one that never ran.
	log.Printf("update: first check in %s, then every %dh",
		updateCheckInitialDelay, config.LoadConfig().UpdateIntervalHours())
	if !sleepCtx(ctx, updateCheckInitialDelay) {
		return
	}
	retries := 0
	for {
		interval := time.Duration(config.LoadConfig().UpdateIntervalHours()) * time.Hour
		// Only an install that ATTEMPTED and failed earns a short retry. An
		// offline machine, a machine already up to date, and one with auto
		// updates switched off all keep the normal cadence — being unable to
		// reach the releases API is the steady state on a locked-down network,
		// not something to poll harder about.
		if runUpdateCheck(ctx) == installFailed && retries < maxUpdateRetries {
			retries++
			interval = updateRetryInterval
		} else {
			retries = 0
		}
		log.Printf("update: next check in %s", interval)
		if !sleepCtx(ctx, interval) {
			return
		}
	}
}

// updateOutcome is what one check round did, as far as the retry policy cares.
type updateOutcome int

const (
	// checkSettled covers every round that should wait out the normal interval:
	// up to date, unreachable, auto-install disabled, or installed fine.
	checkSettled updateOutcome = iota
	// installFailed means a newer release was found and the install of it
	// failed — the one case worth retrying sooner.
	installFailed
)

func runUpdateCheck(ctx context.Context) updateOutcome {
	// CheckUpdate is a package var assigned by cmd/blamely; guard it here as
	// well as at the top of watchForUpdates, symmetrically with ApplyUpdate
	// below, so no caller of this function can nil-deref.
	if CheckUpdate == nil {
		return checkSettled
	}
	if reason := updateCheckDisabledReason(); reason != "" {
		log.Printf("update: checks disabled (%s)", reason)
		return checkSettled
	}
	log.Printf("update: checking now (current %s, channel %s)",
		versionLabel(), channelLabel())
	hint, newer, err := CheckUpdate(ctx)
	if err != nil {
		// One line per interval, at most. An unreachable releases API is the
		// normal state on a locked-down corporate network, not an incident.
		log.Printf("update: check failed: %v", err)
		return checkSettled
	}
	if !newer {
		log.Printf("update: up to date (%s)", versionLabel())
		_ = updatehint.Clear()
		return checkSettled
	}
	hint.CheckedAt = time.Now()
	if err := updatehint.Write(hint); err != nil {
		log.Printf("update: recording the hint failed: %v", err)
	}
	if !config.LoadConfig().Update.Auto || ApplyUpdate == nil {
		log.Printf("update: %s available — auto-install is off, run `blamely update`", hint.Version)
		return checkSettled
	}
	log.Printf("update: %s available — installing in the background", hint.Version)
	if err := ApplyUpdate(ctx); err != nil {
		log.Printf("update: installing %s failed: %v", hint.Version, err)
		return installFailed
	}
	log.Printf("update: %s installed (the daemon is restarted by the installer)", hint.Version)
	// The new binary is in place; the hint no longer describes anything the user
	// needs to act on. (This process is about to be restarted by the installer.)
	_ = updatehint.Clear()
	return checkSettled
}

// versionLabel is CurrentVersion, or a placeholder when cmd/blamely did not set
// it (only reachable from a test or an embedder).
func versionLabel() string {
	if strings.TrimSpace(CurrentVersion) == "" {
		return "unknown"
	}
	return CurrentVersion
}

// channelLabel names the release channel the check will use, resolved the same
// way install.UpdateChannel resolves it — repeated here rather than called for
// the import-cycle reason described on CheckUpdate.
func channelLabel() string {
	if v := strings.TrimSpace(os.Getenv("BLAMELY_CHANNEL")); v != "" {
		return v
	}
	if c := strings.TrimSpace(config.LoadConfig().Update.Channel); c != "" {
		return c
	}
	return "latest"
}

// updateCheckDisabledReason mirrors install.UpdateCheckDisabled. It re-reads
// the env and config here rather than calling that function because of the
// import cycle described on CheckUpdate above.
//
// It names what turned checking off — the env var or the config key — so the
// log can say which one, or "" when checks are on.
// Fleets disable checks via `blamely config set update.check off`, and a log
// line blaming BLAMELY_NO_UPDATE_CHECK would send whoever reads it to the
// wrong knob.
func updateCheckDisabledReason() string {
	if v := strings.TrimSpace(os.Getenv("BLAMELY_NO_UPDATE_CHECK")); v != "" && v != "0" {
		return "BLAMELY_NO_UPDATE_CHECK"
	}
	if !config.LoadConfig().Update.Check {
		return "update.check off"
	}
	return ""
}

// sleepCtx waits for d, returning false if the context is cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
