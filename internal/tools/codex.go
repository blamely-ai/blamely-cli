package tools

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/blamely/blamely/internal/config"
	"github.com/blamely/blamely/internal/daemon"
	"github.com/blamely/blamely/internal/gitutil"
	"github.com/blamely/blamely/internal/store"
)

// CodexWatcher tails ~/.codex/sessions/**/*.jsonl and emits attribution events
// for every `apply_patch`-style file mutation. It also captures `model` and
// token usage from the surrounding response events.
//
// Codex CLI partitions session rollouts by date —
// ~/.codex/sessions/<year>/<month>/<day>/rollout-*.jsonl — rather than writing
// them flat into sessions/, so the scan below must recurse.
//
// The session log format is tolerant — Codex CLI versions vary the exact
// event shape. We extract from any JSON line that:
//   - contains a `model` field at the top level or under `.message.model`, OR
//   - is a tool/function call whose name contains "apply_patch" or "patch", OR
//   - has a `usage` block alongside any of the above.
type CodexWatcher struct {
	// SessionsDir overrides the default ~/.codex/sessions location for tests.
	SessionsDir string
	// DB, when set, persists each session file's read offset so a daemon restart
	// resumes instead of replaying the whole file (re-emitting every edit). Nil in
	// tests → replay-from-start, as before.
	DB *store.DB
}

func (c *CodexWatcher) Name() string { return "codex" }

func (c *CodexWatcher) Run(ctx context.Context, sink daemon.Sink) error {
	// The set of sessions/ dirs to scan. A single test override wins; otherwise
	// scan the UNION of ~/.codex/sessions, $CODEX_HOME/sessions, and every extra
	// dir from config (corp setups) — all are watched at once, none replaces
	// another. Re-resolved each tick so a dir added to config later is picked up.
	dirs := func() []string {
		if c.SessionsDir != "" {
			return []string{c.SessionsDir}
		}
		return config.CodexSessionsDirs()
	}
	for _, dir := range dirs() {
		if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
			// Wait for the directory to appear (Codex may not be installed yet).
			log.Printf("codex: %s not found, will poll", dir)
		}
	}

	tailers := map[string]context.CancelFunc{}
	defer func() {
		for _, cancel := range tailers {
			cancel()
		}
	}()

	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()

	for {
		seen := map[string]bool{}
		for _, dir := range dirs() {
			for _, path := range findCodexSessionFiles(dir) {
				seen[path] = true
				if _, running := tailers[path]; running {
					continue
				}
				tCtx, cancel := context.WithCancel(ctx)
				tailers[path] = cancel
				go func(p string) {
					if err := tailCodexSession(tCtx, p, sink, c.DB, c.Name()); err != nil && tCtx.Err() == nil {
						log.Printf("codex tail %s: %v", p, err)
					}
				}(path)
			}
		}
		// Stop tailers whose files were rotated/deleted.
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

// findCodexSessionFiles recursively collects every rollout-*.jsonl under dir,
// since Codex CLI nests them under <year>/<month>/<day>/ rather than writing
// them flat into the sessions root.
func findCodexSessionFiles(dir string) []string {
	var paths []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		paths = append(paths, p)
		return nil
	})
	return paths
}

// tailCodexSession streams new JSONL events from `path` and emits an Event
// for every detected file mutation. Carries forward the latest seen model
// and usage block so apply_patch calls inherit them.
func tailCodexSession(ctx context.Context, path string, sink daemon.Sink, db *store.DB, watcherName string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer f.Close()

	// Resume from the persisted offset so a daemon restart doesn't replay the
	// whole file and re-emit every edit. Without a DB / saved watermark we replay
	// from the beginning (so we don't miss edits made while the daemon was down).
	var startOffset int64
	if db != nil {
		if wm, ok := db.LoadWatermark(watcherName, path); ok {
			if fi, e := f.Stat(); e == nil && wm.ByteOffset <= fi.Size() {
				if _, e := f.Seek(wm.ByteOffset, io.SeekStart); e == nil {
					startOffset = wm.ByteOffset
				}
			}
		}
	}
	reader := bufio.NewReaderSize(f, 1<<16)

	state := &codexState{sink: sink}

	offset := startOffset
	savedOffset := startOffset
	lastSave := time.Now()
	// saveWM persists the resume point — but ONLY when nothing is buffered. Codex
	// buffers apply_patch events until a `response.complete` flush; advancing the
	// watermark past an unflushed buffer would lose those edits if the daemon
	// crashed before the flush. So the watermark only ever moves past flushed
	// content; a crash mid-turn re-reads (and re-buffers) that turn, never drops it.
	saveWM := func(force bool) {
		if db == nil || offset == savedOffset || len(state.pending) > 0 {
			return
		}
		if !force && time.Since(lastSave) < 500*time.Millisecond {
			return
		}
		var size, mt int64
		if fi, e := f.Stat(); e == nil {
			size, mt = fi.Size(), fi.ModTime().UnixNano()
		}
		if err := db.SaveWatermark(watcherName, path, store.Watermark{ByteOffset: offset, Size: size, MtimeNanos: mt}); err == nil {
			savedOffset, lastSave = offset, time.Now()
		}
	}
	// Drain any patches we buffered without seeing a closing `response.complete`
	// (file rotated / daemon shutting down): flush them, then persist the now-empty
	// buffer's offset. Better token-less rows than lost attribution.
	defer func() {
		state.flush(0, 0, 0, 0, false)
		saveWM(true)
	}()

	for {
		line, err := readLineGrowing(ctx, reader, f)
		if err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return nil
			}
			return err
		}
		// Logical consumed offset = file read position minus what bufio has
		// buffered-but-not-yet-returned. Tracks complete lines only (readLineGrowing
		// seeks back over a partial trailing line), so it never splits a record.
		if pos, e := f.Seek(0, io.SeekCurrent); e == nil {
			offset = pos - int64(reader.Buffered())
		}
		if len(line) == 0 || line[0] != '{' {
			saveWM(false)
			continue
		}
		processCodexLine(line, state)
		saveWM(false)
	}
}

