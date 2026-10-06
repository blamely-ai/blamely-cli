package tools

// AntigravityGeminiWatcher reads Antigravity IDE's bundled Gemini agent
// transcript logs and attributes the file edits the agent makes to tool=gemini.
//
// Antigravity's chat panel doesn't write the textEditGroup shape VS Code-family
// chat panels do (see chatSessionWatcher in copilot_chat.go) — there is no
// gemini hook firing either, since the agent runs inside the IDE rather than
// through Gemini CLI's BeforeTool/AfterTool hook framework (gemini.go). The
// only structured, attributable record of what the agent wrote is its own
// per-conversation transcript:
//
// File layout (per inspection — undocumented, subject to change):
//
//	~/.gemini/antigravity-ide/brain/<conversationId>/.system_generated/logs/transcript.jsonl
//
// Each line is a JSON step record, append-only. Agent-made file edits show up
// as CODE_ACTION steps with source=MODEL, in one of two shapes:
//
//	{"step_index":15,"source":"MODEL","type":"CODE_ACTION","status":"DONE",
//	 "created_at":"2026-06-07T21:00:59Z",
//	 "content":"...replace_file_content tool to: /abs/path/file.ext. ...
//	[diff_block_start]
//	@@ -135,6 +135,16 @@
//	          </svg>
//	+        <button ...>
//	[diff_block_end]..."}
//
//	{"step_index":7,"source":"MODEL","type":"CODE_ACTION","status":"DONE",
//	 "created_at":"2026-06-07T18:22:53Z",
//	 "content":"...Created file file:///abs/path/file.ext with requested content...."}
//
// We tail each transcript for new CODE_ACTION/MODEL lines, pull the target
// file path plus either the unified diff's `+` lines (edit) or the file's
// full current contents (creation) out of `content`, and emit a per-line
// high-confidence chat edit — same shape Copilot/Cursor's textEditGroup
// detection produces, just sourced from Antigravity's agent transcript.

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/blamely/blamely/internal/config"
	"github.com/blamely/blamely/internal/daemon"
	"github.com/blamely/blamely/internal/gitutil"
	"github.com/blamely/blamely/internal/store"
)

const antigravityChatPollInterval = 3 * time.Second
const antigravityChatStaleCutoff = 24 * time.Hour

// AntigravityGeminiWatcher implements daemon.Watcher for Antigravity IDE's
// bundled Gemini agent. It always emits tool=gemini, gen_type=chat.
type AntigravityGeminiWatcher struct {
	// Roots overrides the brain/ scan roots for tests.
	Roots []string
	// DB, when set, persists per-file read offsets across daemon restarts.
	DB *store.DB
}

func (w *AntigravityGeminiWatcher) Name() string { return "antigravity-gemini" }

func (w *AntigravityGeminiWatcher) Run(ctx context.Context, sink daemon.Sink) error {
	roots := w.Roots
	if len(roots) == 0 {
		roots = defaultAntigravityBrainRoots()
	}
	return (&antigravityTranscriptWatcher{roots: roots, name: w.Name(), db: w.DB}).run(ctx, sink)
}

func defaultAntigravityBrainRoots() []string {
	home, err := config.Home()
	if err != nil {
		return nil
	}
	return []string{filepath.Join(home, ".gemini", "antigravity-ide", "brain")}
}

// ── transcript scanner ────────────────────────────────────────────────────────

type antigravityTranscriptWatcher struct {
	roots []string
	name  string
	db    *store.DB
}

// transcriptFileState is the per-file bookkeeping carried between scans.
type transcriptFileState struct {
	offset    int64
	lastTouch time.Time
	seen      map[int]bool // step_index already emitted, dedups re-reads after offset resets

	// model is the conversation's Gemini model, resolved from the conversation's
	// SQLite db. The db is written by Antigravity asynchronously — it typically
	// lags the transcript by seconds to minutes — so we retry on each emit until
	// the model is found rather than locking in an empty string on first miss.
	// modelAttempts caps retries so a conversation with no model data doesn't
	// trigger a DB open on every step indefinitely.
	model         string
	modelAttempts int

	// savedOffset is the last offset persisted to the watermark store; the scan
	// only re-saves when the offset advances past it.
	savedOffset int64

	// createdFromContent holds the resolved abs path of files we've already
	// recorded from a write_to_file tool-call's actual AI content. The matching
	// "Created file" CODE_ACTION must then NOT re-record by reading the file from
	// disk — disk may already contain a human edit (e.g. a paste) made before the
	// watcher got there, which would mis-attribute those lines to the AI.
	createdFromContent map[string]bool
}

