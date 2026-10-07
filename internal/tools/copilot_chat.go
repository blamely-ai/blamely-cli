package tools

// CopilotChatWatcher reads VS Code chat-session JSONL files and attributes them
// to GitHub Copilot. It only watches VS Code's workspaceStorage directory.
//
// Cursor's chat sessions are handled by CursorChatWatcher (cursor_chat.go),
// which watches Cursor's workspaceStorage directory and always emits tool=cursor.
// The two watchers are fully independent — changing one has no effect on the other.
//
// File layout (per inspection — undocumented, subject to change):
//
//	~/Library/Application Support/Code/User/workspaceStorage/
//	  <hash>/chatSessions/<sessionId>.jsonl
//
// Each .jsonl is append-only and uses a delta encoding:
//
//	{"kind":0,"v":{...full initial state, including selectedModel...}}
//	{"kind":1,"k":["requests",0,"modelState"],"v":{...}}
//	{"kind":2,"k":["requests",0,"response"],"v":"...streamed chunk..."}
//
// kind=0 = snapshot, kind=1 = set value at key path, kind=2 = append.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/blamely/blamely/internal/config"
	"github.com/blamely/blamely/internal/daemon"
	"github.com/blamely/blamely/internal/gitutil"
	"github.com/blamely/blamely/internal/store"
)

const copilotChatPollInterval = 2 * time.Second

// copilotChatStaleCutoff is how old a chat session file may be before the watcher
// ignores it (both in the scan and in evictStale). Defaults to 24h — long enough
// to still catch edits made while the daemon was offline — but configurable via
// BLAMELY_CHAT_STALE_HOURS for machines with a very large session history that
// want to scan even fewer files. Resolved once at process start.
var copilotChatStaleCutoff = resolveChatStaleCutoff()

func resolveChatStaleCutoff() time.Duration {
	const def = 24 * time.Hour
	raw := strings.TrimSpace(os.Getenv("BLAMELY_CHAT_STALE_HOURS"))
	if raw == "" {
		return def
	}
	h, err := strconv.ParseFloat(raw, 64)
	if err != nil || h <= 0 {
		return def // ignore garbage / non-positive values, keep the safe default
	}
	return time.Duration(h * float64(time.Hour))
}

// Adaptive polling: the idle base cadence is fine when nothing is happening, but
// it makes a freshly-typed chat edit take up to a couple seconds to show up. So
// whenever a session file changed recently we poll at copilotChatActiveInterval
// instead, for a copilotChatActiveWindow grace period after the last observed
// change. This cuts detection latency during active use without walking
// workspaceStorage any more often than before while idle.
//
// The window is sized to outlast a whole agent turn (thinking + tool calls +
// streamed apply, which can run tens of seconds): Copilot only writes the
// textEditGroup's `done=true` checkpoint at the END of the turn, and if the
// window lapsed mid-turn we'd drop to the idle cadence and not record the edit
// until the next slow poll. Keeping the fast cadence across the turn means the
// edit is read within one active interval of Copilot finalizing it.
const (
	copilotChatActiveInterval = 300 * time.Millisecond
	copilotChatActiveWindow   = 45 * time.Second
)

// ── Public watcher types ──────────────────────────────────────────────────────

// CopilotChatWatcher implements daemon.Watcher for GitHub Copilot chat sessions
// in VS Code. It watches Code/User/workspaceStorage and always emits tool=copilot.
type CopilotChatWatcher struct {
	// Roots overrides the workspaceStorage scan roots for tests.
	Roots []string
	// DB, when set, persists per-file read offsets so a daemon restart resumes
	// where it left off instead of re-parsing whole session files (and re-emitting
	// historical edits as duplicates). Nil in tests → in-memory only, as before.
	DB *store.DB
}

func (c *CopilotChatWatcher) Name() string { return "copilot-chat" }

func (c *CopilotChatWatcher) Run(ctx context.Context, sink daemon.Sink) error {
	roots := c.Roots
	if len(roots) == 0 {
		roots = defaultCopilotChatRoots()
	}
	return (&chatSessionWatcher{tool: store.ToolCopilot, roots: roots, name: c.Name(), db: c.DB}).run(ctx, sink)
}

// defaultCopilotChatRoots returns the VS Code workspaceStorage paths for the
// current OS. Only VS Code — Cursor is handled by CursorChatWatcher.
// findChatSessionPath locates a VS Code / Cursor chatSessions JSONL file by
// session UUID (filename is <sessionID>.jsonl under workspaceStorage).
func findChatSessionPath(sessionID string, roots []string) string {
	if sessionID == "" {
		return ""
	}
	name := sessionID + ".jsonl"
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			p := filepath.Join(root, e.Name(), "chatSessions", name)
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	return ""
}

