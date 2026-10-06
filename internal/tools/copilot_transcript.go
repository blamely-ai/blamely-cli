package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/blamely/blamely/internal/daemon"
	"github.com/blamely/blamely/internal/gitutil"
	"github.com/blamely/blamely/internal/store"
)

// CopilotTranscriptWatcher tails the GitHub Copilot Chat extension's own
// transcript event stream:
//
//	<workspaceStorage>/<hash>/GitHub.copilot-chat/transcripts/<id>.jsonl
//
// Unlike VS Code's built-in chatSessions store (which VS Code flushes lazily —
// sometimes minutes after the edit, so a quick commit attributes the AI's work
// to Human), the extension APPENDS to this transcript in real time as the chat
// streams. Byte-offset tailing it lets blamely record Copilot Chat edits within
// seconds, eliminating the commit-races-the-watcher lag.
//
// Edits arrive as apply_patch tool calls in the codex `*** Begin Patch` format;
// we parse them per line (content_sha-based, like every other blamely source) so
// attribution survives line drift. Tokens/model come from the surrounding
// assistant.message / tool.execution_complete events.
type CopilotTranscriptWatcher struct {
	// Roots overrides the workspaceStorage scan roots for tests.
	Roots []string
	DB    *store.DB
}

func (c *CopilotTranscriptWatcher) Name() string { return "copilot-transcript" }

// copilotTranscriptStaleCutoff bounds discovery to recently-touched transcripts
// (shares BLAMELY_CHAT_STALE_HOURS with the chat watcher).
var copilotTranscriptStaleCutoff = copilotChatStaleCutoff

func (c *CopilotTranscriptWatcher) Run(ctx context.Context, sink daemon.Sink) error {
	roots := c.Roots
	if len(roots) == 0 {
		// Copilot's transcript stream lives under the host editor's
		// workspaceStorage — and Copilot runs in VS Code, VS Code Insiders, AND
		// Cursor. Scan all of them (copilotChatSearchRoots = Code/Insiders +
		// Cursor), not just stock VS Code; otherwise Copilot-in-Cursor (common on
		// Windows) never streams. The GitHub.copilot-chat/transcripts subpath this
		// watcher looks for is Copilot's own, so it never collides with Cursor's
		// native chat (handled by CursorChatWatcher).
		roots = copilotChatSearchRoots()
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
		for _, root := range roots {
			for _, path := range findCopilotTranscriptFiles(root) {
				seen[path] = true
				if _, running := tailers[path]; running {
					continue
				}
				tCtx, cancel := context.WithCancel(ctx)
				tailers[path] = cancel
				go func(p string) {
					if err := tailCopilotTranscript(tCtx, p, sink, c.DB, c.Name()); err != nil && tCtx.Err() == nil {
						log.Printf("copilot-transcript tail %s: %v", p, err)
					}
				}(path)
			}
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

// findCopilotTranscriptFiles returns every recent
// <root>/<hash>/GitHub.copilot-chat/transcripts/*.jsonl.
func findCopilotTranscriptFiles(root string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name(), "GitHub.copilot-chat", "transcripts")
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			p := filepath.Join(dir, f.Name())
			if info, ierr := f.Info(); ierr == nil && time.Since(info.ModTime()) > copilotTranscriptStaleCutoff {
				continue
			}
			out = append(out, p)
		}
	}
	return out
}