func (w *antigravityTranscriptWatcher) run(ctx context.Context, sink daemon.Sink) error {
	var mu sync.Mutex
	state := map[string]*transcriptFileState{}

	tick := time.NewTicker(antigravityChatPollInterval)
	defer tick.Stop()
	for {
		for _, root := range w.roots {
			w.scanRoot(root, state, &mu, sink)
		}
		w.evictStale(state, &mu)
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

func (w *antigravityTranscriptWatcher) scanRoot(root string, state map[string]*transcriptFileState, mu *sync.Mutex, sink daemon.Sink) {
	if root == "" {
		return
	}
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
		// Only .system_generated/logs/transcript.jsonl — the per-conversation
		// agent transcript. Other brain/ artifacts (task.md, walkthrough.md,
		// media__*.jpg, …) aren't structured edit records.
		if filepath.Base(p) != "transcript.jsonl" ||
			!strings.Contains(p, string(filepath.Separator)+".system_generated"+string(filepath.Separator)) {
			return nil
		}
		w.handleTranscriptFile(p, state, mu, sink)
		return nil
	})
}

func (w *antigravityTranscriptWatcher) handleTranscriptFile(path string, state map[string]*transcriptFileState, mu *sync.Mutex, sink daemon.Sink) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return
	}
	mu.Lock()
	st, ok := state[path]
	if !ok {
		st = &transcriptFileState{seen: map[int]bool{}, createdFromContent: map[string]bool{}}
		state[path] = st
		w.loadWatermark(path, st, info) // resume across daemon restarts
	}
	if info.Size() < st.offset {
		st.offset = 0
		st.seen = map[int]bool{}
	}
	startOffset := st.offset
	prime := startOffset == 0
	mu.Unlock()

	defer w.saveWatermark(path, st, mu, info)

	if info.Size() <= startOffset {
		return
	}

	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	if _, err := f.Seek(startOffset, 0); err != nil {
		return
	}
	r := bufio.NewReaderSize(f, 1<<16)
	// Suppress emitting on the very first scan of a file that's been idle for
	// a while — that's a historical transcript from a past session, not a live
	// edit. A transcript still being actively appended to is live.
	emit := !prime || time.Since(info.ModTime()) <= 2*antigravityChatPollInterval
	// Advance the offset only over complete (newline-terminated) lines; a partial
	// trailing line is left unconsumed for the next scan so the persisted offset
	// never strands past a half-written record.
	consumed := startOffset
	for {
		line, rerr := r.ReadString('\n')
		if rerr != nil {
			break
		}
		w.handleLine(path, line, st, mu, sink, emit)
		consumed += int64(len(line))
	}
	mu.Lock()
	st.offset = consumed
	st.lastTouch = time.Now()
	mu.Unlock()
}

// loadWatermark seeds st.offset from the persisted resume point so a daemon
// restart continues instead of re-parsing the whole transcript. No-op without a
// DB / saved watermark, or if the saved offset is past EOF (rotated file).
func (w *antigravityTranscriptWatcher) loadWatermark(path string, st *transcriptFileState, info os.FileInfo) {
	if w.db == nil {
		return
	}
	wm, ok := w.db.LoadWatermark(w.name, path)
	if !ok || wm.ByteOffset > info.Size() {
		return
	}
	st.offset = wm.ByteOffset
	st.savedOffset = wm.ByteOffset
}

