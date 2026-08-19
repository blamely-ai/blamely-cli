package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/blamely/blamely/internal/config"
	"github.com/blamely/blamely/internal/gitutil"
	"github.com/blamely/blamely/internal/store"
)

// UnixHTTPClient returns an *http.Client that routes all requests through the
// Unix domain socket at sockPath, regardless of the URL host. Use
// "http://unix/<path>" as the request URL (the host is ignored).
func UnixHTTPClient(sockPath string) *http.Client {
	return &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sockPath)
			},
		},
	}
}

type Server struct {
	db   *store.DB
	http *http.Server

	// preChatSnaps holds file content PUT by the VS Code plugin right before a
	// chat apply. Keyed by "repo\x00file". The chat watcher consumes the entry
	// (deletes on first read) so it's only used once per apply. Guarded by mu.
	preChatMu    sync.Mutex
	preChatSnaps map[string]string
}

// EditPayload is the JSON body POSTed to /edit by tool integrations.
type EditPayload struct {
	Tool       string `json:"tool"`
	Confidence string `json:"confidence,omitempty"`
	// GenType: chat | cli | completion | unknown
	GenType          string `json:"gen_type,omitempty"`
	RepoPath         string `json:"repo_path"`
	FilePath         string `json:"file_path"`
	Model            string `json:"model,omitempty"`
	InputTokens      *int64 `json:"input_tokens,omitempty"`
	OutputTokens     *int64 `json:"output_tokens,omitempty"`
	CacheReadTokens  *int64 `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens *int64 `json:"cache_write_tokens,omitempty"`
	HashBefore       string `json:"hash_before,omitempty"`
	HashAfter        string `json:"hash_after,omitempty"`
	// SuggestedLines: AI's original suggestion size before user edits.
	SuggestedLines int64             `json:"suggested_lines,omitempty"`
	Lines          []Range           `json:"lines"`
	RemovedLines   []RemovedLineHash `json:"removed_lines,omitempty"`
	RawMeta        string            `json:"raw_meta,omitempty"`
	// Branch is the editor's checked-out branch when the edit was made. Optional:
	// the daemon resolves it from repo_path if empty (e.g. watcher-sourced edits).
	Branch string `json:"branch,omitempty"`
}

type Range struct {
	Start          int    `json:"start"`
	End            int    `json:"end"`
	ContentSHA     string `json:"content_sha,omitempty"`
	ContentSHANorm string `json:"content_sha_norm,omitempty"`
}

// Watchers is the list of background tailers (Codex/Cursor/Copilot) the
// daemon will run. It is assigned by cmd/blamely before calling Run so the
// daemon package itself doesn't have to import the tools package.
var Watchers []Watcher

// DBWatcherFactory, if set, is called at daemon startup to create a Watcher
// that needs DB access. Assigned by cmd/blamely.
//
// Use DBWatcherFactories for additional DB-backed watchers; both are appended
// to the runtime watcher list. DBWatcherFactory is retained for backward
// compatibility with the original single-factory wiring.
var DBWatcherFactory func(db *store.DB) Watcher

// DBWatcherFactories is the list of additional DB-backed watcher factories.
// Each is invoked once at daemon startup; the resulting Watcher is appended
// to the run list alongside DBWatcherFactory's product.
var DBWatcherFactories []func(db *store.DB) Watcher

func Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Route logging to ~/.blamely/daemon.log (with 24h retention) before anything
	// else, so startup, hook traffic, and connection errors are all captured —
	// including on Windows, where `daemon --background` frees its console and so
	// has nowhere to write stderr.
	closeLog := setupLogging(ctx)
	defer closeLog()

	// Single instance: if a daemon is already answering /health, don't start a
	// second one. Plugins (re)spawn a daemon on a failed health ping; without this
	// guard those redundant spawns pile up — each running all watchers and writing
	// the same SQLite DB, multiplying CPU and DB contention. (Best-effort: a busy or
	// just-crashed daemon may not answer, in which case we proceed.)
	if AnotherDaemonHealthy() {
		log.Printf("another blamelyd is already healthy — exiting (single instance)")
		return nil
	}
	// Authoritative single-instance guard: an exclusive OS lock held for our whole
	// lifetime. The /health probe above is only a fast pre-check — it races when
	// launchers (logon/launchd, editor-plugin spawns, the keepalive task,
	// install) start daemons concurrently and all see "no healthy daemon" before
	// any binds, then each binds its own ephemeral port / re-creates the unix
	// socket and they ALL keep running. The lock can be held by exactly one
	// process, so the losers exit here instead of becoming the extra 8-thread
	// duplicates seen in the process list. If the lock mechanism itself fails
	// (unusual FS), fall back to the old best-effort behaviour rather than refusing
	// to start.
	if lockPath, lerr := config.LockFile(); lerr == nil {
		lockFile, ok, err := acquireInstanceLock(lockPath)
		switch {
		case err != nil:
			log.Printf("single-instance lock unavailable (%v) — continuing without it", err)
		case !ok:
			log.Printf("another blamelyd holds the instance lock — exiting (single instance)")
			return nil
		default:
			defer lockFile.Close()
		}
	}
	log.Printf("blamelyd starting (pid=%d, os=%s)", os.Getpid(), runtime.GOOS)

	db, err := store.Open()
	if err != nil {
		return err
	}
	defer db.Close()

	// Drop watermark rows for sources untouched in 60 days (deleted/rotated
	// session & log files) so the table doesn't grow without bound. Best-effort.
	if n, err := db.PruneWatermarks(time.Now().Add(-60 * 24 * time.Hour).UnixNano()); err == nil && n > 0 {
		log.Printf("pruned %d stale watcher watermark(s)", n)
	}

	s := &Server{db: db}

	watchers := append([]Watcher{}, Watchers...) // copy so DB-backed watchers can be appended
	if DBWatcherFactory != nil {
		watchers = append(watchers, DBWatcherFactory(db))
	}
	for _, f := range DBWatcherFactories {
		watchers = append(watchers, f(db))
	}
	if len(watchers) > 0 {
		go runWatchers(ctx, db, watchers)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.health)
	mux.HandleFunc("/edit", s.ingest)
	mux.HandleFunc("/fs", s.fsEvent)
	mux.HandleFunc("/snapshot", s.snapshot)
	mux.HandleFunc("/prechat-snapshot", s.preChatSnapshot)
	if debugHTTP() {
		log.Printf("BLAMELY_DEBUG=on — logging every HTTP request (method/path/status/latency/remote)")
	}

	// Prefer a Unix domain socket — it bypasses network security tools (e.g.
	// Trend Micro) that intercept localhost TCP. Falls back to a random TCP port
	// on systems where AF_UNIX is unusable.
	//
	// Windows is forced onto TCP even though Go can bind an AF_UNIX socket on
	// Win10 1803+: the editor plugins can't reach a *filesystem* AF_UNIX socket
	// reliably there — Node's http `socketPath` is interpreted as a named pipe,
	// not a Unix socket file. If the daemon bound AF_UNIX, it would write
	// daemon.sock (not daemon.port), `blamely status` (Go) would connect fine,
	// but the VS Code / JetBrains plugins would fail and report "daemon not
	// active". Loopback TCP works identically across Go, Node, and the JVM.
	sockPath, sockErr := config.SocketFile()
	var listener net.Listener
	if sockErr == nil && runtime.GOOS != "windows" {
		_ = os.Remove(sockPath) // clean up stale socket from a previous run
		if l, err := net.Listen("unix", sockPath); err == nil {
			listener = l
			if err := writeSocketFile(sockPath); err != nil {
				_ = listener.Close()
				_ = os.Remove(sockPath)
				return err
			}
			defer removeSocketFile()
			go watchListenerFile(ctx, sockPath, cancel)
			log.Printf("listening on unix socket %s", sockPath)
		}
	}
	if listener == nil {
		// AF_UNIX unavailable, disabled (Windows), or path error — fall back to
		// TCP. Remove any stale daemon.sock left by a previous run/version so
		// clients (which prefer the socket when its file exists) don't try a
		// dead socket and skip the live port.
		if sockErr == nil {
			_ = os.Remove(sockPath)
		}
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			log.Printf("FATAL: TCP listen failed: %v", err)
			return fmt.Errorf("listen: %w", err)
		}
		listener = l
		addr := listener.Addr().(*net.TCPAddr)
		if err := writePortFile(addr.Port); err != nil {
			_ = listener.Close()
			return err
		}
		defer removePortFile()
		if portPath, err := config.PortFile(); err == nil {
			go watchListenerFile(ctx, portPath, cancel)
		}
		log.Printf("listening on 127.0.0.1:%d", addr.Port)
	}

	// Record our PID so `blamely uninstall` can kill this exact process directly
	// (a racy by-image-name taskkill on Windows otherwise left the daemon alive
	// holding the binary, so uninstall removed the files but the process kept
	// running). Best-effort: a missing PID file just falls back to that taskkill.
	_ = writePidFile()
	defer removePidFile()

	s.http = &http.Server{
		Handler:           accessLog(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Ask periodically whether a newer blamely exists. Purely advisory unless
	// update.auto is on, and silent on every failure — see watchForUpdates.
	go watchForUpdates(ctx)

	errCh := make(chan error, 1)
	go func() {
		if err := s.http.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		log.Printf("shutting down (signal received)")
		shutdownCtx, sCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer sCancel()
		return s.http.Shutdown(shutdownCtx)
	case err := <-errCh:
		log.Printf("FATAL: HTTP server stopped: %v", err)
		return err
	}
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, `{"ok":true}`)
}

// debugHTTP reports whether verbose per-request access logging is enabled via the
// BLAMELY_DEBUG env var. Off by default so the daemon log isn't flooded by the
// plugins' 5s /health heartbeats; turn it on to debug plugin<->daemon traffic.
func debugHTTP() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("BLAMELY_DEBUG"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// accessLog wraps h so that, when BLAMELY_DEBUG is set, every request is logged
// as one line: method, path, response status, latency, and remote address. This
// makes the full two-way plugin<->daemon conversation (health pings, /edit,
// /snapshot, /prechat-snapshot) visible in ~/.blamely/daemon.log. When debug is
// off it returns h unwrapped, so there is zero per-request overhead.
func accessLog(h http.Handler) http.Handler {
	if !debugHTTP() {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(rec, r)
		log.Printf("http %s %s -> %d (%s) from %s",
			r.Method, r.URL.Path, rec.status, time.Since(start).Round(time.Millisecond), r.RemoteAddr)
	})
}

// statusRecorder captures the response status code for accessLog. A handler that
// never calls WriteHeader implicitly returns 200, matching the default here.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// snapshot returns the cached "before" content for repo/file — the file's
// content as of the last recorded edit, used by whole-file-overwrite tools
// (Write/write_file) and Copilot's textEditGroup edits to compute removed-line
// hashes when they have no "before" content of their own. If nothing has been
// cached yet (this file's first recorded edit), falls back to the file's
// content at HEAD; found=false if neither is available (e.g. a brand-new
// untracked file — correctly yields no removed lines).
func (s *Server) snapshot(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.snapshotGet(w, r)
	case http.MethodPut:
		s.snapshotPut(w, r)
	default:
		http.Error(w, "GET or PUT required", http.StatusMethodNotAllowed)
	}
}

func (s *Server) snapshotGet(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	file := r.URL.Query().Get("file")
	if repo == "" || file == "" {
		http.Error(w, "repo, file required", http.StatusBadRequest)
		return
	}
	// Match the forward-slash normalization the /edit path applies before caching
	// snapshots, so a Windows recorder sending file=src\main.go still finds the
	// snapshot stored under src/main.go.
	file = cleanRel(file)
	content, ok, err := s.db.GetFileSnapshot(repo, file)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		content, ok = headFileContent(repo, file)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Content string `json:"content"`
		Found   bool   `json:"found"`
	}{Content: content, Found: ok})
}

// snapshotPut stores the pre-chat file content in an in-memory map so the
// chat watcher can use it as an accurate diff baseline. Using in-memory (not
// the DB) means: (a) the entry is consumed once and deleted on first read —
// subsequent watcher polls see nothing and fall back to recording all lines,
// which is the safe behavior after a restart; (b) non-plugin users never have
// a pre-chat entry so the watcher always falls back to all-lines attribution,
// which correctly handles AI line-reordering / block-swap edits that the
// multiset diff would otherwise miss.
func (s *Server) snapshotPut(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Repo    string `json:"repo"`
		File    string `json:"file"`
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Repo == "" || req.File == "" {
		http.Error(w, "repo, file, content required", http.StatusBadRequest)
		return
	}
	req.File = cleanRel(req.File)
	key := req.Repo + "\x00" + req.File
	s.preChatMu.Lock()
	if s.preChatSnaps == nil {
		s.preChatSnaps = make(map[string]string)
	}
	s.preChatSnaps[key] = req.Content
	s.preChatMu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// preChatSnapshot serves GET /prechat-snapshot. Returns the content PUT by
// the plugin and immediately deletes the entry (consume-once). Returns
// found=false if no entry exists (non-plugin user, restart, or timing race).
func (s *Server) preChatSnapshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	repo := r.URL.Query().Get("repo")
	file := r.URL.Query().Get("file")
	if repo == "" || file == "" {
		http.Error(w, "repo, file required", http.StatusBadRequest)
		return
	}
	file = cleanRel(file)
	key := repo + "\x00" + file
	s.preChatMu.Lock()
	content, found := s.preChatSnaps[key]
	if found {
		delete(s.preChatSnaps, key)
	}
	s.preChatMu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Content string `json:"content"`
		Found   bool   `json:"found"`
	}{Content: content, Found: found})
}

// headFileContent returns file's content at HEAD in repoPath, or ok=false if
// the repo has no HEAD yet or the file isn't tracked there (e.g. a new file).
func headFileContent(repoPath, file string) (string, bool) {
	out, err := gitutil.Output(repoPath, "show", "HEAD:"+filepath.ToSlash(file))
	if err != nil {
		return "", false
	}
	return string(out), true
}

func (s *Server) ingest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var p EditPayload
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&p); err != nil {
		log.Printf("/edit decode error: %v", err)
		http.Error(w, fmt.Sprintf("decode: %v", err), http.StatusBadRequest)
		return
	}
	if err := validateAndStore(s.db, p); err != nil {
		log.Printf("/edit REJECTED tool=%q gen_type=%q file=%q: %v", p.Tool, p.GenType, p.FilePath, err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	log.Printf("/edit ok tool=%q gen_type=%q repo=%q file=%q lines=%d suggested=%d",
		p.Tool, p.GenType, p.RepoPath, p.FilePath, len(p.Lines), p.SuggestedLines)
	w.WriteHeader(http.StatusNoContent)
}

func validateAndStore(db *store.DB, p EditPayload) error {
	if p.RepoPath == "" || p.FilePath == "" {
		return fmt.Errorf("repo_path, file_path required")
	}
	// Normalize to forward slashes so commit-time matching works on Windows: the
	// Go recorders build file_path with filepath.Rel (backslashes for nested
	// files), but git diff — the source of truth at commit — always uses forward
	// slashes. Without this, a Windows edit to src\main.go never matches the
	// committed src/main.go and the line silently falls back to Human. No-op on
	// Unix (paths have no backslashes). Applied before the snapshot write below so
	// the cached snapshot is keyed the same way the /snapshot lookup normalizes.
	p.FilePath = cleanRel(p.FilePath)
	tool := store.Tool(strings.ToLower(p.Tool))
	gt := store.GenType(strings.ToLower(p.GenType))
	if gt == "" {
		gt = store.GenTypeUnknown
	}
	// Tool is required EXCEPT for human edits, which travel as
	// tool="" + gen_type=human. Anything else with an empty tool is a
	// caller bug.
	if tool == "" && gt != store.GenTypeHuman {
		return fmt.Errorf("tool required (or use gen_type=human for empty tool)")
	}
	switch tool {
	case "",
		store.ToolClaude, store.ToolCursor, store.ToolCodex, store.ToolCopilot, store.ToolGemini, store.ToolDevin, store.ToolCopyPaste,
		store.ToolHuman: // accepted only so the daemon doesn't reject legacy clients mid-upgrade
	default:
		return fmt.Errorf("unknown tool %q", p.Tool)
	}
	conf := store.Confidence(p.Confidence)
	if conf == "" {
		conf = defaultConfidence(tool)
	}
	e := store.Edit{
		TimestampNanos: time.Now().UnixNano(),
		RepoPath:       p.RepoPath,
		FilePath:       p.FilePath,
		Tool:           tool,
		Confidence:     conf,
		GenType:        gt,
		SuggestedLines: p.SuggestedLines,
	}
	if p.Model != "" {
		e.Model.Valid = true
		e.Model.String = p.Model
	}

	// raw_meta must be set BEFORE enrichChatEdit: the enrich step inspects it to
	// recognise an authoritative Cursor Tab-log completion (source=cursor_tab_log)
	// and leave its gen_type alone.
	if p.RawMeta != "" {
		e.RawMeta.Valid = true
		e.RawMeta.String = p.RawMeta
	}

	enrichChatEdit(db, &e)

	setNullInt(&e.InputTokens, p.InputTokens)
	setNullInt(&e.OutputTokens, p.OutputTokens)
	setNullInt(&e.CacheReadTokens, p.CacheReadTokens)
	setNullInt(&e.CacheWriteTokens, p.CacheWriteTokens)
	if p.HashBefore != "" {
		e.HashBefore.Valid = true
		e.HashBefore.String = p.HashBefore
	}
	if p.HashAfter != "" {
		e.HashAfter.Valid = true
		e.HashAfter.String = p.HashAfter
	}
	for _, r := range p.Lines {
		if r.Start <= 0 || r.End < r.Start {
			return fmt.Errorf("invalid line range [%d,%d]", r.Start, r.End)
		}
		e.Lines = append(e.Lines, store.EditLine{
			StartLine: r.Start, EndLine: r.End, ContentSHA: r.ContentSHA, ContentSHANorm: r.ContentSHANorm,
		})
	}
	for _, rl := range p.RemovedLines {
		if rl.ContentSHA == "" {
			continue
		}
		e.RemovedLines = append(e.RemovedLines, store.RemovedLineHash{
			ContentSHA: rl.ContentSHA, ContentSHANorm: rl.ContentSHANorm,
		})
	}
	netUnchangedEditLines(&e)
	sessions.resolve(db, &e, p.Branch)
	if _, err := db.InsertEdit(e); err != nil {
		return err
	}
	updateFileSnapshot(db, p.RepoPath, p.FilePath)
	return nil
}

// netUnchangedEditLines cancels added/removed lines with identical content within
// one edit. When a tool emits a whole-file (or large-region) rewrite, every
// UNCHANGED line — and any human-typed line the assistant re-included verbatim —
// arrives as a matching removed+added pair. Those are not AI authorship: only
// net-new added lines and net-removed lines are. Doing this once here, on the
// common store.Edit every tool funnels through (validateAndStore for hook tools,
// dbSink.Record for watcher tools), means Copilot, Claude, Codex, Cursor, … all
// get it — instead of each parser re-implementing (and drifting on) the rule.
// Matching is by content_sha (multiset), so a re-included duplicate is cancelled
// exactly as many times as it was removed.
func netUnchangedEditLines(e *store.Edit) {
	if len(e.Lines) == 0 || len(e.RemovedLines) == 0 {
		return
	}
	remCount := make(map[string]int, len(e.RemovedLines))
	for _, rl := range e.RemovedLines {
		if rl.ContentSHA != "" {
			remCount[rl.ContentSHA]++
		}
	}
	addCount := make(map[string]int, len(e.Lines))
	for _, ln := range e.Lines {
		if ln.ContentSHA != "" {
			addCount[ln.ContentSHA]++
		}
	}
	keptLines := make([]store.EditLine, 0, len(e.Lines))
	for _, ln := range e.Lines {
		if ln.ContentSHA != "" && remCount[ln.ContentSHA] > 0 {
			remCount[ln.ContentSHA]-- // unchanged line — paired with a removal
			continue
		}
		keptLines = append(keptLines, ln)
	}
	keptRemoved := make([]store.RemovedLineHash, 0, len(e.RemovedLines))
	for _, rl := range e.RemovedLines {
		if rl.ContentSHA != "" && addCount[rl.ContentSHA] > 0 {
			addCount[rl.ContentSHA]-- // unchanged line — paired with an addition
			continue
		}
		keptRemoved = append(keptRemoved, rl)
	}
	e.Lines = keptLines
	e.RemovedLines = keptRemoved
}

// updateFileSnapshot caches repo/file's current on-disk content as the
// "before" baseline for the next edit to this file (see (*Server).snapshot).
// Best-effort: a read failure (file deleted, permission error, race) just
// means the next edit falls back to HEAD instead of this edit's result.
func updateFileSnapshot(db *store.DB, repoPath, filePath string) {
	data, err := os.ReadFile(filepath.Join(repoPath, filePath))
	if err != nil {
		return
	}
	_ = db.SetFileSnapshot(repoPath, filePath, string(data), time.Now().UnixNano())
}

// chatEnrichWindowNanos is the look-back/forward window for correlating a
// committed edit with the chat-session markers emitted by the chat watcher.
const chatEnrichWindowNanos = int64(60 * 1e9)     // 60 seconds — gen_type correlation
const chatModelWindowNanos = int64(30 * 60 * 1e9) // 30 minutes — sticky model backfill

// enrichChatEdit upgrades a chat-tool edit's gen_type + model from recent
// chat-session markers. The plugin / log / velocity paths can't tell a
// chat-panel apply from an inline Tab accept, so a chat-generated edit arrives
// as gen_type=completion with no model. The chat-session watcher writes
// gen_type=chat markers (carrying the selected model) whenever a response
// streams into the chat JSONL; we look those up here so the committed edit
// reflects the chat panel correctly. Runs for both copilot and cursor; a marker
// only exists when the user actually used that tool's chat panel, so a pure
// Tab-completion session is left as gen_type=completion.
// editFromCursorTabLog reports whether an edit was recorded by the Cursor Tab
// completion-log watcher (raw_meta.source == "cursor_tab_log"). Such an edit is
// authoritative that its lines were an accepted Cursor Tab (inline) completion,
// so its gen_type must not be upgraded to chat. Mirrors the gitnotes helper of
// the same name (kept package-local to avoid a daemon→gitnotes import).
func editFromCursorTabLog(e *store.Edit) bool {
	if e == nil || e.Tool != store.ToolCursor || !e.RawMeta.Valid || e.RawMeta.String == "" {
		return false
	}
	var meta struct {
		Source string `json:"source"`
	}
	if json.Unmarshal([]byte(e.RawMeta.String), &meta) != nil {
		return false
	}
	return meta.Source == "cursor_tab_log"
}

func enrichChatEdit(db *store.DB, e *store.Edit) {
	switch e.Tool {
	case store.ToolCopilot, store.ToolCursor:
	default:
		return
	}
	now := e.TimestampNanos
	// Never override a CONFIRMED inline-completion accept. confidence=high on a
	// completion means the editor plugin saw the inline-suggest commit command
	// (or IDE accept action) fire — that's a real Tab/inline accept, not a chat
	// apply, even if the user happened to have a chat panel open nearby. A Cursor
	// Tab-log edit is likewise authoritative: it comes from Cursor's own inline
	// completion log, so it must stay a completion even though it's recorded at
	// medium confidence (repro: 7e1f1912 — Tab completions relabelled chat).
	confirmedCompletion := e.GenType == store.GenTypeCompletion &&
		(e.Confidence == store.ConfidenceHigh || editFromCursorTabLog(e))
	if e.GenType != store.GenTypeChat && !confirmedCompletion {
		if recent := db.LatestChatGenTypeNear(e.Tool, now, chatEnrichWindowNanos); recent == string(store.GenTypeChat) {
			e.GenType = store.GenTypeChat
		}
	}
	if !e.Model.Valid || e.Model.String == "" {
		// Best-effort model backfill for ANY copilot/cursor edit (chat AND
		// inline completion). The selected model is sticky across a session, so
		// use a generous window — an inline completion gets the model the user
		// most recently had active, even if their last chat was minutes ago.
		if m := db.LatestChatModelNear(e.Tool, e.RepoPath, now, chatModelWindowNanos); m != "" {
			e.Model = sql.NullString{Valid: true, String: m}
		}
	}
}

func defaultConfidence(t store.Tool) store.Confidence {
	switch t {
	case store.ToolClaude, store.ToolCodex:
		return store.ConfidenceHigh
	case store.ToolCursor:
		return store.ConfidenceHigh
	case store.ToolCopilot:
		return store.ConfidenceLow
	default:
		return store.ConfidenceHigh
	}
}

func setNullInt(dst *sql.NullInt64, src *int64) {
	if src != nil {
		dst.Valid = true
		dst.Int64 = *src
	}
}

// writeSocketFile is a no-op: net.Listen("unix", sockPath) already creates the
// socket file. Its existence is the indicator — there is nothing to write into it.
func writeSocketFile(_ string) error { return nil }

func removeSocketFile() {
	if p, err := config.SocketFile(); err == nil {
		_ = os.Remove(p)
	}
}

func writePortFile(port int) error {
	p, err := config.PortFile()
	if err != nil {
		return err
	}
	return os.WriteFile(p, []byte(fmt.Sprintf("%d", port)), 0o644)
}

func removePortFile() {
	if p, err := config.PortFile(); err == nil {
		_ = os.Remove(p)
	}
}

func writePidFile() error {
	p, err := config.PidFile()
	if err != nil {
		return err
	}
	return os.WriteFile(p, []byte(fmt.Sprintf("%d", os.Getpid())), 0o644)
}

func removePidFile() {
	if p, err := config.PidFile(); err == nil {
		_ = os.Remove(p)
	}
}

// watchListenerFile periodically confirms that path — the daemon's socket or
// port file, its sole discovery mechanism for `blamely status` and the
// record/snapshot hooks — still exists on disk. If something removes it out
// from under a live listener (e.g. a racing daemon instance's startup
// cleanup at server.go:131, or external deletion), the bound listener becomes
// permanently unreachable to new clients with no way to recreate the file in
// place. Cancelling ctx triggers a clean shutdown so the platform service
// manager (launchd KeepAlive / systemd Restart=always) immediately restarts
// us with a fresh file.
func watchListenerFile(ctx context.Context, path string, cancel context.CancelFunc) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := os.Stat(path); err != nil {
				cancel()
				return
			}
		}
	}
}

// ReadSocket returns the Unix domain socket path if the socket file exists,
// or an error if the daemon is not running. The socket file's existence is the
// indicator — its content must not be read (it is a socket, not a plain file).
func ReadSocket() (string, error) {
	p, err := config.SocketFile()
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("socket file %s: %w", p, err)
	}
	return p, nil
}

// ReadPort is retained for backward compatibility with callers that still use
// the TCP port. Returns an error if the port file does not exist.
func ReadPort() (int, error) {
	p, err := config.PortFile()
	if err != nil {
		return 0, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return 0, fmt.Errorf("read port file %s: %w", p, err)
	}
	var port int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &port); err != nil {
		return 0, fmt.Errorf("parse port: %w", err)
	}
	return port, nil
}
