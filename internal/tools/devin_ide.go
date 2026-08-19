package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/blamely/blamely/internal/config"
	"github.com/blamely/blamely/internal/daemon"
	"github.com/blamely/blamely/internal/gitutil"
	"github.com/blamely/blamely/internal/store"

	_ "modernc.org/sqlite"
)

// DevinIDEWatcher attributes edits made by the agent embedded in the Devin IDE
// (a VS Code fork), which has no hook framework — so unlike Devin CLI there is
// nothing to install, and the edits must be recovered from what the IDE writes
// to disk.
//
// The IDE keeps one SQLite database per agent session under:
//
//	macOS:   ~/Library/Application Support/devin/User/acp-messages/<uuid>.db
//	Windows: %APPDATA%\devin\User\acp-messages\<uuid>.db
//	Linux:   ~/.config/devin/User/acp-messages/<uuid>.db
//
// with a single table of appended protocol messages:
//
//	messages(position INTEGER PRIMARY KEY, kind TEXT, payload TEXT)
//
// A file edit is a row with kind='tool_call' whose payload has
// content.kind == "edit", carrying the written path and text:
//
//	{"kind":"tool_call","content":{
//	   "kind":"edit","status":"completed",
//	   "_meta":{"cognition.ai/inferenceToolName":"write",
//	            "cognition.ai/timestamp":"2026-08-19T15:18:17Z"},
//	   "content":[{"type":"diff","path":"/abs/file.html",
//	               "newText":"...","oldText":"..."}],
//	   "rawInput":{"file_path":"/abs/file.html"}}}
//
// # Local sessions only
//
// The same database also records DevinCLOUD sessions, and those must be
// skipped. A cloud session runs the agent in a remote sandbox: its paths are
// container paths (`/home/ubuntu/repos/...`) that mean nothing here, and its
// newText/oldText are empty because the real content lives in a remote blob
// referenced by `cognition.ai/contentsKey`. There is no local edit to attribute
// and no content to attribute it from.
//
// Rather than sniff for cloud markers, the watcher simply requires what a local
// edit always has and a cloud edit never does: an absolute path that exists on
// this machine inside a git repo, plus non-empty newText. Cloud rows fail both
// tests and fall out naturally.
//
// Cloud work is not lost — it is picked up at commit time from the agent's
// Co-Authored-By trailer instead (see internal/gitnotes/trailer.go).
type DevinIDEWatcher struct {
	// Roots overrides the default acp-messages directories. Tests only.
	Roots []string
	DB    *store.DB
}

const (
	devinIDEPollInterval = 3 * time.Second
	// devinIDEStaleCutoff drops in-memory state for sessions untouched this
	// long, so a long-running daemon doesn't accumulate one entry per session
	// forever.
	devinIDEStaleCutoff = 24 * time.Hour
	// devinIDEMaxRowsPerPoll bounds a single poll. A session that produced
	// thousands of rows while the daemon was down is drained over several
	// ticks instead of blocking the watcher loop on one file.
	devinIDEMaxRowsPerPoll = 500
)

func (w *DevinIDEWatcher) Name() string { return "devin-ide" }

func (w *DevinIDEWatcher) Run(ctx context.Context, sink daemon.Sink) error {
	roots := w.Roots
	if len(roots) == 0 {
		roots = defaultDevinACPRoots()
	}
	if len(roots) == 0 {
		return nil
	}

	state := map[string]*devinIDESessionState{}
	var mu sync.Mutex
	prime := true

	tick := time.NewTicker(devinIDEPollInterval)
	defer tick.Stop()
	for {
		for _, root := range roots {
			w.scanRoot(root, state, &mu, prime, sink)
		}
		prime = false
		devinIDEEvictStale(state, &mu)

		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// defaultDevinACPRoots is the per-OS acp-messages directory.
//
// Deliberately NOT derived from config.DevinConfigDir: that is the Devin CLI's
// config home (~/.config/devin even on macOS), whereas the IDE stores session
// state in the platform's app-data location like any Electron app.
func defaultDevinACPRoots() []string {
	home, err := config.Home()
	if err != nil {
		return nil
	}
	switch runtime.GOOS {
	case "darwin":
		return []string{filepath.Join(home, "Library", "Application Support", "devin", "User", "acp-messages")}
	case "windows":
		if appData := os.Getenv("APPDATA"); appData != "" {
			return []string{filepath.Join(appData, "devin", "User", "acp-messages")}
		}
		return []string{filepath.Join(home, "AppData", "Roaming", "devin", "User", "acp-messages")}
	default:
		return []string{filepath.Join(home, ".config", "devin", "User", "acp-messages")}
	}
}

// devinIDESessionState tracks how far into one session database we have read.
type devinIDESessionState struct {
	lastPosition int64
	lastTouch    time.Time
	loaded       bool
}

func (w *DevinIDEWatcher) scanRoot(root string, state map[string]*devinIDESessionState, mu *sync.Mutex, prime bool, sink daemon.Sink) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		// Only the session databases themselves — SQLite's -wal/-shm siblings
		// are read through the main handle, never opened directly.
		if e.IsDir() || !strings.HasSuffix(name, ".db") {
			continue
		}
		w.handleSessionDB(filepath.Join(root, name), state, mu, prime, sink)
	}
}

