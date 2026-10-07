package tools

// CopilotLogWatcher tails the editor log files that the GitHub Copilot
// plugin writes in VS Code, Cursor, and every JetBrains IDE, looking for
// lines that indicate the user accepted (or had the IDE apply) an inline
// completion. When a matching line carries a file path, we emit a
// per-file Copilot edit event with gen_type=completion; otherwise we
// emit a session-active marker so the existing humanedit fold-in can
// still credit Copilot.
//
// Why a log tailer
// ----------------
// The Copilot extension/plugin has no public API or hook for completion
// accepts in the editor (only the standalone Copilot CLI does). The
// closest local signal is the extension's own log output, which we don't
// own and is undocumented — but every released version of the plugin
// emits *something* recognizable when a completion is produced or
// accepted. Pattern matches are intentionally tolerant: a missed match
// just falls through to the lower-precision CopilotWatcher signal.
//
// Roots scanned per platform
// --------------------------
// macOS:
//   ~/Library/Application Support/{Code,Cursor}/logs/
//   ~/Library/Logs/JetBrains/<IDE>/
// Linux:
//   ~/.config/{Code,Cursor}/logs/
//   ~/.cache/JetBrains/<IDE>/log/
//   ~/.local/share/JetBrains/<IDE>/log/
// Windows:
//   %APPDATA%/{Code,Cursor}/logs/
//   %LOCALAPPDATA%/JetBrains/<IDE>/log/
//
// JetBrains IDEs (IntelliJ, GoLand, PyCharm, WebStorm, RubyMine, …) all
// nest under the same JetBrains parent — we discover them by listing
// that parent directory, which means newly installed IDEs are picked up
// without code changes.