// saveWatermark persists st.offset when it advances — a cheap no-op otherwise.
func (w *antigravityTranscriptWatcher) saveWatermark(path string, st *transcriptFileState, mu *sync.Mutex, info os.FileInfo) {
	if w.db == nil {
		return
	}
	mu.Lock()
	if st.offset == st.savedOffset {
		mu.Unlock()
		return
	}
	off := st.offset
	st.savedOffset = off
	mu.Unlock()

	if err := w.db.SaveWatermark(w.name, path, store.Watermark{
		ByteOffset: off,
		Size:       info.Size(),
		MtimeNanos: info.ModTime().UnixNano(),
	}); err != nil {
		log.Printf("%s: save watermark %s: %v", w.name, path, err)
	}
}

func (w *antigravityTranscriptWatcher) handleLine(path, line string, st *transcriptFileState, mu *sync.Mutex, sink daemon.Sink, emit bool) {
	line = strings.TrimSpace(line)
	if line == "" || line[0] != '{' {
		return
	}
	var step transcriptStep
	if err := json.Unmarshal([]byte(line), &step); err != nil {
		return
	}
	if step.Source != "MODEL" {
		return
	}
	// CODE_ACTION carries file edits; PLANNER_RESPONSE carries shell commands
	// (e.g. `rm file` deletions). Everything else is narrative we ignore.
	if step.Type != "CODE_ACTION" && step.Type != "PLANNER_RESPONSE" {
		return
	}
	mu.Lock()
	if st.seen[step.StepIndex] {
		mu.Unlock()
		return
	}
	st.seen[step.StepIndex] = true
	mu.Unlock()
	if !emit {
		return
	}

	if step.Type == "PLANNER_RESPONSE" {
		w.handleToolCalls(path, step, st, sink)
		return
	}

	abs, ranges, suggested, wholeFile, removed := parseCodeAction(step.Content)
	if abs == "" {
		return
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		abs = r
	}
	if wholeFile {
		// Already recorded from the write_to_file tool-call's actual AI content —
		// don't re-read disk, which may now hold a human edit (e.g. a paste).
		if st.createdFromContent[abs] {
			return
		}
		lr, err := LineRangeForWholeFile(abs)
		if err != nil || len(lr) == 0 {
			return
		}
		ranges = toDaemonLineRanges(lr)
		suggested = int64(len(lr))
	}
	if len(ranges) == 0 {
		return
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
	when := time.Now()
	if t, err := time.Parse(time.RFC3339, step.CreatedAt); err == nil {
		when = t
	}
	const maxModelAttempts = 8
	if st.model == "" && st.modelAttempts < maxModelAttempts {
		st.modelAttempts++
		if m := modelFromConversationDB(conversationDBPath(path)); m != "" {
			st.model = m
			st.modelAttempts = maxModelAttempts // stop retrying
		}
	}
	ev := daemon.Event{
		When:           when,
		Tool:           string(store.ToolGemini),
		Confidence:     "high",
		GenType:        "chat",
		Model:          st.model,
		RepoPath:       repo,
		WorktreePath:   wt,
		FilePath:       rel,
		Lines:          ranges,
		RemovedLines:   removed,
		SuggestedLines: suggested,
		RawMeta: fmt.Sprintf(`{"source":"antigravity_gemini_transcript","transcript_path":%q,"step_index":%d}`,
			path, step.StepIndex),
	}
	if err := sink.Record(ev); err != nil {
		log.Printf("antigravity-gemini sink: %v", err)
	} else {
		log.Printf("antigravity-gemini: apply %s lines=%d", rel, len(ranges))
	}
}

// handleToolCalls processes a PLANNER_RESPONSE step's tool invocations:
//   - run_command shell `rm`/`del` → record the deletion (gemini removal), and
//   - write_to_file / create_file → record the new file from the tool-call's
//     ACTUAL AI content (CodeContent), which is the source of truth — so the
//     matching "Created file" CODE_ACTION won't re-read the file from disk and
//     pick up a human edit (e.g. a paste) as AI-authored.
func (w *antigravityTranscriptWatcher) handleToolCalls(path string, step transcriptStep, st *transcriptFileState, sink daemon.Sink) {
	when := time.Now()
	if t, err := time.Parse(time.RFC3339, step.CreatedAt); err == nil {
		when = t
	}
	for _, tc := range step.ToolCalls {
		switch {
		case isAntigravityShellTool(tc.Name):
			cmd := unquoteAntigravityArg(tc.Args.CommandLine)
			cwd := unquoteAntigravityArg(tc.Args.Cwd)
			if cmd == "" || cwd == "" {
				continue
			}
			for _, tok := range shellDeleteTargets(cmd) {
				abs := tok
				if !filepath.IsAbs(abs) {
					abs = filepath.Join(cwd, tok)
				}
				w.recordAntigravityDeletion(path, abs, cwd, st, when, sink)
			}
		case isAntigravityWriteTool(tc.Name):
			w.recordAntigravityWrite(path, tc, st, when, sink)
		}
	}
}

// recordAntigravityWrite records a file the AI wrote via write_to_file/create_file
// from the tool-call's own CodeContent — never from disk — so a human edit made
// before the watcher catches up can't be mis-credited to the AI. It marks the
// path so the paired "Created file" CODE_ACTION skips its disk re-read.
func (w *antigravityTranscriptWatcher) recordAntigravityWrite(path string, tc transcriptToolCall, st *transcriptFileState, when time.Time, sink daemon.Sink) {
	target := unquoteAntigravityArg(tc.Args.TargetFile)
	content := unquoteAntigravityArg(tc.Args.CodeContent)
	if target == "" || content == "" {
		return
	}
	// Antigravity truncates large tool-call args in the transcript (a literal
	// "<truncated N bytes>" marker). A truncated CodeContent is NOT the full
	// file, so using it would record only the first couple of lines as AI and
	// drop the rest to Human. Bail and leave the path unmarked so the paired
	// "Created file" CODE_ACTION falls back to reading the full file from disk.
	if isTruncatedAntigravityContent(content) {
		return
	}
	abs := fileURIToPath(target)
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		abs = r
	}
	repo, _ := gitutil.RepoID(abs)
	if repo == "" {
		return
	}
	ranges := perLineShaRangesFromContent(content)
	if len(ranges) == 0 {
		return
	}
	wt, _ := gitutil.Toplevel(abs)
	rel := abs
	if wt != "" {
		if r, err := filepath.Rel(wt, abs); err == nil && !strings.HasPrefix(r, "..") {
			rel = r
		}
	}
	st.createdFromContent[abs] = true // tell the CODE_ACTION to skip the disk read

	const maxModelAttempts = 8
	if st.model == "" && st.modelAttempts < maxModelAttempts {
		st.modelAttempts++
		if m := modelFromConversationDB(conversationDBPath(path)); m != "" {
			st.model = m
			st.modelAttempts = maxModelAttempts
		}
	}
	ev := daemon.Event{
		When:           when,
		Tool:           string(store.ToolGemini),
		Confidence:     "high",
		GenType:        "chat",
		Model:          st.model,
		RepoPath:       repo,
		WorktreePath:   wt,
		FilePath:       rel,
		Lines:          toDaemonLineRanges(ranges),
		SuggestedLines: int64(len(ranges)),
		RawMeta:        fmt.Sprintf(`{"source":"antigravity_gemini_write","transcript_path":%q}`, path),
	}
	if err := sink.Record(ev); err != nil {
		log.Printf("antigravity-gemini write sink: %v", err)
	} else {
		log.Printf("antigravity-gemini: write %s lines=%d", rel, len(ranges))
	}
}

