package tools

// Debug tracers for `blamely log <tool>`. They run a tool's real watcher(s)
// against a printing sink instead of the SQLite store, so users can watch — in
// real time — exactly which attribution events blamely would record. This
// reuses the production detection logic verbatim (no parallel parser to drift),
// and writes nothing to the database.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/blamely/blamely/internal/config"
	"github.com/blamely/blamely/internal/daemon"
	"github.com/blamely/blamely/internal/store"
)

// printSink implements daemon.Sink by printing each event to out instead of
// persisting it. Session markers (no file path) are shown too, since their
// model / gen_type is exactly what feeds the chat-vs-completion enrichment.
//
// The output is verbose on purpose — this is a debugging aid. For each event we
// surface the timestamp, gen_type, tool, model, confidence, the raw_meta source
// (which watcher/path produced it), the file (or "no file"), and — for chat /
// log signals tied to a workspace file — which editor (VS Code vs Cursor) the
// underlying artifact lives under. That last column is what makes setup issues
// obvious (e.g. "all my Copilot chat activity is under VS Code, not Cursor").
type printSink struct {
	mu    sync.Mutex
	out   io.Writer
	count int
}

// rawMetaView is the subset of raw_meta the debug printer understands.
type rawMetaView struct {
	Source          string `json:"source"`
	ChatSessionPath string `json:"chat_session_path"`
	Tool            string `json:"tool"`
	Host            string `json:"host"`
	Line            string `json:"line"`
}

func (p *printSink) Record(ev daemon.Event) error {
	var meta rawMetaView
	if ev.RawMeta != "" {
		_ = json.Unmarshal([]byte(ev.RawMeta), &meta)
	}

	model := ev.Model
	if model == "" {
		model = "?"
	}
	gen := ev.GenType
	if gen == "" {
		gen = "unknown"
	}
	source := meta.Source
	if source == "" {
		source = "?"
	}
	loc := ev.FilePath
	if loc == "" {
		loc = "—"
	}
	ts := ev.When
	if ts.IsZero() {
		ts = time.Now()
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.count++
	fmt.Fprintf(p.out, "%s  [%-10s] tool=%-7s model=%-22s conf=%-6s src=%-22s file=%s\n",
		ts.Format("15:04:05"), gen, ev.Tool, model, ev.Confidence, source, loc)
	// Secondary line: the artifact that produced this signal + which editor it
	// belongs to, so cross-editor confusion (Copilot-in-VS-Code vs Cursor) is
	// visible at a glance.
	if meta.ChatSessionPath != "" {
		fmt.Fprintf(p.out, "             ↳ chat session: %s  [editor=%s]\n",
			abbreviateHome(meta.ChatSessionPath), editorOfPath(meta.ChatSessionPath))
	}
	if meta.Host != "" {
		fmt.Fprintf(p.out, "             ↳ host: %s\n", meta.Host)
	}
	if meta.Line != "" {
		fmt.Fprintf(p.out, "             ↳ matched log line: %s\n", truncate(meta.Line, 160))
	}
	return nil
}

// editorOfPath guesses which editor a workspace artifact belongs to from its
// path. Returns "VS Code", "Cursor", or "?".
func editorOfPath(p string) string {
	switch {
	case strings.Contains(p, "/Code/") || strings.Contains(p, "\\Code\\"):
		return "VS Code"
	case strings.Contains(p, "/Cursor/") || strings.Contains(p, "\\Cursor\\"):
		return "Cursor"
	default:
		return "?"
	}
}

func abbreviateHome(p string) string {
	if h := homeDir(); h != "" && strings.HasPrefix(p, h) {
		return "~" + strings.TrimPrefix(p, h)
	}
	return p
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// DebugWatchers runs the given watchers against a printing sink until ctx is
// cancelled. It is the shared backend for the per-tool `blamely log` tracers.
func DebugWatchers(ctx context.Context, out io.Writer, watchers ...daemon.Watcher) error {
	if len(watchers) == 0 {
		return nil
	}
	names := make([]string, 0, len(watchers))
	for _, w := range watchers {
		names = append(names, w.Name())
	}
	fmt.Fprintf(out, "Tracing watchers: %v\n", names)
	fmt.Fprintf(out, "Printing detected attribution events (nothing is written to the DB). Ctrl-C to stop.\n\n")

	sink := &printSink{out: out}
	var wg sync.WaitGroup
	for _, w := range watchers {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := w.Run(ctx, sink); err != nil && ctx.Err() == nil {
				fmt.Fprintf(out, "[error] %s: %v\n", w.Name(), err)
			}
		}()
	}
	<-ctx.Done()
	wg.Wait()
	return nil
}

// DebugCopilotLogs traces GitHub Copilot detection: the chat-session JSONL
// watcher (chat panel, with model) and the editor / JetBrains log watcher
// (inline completion accepts + JetBrains fetch lines).
//
// It also polls SQLite every 2 s for new events from IDE plugin sources
// (intellij_plugin, vscode_plugin) that arrive via the HTTP /edit endpoint
// and are never seen by the watcher stream. Those rows are printed with the
// same format, prefixed with "↳ plugin" so they are visually distinct.
func DebugCopilotLogs(ctx context.Context, out io.Writer) error {
	go tailPluginEdits(ctx, out)
	return DebugWatchers(ctx, out, &CopilotChatWatcher{}, &CopilotLogWatcher{})
}

// tailPluginEdits polls SQLite every 2 s for new rows whose raw_meta source
// is an IDE plugin (intellij_plugin, vscode_plugin). These events come from
// the daemon's HTTP /edit endpoint and are never emitted to the watcher
// stream, so they would otherwise be invisible in `blamely log copilot`.
func tailPluginEdits(ctx context.Context, out io.Writer) {
	db, err := store.Open()
	if err != nil {
		return
	}
	defer db.Close()

	sources := []string{"intellij_plugin", "vscode_plugin"}
	var lastID int64
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rows, err := db.RecentPluginEdits(sources, lastID)
			if err != nil {
				continue
			}
			for _, r := range rows {
				if r.ID > lastID {
					lastID = r.ID
				}
				printPluginEditRow(out, r, "plugin")
			}
		}
	}
}

