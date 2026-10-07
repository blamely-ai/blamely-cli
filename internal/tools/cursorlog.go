package tools

// CursorLogWatcher supplements CursorWatcher by tailing Cursor's window /
// extension-host log files for AI-apply events. Cursor (being based on VS
// Code) writes structured JSON-ish logs at:
//
//   macOS  : ~/Library/Application Support/Cursor/logs/<date>/...
//   Linux  : ~/.config/Cursor/logs/<date>/...
//   Windows: %APPDATA%/Cursor/logs/<date>/...
//
// We look for lines that contain file-path strings alongside keywords like
// "composer", "apply", or "agent" — markers that Cursor's AI made a write.
// This is heuristic (the log schema is undocumented), but gives us a
// near-real-time Cursor signal that doesn't depend on history backups.
//
// Attribution from this watcher is confidence=medium (same as the history
// watcher) and covers the whole file, so the attribute join will only assign
// lines that actually appear in the commit diff.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/blamely/blamely/internal/config"
	"github.com/blamely/blamely/internal/daemon"
	"github.com/blamely/blamely/internal/gitutil"
)

// CursorLogWatcher tails Cursor log files for AI apply events.
type CursorLogWatcher struct {
	// LogsDir overrides the default for tests.
	LogsDir string
}

func (c *CursorLogWatcher) Name() string { return "cursor-log" }

func (c *CursorLogWatcher) Run(ctx context.Context, sink daemon.Sink) error {
	dir := c.LogsDir
	if dir == "" {
		var err error
		dir, err = cursorLogsDir()
		if err != nil {
			return err
		}
	}
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		log.Printf("cursor-log: %s not found, will poll", dir)
	}

	// Track which log files we've already attached a tailer to.
	tailers := map[string]context.CancelFunc{}
	defer func() {
		for _, cancel := range tailers {
			cancel()
		}
	}()

	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	for {
		files := findCursorLogFiles(dir)
		seen := map[string]bool{}
		for _, f := range files {
			seen[f] = true
			if _, running := tailers[f]; running {
				continue
			}
			tCtx, cancel := context.WithCancel(ctx)
			tailers[f] = cancel
			go func(path string) {
				if err := tailCursorLog(tCtx, path, sink); err != nil && tCtx.Err() == nil {
					log.Printf("cursor-log tail %s: %v", path, err)
				}
			}(f)
		}
		for path, cancel := range tailers {
			if !seen[path] {
				cancel()
				delete(tailers, path)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// findCursorLogFiles returns all .log files under dir (searching 2 levels deep
// so it catches logs/<date>/exthost/output/xxx.log etc.).
func findCursorLogFiles(dir string) []string {
	var out []string
	// Walk up to depth 4 (logs/<date>/<subdir>/<file>).
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.HasSuffix(p, ".log") {
			// Only tail recent log files (modified within the last day).
			info, err := d.Info()
			if err == nil && time.Since(info.ModTime()) < 24*time.Hour {
				out = append(out, p)
			}
		}
		return nil
	})
	return out
}

// isCursorTabLog reports whether path looks like a Cursor Tab completion log.
// Cursor Tab writes its output to a file named "Cursor Tab.log" inside an
// extension directory that matches "cursor-always-local".
func isCursorTabLog(path string) bool {
	base := filepath.Base(path)
	dir := filepath.Base(filepath.Dir(path))
	return base == "Cursor Tab.log" || strings.Contains(dir, "cursor-always-local")
}

// tailCursorLog streams a Cursor log file looking for AI-apply events.
//
// For Composer/Agent apply events: emits confidence=medium cursor/chat records
// with the applied file path and whole-file line range.
//
// For Cursor Tab logs (isCursorTabLog): the log entry fires on suggestion
// GENERATION, not user acceptance, so we cannot claim specific lines. We do
// emit a low-confidence session marker (no file, no lines) so the daemon knows
// Cursor Tab was recently active — this aids model backfill and debugging.
// Precise Tab attribution (file + lines) requires the Blamely editor plugin,
// which listens to editor.action.inlineSuggest.commit.
func tailCursorLog(ctx context.Context, path string, sink daemon.Sink) error {
	if isCursorTabLog(path) {
		return tailCursorTabLog(ctx, path, sink)
	}

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer f.Close()

	// Seek to end on startup — we only want future log lines, not the history.
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return fmt.Errorf("seek: %w", err)
	}

	r := bufio.NewReaderSize(f, 1<<16)
	backoff := 200 * time.Millisecond
	const maxBackoff = 2 * time.Second
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return err
			}
			// Partial line — wait for more data.
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}
			continue
		}
		backoff = 200 * time.Millisecond
		if filePath, ok := extractCursorApplyPath(line); ok {
			emitCursorLogEvent(filePath, sink)
		}
	}
}