// codexState carries the latest-known model and a buffer of events that
// have been built from `apply_patch` calls but haven't yet been told the
// final token usage. In OpenAI-style sessions, `usage` is emitted in
// `response.complete` AFTER the function calls — so we buffer and flush
// when usage arrives (or on shutdown).
type codexState struct {
	model   string
	genType string // resolved from session_meta (chat for IDE surfaces, else cli)
	cwd     string // session working dir; fallback when a shell call omits workdir
	pending []daemon.Event
	sink    daemon.Sink
}

// gen returns the session's gen_type, defaulting to "cli" until a session_meta
// line resolves the surface (older flat-format logs carry no session_meta).
func (st *codexState) gen() string {
	if st.genType != "" {
		return st.genType
	}
	return "cli"
}

// codexGenType maps a Codex session's originator/source to a gen_type. An IDE
// surface — the VS Code chat panel (originator "codex_vscode" / source "vscode"),
// JetBrains, etc. — is a conversational panel → "chat"; the terminal CLI → "cli".
// Unknown surfaces default to "cli" (Codex's historical assumption).
func codexGenType(originator, source string) string {
	s := strings.ToLower(originator + " " + source)
	switch {
	case strings.Contains(s, "vscode"), strings.Contains(s, "vs code"),
		strings.Contains(s, "intellij"), strings.Contains(s, "jetbrains"),
		strings.Contains(s, "ide"), strings.Contains(s, "extension"):
		return "chat"
	}
	return "cli"
}

// ReadCodexGenType resolves a Codex session transcript's gen_type from its
// session_meta line (originator/source). Used by the PostToolUse hook path, which
// has only a transcript path. Returns "cli" when unreadable or unrecognized.
func ReadCodexGenType(path string) string {
	if path == "" {
		return "cli"
	}
	f, err := os.Open(path)
	if err != nil {
		return "cli"
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		if !strings.Contains(sc.Text(), "session_meta") {
			continue
		}
		var env codexWrappedLine
		if json.Unmarshal(sc.Bytes(), &env) != nil || env.Type != "session_meta" {
			continue
		}
		var p struct {
			Originator string `json:"originator"`
			Source     string `json:"source"`
		}
		if json.Unmarshal(env.Payload, &p) == nil {
			return codexGenType(p.Originator, p.Source)
		}
		return "cli"
	}
	return "cli"
}

// flush sets token fields (if provided) on every pending event and writes
// them to the sink. Resets the buffer.
func (s *codexState) flush(input, output, cacheRead, cacheWrite int64, hasTokens bool) {
	for _, ev := range s.pending {
		if hasTokens {
			in, out, cr, cw := input, output, cacheRead, cacheWrite
			ev.InputTokens = &in
			ev.OutputTokens = &out
			ev.CacheReadTokens = &cr
			ev.CacheWriteTokens = &cw
		}
		if err := s.sink.Record(ev); err != nil {
			log.Printf("codex sink: %v", err)
		}
	}
	s.pending = nil
}