// copilotChatSearchRoots returns workspaceStorage roots where Copilot chat
// sessions may live (VS Code and Copilot-in-Cursor).
func copilotChatSearchRoots() []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range append(defaultCopilotChatRoots(), defaultCursorChatRoots()...) {
		if r == "" || seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	return out
}

func defaultCopilotChatRoots() []string {
	home, err := config.Home()
	if err != nil {
		return nil
	}
	// Copilot runs in stock VS Code AND in the Insiders build, each with its own
	// user-data dir ("Code" / "Code - Insiders"). Scan both.
	switch runtime.GOOS {
	case "darwin":
		base := filepath.Join(home, "Library", "Application Support")
		return []string{
			filepath.Join(base, "Code", "User", "workspaceStorage"),
			filepath.Join(base, "Code - Insiders", "User", "workspaceStorage"),
		}
	case "windows":
		appData := os.Getenv("APPDATA")
		if appData == "" {
			return nil
		}
		return []string{
			filepath.Join(appData, "Code", "User", "workspaceStorage"),
			filepath.Join(appData, "Code - Insiders", "User", "workspaceStorage"),
		}
	default:
		base := filepath.Join(home, ".config")
		return []string{
			filepath.Join(base, "Code", "User", "workspaceStorage"),
			filepath.Join(base, "Code - Insiders", "User", "workspaceStorage"),
		}
	}
}

// ── Shared implementation ─────────────────────────────────────────────────────

// chatSessionWatcher is the shared JSONL scanner. It is not exported — callers
// use CopilotChatWatcher or CursorChatWatcher. The tool field is fixed at
// construction time: every event emitted by this watcher carries that tool,
// regardless of which model the user selected inside the editor. This keeps
// Copilot and Cursor attribution fully independent.
type chatSessionWatcher struct {
	tool  store.Tool
	roots []string
	// name is the watcher's stable id ("copilot-chat"/"cursor-chat"), used as the
	// watermark key. db, when set, persists per-file offsets across daemon
	// restarts; nil → in-memory only (tests, or a daemon without a DB handle).
	name string
	db   *store.DB
}

// chatWatermarkExtra is the per-file cursor state persisted alongside the byte
// offset, so a restart resumes textEditGroup scanning exactly where it stopped.
type chatWatermarkExtra struct {
	TegOffset  int64 `json:"teg_offset"`
	NextReqIdx int   `json:"next_req_idx"`
}

// sessionState is the per-file bookkeeping carried between scans.
type sessionState struct {
	offset    int64     // bytes read so far from this jsonl
	model     string    // last-known display model (provider prefix stripped)
	lastTouch time.Time // for stale eviction

	// textEditGroup detection scans incrementally from tegOffset (its own
	// cursor, independent of offset above) and is deduped by edit key in
	// seenEdits. tegMtime gates re-scans so an unchanged file costs nothing.
	// nextReqIdx is the requests-array length seen so far, used to assign a
	// stable index to whole-new-request appends (k=["requests"]).
	tegMtime   time.Time
	tegOffset  int64
	nextReqIdx int
	seenEdits  map[string]bool
	// seenReqTokens tracks which chat requests (turns) have already had their
	// token usage attached to an emitted edit, so a turn that edits multiple
	// files credits its prompt/output tokens to exactly ONE edit (not once per
	// file) — keeping the note's token totals from multiplying.
	seenReqTokens map[int]bool

	// loaded marks that a persisted watermark was applied when this state was
	// first created, so handleSessionFile only saves when something advanced.
	savedOffset int64
	savedTeg    int64
}

func (w *chatSessionWatcher) run(ctx context.Context, sink daemon.Sink) error {
	var mu sync.Mutex
	state := map[string]*sessionState{}

	// Adaptive cadence: scanRoot reports the newest session-file mtime it saw;
	// while that's within copilotChatActiveWindow of now we poll fast, otherwise
	// we fall back to the idle base interval.
	timer := time.NewTimer(0)
	defer timer.Stop()
	var lastActivity time.Time
	for {
		var latest time.Time
		for _, root := range w.roots {
			if t := w.scanRoot(root, state, &mu, sink); t.After(latest) {
				latest = t
			}
		}
		w.evictStale(state, &mu)
		if latest.After(lastActivity) {
			lastActivity = latest
		}
		next := copilotChatPollInterval
		if time.Since(lastActivity) < copilotChatActiveWindow {
			next = copilotChatActiveInterval
		}
		timer.Reset(next)
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
	}
}