// tailCursorTabLog tails a Cursor Tab completion log and attributes each
// ACCEPTED Tab suggestion to cursor/completion with precise file + line ranges.
//
// Cursor writes each suggestion as a "=======>Model output" marker followed by
// a lightweight diff (see parseCursorTabBlock). The log fires on suggestion
// GENERATION, not acceptance — but every added line is recorded with its
// content_sha, and commit-time attribution only credits lines whose exact text
// ends up in the committed file. A shown-but-rejected suggestion therefore
// never matches anything and is silently ignored, so recording generations is
// safe and needs no separate "accepted" signal. The repo-relative path in the
// diff header is resolved against the owning window's workspace roots (read
// from the sibling exthost.log).
//
// When a block can't be turned into concrete lines (empty diff, or the file
// can't be resolved) we fall back to the old low-confidence session marker so
// the daemon still knows Cursor Tab was active.
func tailCursorTabLog(ctx context.Context, path string, sink daemon.Sink) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer f.Close()

	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return fmt.Errorf("seek: %w", err)
	}

	roots := cursorWindowWorkspaceRoots(path)
	rootsLoaded := time.Now()
	seen := map[string]bool{}
	var block []string
	inBlock := false
	flush := func() {
		if inBlock {
			// Refresh workspace roots lazily — they're stable for a window, but a
			// folder can be added mid-session.
			if len(roots) == 0 || time.Since(rootsLoaded) > 30*time.Second {
				roots = cursorWindowWorkspaceRoots(path)
				rootsLoaded = time.Now()
			}
			emitCursorTabSuggestion(block, roots, seen, sink)
		}
		block = nil
		inBlock = false
	}

	r := bufio.NewReaderSize(f, 1<<16)
	backoff := 200 * time.Millisecond
	const maxBackoff = 2 * time.Second
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return err
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}
			continue
		}
		backoff = 200 * time.Millisecond

		if strings.Contains(line, "=======>Model output") {
			flush() // close any previous block before opening a new one
			inBlock = true
			continue
		}
		if inBlock {
			if isCursorTabBlockEnd(line) {
				flush()
				continue
			}
			block = append(block, strings.TrimRight(line, "\r\n"))
		}
	}
}

// cursorTabMarker is the legacy low-confidence "Cursor Tab was active" signal,
// emitted when a suggestion block can't be resolved to concrete lines.
func cursorTabMarker(sink daemon.Sink) {
	ev := daemon.Event{
		When:       time.Now(),
		Tool:       "cursor",
		Confidence: "low",
		GenType:    "completion",
		RawMeta:    `{"source":"cursor_tab_log"}`,
	}
	if err := sink.Record(ev); err != nil {
		log.Printf("cursor-tab sink: %v", err)
	}
}