// isTruncatedAntigravityContent reports whether a tool-call's CodeContent was
// clipped by Antigravity's transcript logger, which appends a literal
// "<truncated N bytes>" marker when an arg exceeds its size cap. Such content is
// only a prefix of the real file and must not be used for attribution.
func isTruncatedAntigravityContent(content string) bool {
	return strings.Contains(content, "<truncated ")
}

// isAntigravityWriteTool reports whether a tool_call name is an Antigravity
// whole-file write/create (which carries the AI's content in CodeContent).
func isAntigravityWriteTool(name string) bool {
	switch strings.ToLower(name) {
	case "write_to_file", "write_file", "create_file", "create_text_file":
		return true
	}
	return false
}

// recordAntigravityDeletion fingerprints a deleted file's HEAD content as
// removed lines and records it as a gemini removal. The file is already gone, so
// the repo + relative path are resolved from the still-existing parent dir / cwd.
func (w *antigravityTranscriptWatcher) recordAntigravityDeletion(path, abs, cwd string, st *transcriptFileState, when time.Time, sink daemon.Sink) {
	root := findRepoRoot(abs, cwd)
	if root == "" {
		return
	}
	dir := filepath.Dir(abs)
	if rd, err := filepath.EvalSymlinks(dir); err == nil {
		dir = rd
	}
	rootResolved := root
	if rr, err := filepath.EvalSymlinks(root); err == nil {
		rootResolved = rr
	}
	rel, err := filepath.Rel(rootResolved, filepath.Join(dir, filepath.Base(abs)))
	if err != nil || strings.HasPrefix(rel, "..") {
		return
	}
	removed := headFileRemovedHashes(root, rel)
	if len(removed) == 0 {
		return
	}
	repo, _ := gitutil.RepoID(rootResolved)
	if repo == "" {
		repo = root
	}
	const maxModelAttempts = 8
	if st.model == "" && st.modelAttempts < maxModelAttempts {
		st.modelAttempts++
		if m := modelFromConversationDB(conversationDBPath(path)); m != "" {
			st.model = m
			st.modelAttempts = maxModelAttempts
		}
	}
	ev := daemon.Event{
		When:           when,
		Tool:           string(store.ToolGemini),
		Confidence:     "high",
		GenType:        "chat",
		Model:          st.model,
		RepoPath:       repo,
		WorktreePath:   root,
		FilePath:       rel,
		RemovedLines:   toDaemonRemovedLines(removed),
		SuggestedLines: int64(len(removed)),
		RawMeta:        fmt.Sprintf(`{"source":"antigravity_gemini_delete","transcript_path":%q}`, path),
	}
	if err := sink.Record(ev); err != nil {
		log.Printf("antigravity-gemini delete sink: %v", err)
	} else {
		log.Printf("antigravity-gemini: delete %s lines=%d", rel, len(removed))
	}
}