// codexLine accepts any plausible Codex CLI event shape and extracts the
// fields we care about. Extra fields are ignored.
type codexLine struct {
	Type      string `json:"type"`
	Model     string `json:"model"`
	Timestamp string `json:"timestamp"`
	Message   *struct {
		Model string `json:"model"`
		Usage *struct {
			InputTokens              int64 `json:"input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
	Usage *struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	} `json:"usage"`
	// Function-call shape:
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	// Some Codex versions embed under `function_call`:
	FunctionCall *struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function_call"`
	// And some under tool_call:
	ToolCall *struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"tool_call"`
}

// codexWrappedLine is the envelope used by current Codex CLI session logs
// (observed with cli_version 0.137.x): every line is wrapped as
// {"timestamp", "type": "response_item"|"event_msg"|"turn_context"|"session_meta",
// "payload": {...}} — a completely different shape from the flat `codexLine`
// form older versions (and ReadCodexSessionUsage) expect.
type codexWrappedLine struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

// codexWrappedEnvelope decides whether a line uses the wrapped envelope and, if
// so, returns it. A wrapped line is any {"type", "payload": {...}} pair — the
// known types are listed for clarity, but an unrecognized one with an object
// payload is still treated as wrapped (it no-ops downstream) rather than being
// handed to the flat parser, so a new envelope type — 0.145 added `world_state`
// — can't be misread. Flat-format lines carry no `payload` field at all.
func codexWrappedEnvelope(raw []byte) (*codexWrappedLine, bool) {
	var env codexWrappedLine
	if json.Unmarshal(raw, &env) != nil || env.Type == "" || len(env.Payload) == 0 {
		return nil, false
	}
	switch env.Type {
	case "session_meta", "response_item", "event_msg", "turn_context", "world_state":
		return &env, true
	}
	if env.Payload[0] == '{' {
		return &env, true
	}
	return nil, false
}

// processCodexLine handles one JSONL event from a Codex session. It carries
// the latest seen model on the state, buffers patch-derived events, and
// drains the buffer when a usage block arrives.
//
// Codex CLI's session-line shape has changed across versions: newer releases
// wrap every line in {"type", "payload"} (handled by processCodexWrappedLine);
// older ones emit flat {"type", "model"/"name"/"usage", ...} lines (handled
// below). We try the wrapped shape first since that's what current installs
// produce, and fall back to the flat parser otherwise.
func processCodexLine(raw []byte, st *codexState) {
	if env, ok := codexWrappedEnvelope(raw); ok {
		processCodexWrappedLine(env, st)
		return
	}

	var ln codexLine
	if err := json.Unmarshal(raw, &ln); err != nil {
		return
	}
	if ln.Model != "" {
		st.model = ln.Model
	}
	if ln.Message != nil && ln.Message.Model != "" {
		st.model = ln.Message.Model
	}

	// Function/tool-call → buffer (we don't yet know tokens for this turn).
	if name, args := pickFunctionCall(&ln); name != "" && looksLikePatch(name) {
		files := parsePatchFiles(args)
		when := parseCodexTimestamp(ln.Timestamp)
		if when.IsZero() {
			when = time.Now()
		}
		for _, f := range files {
			// Resolve symlinks so the file path matches the repo's resolved
			// root (macOS /tmp → /private/tmp is the classic foot-gun).
			abs := f.Path
			if r, err := filepath.EvalSymlinks(f.Path); err == nil {
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
			st.pending = append(st.pending, daemon.Event{
				When:         when,
				Tool:         "codex",
				Confidence:   "high",
				GenType:      st.gen(),
				RepoPath:     repo,
				WorktreePath: wt,
				FilePath:     rel,
				Model:        st.model,
				Lines:        []daemon.LineRange{{Start: f.StartLine, End: f.EndLine, ContentSHA: f.ContentSHA, ContentSHANorm: f.ContentSHANorm}},
				RawMeta:      fmt.Sprintf(`{"source":"codex_session","patch_name":%q}`, name),
			})
		}
	}

	// usage → flush buffered events with tokens, then reset.
	if u := ln.Usage; u != nil {
		st.flush(u.InputTokens, u.OutputTokens, u.CacheReadInputTokens, u.CacheCreationInputTokens, true)
	} else if ln.Message != nil && ln.Message.Usage != nil {
		u := ln.Message.Usage
		st.flush(u.InputTokens, u.OutputTokens, u.CacheReadInputTokens, u.CacheCreationInputTokens, true)
	}
}

// processCodexWrappedLine handles one line of the wrapped session-log
// envelope. Of the wrapper types, only three payload shapes carry signal we
// need:
//   - turn_context.payload.model        — tracks the active model for this turn
//   - event_msg/patch_apply_end payload — the actual file mutations (richer
//     than the legacy apply_patch function-call: absolute paths, full content
//     for adds, real unified diffs for updates — no content-sha guessing)
//   - event_msg/token_count payload     — usage to attach to buffered events
func processCodexWrappedLine(env *codexWrappedLine, st *codexState) {
	when := parseCodexTimestamp(env.Timestamp)
	if when.IsZero() {
		when = time.Now()
	}
	switch env.Type {
	case "session_meta":
		var p struct {
			Originator string `json:"originator"`
			Source     string `json:"source"`
			Cwd        string `json:"cwd"`
		}
		if json.Unmarshal(env.Payload, &p) == nil {
			st.genType = codexGenType(p.Originator, p.Source)
			if p.Cwd != "" {
				st.cwd = p.Cwd
			}
		}
	case "turn_context":
		var p struct {
			Model string `json:"model"`
			Cwd   string `json:"cwd"`
		}
		if json.Unmarshal(env.Payload, &p) == nil {
			if p.Model != "" {
				st.model = p.Model
			}
			if p.Cwd != "" {
				st.cwd = p.Cwd
			}
		}
	case "event_msg":
		var head struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(env.Payload, &head) != nil {
			return
		}
		switch head.Type {
		case "patch_apply_end":
			emitCodexPatchApplyEvents(env.Payload, when, st)
		case "token_count":
			flushCodexTokenCount(env.Payload, st)
		}
	case "response_item":
		// Codex deletes files by running a shell `rm` (exec_command), not via
		// apply_patch — those removals are otherwise invisible to this tailer
		// and fall to Human at commit time. Fingerprint each deleted file here.
		emitCodexShellDeletions(env.Payload, when, st)
	}
}

// codexShellNameList are the tool names Codex uses to run a shell command.
// shell_command is what current builds emit (observed with the VS Code surface,
// cli_version 0.142.0 — it names every PowerShell/bash exec "shell_command");
// exec_command and the rest cover other / older builds so a shell deletion is
// caught regardless of the wrapper. Ordered (not just a set) so the 0.145+
// script scan below walks candidates deterministically.
var codexShellNameList = []string{
	"shell_command",
	"exec_command",
	"local_shell_call",
	"local_shell",
	"container.exec",
	"shell",
}

// codexShellNames is codexShellNameList as a lookup set.
var codexShellNames = func() map[string]bool {
	m := make(map[string]bool, len(codexShellNameList))
	for _, n := range codexShellNameList {
		m[n] = true
	}
	return m
}()

// codexShellInvocation is one shell command recovered from a response_item,
// in whichever shape the running Codex build encodes it.
type codexShellInvocation struct {
	Cmd     string
	Workdir string
}

// codexShellInvocations extracts every shell command a response_item payload
// runs. Two encodings are supported, because Codex changed shape at 0.145:
//
//	≤0.144  {"type":"function_call","name":"shell_command",
//	         "arguments":"{\"command\":\"...\",\"workdir\":\"...\"}"}
//	≥0.145  {"type":"custom_tool_call","name":"exec",
//	         "input":"const r = await tools.shell_command({command:\"...\",
//	                  \"workdir\":\"...\"}); text(r)\n"}
//
// The newer form routes every tool through one `exec` call whose body is a
// JavaScript snippet, so the command is no longer a JSON field — it's a JS
// string literal inside source text, and one script may issue several calls.
func codexShellInvocations(payload json.RawMessage) []codexShellInvocation {
	var p struct {
		Type      string          `json:"type"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Input     json.RawMessage `json:"input"`
	}
	if json.Unmarshal(payload, &p) != nil {
		return nil
	}
	switch p.Type {
	case "function_call", "custom_tool_call", "tool_call", "local_shell_call":
	default:
		return nil
	}
	// Legacy shape: the call itself IS the shell tool and carries JSON args.
	if codexShellNames[strings.ToLower(p.Name)] {
		if inv, ok := codexShellArgsJSON(p.Arguments); ok {
			return []codexShellInvocation{inv}
		}
	}
	// 0.145+ shape: a JS body that calls tools.shell_command(...). Keyed off the
	// body's content rather than the wrapper's name ("exec" today), so a future
	// rename of the wrapper doesn't break detection again.
	src := codexJSONString(p.Input)
	if src == "" {
		src = codexJSONString(p.Arguments)
	}
	if src == "" {
		return nil
	}
	return codexExecScriptShellCalls(src)
}

// codexShellArgsJSON reads the legacy `arguments` object. Some builds encode it
// as a JSON string-of-JSON, so that is unwrapped first.
func codexShellArgsJSON(raw json.RawMessage) (codexShellInvocation, bool) {
	if len(raw) == 0 {
		return codexShellInvocation{}, false
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return codexShellInvocation{}, false
		}
		raw = json.RawMessage(s)
	}
	var args struct {
		Cmd     string `json:"cmd"`
		Command string `json:"command"`
		Workdir string `json:"workdir"`
		Cwd     string `json:"cwd"`
	}
	if json.Unmarshal(raw, &args) != nil {
		return codexShellInvocation{}, false
	}
	inv := codexShellInvocation{Cmd: args.Cmd, Workdir: args.Workdir}
	if inv.Cmd == "" {
		inv.Cmd = args.Command
	}
	if inv.Workdir == "" {
		inv.Workdir = args.Cwd
	}
	if inv.Cmd == "" {
		return codexShellInvocation{}, false
	}
	return inv, true
}