// printPluginEditRow renders one polled edits-table row in the same format
// printSink uses for watcher events, so HTTP-arriving and watcher-detected
// signals look identical in the trace output. fallbackSrc is shown when
// raw_meta carries no "source" field.
func printPluginEditRow(out io.Writer, r store.PluginEditRow, fallbackSrc string) {
	ts := time.Unix(0, r.Ts)
	model := r.Model
	if model == "" {
		model = "?"
	}
	file := r.FilePath
	if file == "" {
		file = "—"
	}
	lineRange := ""
	if r.StartLine > 0 {
		if r.EndLine > r.StartLine {
			lineRange = fmt.Sprintf(" L%d-%d", r.StartLine, r.EndLine)
		} else {
			lineRange = fmt.Sprintf(" L%d", r.StartLine)
		}
	}
	var meta struct {
		Source string `json:"source"`
	}
	_ = json.Unmarshal([]byte(r.RawMeta), &meta)
	src := meta.Source
	if src == "" {
		src = fallbackSrc
	}
	fmt.Fprintf(out, "%s  [%-10s] tool=%-7s model=%-22s conf=%-6s src=%-22s file=%s%s\n",
		ts.Format("15:04:05"), r.GenType, r.Tool, model, r.Confidence, src, file, lineRange)
}

// tailToolEdits polls SQLite every 2 s for new edits-table rows for the given
// tool and prints them as they're recorded. Used for hook-driven tools whose
// events arrive solely via the HTTP /edit endpoint — there's no passive log
// or watcher stream to trace, so the database itself is the live signal.
func tailToolEdits(ctx context.Context, out io.Writer, tool store.Tool, fallbackSrc string) {
	db, err := store.Open()
	if err != nil {
		return
	}
	defer db.Close()

	var lastID int64
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rows, err := db.RecentEditsByTool(tool, lastID)
			if err != nil {
				continue
			}
			for _, r := range rows {
				if r.ID > lastID {
					lastID = r.ID
				}
				printPluginEditRow(out, r, fallbackSrc)
			}
		}
	}
}

// DebugCodexLogs traces Codex CLI session detection via the CodexWatcher.
func DebugCodexLogs(ctx context.Context, out io.Writer) error {
	return DebugWatchers(ctx, out, &CodexWatcher{})
}

// DebugClaudeLogs explains that Claude Code attribution is hook-driven and has
// no passive log to tail, then traces the chat-session watcher in case Claude
// is being used through a chat panel that persists a chatSessions JSONL.
func DebugClaudeLogs(ctx context.Context, out io.Writer) error {
	fmt.Fprintln(out, "Claude Code attribution is hook-driven: the PostToolUse hook pipes each")
	fmt.Fprintln(out, "edit to `blamely record claude`, which POSTs directly to the daemon's /edit")
	fmt.Fprintln(out, "endpoint — there is no passive log file to tail. To trace it, replay a")
	fmt.Fprintln(out, "captured hook payload through `blamely record claude` directly (errors")
	fmt.Fprintln(out, "print to that command's own output, not ~/.blamely/daemon.log), or")
	fmt.Fprintln(out, "inspect the git note after a commit. Nothing to stream here.")
	return nil
}