// emitCursorTabSuggestion parses one Cursor Tab model-output block, resolves its
// repo-relative path against the window's workspace roots, and records a
// cursor/completion edit carrying per-line content_sha ranges for the added
// lines (and removed-line hashes for replaced lines). De-duplicates identical
// suggestions within the session so a re-shown completion isn't recorded twice.
func emitCursorTabSuggestion(block []string, roots []string, seen map[string]bool, sink daemon.Sink) {
	sug := parseCursorTabBlock(block)
	if sug.RelPath == "" || (len(sug.Added) == 0 && len(sug.Removed) == 0) {
		cursorTabMarker(sink)
		return
	}
	abs, ok := resolveCursorTabFile(sug.RelPath, roots)
	if !ok {
		cursorTabMarker(sink)
		return
	}
	// Resolve repo + worktree-relative path. The file may already be gone (a
	// suggestion that deleted lines, then the file was removed), so fall back to
	// the parent directory, which still exists.
	resolved := abs
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		resolved = r
	}
	repo, _ := gitutil.RepoID(resolved)
	if repo == "" {
		repo, _ = gitutil.RepoID(filepath.Dir(resolved))
	}
	if repo == "" {
		cursorTabMarker(sink)
		return
	}
	wt, _ := gitutil.Toplevel(resolved)
	if wt == "" {
		wt, _ = gitToplevel(filepath.Dir(resolved))
	}
	rel := resolved
	if wt != "" {
		if r, err := filepath.Rel(wt, resolved); err == nil && !strings.HasPrefix(r, "..") {
			rel = r
		}
	}

	// De-dupe: the same suggestion is re-logged as the user keeps typing. Key on
	// the file plus the first added/removed hash so genuinely different
	// completions still record. Bounded so a long session can't grow it forever.
	key := rel + "|" + cursorTabDedupHash(sug)
	if seen[key] {
		return
	}
	if len(seen) > 5000 {
		for k := range seen {
			delete(seen, k)
		}
	}
	seen[key] = true

	ev := daemon.Event{
		When:           time.Now(),
		Tool:           "cursor",
		Confidence:     "medium",
		GenType:        "completion",
		RepoPath:       repo,
		WorktreePath:   wt,
		FilePath:       rel,
		Lines:          toDaemonLineRanges(sug.Added),
		RemovedLines:   toDaemonRemovedLines(sug.Removed),
		SuggestedLines: int64(len(sug.Added)),
		RawMeta:        `{"source":"cursor_tab_log"}`,
	}
	if err := sink.Record(ev); err != nil {
		log.Printf("cursor-tab sink: %v", err)
	}
}

// cursorTabSuggestion is one parsed Cursor Tab model-output block: the
// repo-relative file it targets plus the added/removed lines of its diff.
type cursorTabSuggestion struct {
	RelPath string
	Added   []LineRange       // per-line content_sha ranges for `+` lines
	Removed []DeletedLineHash // content hashes for `-` lines
}

// cursorTabDedupHash returns a stable key for a suggestion's content (first
// added line, else first removed line) used to suppress duplicate emissions.
func cursorTabDedupHash(s cursorTabSuggestion) string {
	if len(s.Added) > 0 {
		return "a:" + s.Added[0].ContentSHA
	}
	if len(s.Removed) > 0 {
		return "d:" + s.Removed[0].ContentSHA
	}
	return ""
}