// scanRoot processes every chat session file under one workspaceStorage root.
// It returns the newest session-file mtime it observed, which run() uses to
// decide whether to keep polling at the fast active cadence.
//
// Two levels of ReadDir rather than a recursive walk. The layout is fixed —
// <workspaceStorage>/<hash>/chatSessions/*.jsonl — but workspaceStorage also holds
// every workspace's extension state, and VS Code never prunes it: on a developer
// machine that is thousands of directories the walk descended into on every tick,
// to reach a couple hundred files it could have addressed directly. Measured on a
// 128-workspace profile, the recursive walk visited ~5500 entries; this visits
// ~670, and it runs every 2 seconds while a chat is active.
func (w *chatSessionWatcher) scanRoot(root string, state map[string]*sessionState, mu *sync.Mutex, sink daemon.Sink) time.Time {
	if root == "" {
		return time.Time{}
	}
	workspaces, err := os.ReadDir(root)
	if err != nil {
		return time.Time{} // root absent (editor not installed) — nothing to scan
	}
	var latest time.Time
	for _, ws := range workspaces {
		if !ws.IsDir() {
			continue
		}
		// The chatEditingSessions sibling directory has different content we
		// don't currently parse.
		sessDir := filepath.Join(root, ws.Name(), "chatSessions")
		entries, derr := os.ReadDir(sessDir)
		if derr != nil {
			continue // workspace has never opened a chat
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			// Skip stale sessions. A user with many workspaces accumulates hundreds
			// of old chatSessions files (often multi-MB), but only a handful are ever
			// active. Parsing them all on the first scan and stat-ing them every poll
			// makes live detection slow and late — badly so on Windows. A reopened old
			// chat gets a fresh mtime and is picked up again automatically;
			// evictStale drops its state.
			info, ierr := e.Info()
			if ierr == nil && time.Since(info.ModTime()) > copilotChatStaleCutoff {
				continue
			}
			if mtime := w.handleSessionFile(filepath.Join(sessDir, e.Name()), state, mu, sink); mtime.After(latest) {
				latest = mtime
			}
		}
	}
	return latest
}

// handleSessionFile processes one chat session JSONL and returns the file's
// mtime (zero on stat failure) so the caller can track recent activity for
// adaptive polling.
func (w *chatSessionWatcher) handleSessionFile(path string, state map[string]*sessionState, mu *sync.Mutex, sink daemon.Sink) time.Time {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return time.Time{}
	}
	mtime := info.ModTime()
	mu.Lock()
	st, ok := state[path]
	if !ok {
		st = &sessionState{}
		state[path] = st
		// First time we see this file this run: resume from the persisted
		// watermark so a restart doesn't re-parse the whole file or re-emit its
		// historical edits. The shrink guard below still handles a rotated file
		// whose saved offset now exceeds its size.
		w.loadWatermark(path, st, info)
	}
	// Reset the streaming offset if the file shrank — VS Code periodically
	// rewrites a session as a fresh snapshot, which would otherwise strand the
	// offset past EOF and silently stop processing.
	if info.Size() < st.offset {
		st.offset = 0
	}
	startOffset := st.offset
	prime := startOffset == 0
	mu.Unlock()

	// Persist the resume point on the way out (only if something advanced), so
	// the next scan / a daemon restart continues from here.
	defer w.saveWatermark(path, st, mu, info)

	// textEditGroup detection runs on every scan via its own mtime gate —
	// independent of the append-offset so it survives snapshot rewrites.
	// Must run BEFORE the no-new-bytes return below.
	w.scanTextEdits(path, st, mu, sink, info.ModTime(), info.Size())

	if info.Size() <= startOffset {
		return mtime
	}

	f, err := os.Open(path)
	if err != nil {
		return mtime
	}
	defer f.Close()
	if _, err := f.Seek(startOffset, 0); err != nil {
		return mtime
	}
	r := bufio.NewReaderSize(f, 1<<16)
	now := time.Now()
	// Advance the offset only over COMPLETE (newline-terminated) lines. A partial
	// trailing line (the file was caught mid-write) is left unconsumed so the next
	// scan re-reads it whole — otherwise the offset (which we now also persist)
	// would strand past a half-written record and corrupt/skip it on resume.
	consumed := startOffset
	for {
		line, rerr := r.ReadString('\n')
		if rerr == nil {
			w.handleLine(path, line, st, sink, prime, now, info.ModTime())
			consumed += int64(len(line))
			continue
		}
		break // EOF/error: 'line' (if any) is an incomplete trailing line — skip it.
	}
	mu.Lock()
	st.offset = consumed
	st.lastTouch = now
	mu.Unlock()
	return mtime
}

