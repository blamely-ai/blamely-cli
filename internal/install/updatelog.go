package install

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/blamely/blamely/internal/config"
	"github.com/blamely/blamely/internal/hookdrift"
)

// maxUpdateLogReason caps how much of an error goes on the line. Some failures
// carry a staged installer's whole output; the log is a history, not a dump —
// daemon.log still has the detail.
//
// Every line written here stays ASCII on purpose. Windows PowerShell 5.1's
// Get-Content decodes a BOM-less file with the system ANSI codepage, so an em
// dash or an ellipsis would reach support as mojibake ("â€”") in the one file
// they asked the customer to paste.
const maxUpdateLogReason = 300

// logUpdateOutcome appends one line to ~/.blamely/update.log describing what an
// update attempt did. Called from Update's defer, so every path is covered.
//
// Deliberately silent when nothing was attempted (already on the latest
// version, update checks disabled): a daemon that checks every 24h would
// otherwise write a line a day saying nothing happened, and the interesting
// lines would drown in it.
func logUpdateOutcome(res UpdateResult, err error) {
	var line string
	switch {
	case res.Updated:
		line = fmt.Sprintf("update ok: %s -> %s (channel %s)",
			versionOrUnknown(res.From), versionOrUnknown(res.To), updateChannelOf(res))
		// The new version may well read the hook payloads the old one could not,
		// so start counting afresh: a shape still unread after the update shows
		// up again on the next call.
		_ = hookdrift.Reset()
	case err != nil && res.Reason != "":
		// Reason is the short, human phrase ("checksum mismatch", "staged binary
		// failed to run"); the error is the detail behind it.
		line = fmt.Sprintf("update failed: %s: %s - %s",
			updateVersionRange(res), res.Reason, oneLine(err.Error()))
	case err != nil:
		line = fmt.Sprintf("update failed: %s: %s", updateVersionRange(res), oneLine(err.Error()))
	default:
		return
	}
	appendUpdateLog(line)
}

// updateVersionRange renders "1.8.1 -> 1.8.2", or just "1.8.1" when the attempt
// failed before it learned which version it was going to.
func updateVersionRange(res UpdateResult) string {
	from, to := versionOrUnknown(res.From), versionOrUnknown(res.To)
	if to == from {
		return from
	}
	return from + " -> " + to
}

// updateChannelOf prefers the channel the attempt actually used over the
// currently configured one: a `--channel beta` run must not be logged as
// "latest" just because that is the default now.
func updateChannelOf(res UpdateResult) string {
	if c := strings.TrimSpace(res.Channel); c != "" {
		return c
	}
	return UpdateChannel()
}

func versionOrUnknown(v string) string {
	if v = strings.TrimSpace(v); v != "" {
		return v
	}
	return "unknown"
}

// oneLine flattens and truncates text so a log line stays a line.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > maxUpdateLogReason {
		return s[:maxUpdateLogReason] + "..."
	}
	return s
}

// appendUpdateLog writes one timestamped line. Best-effort: an update must never
// fail because its history couldn't be recorded.
//
// Not rotated, on purpose. A line is ~70 bytes and there is at most one update a
// day, so this stays a few KB a year — small enough that the whole history is
// worth more than the code to trim it.
func appendUpdateLog(line string) {
	path, err := config.UpdateLogFile()
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = fmt.Fprintf(f, "%s %s\n", time.Now().Format("2006-01-02 15:04:05"), line)
}

// LastUpdateLogLine returns the most recent update.log line, for `blamely
// doctor` — the one place a user is already looking when they ask why their
// version is old.
func LastUpdateLogLine() (string, bool) {
	path, err := config.UpdateLogFile()
	if err != nil {
		return "", false
	}
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	var last string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" {
			last = l
		}
	}
	if last == "" {
		return "", false
	}
	return last, true
}