// parseCursorTabBlock parses the raw lines following a "=======>Model output"
// marker. The block is a lightweight diff:
//
//	@@ <repo-relative-path>:<startLine>
//	-|<old line>            (a removed line)
//	+|<new line>            (an added line)
//	 |<context line>        (unchanged)
//
// Multiple @@ hunks may appear. Added lines are assigned new-file line numbers
// starting at each hunk's start line (removed lines don't advance the counter);
// each carries a content_sha so commit-time attribution credits it only if the
// exact text actually lands in the file (i.e. the suggestion was accepted).
func parseCursorTabBlock(block []string) cursorTabSuggestion {
	var s cursorTabSuggestion
	newLine := 0
	for _, line := range block {
		if p, start, ok := parseCursorTabHunkHeader(line); ok {
			if s.RelPath == "" {
				s.RelPath = p
			}
			newLine = start
			continue
		}
		if s.RelPath == "" || newLine == 0 {
			continue // body before any @@ header — ignore
		}
		marker, text, ok := splitCursorTabDiffLine(line)
		if !ok {
			continue
		}
		switch marker {
		case '+':
			if strings.TrimSpace(text) != "" {
				s.Added = append(s.Added, LineRange{
					Start: newLine, End: newLine,
					ContentSHA:     sha256Hex([]byte(text)),
					ContentSHANorm: sha256HexNorm(text),
				})
			}
			newLine++
		case '-':
			if strings.TrimSpace(text) != "" {
				s.Removed = append(s.Removed, DeletedLineHash{
					ContentSHA:     sha256Hex([]byte(text)),
					ContentSHANorm: sha256HexNorm(text),
				})
			}
			// a removed line doesn't advance the new-file line counter
		default: // context line
			newLine++
		}
	}
	return s
}

// parseCursorTabHunkHeader parses an "@@ <path>:<line>" header, returning the
// repo-relative path and the 1-based start line (defaulting to 1 when no
// line-number suffix is present).
func parseCursorTabHunkHeader(line string) (path string, startLine int, ok bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "@@") {
		return "", 0, false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "@@"))
	startLine = 1
	if i := strings.LastIndex(rest, ":"); i > 0 {
		var n int
		if _, err := fmt.Sscanf(rest[i+1:], "%d", &n); err == nil && n > 0 {
			startLine = n
			rest = rest[:i]
		}
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "", 0, false
	}
	return rest, startLine, true
}

// splitCursorTabDiffLine splits a Cursor Tab diff body line into its marker
// (`+`, `-`, or space for context) and text. Cursor separates the marker from
// the content with a `|` (e.g. "+|  foo"); a missing pipe is tolerated. Returns
// ok=false for lines that don't start with a diff marker.
func splitCursorTabDiffLine(line string) (marker byte, text string, ok bool) {
	if line == "" {
		return 0, "", false
	}
	m := line[0]
	if m != '+' && m != '-' && m != ' ' {
		return 0, "", false
	}
	return m, strings.TrimPrefix(line[1:], "|"), true
}

// isCursorTabBlockEnd reports whether a line ends a Cursor Tab model-output
// block. The diff body is written as raw continuation lines with no timestamp;
// the next normal log entry (timestamped, or another "=======>" section)
// terminates it.
func isCursorTabBlockEnd(line string) bool {
	if strings.Contains(line, "=======>") {
		return true
	}
	return cursorLogTimestampRe.MatchString(line)
}

var cursorLogTimestampRe = regexp.MustCompile(`^\d{4}-\d\d-\d\d[ T]\d\d:\d\d:\d\d`)

// cursorCwdRe extracts workspace folder roots from a Cursor window's
// exthost.log (the extension host logs each folderQuery's "cwd").
var cursorCwdRe = regexp.MustCompile(`"cwd":"([^"]+)"`)