// loadWatermark seeds st from the persisted resume point for this file, so a
// daemon restart continues instead of re-parsing the whole session. No-op when
// there's no DB or no saved watermark (st stays zero → prime from the start).
func (w *chatSessionWatcher) loadWatermark(path string, st *sessionState, info os.FileInfo) {
	if w.db == nil {
		return
	}
	wm, ok := w.db.LoadWatermark(w.name, path)
	if !ok {
		return
	}
	// Don't trust an offset past EOF (file rotated/truncated since the save) — the
	// shrink guard would reset it anyway; re-prime from the start instead.
	if wm.ByteOffset > info.Size() {
		return
	}
	st.offset = wm.ByteOffset
	var ex chatWatermarkExtra
	if wm.Extra != "" {
		_ = json.Unmarshal([]byte(wm.Extra), &ex)
	}
	st.tegOffset = ex.TegOffset
	st.nextReqIdx = ex.NextReqIdx
	if wm.MtimeNanos > 0 {
		// Resume the textEditGroup mtime gate so an unchanged file isn't re-scanned.
		st.tegMtime = time.Unix(0, wm.MtimeNanos)
	}
	st.savedOffset = st.offset
	st.savedTeg = st.tegOffset
}

// saveWatermark persists the resume point if it advanced since the last save —
// a cheap no-op when nothing changed or there's no DB.
func (w *chatSessionWatcher) saveWatermark(path string, st *sessionState, mu *sync.Mutex, info os.FileInfo) {
	if w.db == nil {
		return
	}
	mu.Lock()
	if st.offset == st.savedOffset && st.tegOffset == st.savedTeg {
		mu.Unlock()
		return
	}
	off, teg, nri := st.offset, st.tegOffset, st.nextReqIdx
	st.savedOffset = off
	st.savedTeg = teg
	mu.Unlock()

	exb, _ := json.Marshal(chatWatermarkExtra{TegOffset: teg, NextReqIdx: nri})
	if err := w.db.SaveWatermark(w.name, path, store.Watermark{
		ByteOffset: off,
		Size:       info.Size(),
		MtimeNanos: info.ModTime().UnixNano(),
		Extra:      string(exb),
	}); err != nil {
		log.Printf("%s: save watermark %s: %v", w.name, path, err)
	}
}

