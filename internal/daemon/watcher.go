package daemon

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"path/filepath"
	"sync"
	"time"

	"github.com/blamely/blamely/internal/authorship"
	"github.com/blamely/blamely/internal/store"
)

// Watcher is a long-running, cancellable component that pushes attribution
// events. Each AI tool (Codex, Cursor, Copilot) implements this interface.
//
// Run MUST honor ctx.Done() and return promptly. It should be safe to start
// multiple Watchers in parallel under the same daemon process.
type Watcher interface {
	Name() string
	Run(ctx context.Context, sink Sink) error
}

// Sink is what a Watcher calls when it observes an AI-tagged edit.
// The daemon implementation writes through to the SQLite store.
type Sink interface {
	Record(ev Event) error
}

// Event is the normalized payload every Watcher produces. It is shaped like
// EditPayload but with parsed-out tokens and an explicit timestamp so a
// historical replay (e.g. parsing yesterday's session log on daemon startup)
// keeps the original timing.
type Event struct {
	When       time.Time
	Tool       string // claude|cursor|codex|copilot|gemini|devin
	Confidence string // high|medium|low — defaulted from Tool if blank
	// GenType describes how the edit was produced.
	// Values: chat | cli | completion | unknown
	GenType          string
	RepoPath         string
	FilePath         string // relative to repo root
	Model            string
	InputTokens      *int64
	OutputTokens     *int64
	CacheReadTokens  *int64
	CacheWriteTokens *int64
	HashBefore       string
	HashAfter        string
	RawMeta          string
	// SuggestedLines is the AI's original suggestion size at the moment the
	// watcher observed the event, before any partial-acceptance/user-editing.
	SuggestedLines int64
	Lines          []LineRange
	// RemovedLines holds content hashes of lines this edit DELETED (from
	// old_string / unified-diff "-" lines). Used at commit time to attribute
	// `type:"delete"` ranges back to this edit.
	RemovedLines []RemovedLineHash
	// Branch is the checked-out branch for this edit. Watchers usually leave it
	// empty; the sink resolves it from RepoPath. Editor-pushed events may set it.
	Branch string
}

type LineRange struct {
	Start          int
	End            int
	ContentSHA     string
	ContentSHANorm string
}

// RemovedLineHash is the content hash of a single line an edit deleted — the
// deletion-side counterpart to LineRange's ContentSHA/ContentSHANorm. Shared
// by Event.RemovedLines and EditPayload.RemovedLines (the json tags are
// inert on Event, which is never marshaled).
type RemovedLineHash struct {
	ContentSHA     string `json:"content_sha"`
	ContentSHANorm string `json:"content_sha_norm,omitempty"`
}

// dbSink writes Events directly through to the SQLite store.
type dbSink struct {
	db *store.DB
}