// cursorWindowWorkspaceRoots returns the workspace folder roots for the Cursor
// window that owns tabLogPath. The Tab log lives at
// <window>/exthost/anysphere.cursor-always-local/Cursor Tab.log, and the window's
// exthost.log (two dirs up) records each open folder's absolute path — which is
// what turns the Tab diff's repo-relative path into a real file. Only the first
// chunk of exthost.log is read (the workspace folders are logged at startup).
func cursorWindowWorkspaceRoots(tabLogPath string) []string {
	exthostDir := filepath.Dir(filepath.Dir(tabLogPath))
	f, err := os.Open(filepath.Join(exthostDir, "exthost.log"))
	if err != nil {
		return nil
	}
	defer f.Close()
	buf := make([]byte, 512<<10)
	n, _ := io.ReadFull(f, buf)
	data := buf[:n]

	seen := map[string]bool{}
	var out []string
	for _, m := range cursorCwdRe.FindAllSubmatch(data, -1) {
		p := unescapeJSONString(string(m[1]))
		if p == "" || seen[p] {
			continue
		}
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// unescapeJSONString decodes a raw JSON string value captured by regex (no
// surrounding quotes). On Windows the exthost.log cwd is JSON-escaped
// ("c:\\Users\\name\\repo"); the doubled backslashes must be unescaped before
// os.Stat. The regex capture can't contain a `"`, so re-wrapping is safe.
// Falls back to the raw input if it isn't valid JSON.
func unescapeJSONString(s string) string {
	if !strings.Contains(s, `\`) {
		return s // fast path: nothing escaped (all Unix paths)
	}
	var out string
	if err := json.Unmarshal([]byte(`"`+s+`"`), &out); err != nil {
		return s
	}
	return out
}

// resolveCursorTabFile turns a Cursor Tab diff's repo-relative path into an
// absolute path using the window's workspace roots. An already-absolute path is
// returned as-is; otherwise it's joined onto the root whose directory exists
// (so a since-deleted file still resolves via its surviving parent dir). With a
// single workspace root the join is unconditional.
func resolveCursorTabFile(relPath string, roots []string) (string, bool) {
	if relPath == "" {
		return "", false
	}
	if filepath.IsAbs(relPath) {
		return relPath, true
	}
	for _, root := range roots {
		cand := filepath.Join(root, relPath)
		if _, err := os.Stat(filepath.Dir(cand)); err == nil {
			return cand, true
		}
	}
	if len(roots) == 1 {
		return filepath.Join(roots[0], relPath), true
	}
	return "", false
}

// extractCursorApplyPath looks for apply-event markers in a log line.
// Returns the file path if one is found.
//
// The keyword set is intentionally narrow: it must look like an explicit
// Composer or Agent APPLY event. We deliberately exclude the generic
// "applyedit" / "applyEdit" keyword even though Cursor logs it for AI
// applies — VS Code also emits applyEdit for Tab completions, formatter
// rewrites, auto-import inserts, and other non-AI edits. Matching it
// caused two compounding bugs:
//
//  1. A Tab completion's applyEdit log line (written AFTER the file save)
//     emitted a whole-file cursor/chat row newer than the humanedit row for
//     manually-typed lines in the same session, overriding their attribution.
//
//  2. The gen_type was hard-coded to "chat" in emitCursorLogEvent, so Tab
//     completions showed up as "chat" instead of "completion".
//
// Tab completion attribution is handled by the editor plugin
// (Cursor/VS Code DocumentListener → /edit endpoint).
func extractCursorApplyPath(line string) (string, bool) {
	lower := strings.ToLower(line)
	if !strings.Contains(lower, "composerapply") &&
		!strings.Contains(lower, "composer.apply") &&
		!strings.Contains(lower, "agentedit") &&
		!strings.Contains(lower, "agent.edit") &&
		!strings.Contains(lower, "agent.apply") {
		return "", false
	}
	// Extract the first file:// URI or /abs/path or C:\abs\path from the line.
	return extractFilePath(line)
}

// extractFilePath picks out the first plausible file path from a log line.
// It handles file:// URIs and bare absolute paths.
func extractFilePath(line string) (string, bool) {
	// Try file:// URI first.
	if i := strings.Index(line, "file:///"); i >= 0 {
		rest := line[i:]
		end := strings.IndexAny(rest, " \t\n\r\"'`}),")
		if end < 0 {
			end = len(rest)
		}
		uri := rest[:end]
		p := uriToPath(uri)
		if p != "" {
			return p, true
		}
	}
	// Bare absolute path (Unix: starts with /, Windows: any drive letter,
	// either separator — VS Code/Cursor emit lowercase drives and both
	// "c:\..." and "c:/..." forms). The EARLIEST match wins: "c:/Users/x"
	// contains "/Users/" two bytes in, and only earliest-wins keeps the
	// drive prefix instead of truncating to the embedded Unix form.
	start, minLen := -1, 0
	for _, pfx := range []string{"/Users/", "/home/", "/var/", "/tmp/"} {
		if i := strings.Index(line, pfx); i >= 0 && (start < 0 || i < start) {
			start, minLen = i, len(pfx)
		}
	}
	if m := windowsDrivePathRe.FindStringSubmatchIndex(line); m != nil {
		// m[2]: start of group 1 — the path itself, minus any preceding char.
		if start < 0 || m[2] < start {
			start, minLen = m[2], 3 // more than just "C:\"
		}
	}
	if start >= 0 {
		rest := line[start:]
		end := strings.IndexAny(rest, " \t\n\r\"'`}),")
		if end < 0 {
			end = len(rest)
		}
		p := strings.TrimSpace(rest[:end])
		if len(p) > minLen {
			return p, true
		}
	}
	return "", false
}