// isAntigravityShellTool reports whether a tool_call name is an Antigravity
// shell-command invocation (which is how it runs `rm`/`del`).
func isAntigravityShellTool(name string) bool {
	switch strings.ToLower(name) {
	case "run_command", "runcommand", "run_terminal_command", "execute_command", "command", "shell":
		return true
	}
	return false
}

// unquoteAntigravityArg decodes a tool_call arg, whose value is a JSON-encoded
// string (e.g. "\"rm x.html\"" -> rm x.html). Falls back to trimming quotes.
func unquoteAntigravityArg(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	var out string
	if json.Unmarshal([]byte(s), &out) == nil {
		return out
	}
	return strings.Trim(s, `"`)
}

func (w *antigravityTranscriptWatcher) evictStale(state map[string]*transcriptFileState, mu *sync.Mutex) {
	mu.Lock()
	defer mu.Unlock()
	cutoff := time.Now().Add(-antigravityChatStaleCutoff)
	for k, st := range state {
		if st.lastTouch.Before(cutoff) {
			delete(state, k)
		}
	}
}

// transcriptStep is the minimal shape of one transcript.jsonl record we need.
// CODE_ACTION steps carry their edit in Content; PLANNER_RESPONSE steps carry
// shell invocations in ToolCalls (e.g. a `run_command` running `rm file`).
type transcriptStep struct {
	StepIndex int                  `json:"step_index"`
	Source    string               `json:"source"`
	Type      string               `json:"type"`
	CreatedAt string               `json:"created_at"`
	Content   string               `json:"content"`
	ToolCalls []transcriptToolCall `json:"tool_calls"`
}

// transcriptToolCall is one tool invocation inside a PLANNER_RESPONSE step. Arg
// values are stored as JSON-encoded strings (e.g. CommandLine: "\"rm x.html\"").
type transcriptToolCall struct {
	Name string `json:"name"`
	Args struct {
		CommandLine string `json:"CommandLine"`
		Cwd         string `json:"Cwd"`
		// write_to_file / create_file carry the AI's actual new content here —
		// the source of truth for an "AI created this file" attribution, used
		// instead of reading the (possibly human-edited) file from disk.
		TargetFile  string `json:"TargetFile"`
		CodeContent string `json:"CodeContent"`
	} `json:"args"`
}