func (w *DevinIDEWatcher) handleSessionDB(path string, state map[string]*devinIDESessionState, mu *sync.Mutex, prime bool, sink daemon.Sink) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}

	mu.Lock()
	st := state[path]
	if st == nil {
		st = &devinIDESessionState{}
		state[path] = st
	}
	st.lastTouch = time.Now()
	if !st.loaded {
		st.loaded = true
		if w.DB != nil {
			if wm, ok := w.DB.LoadWatermark(w.Name(), path); ok {
				st.lastPosition = wm.ByteOffset
			}
		}
	}
	from := st.lastPosition
	mu.Unlock()

	// On the daemon's FIRST look at a session we advance the cursor without
	// emitting, unless the file was just written. Otherwise a cold start would
	// replay every session on disk as if the edits were happening now — and
	// they'd be attributed against today's file contents, which no longer match.
	emit := !prime || time.Since(info.ModTime()) <= 2*devinIDEPollInterval

	rows, maxPos, err := readDevinACPRows(path, from, devinIDEMaxRowsPerPoll)
	if err != nil || maxPos <= from {
		// Still refresh the watermark's updated_nanos so a live-but-quiet
		// session isn't pruned as stale.
		w.saveWatermark(path, from, info)
		return
	}

	if emit {
		for _, r := range rows {
			w.recordDevinIDEEdit(path, r, sink)
		}
	}

	mu.Lock()
	st.lastPosition = maxPos
	mu.Unlock()
	w.saveWatermark(path, maxPos, info)
}

func (w *DevinIDEWatcher) saveWatermark(path string, pos int64, info os.FileInfo) {
	if w.DB == nil {
		return
	}
	_ = w.DB.SaveWatermark(w.Name(), path, store.Watermark{
		ByteOffset: pos,
		Size:       info.Size(),
		MtimeNanos: info.ModTime().UnixNano(),
	})
}

// devinACPEdit is one file edit recovered from a session database.
type devinACPEdit struct {
	Position  int64
	Path      string
	NewText   string
	OldText   string
	Tool      string
	Timestamp time.Time
}

// readDevinACPRows reads edit rows with position > after, up to limit, and
// returns them alongside the highest position seen (including rows that were
// not edits, so the cursor advances past them too).
//
// The database is opened read-only for each poll and closed immediately: the
// IDE holds it open in WAL mode, and a short-lived read-only handle with a busy
// timeout is what coexists with that safely.
func readDevinACPRows(path string, after int64, limit int) ([]devinACPEdit, int64, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(2000)")
	if err != nil {
		return nil, after, err
	}
	defer db.Close()

	rows, err := db.Query(
		`SELECT position, payload FROM messages WHERE kind = 'tool_call' AND position > ? ORDER BY position LIMIT ?`,
		after, limit)
	if err != nil {
		return nil, after, err
	}
	defer rows.Close()

	maxPos := after
	var out []devinACPEdit
	for rows.Next() {
		var pos int64
		var payload string
		if err := rows.Scan(&pos, &payload); err != nil {
			continue
		}
		if pos > maxPos {
			maxPos = pos
		}
		if e, ok := parseDevinACPEdit(payload); ok {
			e.Position = pos
			out = append(out, e)
		}
	}
	if err := rows.Err(); err != nil {
		return out, maxPos, err
	}
	return out, maxPos, nil
}