// copilotTranscriptEvent is the tolerant view of one transcript line.
type copilotTranscriptEvent struct {
	Type string `json:"type"`
	Data struct {
		ModelID      string `json:"modelId"`
		Model        string `json:"model"`
		OutputTokens int64  `json:"outputTokens"`
		ToolRequests []struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"toolRequests"`
	} `json:"data"`
}

func tailCopilotTranscript(ctx context.Context, path string, sink daemon.Sink, db *store.DB, watcherName string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer f.Close()

	var startOffset int64
	resumed := false
	if db != nil {
		if wm, ok := db.LoadWatermark(watcherName, path); ok {
			if fi, e := f.Stat(); e == nil && wm.ByteOffset <= fi.Size() {
				if _, e := f.Seek(wm.ByteOffset, io.SeekStart); e == nil {
					startOffset = wm.ByteOffset
					resumed = true
				}
			}
		}
	}
	if !resumed {
		// Prime to end on first sight: don't replay a transcript's history (those
		// edits were already attributed via chatSessions / past commits). We only
		// want NEW appends as the chat streams live — that's the whole point of
		// reading this real-time stream.
		if end, e := f.Seek(0, io.SeekEnd); e == nil {
			startOffset = end
		}
	}
	reader := bufio.NewReaderSize(f, 1<<16)
	offset := startOffset
	savedOffset := startOffset
	var lastSave time.Time // zero → first real save (on a new append) fires immediately
	saveWM := func(force bool) {
		if db == nil || offset == savedOffset {
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
	defer saveWM(true)

	model := ""
	for {
		line, rerr := readLineGrowing(ctx, reader, f)
		if rerr != nil {
			if rerr == io.EOF || ctx.Err() != nil {
				return nil
			}
			return rerr
		}
		if pos, e := f.Seek(0, io.SeekCurrent); e == nil {
			offset = pos - int64(reader.Buffered())
		}
		if len(line) == 0 || line[0] != '{' {
			saveWM(false)
			continue
		}
		var ev copilotTranscriptEvent
		if json.Unmarshal(line, &ev) != nil {
			saveWM(false)
			continue
		}
		switch ev.Type {
		case "tool.execution_complete":
			if ev.Data.Model != "" {
				model = displayModel(ev.Data.Model)
			}
		case "assistant.message":
			if ev.Data.ModelID != "" {
				model = displayModel(ev.Data.ModelID)
			}
			hasEdit := false
			for _, tr := range ev.Data.ToolRequests {
				if looksLikePatch(tr.Name) || looksLikeCreateFile(tr.Name) ||
					looksLikeReplaceString(tr.Name) || looksLikeInsertEdit(tr.Name) {
					hasEdit = true
					break
				}
			}
			if !hasEdit {
				saveWM(false)
				continue
			}
			// The transcript carries the edits but NOT the model or token counts —
			// those live in the sibling chatSessions file. Read them on demand so
			// real-time Copilot Chat edits still report model + tokens.
			m, inTok, outTok := chatSessionModelUsage(path)
			if m == "" {
				m = model
			}
			for _, tr := range ev.Data.ToolRequests {
				switch {
				case looksLikePatch(tr.Name):
					emitCopilotPatchEdits(tr.Arguments, m, inTok, outTok, path, sink)
				case looksLikeCreateFile(tr.Name):
					// Agent mode creates brand-new files with create_file, not
					// apply_patch — without this their lines fall to Human at commit.
					emitCopilotCreateFileEdits(tr.Arguments, m, inTok, outTok, path, sink)
				case looksLikeReplaceString(tr.Name):
					// Edits to EXISTING files use replace_string_in_file (not
					// apply_patch) — the "file replace" case; without this the edit
					// isn't recorded and the changed lines fall to Human.
					emitCopilotReplaceStringEdits(tr.Arguments, m, inTok, outTok, path, sink)
				case looksLikeInsertEdit(tr.Name):
					emitCopilotInsertEditEdits(tr.Arguments, m, inTok, outTok, path, sink)
				}
				// Anything else (read_file/manage_todo_list/etc. and inline
				// completions) is never recorded here.
			}
		}
		saveWM(false)
	}
}

// emitCopilotPatchEdits parses an apply_patch tool argument (codex `*** Begin
// Patch` body, possibly double-encoded as a JSON string) and emits one chat edit
// per touched file, with per-line added content_shas and removed-line hashes.
func emitCopilotPatchEdits(args json.RawMessage, model string, inputTokens, outputTokens int64, sessionPath string, sink daemon.Sink) {
	// arguments may be a JSON string literal wrapping the real JSON.
	raw := args
	var asStr string
	if json.Unmarshal(args, &asStr) == nil && strings.TrimSpace(asStr) != "" {
		raw = json.RawMessage(asStr)
	}
	body, _ := patchEnvelope(raw)
	if body == "" {
		return
	}
	for _, fe := range parseApplyPatchPerLine(body) {
		if fe.repoPath == "" || (len(fe.added) == 0 && len(fe.removed) == 0) {
			continue
		}
		ev := daemon.Event{
			When:         time.Now(),
			Tool:         "copilot",
			Confidence:   "high",
			GenType:      "chat",
			Model:        model,
			RepoPath:     fe.repoPath,
			WorktreePath: fe.worktreePath,
			FilePath:     fe.rel,
			Lines:        fe.added,
			RemovedLines: fe.removed,
			RawMeta:      fmt.Sprintf(`{"source":"copilot_transcript","transcript_path":%q}`, sessionPath),
		}
		if inputTokens > 0 {
			v := inputTokens
			ev.InputTokens = &v
		}
		if outputTokens > 0 {
			v := outputTokens
			ev.OutputTokens = &v
		}
		if err := sink.Record(ev); err != nil {
			log.Printf("copilot-transcript sink: %v", err)
		} else {
			log.Printf("copilot-transcript: apply %s +%d -%d model=%s", fe.rel, len(fe.added), len(fe.removed), model)
		}
	}
}

// VS Code agent mode expresses file changes with several tools BESIDES apply_patch
// — create_file for new files, replace_string_in_file / insert_edit_into_file for
// edits to existing files. Each must be recognized or its lines fall to Human at
// commit. apply_patch is handled by looksLikePatch / emitCopilotPatchEdits.
func looksLikeCreateFile(name string) bool {
	n := strings.ToLower(name)
	return n == "create_file" || n == "createfile" || n == "create_new_file"
}
func looksLikeReplaceString(name string) bool {
	n := strings.ToLower(name)
	return n == "replace_string_in_file" || n == "replace_string" || n == "str_replace"
}
func looksLikeInsertEdit(name string) bool {
	n := strings.ToLower(name)
	return n == "insert_edit_into_file" || n == "insert_edit"
}

// unwrapToolArgs returns the inner JSON for a tool argument that may be a JSON
// string literal wrapping the real object (Copilot double-encodes some args).
func unwrapToolArgs(args json.RawMessage) json.RawMessage {
	var asStr string
	if json.Unmarshal(args, &asStr) == nil && strings.TrimSpace(asStr) != "" {
		return json.RawMessage(asStr)
	}
	return args
}

// copilotAddedRangesFromContent returns one per-line LineRange (with content_sha)
// for every non-blank line of content, numbered by position. Commit-time
// attribution matches by content_sha, so positions are just stable placeholders.
func copilotAddedRangesFromContent(content string) []daemon.LineRange {
	var out []daemon.LineRange
	for i, line := range strings.Split(content, "\n") {
		text := strings.TrimRight(line, "\r")
		if strings.TrimSpace(text) == "" {
			continue
		}
		out = append(out, daemon.LineRange{
			Start: i + 1, End: i + 1,
			ContentSHA: sha256Hex([]byte(text)), ContentSHANorm: sha256HexNorm(text),
		})
	}
	return out
}

// copilotRemovedHashesFromContent returns removed-line hashes for every non-blank
// line of content (the deletion-side counterpart of copilotAddedRangesFromContent).
func copilotRemovedHashesFromContent(content string) []daemon.RemovedLineHash {
	var out []daemon.RemovedLineHash
	for _, line := range strings.Split(content, "\n") {
		text := strings.TrimRight(line, "\r")
		if strings.TrimSpace(text) == "" {
			continue
		}
		out = append(out, daemon.RemovedLineHash{
			ContentSHA: sha256Hex([]byte(text)), ContentSHANorm: sha256HexNorm(text),
		})
	}
	return out
}

// emitCopilotFileEdit records ONE normalized Copilot chat edit for absPath, given
// already-parsed added/removed lines. It resolves the repo + relative path and
// attaches the turn's model/tokens. Shared by every non-apply_patch tool parser
// (create_file, replace_string_in_file, insert_edit_into_file) so they all produce
// the same normalized edit — and the daemon's common netUnchangedEditLines then
// drops any unchanged/human lines the tool re-emitted.
func emitCopilotFileEdit(absPath string, added []daemon.LineRange, removed []daemon.RemovedLineHash, model string, inputTokens, outputTokens int64, sessionPath, kind string, sink daemon.Sink) {
	if strings.TrimSpace(absPath) == "" || (len(added) == 0 && len(removed) == 0) {
		return
	}
	abs := strings.TrimSpace(absPath)
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		abs = r
	}
	repo, _ := gitutil.RepoID(abs)
	if repo == "" {
		return
	}
	rel := abs
	wt, _ := gitutil.Toplevel(abs)
	if wt != "" {
		if r, err := filepath.Rel(wt, abs); err == nil && !strings.HasPrefix(r, "..") {
			rel = r
		}
	}
	ev := daemon.Event{
		When:         time.Now(),
		Tool:         "copilot",
		Confidence:   "high",
		GenType:      "chat",
		Model:        model,
		RepoPath:     repo,
		WorktreePath: wt,
		FilePath:     rel,
		Lines:        added,
		RemovedLines: removed,
		RawMeta:      fmt.Sprintf(`{"source":"copilot_transcript","transcript_path":%q,"tool_kind":%q}`, sessionPath, kind),
	}
	if inputTokens > 0 {
		v := inputTokens
		ev.InputTokens = &v
	}
	if outputTokens > 0 {
		v := outputTokens
		ev.OutputTokens = &v
	}
	if err := sink.Record(ev); err != nil {
		log.Printf("copilot-transcript sink: %v", err)
	} else {
		log.Printf("copilot-transcript: %s %s +%d -%d model=%s", kind, rel, len(added), len(removed), model)
	}
}

// emitCopilotCreateFileEdits parses a create_file tool argument
// ({"filePath","content"}) and records every non-blank content line as a chat
// edit, so a brand-new AI file is attributed to Copilot instead of falling to Human.
func emitCopilotCreateFileEdits(args json.RawMessage, model string, inputTokens, outputTokens int64, sessionPath string, sink daemon.Sink) {
	var cf struct {
		FilePath string `json:"filePath"`
		Content  string `json:"content"`
	}
	if json.Unmarshal(unwrapToolArgs(args), &cf) != nil {
		return
	}
	emitCopilotFileEdit(cf.FilePath, copilotAddedRangesFromContent(cf.Content), nil,
		model, inputTokens, outputTokens, sessionPath, "create_file", sink)
}

// emitCopilotReplaceStringEdits parses a replace_string_in_file tool argument
// ({"filePath","oldString","newString"}) — Copilot's edit-an-existing-file tool —
// recording newString lines as added and oldString lines as removed. The daemon's
// netUnchangedEditLines then cancels the unchanged context the model re-emitted, so
// only the genuinely-changed lines are credited to the AI.
func emitCopilotReplaceStringEdits(args json.RawMessage, model string, inputTokens, outputTokens int64, sessionPath string, sink daemon.Sink) {
	var rep struct {
		FilePath  string `json:"filePath"`
		OldString string `json:"oldString"`
		NewString string `json:"newString"`
	}
	if json.Unmarshal(unwrapToolArgs(args), &rep) != nil {
		return
	}
	emitCopilotFileEdit(rep.FilePath,
		copilotAddedRangesFromContent(rep.NewString),
		copilotRemovedHashesFromContent(rep.OldString),
		model, inputTokens, outputTokens, sessionPath, "replace_string_in_file", sink)
}

// emitCopilotInsertEditEdits parses an insert_edit_into_file tool argument
// ({"filePath","code"}) — Copilot's other edit tool — recording the inserted code
// lines as added. (`code` carries only the new/changed region; any context line it
// includes that already exists won't match the file at commit, so it's harmless.)
func emitCopilotInsertEditEdits(args json.RawMessage, model string, inputTokens, outputTokens int64, sessionPath string, sink daemon.Sink) {
	var in struct {
		FilePath string `json:"filePath"`
		Code     string `json:"code"`
	}
	if json.Unmarshal(unwrapToolArgs(args), &in) != nil {
		return
	}
	emitCopilotFileEdit(in.FilePath, copilotAddedRangesFromContent(in.Code), nil,
		model, inputTokens, outputTokens, sessionPath, "insert_edit_into_file", sink)
}

// chatSessionModelUsage reads the model + latest token usage from the chatSessions
// file that pairs with a transcript (same session id, sibling directory). The
// transcript stream itself has neither, so this is the only source. Best-effort:
// returns zero values if the chatSession isn't written yet (it lags), in which
// case the edit still records in real time, just without tokens.
func chatSessionModelUsage(transcriptPath string) (model string, inputTokens, outputTokens int64) {
	// .../<hash>/GitHub.copilot-chat/transcripts/<id>.jsonl
	//   → .../<hash>/chatSessions/<id>.jsonl
	id := filepath.Base(transcriptPath)
	hashDir := filepath.Dir(filepath.Dir(filepath.Dir(transcriptPath)))
	cs, err := parseChatSession(filepath.Join(hashDir, "chatSessions", id))
	if err != nil || cs == nil {
		return "", 0, 0
	}
	model = cs.model
	for i := len(cs.requests) - 1; i >= 0; i-- {
		r := cs.requests[i]
		if r.promptTokens > 0 || r.completionTokens > 0 {
			return model, r.promptTokens, r.completionTokens
		}
	}
	return model, 0, 0
}

type copilotPatchFile struct {
	repoPath     string
	worktreePath string
	rel          string
	added        []daemon.LineRange
	removed      []daemon.RemovedLineHash
}

// parseApplyPatchPerLine parses a codex `*** Begin Patch` body into per-file,
// per-line added ranges (content_sha + norm) and removed-line hashes. Positions
// are sequential placeholders — commit-time attribution matches by content_sha,
// not line number, so the AI lines survive drift.
//
// This records the raw +/- lines as-is; unchanged lines an agent re-emits as
// matching -X/+X pairs (whole-file rewrites) are netted out centrally in
// netUnchangedEditLines at daemon ingestion, so every tool shares that rule.
// deletedFileLines returns one removed-line hash per non-blank line of the file at
// absPath — the content a `*** Delete File:` directive removes but doesn't spell out.
// It reads the working copy if it's still there, falling back to the HEAD version once
// the file has actually been removed from disk. Used so an AI file deletion is credited
// to the assistant at commit instead of defaulting to Human.
func deletedFileLines(absPath string) []daemon.RemovedLineHash {
	abs := strings.TrimSpace(absPath)
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		abs = r
	}
	data, err := os.ReadFile(abs)
	if err != nil || len(data) == 0 {
		// Already removed on disk — read the version HEAD still has.
		if wt, ok := gitutil.Toplevel(abs); ok {
			if rel, rerr := filepath.Rel(wt, abs); rerr == nil && !strings.HasPrefix(rel, "..") {
				if out, gerr := gitutil.Output(wt, "show", "HEAD:"+filepath.ToSlash(rel)); gerr == nil {
					data = out
				}
			}
		}
	}
	var removed []daemon.RemovedLineHash
	for _, line := range strings.Split(string(data), "\n") {
		text := strings.TrimRight(line, "\r")
		if strings.TrimSpace(text) == "" {
			continue
		}
		removed = append(removed, daemon.RemovedLineHash{
			ContentSHA:     sha256Hex([]byte(text)),
			ContentSHANorm: sha256HexNorm(text),
		})
	}
	return removed
}