// codexJSONString returns raw decoded as a JSON string, or "" if it isn't one.
func codexJSONString(raw json.RawMessage) string {
	if len(raw) == 0 || raw[0] != '"' {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// codexExecScriptShellCalls pulls (command, workdir) out of the JavaScript body
// Codex 0.145+ hands its single `exec` tool, e.g.
//
//	const r = await tools.shell_command({command:"Remove-Item x", "workdir":"D:\\Dev"}); text(r)
//
// One script may run several commands, so every call site is scanned.
func codexExecScriptShellCalls(src string) []codexShellInvocation {
	var out []codexShellInvocation
	seen := map[codexShellInvocation]bool{}
	for _, name := range codexShellNameList {
		needle := "tools." + name
		for i := 0; i < len(src); {
			j := strings.Index(src[i:], needle)
			if j < 0 {
				break
			}
			at := i + j + len(needle)
			i = at
			// Require a call here, so needle "tools.shell" doesn't also match
			// inside "tools.shell_command" and double-report the command.
			k := at
			for k < len(src) && isJSSpaceByte(src[k]) {
				k++
			}
			if k >= len(src) || src[k] != '(' {
				continue
			}
			rest := src[k:]
			inv := codexShellInvocation{
				Cmd:     firstNonEmpty(jsObjectStringField(rest, "command"), jsObjectStringField(rest, "cmd")),
				Workdir: firstNonEmpty(jsObjectStringField(rest, "workdir"), jsObjectStringField(rest, "cwd")),
			}
			if inv.Cmd == "" || seen[inv] {
				continue
			}
			seen[inv] = true
			out = append(out, inv)
		}
	}
	return out
}

func isJSSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

func isJSWordByte(c byte) bool {
	return c == '_' || c == '$' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// jsObjectStringField finds `key: "value"` in JS object-literal source and
// returns the value with its escapes resolved. Tolerates quoted keys — Codex
// emits the mixed `{command:"...", "workdir":"..."}` form — and single, double,
// or backtick quoting. Returns "" when the key is absent or isn't a literal.
func jsObjectStringField(src, key string) string {
	for i := 0; i+len(key) <= len(src); i++ {
		if !strings.HasPrefix(src[i:], key) {
			continue
		}
		// Skip a match that's really the tail of a longer identifier, so "cmd"
		// doesn't match "cmdline" and "command" doesn't match "shell_command".
		if i > 0 {
			p := src[i-1]
			if isJSWordByte(p) {
				continue
			}
			if (p == '"' || p == '\'') && i > 1 && isJSWordByte(src[i-2]) {
				continue
			}
		}
		j := i + len(key)
		if j < len(src) && (src[j] == '"' || src[j] == '\'') {
			j++
		}
		for j < len(src) && isJSSpaceByte(src[j]) {
			j++
		}
		if j >= len(src) || src[j] != ':' {
			continue
		}
		j++
		for j < len(src) && isJSSpaceByte(src[j]) {
			j++
		}
		if j >= len(src) {
			return ""
		}
		if q := src[j]; q == '"' || q == '\'' || q == '`' {
			if v, ok := scanJSStringLiteral(src[j:]); ok {
				return v
			}
		}
	}
	return ""
}

// scanJSStringLiteral decodes the quoted JS string literal starting at src[0]
// (which must be the opening quote). Escapes matter here: a Windows path is
// written `D:\\Dev\\x` inside the JS body and has to come back as `D:\Dev\x`,
// or the deleted file's path won't resolve.
func scanJSStringLiteral(src string) (string, bool) {
	if len(src) < 2 {
		return "", false
	}
	quote := src[0]
	var b strings.Builder
	for i := 1; i < len(src); i++ {
		switch c := src[i]; {
		case c == '\\' && i+1 < len(src):
			i++
			switch src[i] {
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			default:
				b.WriteByte(src[i])
			}
		case c == quote:
			return b.String(), true
		default:
			b.WriteByte(c)
		}
	}
	return "", false
}

// emitCodexShellDeletions inspects a response_item function_call. When it's a
// shell command that deletes files (`rm`/`git rm`, or Windows `del`/`erase`/
// `Remove-Item`), it fingerprints each deleted file's HEAD content as removed
// lines and buffers a codex event — mirroring how Claude handles a `Bash rm`,
// so commit-time attribution credits Codex for the deletion instead of Human.
func emitCodexShellDeletions(payload json.RawMessage, when time.Time, st *codexState) {
	for _, inv := range codexShellInvocations(payload) {
		emitCodexShellDeletion(inv, when, st)
	}
}

// emitCodexShellDeletion handles one recovered shell command.
func emitCodexShellDeletion(inv codexShellInvocation, when time.Time, st *codexState) {
	cmd := inv.Cmd
	workdir := inv.Workdir
	if workdir == "" {
		// 0.145+ scripts may omit workdir and inherit the session's cwd.
		workdir = st.cwd
	}
	if cmd == "" || workdir == "" {
		return
	}
	for _, tok := range shellDeleteTargets(cmd) {
		abs := tok
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(workdir, tok)
		}
		root := findRepoRoot(abs, workdir)
		if root == "" {
			continue
		}
		// The file is already gone, so resolve its repo-relative name via the
		// surviving parent dir, in the root's symlink-space.
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
			continue
		}
		removed := headFileRemovedHashes(root, rel)
		if len(removed) == 0 {
			continue
		}
		repo, _ := gitutil.RepoID(rootResolved)
		if repo == "" {
			repo = root
		}
		st.pending = append(st.pending, daemon.Event{
			When:           when,
			Tool:           "codex",
			Confidence:     "high",
			GenType:        st.gen(),
			RepoPath:       repo,
			WorktreePath:   root,
			FilePath:       rel,
			Model:          st.model,
			RemovedLines:   toDaemonRemovedLines(removed),
			SuggestedLines: int64(len(removed)),
			RawMeta:        `{"source":"codex_session","shell_delete":true}`,
		})
	}
}

// emitCodexPatchApplyEvents reads a patch_apply_end payload — one entry per
// changed file, each either `{"type":"add","content":...}` (full new file
// content) or `{"type":"update","unified_diff":...}` (a real unified diff) —
// and buffers one Event per file. Buffered rather than recorded immediately
// so the trailing token_count line can attach accurate usage, mirroring the
// legacy apply_patch buffering above.
func emitCodexPatchApplyEvents(payload json.RawMessage, when time.Time, st *codexState) {
	var p struct {
		Success bool `json:"success"`
		Changes map[string]struct {
			Type        string `json:"type"`
			Content     string `json:"content"`
			UnifiedDiff string `json:"unified_diff"`
		} `json:"changes"`
	}
	if err := json.Unmarshal(payload, &p); err != nil || !p.Success {
		return
	}
	for absPath, change := range p.Changes {
		abs := absPath
		if r, err := filepath.EvalSymlinks(absPath); err == nil {
			abs = r
		}
		repo, _ := gitutil.RepoID(abs)
		wt, _ := gitutil.Toplevel(abs)
		// A deleted file is gone from disk, so RepoID/Toplevel (which stat the
		// path) fail — resolve from its still-existing parent directory instead.
		if repo == "" {
			repo, _ = gitutil.RepoID(filepath.Dir(abs))
		}
		if wt == "" {
			wt, _ = gitToplevel(filepath.Dir(abs))
		}
		rel := abs
		if wt != "" {
			if r, err := filepath.Rel(wt, abs); err == nil && !strings.HasPrefix(r, "..") {
				rel = r
			}
		}

		var lines []daemon.LineRange
		var removed []daemon.RemovedLineHash
		var suggested int64
		switch change.Type {
		case "add":
			full, err := LineRangeForWholeFile(abs)
			if err != nil || len(full) == 0 {
				continue
			}
			lines = toDaemonLineRanges(full)
			suggested = int64(len(full))
			// Codex reports a full-file OVERWRITE as an "add" (it treats writing
			// the whole file as creating it — e.g. "replace the entire file", which
			// prints "+N -0" in its own summary), even when the file already exists
			// at HEAD. The old HEAD lines it overwrote are deletions BY codex;
			// without fingerprinting them the commit falls those lines back to Human.
			// A genuinely new file isn't at HEAD, so headFileRemovedHashes returns
			// nothing — no false deletions. Commit-time matching credits only the
			// lines actually removed in the diff, so surviving lines aren't touched.
			root := wt
			if root == "" {
				root, _ = gitToplevel(filepath.Dir(abs))
			}
			if root != "" {
				removed = toDaemonRemovedLines(headFileRemovedHashes(root, rel))
			}
		case "update":
			ranges, n := UnifiedDiffAddedRanges(change.UnifiedDiff)
			removed = toDaemonRemovedLines(UnifiedDiffRemovedLineHashes(change.UnifiedDiff))
			// A pure-deletion hunk has removed lines but no added ranges. Don't
			// drop it — otherwise a Codex apply_patch that only deletes lines
			// records nothing, and the deletion falls back to Human at commit
			// time. Skip only when the hunk has neither adds nor removes.
			if len(ranges) == 0 && len(removed) == 0 {
				continue
			}
			lines = toDaemonLineRanges(ranges)
			suggested = n
		case "delete":
			// A removed file carries no added lines — fingerprint its content
			// as removed so commit-time attribution credits codex for the
			// deletion. Prefer the patch's own record of the removed text (the
			// file is already gone from disk); fall back to HEAD when the
			// payload didn't include it.
			if change.UnifiedDiff != "" {
				removed = toDaemonRemovedLines(UnifiedDiffRemovedLineHashes(change.UnifiedDiff))
			}
			if len(removed) == 0 && change.Content != "" {
				removed = toDaemonRemovedLines(RemovedLineHashes(change.Content, ""))
			}
			if len(removed) == 0 {
				root := wt
				if root == "" {
					root, _ = gitToplevel(filepath.Dir(abs))
				}
				if root != "" {
					removed = toDaemonRemovedLines(headFileRemovedHashes(root, rel))
				}
			}
			if len(removed) == 0 {
				continue
			}
			suggested = int64(len(removed))
		default:
			continue
		}

		st.pending = append(st.pending, daemon.Event{
			When:           when,
			Tool:           "codex",
			Confidence:     "high",
			GenType:        st.gen(),
			RepoPath:       repo,
			WorktreePath:   wt,
			FilePath:       rel,
			Model:          st.model,
			Lines:          lines,
			RemovedLines:   removed,
			SuggestedLines: suggested,
			RawMeta:        fmt.Sprintf(`{"source":"codex_session","patch_apply":%q}`, change.Type),
		})
	}
}

// codexTokenUsage is one `info.*_token_usage` block from an event_msg/
// token_count payload. `cached_input_tokens` maps to cache-read;
// `cache_write_input_tokens` is only present from 0.145 on, and stays 0 for
// older logs that never reported it.
type codexTokenUsage struct {
	InputTokens           int64 `json:"input_tokens"`
	CachedInputTokens     int64 `json:"cached_input_tokens"`
	CacheWriteInputTokens int64 `json:"cache_write_input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
}

// codexTokenCountPayload is the event_msg/token_count body. `last_token_usage`
// is the per-turn delta, which is what we attribute; the running total is
// ignored.
type codexTokenCountPayload struct {
	Info struct {
		LastTokenUsage codexTokenUsage `json:"last_token_usage"`
	} `json:"info"`
}

// flushCodexTokenCount flushes any buffered patch events with this turn's usage.
func flushCodexTokenCount(payload json.RawMessage, st *codexState) {
	var p codexTokenCountPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return
	}
	u := p.Info.LastTokenUsage
	if u.InputTokens == 0 && u.OutputTokens == 0 {
		return
	}
	st.flush(u.InputTokens, u.OutputTokens, u.CachedInputTokens, u.CacheWriteInputTokens, true)
}

// toDaemonLineRanges converts the local LineRange (shared with antigravity_chat.go's
// diff-walking helpers) to daemon.LineRange — identical fields, different package.
func toDaemonLineRanges(rs []LineRange) []daemon.LineRange {
	out := make([]daemon.LineRange, len(rs))
	for i, r := range rs {
		out[i] = daemon.LineRange{Start: r.Start, End: r.End, ContentSHA: r.ContentSHA, ContentSHANorm: r.ContentSHANorm}
	}
	return out
}

func pickFunctionCall(ln *codexLine) (string, json.RawMessage) {
	if ln.Name != "" && len(ln.Arguments) > 0 {
		return ln.Name, ln.Arguments
	}
	if ln.FunctionCall != nil && ln.FunctionCall.Name != "" {
		return ln.FunctionCall.Name, ln.FunctionCall.Arguments
	}
	if ln.ToolCall != nil && ln.ToolCall.Name != "" {
		return ln.ToolCall.Name, ln.ToolCall.Arguments
	}
	return "", nil
}

func looksLikePatch(name string) bool {
	n := strings.ToLower(name)
	return n == "apply_patch" || strings.Contains(n, "patch") || n == "shell" /* codex writes via shell+heredoc too */
}

// patchedFile is one file mutated by an apply_patch call.
type patchedFile struct {
	Path           string
	StartLine      int
	EndLine        int
	ContentSHA     string
	ContentSHANorm string
}

// parsePatchFiles handles three shapes the arguments may take:
//
//  1. {"input": "*** Begin Patch\n*** Update File: path/to/foo.go\n@@ ..."}
//     (the canonical Codex apply_patch envelope)
//  2. {"path": "...", "patch": "..."}
//  3. A raw JSON object containing a "file_path" or "path" field with new content.
//
// For each file, returns the post-patch line range. If the patch doesn't
// supply line numbers, we mark it as range [1, len(file)] as a worst-case
// fallback so the file is still recorded against codex.
func parsePatchFiles(raw json.RawMessage) []patchedFile {
	if len(raw) == 0 {
		return nil
	}
	body, env := patchEnvelope(raw)
	if body != "" {
		return parsePatchBody(body)
	}
	if env.Path != "" || env.File != "" {
		path := env.Path
		if path == "" {
			path = env.File
		}
		return []patchedFile{{Path: path, StartLine: 1, EndLine: 1}}
	}
	return nil
}

// patchEnvelope unwraps the several argument shapes a Codex apply_patch call
// may take (a JSON string, {"input":...}, {"patch":...}, {"path"/"file_path"},
// or a bare patch body) and returns the patch text plus the parsed envelope.
func patchEnvelope(raw json.RawMessage) (body string, env struct {
	Input string `json:"input"`
	Patch string `json:"patch"`
	Path  string `json:"path"`
	File  string `json:"file_path"`
}) {
	if len(raw) == 0 {
		return "", env
	}
	// Arguments may itself be a JSON string containing JSON. Unwrap.
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			raw = json.RawMessage(s)
		}
	}
	_ = json.Unmarshal(raw, &env)
	body = env.Input
	if body == "" {
		body = env.Patch
	}
	if body == "" && len(raw) > 0 && raw[0] != '{' {
		body = string(raw)
	}
	return body, env
}

// parsePatchDeletedFiles returns the paths a Codex apply_patch removes via
// `*** Delete File: <path>` directives. A single patch can delete files
// alongside adds/updates, so callers must check this independently of the
// add/update primary path.
func parsePatchDeletedFiles(raw json.RawMessage) []string {
	body, _ := patchEnvelope(raw)
	if body == "" {
		return nil
	}
	var out []string
	scanner := bufio.NewScanner(strings.NewReader(body))
	scanner.Buffer(make([]byte, 0, 1<<16), 1<<22)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "*** Delete File: ") {
			if p := strings.TrimSpace(strings.TrimPrefix(line, "*** Delete File: ")); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

// parsePatchBody walks a Codex/OpenAI patch envelope:
//
//	*** Begin Patch
//	*** Update File: relative/or/abs/path.go
//	@@ optional header
//	-old line
//	+new line one
//	+new line two
//	*** End Patch
//
// We record one (StartLine, EndLine) per file based on the count of added
// lines. Without true post-edit line numbers (the patch is contextual), we
// approximate by counting `+` lines; the attribute step then resolves the
// range against the post-commit file by content match.
func parsePatchBody(body string) []patchedFile {
	var out []patchedFile
	scanner := bufio.NewScanner(strings.NewReader(body))
	scanner.Buffer(make([]byte, 0, 1<<16), 1<<22)

	var curPath string
	var added []string
	flush := func() {
		if curPath == "" || len(added) == 0 {
			curPath = ""
			added = nil
			return
		}
		content := strings.Join(added, "\n")
		out = append(out, patchedFile{
			Path:           curPath,
			StartLine:      1, // anchored later via content_sha
			EndLine:        len(added),
			ContentSHA:     sha256Of(content),
			ContentSHANorm: sha256OfNorm(content),
		})
		curPath = ""
		added = nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "*** Update File: "):
			flush()
			curPath = strings.TrimPrefix(line, "*** Update File: ")
		case strings.HasPrefix(line, "*** Add File: "):
			flush()
			curPath = strings.TrimPrefix(line, "*** Add File: ")
		case strings.HasPrefix(line, "*** Delete File: "):
			flush()
			curPath = ""
		case strings.HasPrefix(line, "*** End Patch"):
			flush()
		case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
			added = append(added, strings.TrimPrefix(line, "+"))
		}
	}
	flush()
	return out
}

func sha256Of(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// sha256OfNorm is sha256Of for the whitespace-normalized text, mirroring
// content_sha_norm's blank-string convention for empty/whitespace-only input.
func sha256OfNorm(s string) string {
	norm := NormalizeLineText(s)
	if norm == "" {
		return ""
	}
	return sha256Of(norm)
}

// ReadCodexSessionUsage scans a Codex session JSONL and returns the latest
// non-empty usage block (response.complete / message.usage). Used by the
// `blamely record codex` hook so tokens land in SQLite at detection time.
func ReadCodexSessionUsage(path string) (*TranscriptUsage, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open codex session: %w", err)
	}
	defer f.Close()

	var model string
	var last *TranscriptUsage
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		// Wrapped format (0.13x and up, incl. 0.145): model comes from
		// turn_context, usage from event_msg/token_count. Without this branch the
		// flat parse below finds neither and the hook records no tokens at all.
		if env, ok := codexWrappedEnvelope(line); ok {
			switch env.Type {
			case "turn_context":
				var p struct {
					Model string `json:"model"`
				}
				if json.Unmarshal(env.Payload, &p) == nil && p.Model != "" {
					model = p.Model
				}
			case "event_msg":
				var head struct {
					Type string `json:"type"`
				}
				if json.Unmarshal(env.Payload, &head) != nil || head.Type != "token_count" {
					continue
				}
				var p codexTokenCountPayload
				if json.Unmarshal(env.Payload, &p) != nil {
					continue
				}
				u := p.Info.LastTokenUsage
				if u.InputTokens == 0 && u.OutputTokens == 0 {
					continue
				}
				last = &TranscriptUsage{
					Model:            model,
					InputTokens:      u.InputTokens,
					OutputTokens:     u.OutputTokens,
					CacheReadTokens:  u.CachedInputTokens,
					CacheWriteTokens: u.CacheWriteInputTokens,
				}
			}
			continue
		}
		var ln codexLine
		if json.Unmarshal(line, &ln) != nil {
			continue
		}
		if ln.Model != "" {
			model = ln.Model
		}
		if ln.Message != nil && ln.Message.Model != "" {
			model = ln.Message.Model
		}
		var in, out, cr, cw int64
		var ok bool
		if ln.Usage != nil {
			in, out = ln.Usage.InputTokens, ln.Usage.OutputTokens
			cr, cw = ln.Usage.CacheReadInputTokens, ln.Usage.CacheCreationInputTokens
			ok = in > 0 || out > 0
		} else if ln.Message != nil && ln.Message.Usage != nil {
			u := ln.Message.Usage
			in, out = u.InputTokens, u.OutputTokens
			cr, cw = u.CacheReadInputTokens, u.CacheCreationInputTokens
			ok = in > 0 || out > 0
		}
		if !ok {
			continue
		}
		last = &TranscriptUsage{
			Model:            model,
			InputTokens:      in,
			OutputTokens:     out,
			CacheReadTokens:  cr,
			CacheWriteTokens: cw,
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan codex session: %w", err)
	}
	return last, nil
}

func parseCodexTimestamp(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02T15:04:05Z",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// readLineGrowing reads a single newline-terminated line from r, blocking
// (with backoff) when EOF is hit so the loop tails the file as it grows.
// Stops promptly when ctx is canceled.
func readLineGrowing(ctx context.Context, r *bufio.Reader, f *os.File) ([]byte, error) {
	backoff := 100 * time.Millisecond
	const maxBackoff = 2 * time.Second
	for {
		line, err := r.ReadBytes('\n')
		if err == nil {
			return bytesTrimNewline(line), nil
		}
		if !errors.Is(err, io.EOF) {
			return nil, err
		}
		if len(line) > 0 {
			// Partial last line (no newline). Skip — wait for it to complete.
			// (Seek back so next ReadBytes sees the full line.)
			if _, sErr := f.Seek(-int64(len(line)), io.SeekCurrent); sErr != nil {
				return nil, sErr
			}
		}
		select {
		case <-ctx.Done():
			return nil, io.EOF
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func bytesTrimNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

// findRepoRootForPath resolves symlinks then runs `git -C <dir> rev-parse
// --show-toplevel`. Returns "" if the file isn't in a git repo.
func findRepoRootForPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	dir := filepath.Dir(p)
	if root, ok := gitToplevel(dir); ok {
		return root
	}
	return ""
}