// windowsDrivePathRe matches the start of a bare Windows absolute path: any
// drive letter (either case), either separator. The negative char classes
// keep it from firing inside URLs ("https://...") — a URL's scheme letter is
// preceded by more letters and followed by a double slash.
var windowsDrivePathRe = regexp.MustCompile(`(?:^|[^A-Za-z0-9])([A-Za-z]:[\\/][^\\/])`)

func emitCursorLogEvent(abs string, sink daemon.Sink) {
	if _, err := os.Stat(abs); err != nil {
		return
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		abs = r
	}
	repo, _ := gitutil.RepoID(abs)
	wt, _ := gitutil.Toplevel(abs)
	rel := abs
	if wt != "" {
		if r, err := filepath.Rel(wt, abs); err == nil && !strings.HasPrefix(r, "..") {
			rel = r
		}
	}
	lr, err := LineRangeForWholeFile(abs)
	if err != nil || len(lr) == 0 {
		return
	}
	ev := daemon.Event{
		When:         time.Now(),
		Tool:         "cursor",
		Confidence:   "medium",
		GenType:      "chat", // cursor log events are AI apply events (Composer)
		RepoPath:     repo,
		WorktreePath: wt,
		FilePath:     rel,
		Lines:        toDaemonLineRanges(lr),
		RawMeta:      `{"source":"cursor_log"}`,
	}
	if err := sink.Record(ev); err != nil {
		log.Printf("cursor-log sink: %v", err)
	}
}