// scanTextEdits incrementally scans the bytes appended to the session file
// since the last call (gated by mtime, so an unchanged file is a no-op) and
// records a chat edit for every new textEditGroup it hasn't emitted before.
//
// Scanning incrementally — rather than re-reading the whole file from byte 0
// on every poll — matters for long-running chat sessions: a session's JSONL
// can grow to several MB over many turns, and re-parsing plus re-hashing that
// entire history every 3s would make detection latency grow with session
// size (fast for new sessions, increasingly slow for long ones).
func (w *chatSessionWatcher) scanTextEdits(path string, st *sessionState, mu *sync.Mutex, sink daemon.Sink, mtime time.Time, size int64) {
	mu.Lock()
	if st.seenEdits == nil {
		st.seenEdits = map[string]bool{}
	}
	firstScan := st.tegMtime.IsZero()
	if !mtime.After(st.tegMtime) {
		mu.Unlock()
		return
	}
	st.tegMtime = mtime
	model := st.model
	startOffset := st.tegOffset
	nextReqIdx := st.nextReqIdx
	// VS Code occasionally rewrites a session as a fresh, smaller snapshot —
	// re-scan from the start when that happens (rare); seenEdits is cleared
	// too since the rewritten file may renumber requests.
	if size < startOffset {
		startOffset = 0
		nextReqIdx = 0
		st.seenEdits = map[string]bool{}
		firstScan = true
	}
	mu.Unlock()

	// Suppress emitting on the very first scan of an OLD file (historical edits);
	// always emit once the file is actively being modified.
	emit := !firstScan || time.Since(mtime) <= 2*copilotChatPollInterval

	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	if startOffset > 0 {
		if _, err := f.Seek(startOffset, 0); err != nil {
			return
		}
	}
	br := bufio.NewReaderSize(f, 1<<16)
	// Advance tegOffset only over complete (newline-terminated) lines so a partial
	// trailing line is re-read whole next scan (the offset is persisted now).
	consumed := startOffset
	for {
		line, rerr := br.ReadString('\n')
		if rerr != nil {
			break // incomplete trailing line — leave unconsumed
		}
		consumed += int64(len(line))
		trimmed := strings.TrimRight(line, "\r\n")
		if len(trimmed) > 0 && trimmed[0] == '{' {
			var cl chatLine
			if json.Unmarshal([]byte(trimmed), &cl) == nil {
				// Keep the display model fresh as we scan (used when recording edits).
				if m := findSelectedModel(cl); m != "" {
					model = displayModel(m)
				}
				// textEditGroups are tagged with the chat request index they belong to
				// (requests[N].response[...]) so that edits from different turns never
				// collide in recordTextEditGroup's dedup key, even when both happen to
				// start at the same line (e.g. the model rewrites the whole file again
				// in a follow-up turn).
				switch cl.Kind {
				case 0:
					var snap struct {
						Requests []struct {
							Response []json.RawMessage `json:"response"`
						} `json:"requests"`
					}
					if json.Unmarshal(cl.V, &snap) == nil {
						for reqIdx, r := range snap.Requests {
							for _, part := range r.Response {
								var groups []textEditGroupPart
								findTextEditGroups(part, &groups)
								for i := range groups {
									w.recordTextEditGroup(&groups[i], model, path, st, mu, sink, emit, reqIdx)
								}
							}
						}
						nextReqIdx = len(snap.Requests)
					}
				case 2:
					if reqIdx, ok := requestIndex(cl.K, "response"); ok {
						// Streaming delta: new response part(s) appended to an
						// existing requests[reqIdx].response array.
						var parts []json.RawMessage
						if json.Unmarshal(cl.V, &parts) == nil {
							for _, part := range parts {
								var groups []textEditGroupPart
								findTextEditGroups(part, &groups)
								for i := range groups {
									w.recordTextEditGroup(&groups[i], model, path, st, mu, sink, emit, reqIdx)
								}
							}
						}
					} else if keyPathEquals(cl.K, "requests") {
						// A whole new request (turn) appended to the top-level
						// requests array. Its response array may already be
						// populated — including finalized textEditGroups — in
						// this single delta, e.g. for a fast/non-streamed turn.
						var newReqs []struct {
							Response []json.RawMessage `json:"response"`
						}
						if json.Unmarshal(cl.V, &newReqs) == nil {
							for _, req := range newReqs {
								for _, part := range req.Response {
									var groups []textEditGroupPart
									findTextEditGroups(part, &groups)
									for i := range groups {
										w.recordTextEditGroup(&groups[i], model, path, st, mu, sink, emit, nextReqIdx)
									}
								}
								nextReqIdx++
							}
						}
					}
				}
			}
		}
	}
	mu.Lock()
	st.tegOffset = consumed
	st.nextReqIdx = nextReqIdx
	mu.Unlock()
}

func (w *chatSessionWatcher) handleLine(path, line string, st *sessionState, sink daemon.Sink, prime bool, scanTime, fileMTime time.Time) {
	var cl chatLine
	if err := json.Unmarshal([]byte(line), &cl); err != nil {
		return
	}
	switch cl.Kind {
	case 0:
		var snap struct {
			InputState struct {
				SelectedModel json.RawMessage `json:"selectedModel"`
			} `json:"inputState"`
		}
		if json.Unmarshal(cl.V, &snap) == nil {
			st.applyModel(extractSelectedModel(snap.InputState.SelectedModel))
		}
	case 1:
		if keyPathHasSuffix(cl.K, "selectedModel") {
			st.applyModel(extractSelectedModel(cl.V))
		}
	case 2:
		if !keyPathHasSuffix(cl.K, "response") {
			return
		}
		if prime && time.Since(fileMTime) > 2*copilotChatPollInterval {
			return
		}
		ev := daemon.Event{
			When:       scanTime,
			Tool:       string(w.tool),
			Confidence: "low",
			GenType:    "chat",
			Model:      st.model,
			RawMeta: fmt.Sprintf(`{"source":"copilot_chat_session","tool":%q,"chat_session_path":%q}`,
				string(w.tool), path),
		}
		if err := sink.Record(ev); err != nil {
			log.Printf("%s-chat sink: %v", w.tool, err)
		}
	}
}