// codeActionEditPathRe extracts the edited file's absolute path from a
// replace_file_content CODE_ACTION's narrative, e.g.
//
//	"...replace_file_content tool to: /Users/me/proj/index.html. If relevant..."
//
// The path is followed by ". " — file paths don't contain that sequence.
var codeActionEditPathRe = regexp.MustCompile(`tool to: (.+?)\.\s`)

// codeActionCreatePathRe extracts the created file's URI from a whole-file
// creation CODE_ACTION's narrative, e.g.
//
//	"...Created file file:///Users/me/proj/style.css with requested content...."
var codeActionCreatePathRe = regexp.MustCompile(`Created file (file://\S+) with requested content`)

// diffHunkHeaderRe matches a unified-diff hunk header and captures the new
// file's starting line number, e.g. "@@ -135,6 +135,16 @@" → "135".
var diffHunkHeaderRe = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// parseCodeAction extracts the target file path from a CODE_ACTION step's
// narrative `content`, plus either:
//   - the per-line additions from its unified diff (replace_file_content), and
//     the content hashes of any lines the diff removed, or
//   - a `wholeFile` signal telling the caller to attribute every line in the
//     file as written (file creation — content isn't echoed in the transcript,
//     so the caller re-reads the file from disk via LineRangeForWholeFile).
//
// Only `+` diff lines are attributed to the model as additions — context lines
// carry no new AI-written content. `-` lines are hashed separately (via
// UnifiedDiffRemovedLineHashes) so the commit-time attribution pass can
// recognize lines this edit deleted.
func parseCodeAction(content string) (path string, ranges []daemon.LineRange, suggested int64, wholeFile bool, removed []daemon.RemovedLineHash) {
	if m := codeActionCreatePathRe.FindStringSubmatch(content); m != nil {
		return fileURIToPath(m[1]), nil, 0, true, nil
	}
	m := codeActionEditPathRe.FindStringSubmatch(content)
	if m == nil {
		return "", nil, 0, false, nil
	}
	path = strings.TrimSpace(m[1])

	start := strings.Index(content, "[diff_block_start]")
	end := strings.Index(content, "[diff_block_end]")
	if start < 0 || end < 0 || end <= start {
		return path, nil, 0, false, nil
	}
	block := content[start+len("[diff_block_start]") : end]

	newLine := 0
	for _, raw := range strings.Split(block, "\n") {
		if hm := diffHunkHeaderRe.FindStringSubmatch(raw); hm != nil {
			if n, err := strconv.Atoi(hm[1]); err == nil {
				newLine = n
			}
			continue
		}
		if newLine == 0 {
			continue // narrative text before the first hunk header
		}
		switch {
		case strings.HasPrefix(raw, "+"):
			text := strings.TrimRight(raw[1:], "\r")
			ranges = append(ranges, daemon.LineRange{
				Start: newLine, End: newLine,
				ContentSHA:     sha256Hex([]byte(text)),
				ContentSHANorm: sha256HexNorm(text),
			})
			suggested++
			newLine++
		case strings.HasPrefix(raw, "-"):
			// removed from the old file — doesn't advance the new-file counter
		default:
			// context line (unified diff prefixes these with a space)
			newLine++
		}
	}
	removed = toDaemonRemovedLines(UnifiedDiffRemovedLineHashes(block))
	return path, ranges, suggested, false, removed
}

// fileURIToPath converts a "file:///abs/path" URI from the transcript's
// narrative into a filesystem path, decoding any percent-escapes and
// stripping the leading slash a Windows drive URI carries ("/C:/..." → "C:/...").
func fileURIToPath(uri string) string {
	const prefix = "file://"
	if !strings.HasPrefix(uri, prefix) {
		return uri
	}
	p := strings.TrimPrefix(uri, prefix)
	if u, err := url.PathUnescape(p); err == nil {
		p = u
	}
	return stripWindowsDriveSlash(p)
}