// devinACPPayload is the subset of an ACP tool_call message blamely reads.
type devinACPPayload struct {
	Content struct {
		Kind   string `json:"kind"`
		Status string `json:"status"`
		Meta   struct {
			InferenceToolName string `json:"cognition.ai/inferenceToolName"`
			Timestamp         string `json:"cognition.ai/timestamp"`
		} `json:"_meta"`
		Content []struct {
			Type    string `json:"type"`
			Path    string `json:"path"`
			NewText string `json:"newText"`
			OldText string `json:"oldText"`
		} `json:"content"`
		RawInput struct {
			FilePath string `json:"file_path"`
			Path     string `json:"path"`
		} `json:"rawInput"`
	} `json:"content"`
}

// parseDevinACPEdit extracts a completed file edit from a tool_call payload.
// Anything else — reads, shell execs, searches, failed calls, and cloud-session
// rows whose text lives in a remote blob — returns ok=false.
func parseDevinACPEdit(payload string) (devinACPEdit, bool) {
	var p devinACPPayload
	if json.Unmarshal([]byte(payload), &p) != nil {
		return devinACPEdit{}, false
	}
	c := p.Content
	if c.Kind != "edit" || c.Status != "completed" {
		return devinACPEdit{}, false
	}

	var e devinACPEdit
	for _, item := range c.Content {
		if item.Type != "diff" || item.NewText == "" {
			continue
		}
		e.Path = item.Path
		e.NewText = item.NewText
		e.OldText = item.OldText
		break
	}
	// No usable diff entry: either a cloud row (newText empty, content held
	// remotely) or a shape we don't understand. Either way there is nothing to
	// attribute from.
	if e.NewText == "" {
		return devinACPEdit{}, false
	}
	if e.Path == "" {
		e.Path = firstNonEmpty(c.RawInput.FilePath, c.RawInput.Path)
	}
	if e.Path == "" {
		return devinACPEdit{}, false
	}

	e.Tool = c.Meta.InferenceToolName
	if e.Tool == "" {
		e.Tool = "edit"
	}
	if ts, err := time.Parse(time.RFC3339, c.Meta.Timestamp); err == nil {
		e.Timestamp = ts
	} else {
		e.Timestamp = time.Now()
	}
	return e, true
}

// recordDevinIDEEdit turns one recovered edit into a daemon Event.
func (w *DevinIDEWatcher) recordDevinIDEEdit(dbPath string, e devinACPEdit, sink daemon.Sink) {
	// A cloud session's container path (/home/ubuntu/repos/...) will not exist
	// here, which is exactly how those rows get filtered out. A local path that
	// has since been deleted is skipped too — without the file there is no way
	// to place the lines.
	abs := e.Path
	if !filepath.IsAbs(abs) {
		return
	}
	if _, err := os.Stat(abs); err != nil {
		return
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		abs = r
	}
	repo, _ := gitutil.RepoID(abs)
	if repo == "" {
		return
	}
	wt, _ := gitutil.Toplevel(abs)
	rel := abs
	if wt != "" {
		if r, err := filepath.Rel(wt, abs); err == nil && !strings.HasPrefix(r, "..") {
			rel = r
		}
	}

	// Line ranges come from the agent's own text, never from re-reading disk:
	// the user may have typed over it between the edit and this poll, and those
	// lines are theirs, not the agent's.
	ranges := perLineShaRangesFromContent(e.NewText)
	if len(ranges) == 0 {
		return
	}
	removed := RemovedLineHashes(e.OldText, e.NewText)

	ev := daemon.Event{
		When:           e.Timestamp,
		Tool:           string(store.ToolDevin),
		Confidence:     "high",
		GenType:        "chat", // an IDE agent panel, not the terminal CLI
		RepoPath:       repo,
		FilePath:       rel,
		Lines:          toDaemonLineRanges(ranges),
		RemovedLines:   toDaemonRemovedLines(removed),
		SuggestedLines: int64(len(ranges)),
		RawMeta: fmt.Sprintf(`{"source":"devin_ide_acp","session_db":%q,"position":%d,"tool":%q}`,
			dbPath, e.Position, e.Tool),
	}
	if err := sink.Record(ev); err != nil {
		log.Printf("devin-ide sink: %v", err)
		return
	}
	log.Printf("devin-ide: edit %s lines=%d", rel, len(ranges))
}

func devinIDEEvictStale(state map[string]*devinIDESessionState, mu *sync.Mutex) {
	mu.Lock()
	defer mu.Unlock()
	for k, st := range state {
		if time.Since(st.lastTouch) > devinIDEStaleCutoff {
			delete(state, k)
		}
	}
}