// recordTextEditGroup records one finalized textEditGroup as a per-file chat
// edit. The tool is always w.tool — the watcher knows which editor it belongs to.
// reqIdx is the chat request (turn) this textEditGroup belongs to, used to keep
// the dedup key below from colliding across separate turns.
func (w *chatSessionWatcher) recordTextEditGroup(teg *textEditGroupPart, model, sessionPath string, st *sessionState, mu *sync.Mutex, sink daemon.Sink, emit bool, reqIdx int) {
	if !teg.Done {
		return
	}
	abs := teg.URI.FsPath
	if abs == "" {
		// URI.Path carries a spurious leading slash before a Windows drive
		// letter ("/c:/Users/..."); fsPath is the OS-native form when present.
		abs = stripWindowsDriveSlash(teg.URI.Path)
	}
	if abs == "" || (teg.URI.Scheme != "" && teg.URI.Scheme != "file") {
		return
	}

	// Resolve the repo and relative path early so we can fetch the pre-chat
	// snapshot for narrowing (see snapshotPut in server.go).
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		abs = r
	}
	repo, _ := gitutil.RepoID(abs)
	wt, _ := gitutil.Toplevel(abs)
	rel := abs
	if repo != "" {
		if wt != "" {
			if r, err := filepath.Rel(wt, abs); err == nil && !strings.HasPrefix(r, "..") {
				rel = r
			}
		}
	}

	// Fetch the pre-chat snapshot stored by the VS Code plugin via PUT /snapshot.
	// This is consume-once (deleted on first read) and only exists when the
	// plugin is installed. If absent (non-plugin user, daemon restart, or timing
	// race), hasFreshSnap=false and the watcher falls back to recording all lines
	// rather than applying multiset narrowing — this preserves correct attribution
	// for AI line-reordering / block-swap edits that the multiset diff would miss.
	snapshot, hasFreshSnap := "", false
	if repo != "" {
		snapshot, hasFreshSnap = fetchPreChatSnapshot(wt, rel)
	}

	var ranges []daemon.LineRange
	var suggested int64
	hasRemoval := false
	// narrowed marks this edit as a DELTA (it recorded only the lines that
	// differ from the pre-chat snapshot, dropping unchanged ones). The note's
	// drift budget needs this: a delta records DISTINCT new AI lines, so its
	// per-content counts SUM across edits, whereas a full re-save records the
	// same lines repeatedly and must be MAX'd. Without the distinction, an AI
	// line the agent re-included unchanged in a later narrowed apply (so this
	// edit didn't re-record it) loses its drift budget and falls to Human.
	narrowed := false
	for _, grp := range teg.Edits {
		for _, e := range grp {
			// endLineNumber > startLineNumber means this edit's range spans
			// existing lines (VS Code's half-open convention) — a candidate
			// for removed-line attribution, computed below once we have the
			// pre-edit snapshot. Checked regardless of e.Text so pure
			// multi-line deletions (empty replacement text) are caught too.
			if e.Range.EndLineNumber > e.Range.StartLineNumber {
				hasRemoval = true
			}
			if strings.TrimSpace(e.Text) == "" {
				continue
			}
			start := e.Range.StartLineNumber
			if start <= 0 {
				start = 1
			}
			allLines := strings.Split(strings.TrimSuffix(e.Text, "\n"), "\n")
			suggested += int64(len(allLines))

			if e.Range.EndLineNumber > e.Range.StartLineNumber && hasFreshSnap {
				// Narrowing: only record lines that differ from the pre-chat file
				// content. The plugin PUT the pre-chat snapshot before the watcher
				// polls, so this diff correctly excludes unchanged human-typed lines.
				narrowed = true
				for _, relPos := range chatNarrowedPositions(snapshot, start, e.Range.EndLineNumber, e.Text) {
					text := strings.TrimRight(allLines[relPos-1], "\r")
					ranges = append(ranges, daemon.LineRange{
						Start: start + relPos - 1, End: start + relPos - 1,
						ContentSHA:     sha256Hex([]byte(text)),
						ContentSHANorm: sha256HexNorm(text),
					})
				}
			} else {
				// Pure insertion or no snapshot: record all lines.
				for i, lt := range allLines {
					text := strings.TrimRight(lt, "\r")
					ranges = append(ranges, daemon.LineRange{
						Start: start + i, End: start + i,
						ContentSHA:     sha256Hex([]byte(text)),
						ContentSHANorm: sha256HexNorm(text),
					})
				}
			}
		}
	}
	if len(ranges) == 0 && !hasRemoval {
		return
	}

	// Dedup key: file + chat request index + startLineNumber of each edit group.
	// Streaming applies write multiple done=true checkpoints for the same region
	// with growing content within ONE turn — keying on start positions collapses
	// those into one emission. The request index keeps separate turns (e.g. a
	// follow-up that rewrites the file again, also starting at line 1) from
	// colliding and silently dropping the later, correct edit.
	key := fmt.Sprintf("%s|%d", abs, reqIdx)
	for _, grp := range teg.Edits {
		for _, e := range grp {
			key += fmt.Sprintf("|%d", e.Range.StartLineNumber)
			break
		}
	}
	mu.Lock()
	if st.seenEdits[key] {
		mu.Unlock()
		return
	}
	st.seenEdits[key] = true
	mu.Unlock()
	if !emit {
		return
	}

	if repo == "" {
		return
	}

	// Removed-line attribution: use the pre-chat snapshot if the plugin
	// provided one; otherwise fall back to the regular cached snapshot so
	// non-plugin users still get removed-line attribution.
	var removed []DeletedLineHash
	if hasRemoval {
		removalSnap := snapshot
		if !hasFreshSnap {
			removalSnap, _ = fetchSnapshot(wt, rel)
		}
		if removalSnap != "" {
			for _, grp := range teg.Edits {
				for _, e := range grp {
					if e.Range.EndLineNumber > e.Range.StartLineNumber {
						removed = append(removed, removedLinesForTextEditRange(removalSnap, e.Range.StartLineNumber, e.Range.EndLineNumber, e.Text)...)
					}
				}
			}
		}
	}

	ev := daemon.Event{
		When:           time.Now(),
		Tool:           string(w.tool),
		Confidence:     "high",
		GenType:        "chat",
		Model:          model,
		RepoPath:       repo,
		WorktreePath:   wt,
		FilePath:       rel,
		Lines:          ranges,
		SuggestedLines: suggested,
		RemovedLines:   toDaemonRemovedLines(removed),
		RawMeta: fmt.Sprintf(`{"source":"copilot_chat_textedit","tool":%q,"chat_session_path":%q,"narrowed":%t}`,
			string(w.tool), sessionPath, narrowed),
	}
	// Attach this turn's token usage (prompt/output) to the FIRST edit it
	// produces, so the git note's per-tool token totals include chat-panel
	// edits (Copilot/Cursor) — not just hook-driven tools. Crediting only the
	// first edit per request avoids multiplying a turn's tokens across the
	// several files it may have touched.
	mu.Lock()
	if st.seenReqTokens == nil {
		st.seenReqTokens = map[int]bool{}
	}
	creditTokens := !st.seenReqTokens[reqIdx]
	if creditTokens {
		st.seenReqTokens[reqIdx] = true
	}
	mu.Unlock()
	if creditTokens {
		if u, _ := ReadChatSessionRequestUsage(sessionPath, reqIdx); u != nil {
			if ev.Model == "" {
				ev.Model = u.Model
			}
			if u.InputTokens > 0 {
				v := u.InputTokens
				ev.InputTokens = &v
			}
			if u.OutputTokens > 0 {
				v := u.OutputTokens
				ev.OutputTokens = &v
			}
		}
	}

	if err := sink.Record(ev); err != nil {
		log.Printf("%s-chat textedit sink: %v", w.tool, err)
	} else {
		log.Printf("%s-chat: apply %s lines=%d model=%s", w.tool, rel, len(ranges), model)
	}
}