// ── model resolution ──────────────────────────────────────────────────────────
//
// The transcript carries no model name. Antigravity does record it, but only
// inside each conversation's own SQLite database
// (antigravity-ide/conversations/<id>.db, sibling of brain/<id>/), as opaque,
// undocumented protobuf blobs in gen_metadata/executor_metadata. Rather than
// reverse the schema, we exploit one structural fact of protobuf encoding:
// a string field is a length byte (or varint) immediately followed by exactly
// that many bytes of payload. Scanning for "byte N, then N bytes that look
// like a Gemini model name" is robust without the schema — a coincidental
// length/content match is vanishingly unlikely (the regex anchors on the
// "gemini-<digit>" prefix real model ids use, which workspace/project names
// like "gemini-test" don't).

// conversationDBPath derives an Antigravity conversation's SQLite db path from
// its transcript path:
//
//	.../antigravity-ide/brain/<id>/.system_generated/logs/transcript.jsonl
//	  -> .../antigravity-ide/conversations/<id>.db
func conversationDBPath(transcriptPath string) string {
	sysGenDir := filepath.Dir(filepath.Dir(transcriptPath)) // brain/<id>/.system_generated
	if filepath.Base(sysGenDir) != ".system_generated" {
		return ""
	}
	convDir := filepath.Dir(sysGenDir) // brain/<id>
	id := filepath.Base(convDir)
	brainDir := filepath.Dir(convDir) // brain
	ideRoot := filepath.Dir(brainDir) // antigravity-ide
	if id == "" || id == "." || id == string(filepath.Separator) {
		return ""
	}
	return filepath.Join(ideRoot, "conversations", id+".db")
}

// antigravityModelNameRe matches plausible model identifiers Antigravity can
// run. Antigravity is model-agnostic — the user can pick Gemini, Claude, GPT,
// etc. — so this must recognize each provider's id format, not just Gemini's.
// Examples: "gemini-3-flash", "claude-sonnet-4-6", "gpt-4o", "gpt-5",
// "o3-mini". Each alternative anchors on a known provider prefix so a stray
// string in the same blob (e.g. a "gemini-test" project path or the word
// "claude" in prose) can't match — and the scanner's exact length-byte check
// makes a coincidental hit vanishingly unlikely.
var antigravityModelNameRe = regexp.MustCompile(
	`^(?:gemini-\d[\w.-]*|claude-[a-z][\w.-]+|gpt-\d[\w.+-]*|o[1-4](?:-[\w.-]+)?|grok-[\w.-]+|deepseek-[\w.-]+)$`)

// modelFromConversationDB best-effort opens an Antigravity conversation's
// SQLite db read-only and returns the most recent Gemini model name found in
// its turn-metadata blobs, or "" if none is found or the db can't be read
// (e.g. still locked by Antigravity, or the schema has changed).
func modelFromConversationDB(dbPath string) string {
	if dbPath == "" {
		return ""
	}
	if _, err := os.Stat(dbPath); err != nil {
		return ""
	}
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&_pragma=busy_timeout(2000)")
	if err != nil {
		return ""
	}
	defer db.Close()

	for _, table := range []string{"gen_metadata", "executor_metadata"} {
		rows, err := db.Query(fmt.Sprintf("SELECT data FROM %s ORDER BY idx DESC LIMIT 12", table))
		if err != nil {
			continue
		}
		for rows.Next() {
			var blob []byte
			if rows.Scan(&blob) != nil {
				continue
			}
			if m := scanProtobufModelString(blob); m != "" {
				rows.Close()
				return m
			}
		}
		rows.Close()
	}
	return ""
}

// scanProtobufModelString walks a protobuf-encoded blob looking for a
// length-delimited string field whose payload matches antigravityModelNameRe.
// The length byte must equal the candidate's exact byte length — a constraint a
// coincidental substring can't satisfy — making this reliable without
// decoding the surrounding message. The lower bound covers short ids like
// "gpt-5" (5 bytes) while the regex's provider-prefix anchoring keeps that
// loosening from admitting noise.
func scanProtobufModelString(blob []byte) string {
	for i := 0; i+1 < len(blob); i++ {
		n := int(blob[i])
		if n < 5 || n > 48 {
			continue
		}
		end := i + 1 + n
		if end > len(blob) {
			continue
		}
		if cand := blob[i+1 : end]; antigravityModelNameRe.Match(cand) {
			return string(cand)
		}
	}
	return ""
}