func (s *dbSink) Record(ev Event) error {
	// A blank repo_path means the watcher couldn't resolve the file to a git
	// repo (e.g. edits outside any worktree, or gitutil lookups that failed
	// silently). Such rows can never be matched to a project in `blamely
	// report`, so skip them rather than polluting the store with orphans.
	if ev.RepoPath == "" {
		return nil
	}
	// Forward-slash the file path so it matches git diff's paths at commit time.
	// The CodexWatcher (and other in-process watchers) build file_path with
	// filepath.Rel, which yields backslashes for nested files on Windows; git
	// uses forward slashes. No-op on Unix. Mirrors the /edit HTTP path.
	ev.FilePath = cleanRel(ev.FilePath)
	tool := store.Tool(ev.Tool)
	gt := store.GenType(ev.GenType)
	if gt == "" {
		gt = store.GenTypeUnknown
	}
	// Empty tool is only legitimate for human-typed code, which the
	// HumanEditWatcher emits as tool="" + gen_type=human. Anything else
	// with an empty tool is a watcher bug.
	if tool == "" && gt != store.GenTypeHuman {
		return fmt.Errorf("watcher sink: tool required (or use gen_type=human for empty tool)")
	}
	switch tool {
	case "",
		store.ToolClaude, store.ToolCursor, store.ToolCodex, store.ToolCopilot, store.ToolGemini, store.ToolDevin, store.ToolCopyPaste,
		store.ToolHuman: // accepted for legacy in-flight events; new emissions should use ""
	default:
		return fmt.Errorf("watcher sink: unknown tool %q", ev.Tool)
	}
	conf := store.Confidence(ev.Confidence)
	if conf == "" {
		conf = defaultConfidence(tool)
	}
	ts := ev.When
	if ts.IsZero() {
		ts = time.Now()
	}
	e := store.Edit{
		TimestampNanos: ts.UnixNano(),
		RepoPath:       ev.RepoPath,
		FilePath:       ev.FilePath,
		Tool:           tool,
		Confidence:     conf,
		GenType:        gt,
		SuggestedLines: ev.SuggestedLines,
	}
	if ev.Model != "" {
		e.Model = sql.NullString{Valid: true, String: ev.Model}
	}
	setNullInt(&e.InputTokens, ev.InputTokens)
	setNullInt(&e.OutputTokens, ev.OutputTokens)
	setNullInt(&e.CacheReadTokens, ev.CacheReadTokens)
	setNullInt(&e.CacheWriteTokens, ev.CacheWriteTokens)
	if ev.HashBefore != "" {
		e.HashBefore = sql.NullString{Valid: true, String: ev.HashBefore}
	}
	if ev.HashAfter != "" {
		e.HashAfter = sql.NullString{Valid: true, String: ev.HashAfter}
	}
	if ev.RawMeta != "" {
		e.RawMeta = sql.NullString{Valid: true, String: ev.RawMeta}
	}
	// Same chat enrichment as the HTTP path: upgrade gen_type + backfill model
	// from recent chat-session markers for copilot/cursor edits arriving via a
	// log/velocity watcher (which can't tell a chat apply from a Tab accept).
	enrichChatEdit(s.db, &e)
	for _, r := range ev.Lines {
		if r.Start <= 0 || r.End < r.Start {
			continue
		}
		e.Lines = append(e.Lines, store.EditLine{StartLine: r.Start, EndLine: r.End, ContentSHA: r.ContentSHA, ContentSHANorm: r.ContentSHANorm})
	}
	for _, rl := range ev.RemovedLines {
		if rl.ContentSHA == "" {
			continue
		}
		e.RemovedLines = append(e.RemovedLines, store.RemovedLineHash{ContentSHA: rl.ContentSHA, ContentSHANorm: rl.ContentSHANorm})
	}
	netUnchangedEditLines(&e)
	sessions.resolve(s.db, &e, ev.Branch)
	if _, err := s.db.InsertEdit(e); err != nil {
		log.Printf("watcher %q: insert edit failed file=%q: %v", ev.Tool, ev.FilePath, err)
		return err
	}
	log.Printf("watcher %q: edit gen_type=%q repo=%q file=%q lines=%d",
		tool, gt, ev.RepoPath, ev.FilePath, len(e.Lines))
	// When a chat-session marker lands, retroactively re-stamp the apply edit
	// that the editor plugin already recorded as a completion a beat earlier
	// (the chat response streams in slightly after the apply hits the file).
	if e.GenType == store.GenTypeChat && (e.Tool == store.ToolCopilot || e.Tool == store.ToolCursor) {
		if err := s.db.UpgradeRecentCompletionsToChat(e.Tool, e.TimestampNanos, chatEnrichWindowNanos); err != nil {
			log.Printf("watcher sink: upgrade completions: %v", err)
		}
	}
	captureWatcherAuthorship(ev)
	return nil
}

// captureWatcherAuthorship feeds a LIVE watcher edit into the Attribution working log, so
// tools with no hook/plugin signal (Copilot transcript, Codex/Cursor sessions,
// antigravity) are tracked. Flag-gated
// and best-effort.
//
// It is skipped for HISTORICAL replay (old events parsed on daemon startup):
// RecordEdit diffs the file's CURRENT content, which only corresponds to a recent
// edit, so attributing a stale replayed event against today's content would
// mis-credit lines. The liveness window keeps it to edits the file actually reflects.
func captureWatcherAuthorship(ev Event) {
	if !authorship.Enabled() || ev.RepoPath == "" || ev.FilePath == "" {
		return
	}
	if !ev.When.IsZero() && time.Since(ev.When) > watcherLivenessWindow {
		return // historical replay — the file no longer reflects this edit
	}
	author := authorship.Author{Type: authorship.AI, Tool: ev.Tool, GenType: ev.GenType, Model: ev.Model}
	if ev.Tool == "" || ev.GenType == "human" {
		author = authorship.HumanAuthor()
	}
	_, _ = authorship.RecordEdit(filepath.Join(ev.RepoPath, ev.FilePath), author)
}

// watcherLivenessWindow bounds how recent a watcher event must be to feed the
// working log (older = startup replay, whose stale diff would mis-attribute).
const watcherLivenessWindow = 2 * time.Minute

// runWatchers fans the configured watchers out as goroutines. Errors are
// logged but never bring the daemon down — a broken tailer for one tool
// shouldn't take out attribution for the others.
func runWatchers(ctx context.Context, db *store.DB, watchers []Watcher) {
	if len(watchers) == 0 {
		return
	}
	sink := &dbSink{db: db}
	var wg sync.WaitGroup
	for _, w := range watchers {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			log.Printf("watcher started: %s", w.Name())
			if err := w.Run(ctx, sink); err != nil && ctx.Err() == nil {
				log.Printf("watcher %s exited: %v", w.Name(), err)
			}
		}()
	}
	<-ctx.Done()
	wg.Wait()
}