// DebugCursorLogs tails Cursor's log files and prints detected AI-apply
// events to out. It is the backing implementation of `blamely log cursor`.
//
// When debug is false (default) only lines that match an AI-apply keyword are
// printed. When debug is true every scanned line is printed, prefixed with
// "[MATCH]" or "[skip]", so users can trace why a Cursor Composer action was
// or was not detected.
//
// The function blocks until ctx is cancelled; it is safe to call from a
// cobra RunE with cmd.Context().
func DebugCursorLogs(ctx context.Context, debug bool, out io.Writer) error {
	dir, err := cursorLogsDir()
	if err != nil {
		return fmt.Errorf("cursor logs dir: %w", err)
	}
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("Cursor log directory not found: %s\n"+
			"Is Cursor installed and has it been opened at least once?", dir)
	}

	fmt.Fprintf(out, "Scanning Cursor logs in %s\n", dir)
	if debug {
		fmt.Fprintf(out, "Debug mode: all scanned lines are shown.\n")
		fmt.Fprintf(out, "  [MATCH]            = Composer/Agent apply event (recorded by daemon)\n")
		fmt.Fprintf(out, "  [Tab suggest]      = Cursor Tab suggestion with a diff — added lines recorded with\n")
		fmt.Fprintf(out, "                       content_sha; only those that land in the commit attribute as AI.\n")
		fmt.Fprintf(out, "  [Tab shown]        = Cursor Tab suggestion with no diff (session marker only)\n")
		fmt.Fprintf(out, "  [diff]             = a line of the Tab suggestion's diff body\n")
		fmt.Fprintf(out, "  [skip]             = line scanned, no apply keyword found\n")
	} else {
		fmt.Fprintf(out, "Showing detected AI-apply events only (use --debug to see all lines).\n")
	}
	fmt.Fprintln(out)

	tailers := map[string]context.CancelFunc{}
	defer func() {
		for _, cancel := range tailers {
			cancel()
		}
	}()

	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	for {
		files := findCursorLogFiles(dir)
		for _, f := range files {
			if _, running := tailers[f]; running {
				continue
			}
			tCtx, cancel := context.WithCancel(ctx)
			tailers[f] = cancel
			go func(path string) {
				if err := debugTailCursorLog(tCtx, path, debug, out); err != nil && tCtx.Err() == nil {
					fmt.Fprintf(out, "[error] %s: %v\n", path, err)
				}
			}(f)
		}
		// Prune tailers for files that have gone away.
		seen := make(map[string]bool, len(files))
		for _, f := range files {
			seen[f] = true
		}
		for path, cancel := range tailers {
			if !seen[path] {
				cancel()
				delete(tailers, path)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// debugTailCursorLog tails one log file and writes each line to out.
// In debug mode every line is printed; otherwise only matching lines.
// Handles both Composer apply logs and Cursor Tab completion logs.
func debugTailCursorLog(ctx context.Context, path string, debug bool, out io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer f.Close()

	// Seek to end so we only show new lines written after the command starts.
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return fmt.Errorf("seek: %w", err)
	}

	isTabLog := isCursorTabLog(path)
	shortPath := filepath.Base(filepath.Dir(path)) + "/" + filepath.Base(path)
	roots := cursorWindowWorkspaceRoots(path)
	r := bufio.NewReaderSize(f, 1<<16)
	backoff := 200 * time.Millisecond
	const maxBackoff = 2 * time.Second
	var block []string
	inBlock := false
	flushBlock := func() {
		if !inBlock {
			return
		}
		inBlock = false
		sug := parseCursorTabBlock(block)
		block = nil
		if sug.RelPath == "" || (len(sug.Added) == 0 && len(sug.Removed) == 0) {
			fmt.Fprintf(out, "[Tab shown] %s  (no diff — session marker only)\n", shortPath)
			return
		}
		loc := sug.RelPath
		if abs, ok := resolveCursorTabFile(sug.RelPath, roots); ok {
			loc = abs
		}
		fmt.Fprintf(out, "[Tab suggest] %s  →  %s  +%d/-%d lines (content-matched at commit; only accepted lines attribute)\n",
			shortPath, loc, len(sug.Added), len(sug.Removed))
	}
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return err
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}
			continue
		}
		backoff = 200 * time.Millisecond
		trimmed := strings.TrimRight(line, "\r\n")

		if isTabLog {
			if strings.Contains(line, "=======>Model output") {
				flushBlock()
				inBlock = true
				continue
			}
			if inBlock {
				if isCursorTabBlockEnd(line) {
					flushBlock()
				} else {
					block = append(block, trimmed)
					if debug {
						fmt.Fprintf(out, "[diff]  %s  %s\n", shortPath, trimmed)
					}
					continue
				}
			}
		} else {
			if filePath, ok := extractCursorApplyPath(line); ok {
				fmt.Fprintf(out, "[MATCH] %s  →  %s\n", shortPath, filePath)
				continue
			}
		}

		if debug {
			fmt.Fprintf(out, "[skip]  %s  %s\n", shortPath, trimmed)
		}
	}
}

func cursorLogsDir() (string, error) {
	home, err := config.Home()
	if err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Cursor", "logs"), nil
	case "windows":
		if appData := os.Getenv("APPDATA"); appData != "" {
			return filepath.Join(appData, "Cursor", "logs"), nil
		}
		return "", errors.New("APPDATA not set")
	default:
		return filepath.Join(home, ".config", "Cursor", "logs"), nil
	}
}