import (
	"bufio"
	"context"
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

// copilotLogTailRefresh is how often we re-scan the root list for new log
// files (or new IDE directories the user just installed). 5s matches the
// other polling watchers' rhythm.
const copilotLogTailRefresh = 5 * time.Second

// copilotLogMaxAge is the cut-off for a log file being "current". Older
// files are skipped entirely (no tailer is attached) so we don't reopen
// archived logs from previous IDE sessions.
const copilotLogMaxAge = 24 * time.Hour

// CopilotLogWatcher implements daemon.Watcher.
type CopilotLogWatcher struct {
	// Roots overrides the discovered roots for tests.
	Roots []string
}

func (c *CopilotLogWatcher) Name() string { return "copilot-log" }

func (c *CopilotLogWatcher) Run(ctx context.Context, sink daemon.Sink) error {
	// running map ensures we don't double-attach a tailer to the same log
	// file. The value is a cancel func so a deleted/rotated log can have
	// its tailer torn down on the next refresh.
	running := map[string]context.CancelFunc{}
	defer func() {
		for _, cancel := range running {
			cancel()
		}
	}()

	tick := time.NewTicker(copilotLogTailRefresh)
	defer tick.Stop()
	for {
		c.refresh(ctx, running, sink)
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

func (c *CopilotLogWatcher) refresh(ctx context.Context, running map[string]context.CancelFunc, sink daemon.Sink) {
	roots := c.Roots
	if len(roots) == 0 {
		roots = defaultCopilotLogRoots()
	}
	// Discover candidate log files across all roots.
	seen := map[string]bool{}
	for _, root := range roots {
		for _, p := range findRecentLogFiles(root) {
			seen[p] = true
			if _, ok := running[p]; ok {
				continue
			}
			tCtx, cancel := context.WithCancel(ctx)
			running[p] = cancel
			go func(path string) {
				// Cursor Tab log files live inside the Cursor logs tree but are
				// not Copilot events — they're handled (or intentionally skipped)
				// by CursorLogWatcher. Tailing them here would mis-attribute
				// cursor completions as copilot.
				if isCursorTabLog(path) {
					return
				}
				if err := tailCopilotLog(tCtx, path, sink); err != nil && tCtx.Err() == nil {
					log.Printf("copilot-log tail %s: %v", path, err)
				}
			}(p)
		}
	}
	// Tear down tailers for log files that disappeared (rotation, IDE
	// uninstall, log dir reset).
	for path, cancel := range running {
		if !seen[path] {
			cancel()
			delete(running, path)
		}
	}
}

// findRecentLogFiles walks `root` for *.log (and the JetBrains rotated
// idea.<n>.log naming) modified within copilotLogMaxAge.
func findRecentLogFiles(root string) []string {
	if root == "" {
		return nil
	}
	var out []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		// Accept *.log and idea.<n>.log (JetBrains rotation).
		if !strings.HasSuffix(name, ".log") {
			return nil
		}
		info, err := d.Info()
		if err != nil || time.Since(info.ModTime()) > copilotLogMaxAge {
			return nil
		}
		out = append(out, p)
		return nil
	})
	return out
}

// copilotAcceptLine matches the per-line activity markers we look for in
// editor logs. It's intentionally permissive — different Copilot plugin
// versions phrase the events differently, and the cost of a false match
// is a low-confidence row, not a wrong attribution.
var copilotAcceptLine = regexp.MustCompile(
	`(?i)copilot.*(accept|accepted|applied|complet(?:ion|ed)|inline|ghost(?:text)?|apply.?edit)`,
)

// copilotJetBrainsLine catches JetBrains-specific "DETECTED SOURCE:
// github-copilot-jetbrains" markers and stack frames under
// com.github.copilot.*.
var copilotJetBrainsLine = regexp.MustCompile(
	`(?i)(github-copilot-jetbrains|com\.github\.copilot\.[a-z.]*(?:complet|inline|ghost|apply))`,
)

// copilotFetch is a HIGH-precision pattern that matches the backend round-trip
// log lines emitted by both the VS Code and JetBrains Copilot plugins:
//
//	VS Code   : [fetchCompletions] Request <id> at <https://proxy.individual.githubcopilot.com/v1/engines/<model>/completions> finished …
//	VS Code   : [fetchChat]        Request <id> at <https://.../chat/completions> finished …
//	JetBrains : #copilot - [fetchCompletions] Request <id> at <…> finished …
//	JetBrains : #copilot - [fetchChat]        Request <id> at <…> finished …
//
// `[fetchChat]` vs `[fetchCompletions]` tells us chat-vs-Tab directly, and
// `/v1/engines/<model>/completions` in the URL embeds the model for inline
// completions. We extract both so the emitted event has gen_type + model
// without guessing. The `(?:#copilot - )?` prefix is optional to cover both
// VS Code (no prefix) and JetBrains (with `#copilot - ` prefix).
var copilotFetch = regexp.MustCompile(
	`(?:#copilot - )?\[(fetchChat|fetchCompletions)\][^<]*<([^>]+)>`,
)

// copilotJetBrainsAutoModel matches the AutoModelService line that prints
// the currently-active model. The Copilot plugin emits this whenever the
// auto-selected model changes (and at startup). We cache the model so any
// `[fetchChat]` line that follows (whose URL doesn't contain the model)
// can still be tagged.
//
//	#copilot - [AutoModelService] Fetched auto model for active in 236ms: gpt-5.4-mini
var copilotJetBrainsAutoModel = regexp.MustCompile(
	`#copilot - \[AutoModelService\] Fetched auto model for active[^:]*:\s*(\S+)`,
)

// copilotEngineInURL extracts the model from a JetBrains completions URL:
//
//	/v1/engines/gpt-41-copilot/completions  →  gpt-41-copilot
var copilotEngineInURL = regexp.MustCompile(`/v1/engines/([^/]+)/completions`)

// tailCopilotLog streams a log file from EOF, looking for completion
// accept markers and emitting a Copilot event for each. A per-tailer
// `activeModel` string is updated whenever an AutoModelService line is
// seen, so subsequent fetch lines (whose URL may not encode a model)
// can still be tagged with the active model.
func tailCopilotLog(ctx context.Context, path string, sink daemon.Sink) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer f.Close()
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return fmt.Errorf("seek: %w", err)
	}
	r := bufio.NewReaderSize(f, 1<<16)
	backoff := 200 * time.Millisecond
	const maxBackoff = 2 * time.Second
	activeModel := ""
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
		// 1) AutoModelService → just update active model, don't emit.
		if m := copilotJetBrainsAutoModel.FindStringSubmatch(line); m != nil {
			activeModel = m[1]
			continue
		}
		// 2) High-precision JetBrains fetch lines → emit directly.
		if m := copilotFetch.FindStringSubmatch(line); m != nil {
			emitCopilotFetch(m[1], m[2], activeModel, sink)
			continue
		}
		// 3) Generic accept-marker lines → existing fallback path.
		if lineLooksLikeCopilotAccept(line) {
			emitCopilotLogEvent(line, sink)
		}
	}
}

// emitCopilotFetch handles [fetchChat] / [fetchCompletions] log lines from
// both VS Code and JetBrains Copilot plugins. `fetchKind` is "fetchChat" or
// "fetchCompletions"; `url` is the request URL (the model is encoded in
// inline-completion URLs as /v1/engines/<model>/completions).
func emitCopilotFetch(fetchKind, url, activeModel string, sink daemon.Sink) {
	gen := "completion"
	if fetchKind == "fetchChat" {
		gen = "chat"
	}
	model := activeModel
	if gen == "completion" {
		if m := copilotEngineInURL.FindStringSubmatch(url); m != nil {
			model = m[1]
		}
	}
	ev := daemon.Event{
		When:       time.Now(),
		Tool:       "copilot",
		Confidence: "medium",
		GenType:    gen,
		Model:      model,
		RawMeta:    fmt.Sprintf(`{"source":"copilot_log_fetch","fetch":%q}`, fetchKind),
	}
	if err := sink.Record(ev); err != nil {
		log.Printf("copilot-log sink: %v", err)
	}
}