// DebugGeminiLogs traces Gemini CLI attribution. Gemini is hook-driven — the
// AfterTool/BeforeTool hooks pipe each tool call straight to `blamely record
// gemini`, which POSTs to the daemon's /edit endpoint — so there's no passive
// log file to tail. The edits table itself is the only live signal: we poll
// it for new tool=gemini rows and print them as they land, in the same format
// `blamely log copilot` uses for its plugin events.
func DebugGeminiLogs(ctx context.Context, out io.Writer) error {
	fmt.Fprintln(out, "Gemini CLI attribution is hook-driven: the AfterTool/BeforeTool hooks pipe")
	fmt.Fprintln(out, "each tool call to `blamely record gemini`, which POSTs directly to the")
	fmt.Fprintln(out, "daemon's /edit endpoint — there is no passive log file to tail, so this")
	fmt.Fprintln(out, "traces the database directly: every new tool=gemini row is printed the")
	fmt.Fprintln(out, "moment it's recorded. Trigger an edit in Gemini CLI now and watch for it")
	fmt.Fprintln(out, "below. Nothing is written to the database by this command. Ctrl-C to stop.")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "If nothing appears after you edit a file via Gemini, check:")
	fmt.Fprintln(out, "  1. ~/.gemini/settings.json has tools.enableHooks=true and an")
	fmt.Fprintln(out, "     AfterTool/BeforeTool hook running `blamely record gemini` (run")
	fmt.Fprintln(out, "     `blamely doctor` or `blamely install` to (re)write it).")
	fmt.Fprintln(out, "  2. The daemon is running (`blamely status`).")
	fmt.Fprintln(out, "  3. `echo '<hook payload>' | blamely record gemini` prints no error.")
	fmt.Fprintln(out, "     A rejection (e.g. \"daemon rejected: 400 ...\") prints directly to")
	fmt.Fprintln(out, "     this command's own output — the daemon does NOT log rejected or")
	fmt.Fprintln(out, "     malformed POSTs to ~/.blamely/daemon.log, so check here first.")
	fmt.Fprintln(out)

	tailToolEdits(ctx, out, store.ToolGemini, "gemini_hook")
	return nil
}

// DebugDevinLogs is the Devin CLI twin of DebugGeminiLogs: there is no passive
// log to tail, so it traces the database for new tool=devin rows.
func DebugDevinLogs(ctx context.Context, out io.Writer) error {
	fmt.Fprintln(out, "Devin CLI attribution is hook-driven: the PostToolUse hook pipes each tool")
	fmt.Fprintln(out, "call to `blamely record devin`, which POSTs directly to the daemon's /edit")
	fmt.Fprintln(out, "endpoint — there is no passive log file to tail, so this traces the")
	fmt.Fprintln(out, "database directly: every new tool=devin row is printed the moment it's")
	fmt.Fprintln(out, "recorded. Trigger an edit in Devin CLI now and watch for it below. Nothing")
	fmt.Fprintln(out, "is written to the database by this command. Ctrl-C to stop.")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Scope: this covers the LOCAL Devin CLI only. A Devin Cloud session edits")
	fmt.Fprintln(out, "files inside a remote sandbox and reaches this machine as an ordinary git")
	fmt.Fprintln(out, "pull, so no hook fires and nothing will appear here for those commits.")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "If nothing appears after you edit a file via Devin CLI, check:")
	fmt.Fprintln(out, "  1. ~/.config/devin/config.json has a PostToolUse hook running")
	fmt.Fprintln(out, "     `blamely record devin` (run `blamely doctor` or `blamely install`).")
	fmt.Fprintln(out, "  2. The daemon is running (`blamely status`).")
	fmt.Fprintln(out, "  3. `echo '<hook payload>' | blamely record devin` prints no error.")
	fmt.Fprintln(out, "     A rejection (e.g. \"daemon rejected: 400 ...\") prints directly to")
	fmt.Fprintln(out, "     this command's own output — the daemon does NOT log rejected or")
	fmt.Fprintln(out, "     malformed POSTs to ~/.blamely/daemon.log, so check here first.")
	fmt.Fprintln(out)

	tailToolEdits(ctx, out, store.ToolDevin, "devin_hook")
	return nil
}

// homeDir returns the user's home directory (best-effort).
func homeDir() string {
	h, _ := config.Home()
	return h
}
