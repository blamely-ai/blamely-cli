package daemon

import (
	"testing"
	"time"

	"github.com/blamely/blamely/internal/store"
)

const (
	dupRepo = "/tmp/dup-repo"
	dupFile = "tests/basic.spec.ts"
)

func dupLines() []LineRange {
	return []LineRange{
		{Start: 1, End: 1, ContentSHA: "sha-a", ContentSHANorm: "norm-a"},
		{Start: 3, End: 3, ContentSHA: "sha-b", ContentSHANorm: "norm-b"},
	}
}

// hookPayload is the agent hook's report: real positions, a blank line with no
// content_sha in between, no model (VS Code's chatSessions file can lag).
func hookPayload() EditPayload {
	return EditPayload{
		Tool: "copilot", Confidence: "high", GenType: "chat",
		RepoPath: dupRepo, FilePath: dupFile, SuggestedLines: 3,
		Lines: []Range{
			{Start: 1, End: 1, ContentSHA: "sha-a", ContentSHANorm: "norm-a"},
			{Start: 2, End: 2},
			{Start: 3, End: 3, ContentSHA: "sha-b", ContentSHANorm: "norm-b"},
		},
		RawMeta: `{"session_id":"s1","tool":"create_file","source":"copilot_hook"}`,
	}
}

// transcriptEvent is the transcript watcher's report of the same edit, with the
// model and token counts the hook lacked.
func transcriptEvent(when time.Time) Event {
	in, out := int64(1200), int64(340)
	return Event{
		When: when, Tool: "copilot", Confidence: "high", GenType: "chat",
		RepoPath: dupRepo, FilePath: dupFile, Model: "gpt-5-mini",
		InputTokens: &in, OutputTokens: &out,
		Lines:   dupLines(),
		RawMeta: `{"source":"copilot_transcript","tool_kind":"create_file"}`,
	}
}

func copilotEdits(t *testing.T, db *store.DB) []store.Edit {
	t.Helper()
	edits, err := db.EditsForFileSince(dupRepo, dupFile, 0)
	if err != nil {
		t.Fatal(err)
	}
	return edits
}

func TestCopilotDedupe_HookThenTranscript_MergesUsage(t *testing.T) {
	db := openTestDB(t)
	if err := validateAndStore(db, hookPayload()); err != nil {
		t.Fatal(err)
	}
	if err := (&dbSink{db: db}).Record(transcriptEvent(time.Now())); err != nil {
		t.Fatal(err)
	}
	edits := copilotEdits(t, db)
	if len(edits) != 1 {
		t.Fatalf("want the transcript report merged into the hook's row, got %d rows", len(edits))
	}
	e := edits[0]
	if e.Model.String != "gpt-5-mini" || e.InputTokens.Int64 != 1200 || e.OutputTokens.Int64 != 340 {
		t.Errorf("model/tokens not folded in: model=%v in=%v out=%v", e.Model, e.InputTokens, e.OutputTokens)
	}
	if e.SuggestedLines != 3 {
		t.Errorf("suggested_lines must keep the larger value 3, got %d", e.SuggestedLines)
	}
}

func TestCopilotDedupe_TranscriptThenHook(t *testing.T) {
	db := openTestDB(t)
	if err := (&dbSink{db: db}).Record(transcriptEvent(time.Now())); err != nil {
		t.Fatal(err)
	}
	if err := validateAndStore(db, hookPayload()); err != nil {
		t.Fatal(err)
	}
	edits := copilotEdits(t, db)
	if len(edits) != 1 {
		t.Fatalf("want 1 row, got %d", len(edits))
	}
	if edits[0].Model.String != "gpt-5-mini" {
		t.Errorf("the transcript's model must survive, got %v", edits[0].Model)
	}
}

// Two reports from the SAME recorder are two real edits (the agent wrote the
// same content twice), never merged.
func TestCopilotDedupe_SameSourceKeepsBoth(t *testing.T) {
	db := openTestDB(t)
	for i := 0; i < 2; i++ {
		if err := validateAndStore(db, hookPayload()); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(copilotEdits(t, db)); n != 2 {
		t.Fatalf("want 2 rows, got %d", n)
	}
}

func TestCopilotDedupe_DifferentLinesKeepsBoth(t *testing.T) {
	db := openTestDB(t)
	if err := validateAndStore(db, hookPayload()); err != nil {
		t.Fatal(err)
	}
	ev := transcriptEvent(time.Now())
	ev.Lines = ev.Lines[:1]
	if err := (&dbSink{db: db}).Record(ev); err != nil {
		t.Fatal(err)
	}
	if n := len(copilotEdits(t, db)); n != 2 {
		t.Fatalf("want 2 rows, got %d", n)
	}
}

func TestCopilotDedupe_OutsideWindowKeepsBoth(t *testing.T) {
	db := openTestDB(t)
	old := time.Now().Add(-time.Duration(copilotDupWindowNanos) - time.Minute)
	if err := (&dbSink{db: db}).Record(transcriptEvent(old)); err != nil {
		t.Fatal(err)
	}
	if err := validateAndStore(db, hookPayload()); err != nil {
		t.Fatal(err)
	}
	if n := len(copilotEdits(t, db)); n != 2 {
		t.Fatalf("want 2 rows, got %d", n)
	}
}