// chatNarrowedPositions returns the 1-based positions (within newText) of lines
// that are genuinely new or changed relative to the pre-chat file content at
// [startLine, endLine). Used to exclude unchanged human-typed lines from the
// watcher's attribution when the plugin has stored the pre-chat snapshot.
func chatNarrowedPositions(snapshot string, startLine, endLine int, newText string) []int {
	lines := strings.Split(snapshot, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	last := endLine - 1
	if last > len(lines) {
		last = len(lines)
	}
	var oldSection []byte
	if startLine-1 >= 0 && startLine-1 < len(lines) {
		oldSection = []byte(strings.Join(lines[startLine-1:last], "\n"))
	}
	var positions []int
	// LCS diff (not multiset): a whole-file rewrite that MOVES a line (same
	// content, new position) must still credit the AI for the relocated line.
	// Multiset narrowing drops it (content count unchanged), which sent moved
	// lines to Human at commit time.
	for _, r := range addedOrMovedLineRanges(oldSection, []byte(newText)) {
		for p := r.Start; p <= r.End; p++ {
			positions = append(positions, p)
		}
	}
	return positions
}

// applyModel updates the session's display model from a raw selectedModel
// identifier (e.g. "copilot/gpt-5-mini"). The tool is NOT changed here —
// it is fixed on the watcher, not per-session.
func (st *sessionState) applyModel(identifier string) {
	if identifier == "" {
		return
	}
	st.model = displayModel(identifier)
}

// displayModel strips the provider prefix from a chat model identifier so the
// report's per-model rollup reads "gpt-5-mini" rather than "copilot/gpt-5-mini".
func displayModel(identifier string) string {
	if i := strings.IndexByte(identifier, '/'); i >= 0 {
		return identifier[i+1:]
	}
	return identifier
}

func (w *chatSessionWatcher) evictStale(state map[string]*sessionState, mu *sync.Mutex) {
	mu.Lock()
	defer mu.Unlock()
	cutoff := time.Now().Add(-copilotChatStaleCutoff)
	for k, st := range state {
		if st.lastTouch.Before(cutoff) {
			delete(state, k)
		}
	}
}

// ── Shared JSONL parsing helpers ──────────────────────────────────────────────

// chatLine is the minimal delta-encoding envelope. K and V are RawMessage so
// we don't pay for schema we don't use.
type chatLine struct {
	Kind int             `json:"kind"`
	K    json.RawMessage `json:"k"`
	V    json.RawMessage `json:"v"`
}

// textEditGroupPart is the response-part shape the chat panel writes when it
// applies edits: target URI, text edits with line ranges, and a done flag.
type textEditGroupPart struct {
	Kind string `json:"kind"`
	URI  struct {
		FsPath string `json:"fsPath"`
		Path   string `json:"path"`
		Scheme string `json:"scheme"`
	} `json:"uri"`
	Edits [][]struct {
		Text  string `json:"text"`
		Range struct {
			StartLineNumber int `json:"startLineNumber"`
			EndLineNumber   int `json:"endLineNumber"`
		} `json:"range"`
	} `json:"edits"`
	Done bool `json:"done"`
}

// findSelectedModel extracts a selectedModel identifier from a delta/snapshot
// line, tolerating both the inputState.selectedModel delta and snapshot shapes.
func findSelectedModel(cl chatLine) string {
	if keyPathHasSuffix(cl.K, "selectedModel") {
		return extractSelectedModel(cl.V)
	}
	if cl.Kind == 0 {
		var snap struct {
			InputState struct {
				SelectedModel json.RawMessage `json:"selectedModel"`
			} `json:"inputState"`
		}
		if json.Unmarshal(cl.V, &snap) == nil {
			return extractSelectedModel(snap.InputState.SelectedModel)
		}
	}
	return ""
}

// findTextEditGroups recursively collects every finalized textEditGroup object
// anywhere within raw (response arrays, snapshot request trees, etc.).
func findTextEditGroups(raw json.RawMessage, out *[]textEditGroupPart) {
	if len(raw) == 0 {
		return
	}
	switch raw[0] {
	case '{':
		var obj map[string]json.RawMessage
		if json.Unmarshal(raw, &obj) != nil {
			return
		}
		if k, ok := obj["kind"]; ok {
			var ks string
			if json.Unmarshal(k, &ks) == nil && ks == "textEditGroup" {
				var teg textEditGroupPart
				if json.Unmarshal(raw, &teg) == nil {
					*out = append(*out, teg)
				}
				return
			}
		}
		for _, v := range obj {
			findTextEditGroups(v, out)
		}
	case '[':
		var arr []json.RawMessage
		if json.Unmarshal(raw, &arr) != nil {
			return
		}
		for _, v := range arr {
			findTextEditGroups(v, out)
		}
	}
}

// extractSelectedModel pulls selectedModel.identifier from a kind=0 or kind=1
// value blob. Tolerant of flat and wrapped shapes.
func extractSelectedModel(v json.RawMessage) string {
	if len(v) == 0 {
		return ""
	}
	var probe struct {
		SelectedModel struct {
			Identifier string `json:"identifier"`
			Family     string `json:"family"`
		} `json:"selectedModel"`
		Identifier string `json:"identifier"`
		Family     string `json:"family"`
	}
	if err := json.Unmarshal(v, &probe); err != nil {
		return ""
	}
	if probe.SelectedModel.Identifier != "" {
		return probe.SelectedModel.Identifier
	}
	if probe.SelectedModel.Family != "" {
		return probe.SelectedModel.Family
	}
	if probe.Identifier != "" {
		return probe.Identifier
	}
	return probe.Family
}

// keyPathHasSuffix reports whether the JSON-encoded k ends with the given
// string element (e.g. ["requests", 0, "response"] ends with "response").
func keyPathHasSuffix(k json.RawMessage, suffix string) bool {
	if len(k) == 0 {
		return false
	}
	var path []json.RawMessage
	if err := json.Unmarshal(k, &path); err != nil || len(path) == 0 {
		return false
	}
	last := path[len(path)-1]
	var s string
	if err := json.Unmarshal(last, &s); err != nil {
		return false
	}
	return s == suffix
}

// keyPathEquals reports whether the JSON-encoded k is exactly the
// single-element path [elem] (e.g. ["requests"]).
func keyPathEquals(k json.RawMessage, elem string) bool {
	if len(k) == 0 {
		return false
	}
	var path []json.RawMessage
	if err := json.Unmarshal(k, &path); err != nil || len(path) != 1 {
		return false
	}
	var s string
	if err := json.Unmarshal(path[0], &s); err != nil {
		return false
	}
	return s == elem
}