func parseApplyPatchPerLine(body string) []copilotPatchFile {
	var out []copilotPatchFile
	var cur *copilotPatchFile
	flush := func() {
		if cur != nil && (len(cur.added) > 0 || len(cur.removed) > 0) {
			out = append(out, *cur)
		}
		cur = nil
	}
	setFile := func(p string) {
		flush()
		abs := strings.TrimSpace(p)
		if r, err := filepath.EvalSymlinks(abs); err == nil {
			abs = r
		}
		repo, _ := gitutil.RepoID(abs)
		rel := abs
		wt, _ := gitutil.Toplevel(abs)
		if wt != "" {
			if r, err := filepath.Rel(wt, abs); err == nil && !strings.HasPrefix(r, "..") {
				rel = r
			}
		}
		cur = &copilotPatchFile{repoPath: repo, worktreePath: wt, rel: rel}
	}
	addN := 0
	for _, line := range strings.Split(body, "\n") {
		switch {
		case strings.HasPrefix(line, "*** Update File: "):
			setFile(strings.TrimPrefix(line, "*** Update File: "))
		case strings.HasPrefix(line, "*** Add File: "):
			setFile(strings.TrimPrefix(line, "*** Add File: "))
		case strings.HasPrefix(line, "*** Delete File: "):
			p := strings.TrimPrefix(line, "*** Delete File: ")
			setFile(p)
			// A delete-file directive carries NO -lines — the removed content isn't in
			// the patch. Without recording it, the AI deletion has no edit and the
			// commit credits the removal to Human. Record every line of the file (read
			// from disk, or from HEAD if it's already gone) as removed, attributed to
			// the AI like any other apply_patch removal.
			if cur != nil {
				cur.removed = append(cur.removed, deletedFileLines(p)...)
			}
		case strings.HasPrefix(line, "*** End Patch"):
			flush()
		case cur == nil:
			// outside a file section
		case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
			text := strings.TrimRight(strings.TrimPrefix(line, "+"), "\r")
			if strings.TrimSpace(text) == "" {
				continue
			}
			addN++
			cur.added = append(cur.added, daemon.LineRange{
				Start: addN, End: addN,
				ContentSHA:     sha256Hex([]byte(text)),
				ContentSHANorm: sha256HexNorm(text),
			})
		case strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---"):
			text := strings.TrimRight(strings.TrimPrefix(line, "-"), "\r")
			if strings.TrimSpace(text) == "" {
				continue
			}
			cur.removed = append(cur.removed, daemon.RemovedLineHash{
				ContentSHA:     sha256Hex([]byte(text)),
				ContentSHANorm: sha256HexNorm(text),
			})
		}
	}
	flush()
	return out
}