// lineLooksLikeCopilotAccept centralises the matcher so the two regex
// families stay easy to evolve.
func lineLooksLikeCopilotAccept(line string) bool {
	return copilotAcceptLine.MatchString(line) || copilotJetBrainsLine.MatchString(line)
}

// emitCopilotLogEvent fires off a Copilot edit event for `line`. We try
// to extract a workspace file path — when we get one, the event carries
// the file/line range so attribution can apply per-file. Otherwise we
// emit a no-file session marker, which still flows through the
// HumanEditWatcher copilot-active path so the user's just-typed lines
// get credited.
func emitCopilotLogEvent(line string, sink daemon.Sink) {
	gen := copilotLogGenType(line)
	abs, ok := extractFilePath(line)
	if !ok {
		// No file path — emit a session-active marker so the fold-in
		// can still credit Copilot for the user's recent edits.
		ev := daemon.Event{
			When:       time.Now(),
			Tool:       "copilot",
			Confidence: "low",
			GenType:    gen,
			RawMeta:    `{"source":"copilot_log_marker"}`,
		}
		if err := sink.Record(ev); err != nil {
			log.Printf("copilot-log sink: %v", err)
		}
		return
	}
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
		Tool:         "copilot",
		Confidence:   "medium",
		GenType:      gen,
		RepoPath:     repo,
		WorktreePath: wt,
		FilePath:     rel,
		Lines:        toDaemonLineRanges(lr),
		RawMeta:      `{"source":"copilot_log"}`,
	}
	if err := sink.Record(ev); err != nil {
		log.Printf("copilot-log sink: %v", err)
	}
}

// copilotLogGenType decides whether a matched log line is from Copilot's
// chat panel or from an inline-completion accept. The Copilot extension
// names its chat APIs / classes with "chat" in them ("copilot-chat",
// "ChatRequest", "ChatCompletion", "ChatService", …), so the presence
// of that substring is a reliable signal. Without it, the line is a
// regular completion-accept marker (inline / ghost text).
func copilotLogGenType(line string) string {
	if strings.Contains(strings.ToLower(line), "chat") {
		return "chat"
	}
	return "completion"
}

// defaultCopilotLogRoots returns the per-platform list of log directories
// to scan. JetBrains IDE subdirectories are discovered dynamically so we
// pick up newly installed IDEs without code changes.
func defaultCopilotLogRoots() []string {
	home, err := config.Home()
	if err != nil {
		return nil
	}
	var out []string
	switch runtime.GOOS {
	case "darwin":
		out = append(out,
			filepath.Join(home, "Library", "Application Support", "Code", "logs"),
			filepath.Join(home, "Library", "Application Support", "Cursor", "logs"),
		)
		out = append(out, jetBrainsLogDirs(filepath.Join(home, "Library", "Logs", "JetBrains"), false)...)
	case "windows":
		if appData := os.Getenv("APPDATA"); appData != "" {
			out = append(out,
				filepath.Join(appData, "Code", "logs"),
				filepath.Join(appData, "Cursor", "logs"),
			)
		}
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			out = append(out, jetBrainsLogDirs(filepath.Join(local, "JetBrains"), true)...)
		}
	default:
		out = append(out,
			filepath.Join(home, ".config", "Code", "logs"),
			filepath.Join(home, ".config", "Cursor", "logs"),
		)
		out = append(out, jetBrainsLogDirs(filepath.Join(home, ".cache", "JetBrains"), true)...)
		out = append(out, jetBrainsLogDirs(filepath.Join(home, ".local", "share", "JetBrains"), true)...)
	}
	return out
}

// jetBrainsLogDirs lists subdirectories of `parent` (one per installed
// IDE: IntelliJIdea2026.1, GoLand2025.3, …). On Windows/Linux the log
// files live inside a "log" sub-directory of each IDE; on macOS they
// live directly under the IDE directory under ~/Library/Logs/JetBrains.
func jetBrainsLogDirs(parent string, needsLogSubdir bool) []string {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		p := filepath.Join(parent, e.Name())
		if needsLogSubdir {
			p = filepath.Join(p, "log")
		}
		out = append(out, p)
	}
	return out
}
