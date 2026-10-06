package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestParseApplyPatchPerLine(t *testing.T) {
	repo := gitInitRepo(t)
	target := filepath.Join(repo, "index.html")
	if err := os.WriteFile(target, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	body := "*** Begin Patch\n*** Update File: " + target + "\n@@\n" +
		"-  <p>old1</p>\n-  <p>old2</p>\n" +
		"+  <p>new1</p>\n+  <p>new2</p>\n+  <p>new3</p>\n" +
		"*** End Patch"
	files := parseApplyPatchPerLine(body)
	if len(files) != 1 {
		t.Fatalf("want 1 file, got %d", len(files))
	}
	f := files[0]
	if f.rel != "index.html" {
		t.Errorf("rel: got %q", f.rel)
	}
	if len(f.added) != 3 {
		t.Errorf("added: want 3, got %d", len(f.added))
	}
	if len(f.removed) != 2 {
		t.Errorf("removed: want 2, got %d", len(f.removed))
	}
	// per-line content_sha present + matches the daemon convention
	if f.added[0].ContentSHA != sha256Hex([]byte("  <p>new1</p>")) {
		t.Errorf("added[0] content_sha mismatch")
	}
	if f.removed[0].ContentSHA != sha256Hex([]byte("  <p>old1</p>")) {
		t.Errorf("removed[0] content_sha mismatch")
	}
}

// emit path: arguments double-encoded as a JSON string wrapping {"input": body}.
func TestEmitCopilotPatchEdits_DoubleEncodedArgs(t *testing.T) {
	repo := gitInitRepo(t)
	target := filepath.Join(repo, "a.go")
	if err := os.WriteFile(target, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	body := "*** Begin Patch\n*** Update File: " + target + "\n@@\n-old\n+new\n*** End Patch"
	inner, _ := json.Marshal(map[string]string{"input": body})
	args, _ := json.Marshal(string(inner)) // double-encode (JSON string literal)

	sink := &mockSink{}
	emitCopilotPatchEdits(json.RawMessage(args), "gpt-5-mini", 100, 42, "/tmp/t.jsonl", sink)
	if len(sink.events) != 1 {
		t.Fatalf("want 1 emitted edit, got %d", len(sink.events))
	}
	e := sink.events[0]
	if e.Tool != "copilot" || e.GenType != "chat" || e.Model != "gpt-5-mini" {
		t.Errorf("event meta wrong: %+v", e)
	}
	if e.FilePath != "a.go" || len(e.Lines) != 1 || len(e.RemovedLines) != 1 {
		t.Errorf("event content wrong: file=%s added=%d removed=%d", e.FilePath, len(e.Lines), len(e.RemovedLines))
	}
	if e.OutputTokens == nil || *e.OutputTokens != 42 {
		t.Errorf("output tokens not set")
	}
	if e.InputTokens == nil || *e.InputTokens != 100 {
		t.Errorf("input tokens not set")
	}
}

// Agent mode creates brand-new files with create_file (not apply_patch); without
// handling it the file's lines fall to Human at commit. Repro from the field
// (scenerio_122_coilot.txt): a create_file of 5 "hello1, hello2" lines must emit
// a 5-line copilot chat edit.
func TestEmitCopilotCreateFileEdits_DoubleEncodedArgs(t *testing.T) {
	repo := gitInitRepo(t)
	target := filepath.Join(repo, "scenerio_122_coilot.txt")
	content := "hello1, hello2\nhello1, hello2\nhello1, hello2\nhello1, hello2\nhello1, hello2\n"
	if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	inner, _ := json.Marshal(map[string]string{"filePath": target, "content": content})
	args, _ := json.Marshal(string(inner)) // double-encode (JSON string literal)

	sink := &mockSink{}
	emitCopilotCreateFileEdits(json.RawMessage(args), "gpt-5-mini", 100, 42, "/tmp/t.jsonl", sink)
	if len(sink.events) != 1 {
		t.Fatalf("want 1 emitted edit, got %d", len(sink.events))
	}
	e := sink.events[0]
	if e.Tool != "copilot" || e.GenType != "chat" || e.Model != "gpt-5-mini" {
		t.Errorf("event meta wrong: %+v", e)
	}
	if e.FilePath != "scenerio_122_coilot.txt" || len(e.Lines) != 5 {
		t.Errorf("event content wrong: file=%s added=%d (want 5)", e.FilePath, len(e.Lines))
	}
	if e.InputTokens == nil || *e.InputTokens != 100 || e.OutputTokens == nil || *e.OutputTokens != 42 {
		t.Errorf("tokens not set")
	}
}

// Blank lines in created content are skipped but real line numbers are preserved,
// so the AI block at lines 3-7 (two blanks/non-AI lines above) still attributes.
func TestEmitCopilotCreateFileEdits_SkipsBlanksKeepsLineNumbers(t *testing.T) {
	repo := gitInitRepo(t)
	target := filepath.Join(repo, "f.txt")
	// line1 text, line2 blank, line3 text → expect Start positions 1 and 3.
	content := "a\n\nb\n"
	if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	inner, _ := json.Marshal(map[string]string{"filePath": target, "content": content})
	args, _ := json.Marshal(string(inner))

	sink := &mockSink{}
	emitCopilotCreateFileEdits(json.RawMessage(args), "gpt-5-mini", 0, 0, "/tmp/t.jsonl", sink)
	if len(sink.events) != 1 || len(sink.events[0].Lines) != 2 {
		t.Fatalf("want 1 event with 2 lines, got %+v", sink.events)
	}
	got := []int{sink.events[0].Lines[0].Start, sink.events[0].Lines[1].Start}
	if got[0] != 1 || got[1] != 3 {
		t.Errorf("line numbers wrong: got %v, want [1 3]", got)
	}
}

// Copilot edits an EXISTING file with replace_string_in_file (not apply_patch) —
// the "file replace" case. It must record newString as added + oldString as
// removed so the daemon's netting keeps only genuinely-changed lines as AI.
func TestEmitCopilotReplaceStringEdits(t *testing.T) {
	repo := gitInitRepo(t)
	target := filepath.Join(repo, "login.html")
	if err := os.WriteFile(target, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	inner, _ := json.Marshal(map[string]string{
		"filePath":  target,
		"oldString": "<button>Login</button>",
		"newString": "<button>Login</button>\n<button>Google</button>",
	})
	args, _ := json.Marshal(string(inner)) // double-encoded JSON string

	sink := &mockSink{}
	emitCopilotReplaceStringEdits(json.RawMessage(args), "gpt-5-mini", 10, 5, "/tmp/t.jsonl", sink)
	if len(sink.events) != 1 {
		t.Fatalf("want 1 emitted edit, got %d", len(sink.events))
	}
	e := sink.events[0]
	if e.Tool != "copilot" || e.GenType != "chat" || e.FilePath != "login.html" {
		t.Errorf("event meta wrong: %+v", e)
	}
	if len(e.Lines) != 2 || len(e.RemovedLines) != 1 {
		t.Errorf("want 2 added + 1 removed, got %d added %d removed", len(e.Lines), len(e.RemovedLines))
	}
}

// insert_edit_into_file ({"filePath","code"}) records the inserted code as added.
func TestEmitCopilotInsertEditEdits(t *testing.T) {
	repo := gitInitRepo(t)
	target := filepath.Join(repo, "app.js")
	if err := os.WriteFile(target, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	inner, _ := json.Marshal(map[string]string{"filePath": target, "code": "const a = 1;\nconst b = 2;"})
	args, _ := json.Marshal(string(inner))

	sink := &mockSink{}
	emitCopilotInsertEditEdits(json.RawMessage(args), "gpt-5-mini", 0, 0, "/tmp/t.jsonl", sink)
	if len(sink.events) != 1 || len(sink.events[0].Lines) != 2 {
		t.Fatalf("want 1 event with 2 added lines, got %+v", sink.events)
	}
}

// multi_replace_string_in_file carries several replacements, possibly across
// files; each must be recorded against its own file (it was never recognized,
// so every such edit fell to Human).
func TestEmitCopilotMultiReplaceStringEdits(t *testing.T) {
	repo := gitInitRepo(t)
	a := filepath.Join(repo, "a.ts")
	b := filepath.Join(repo, "b.ts")
	for _, f := range []string{a, b} {
		if err := os.WriteFile(f, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	inner, _ := json.Marshal(map[string]any{
		"explanation": "rename across files",
		"replacements": []map[string]string{
			{"filePath": a, "oldString": "oldName()", "newString": "newName()"},
			{"filePath": b, "oldString": "import { oldName }", "newString": "import { newName }\nnewName()"},
		},
	})
	args, _ := json.Marshal(string(inner)) // double-encoded JSON string

	sink := &mockSink{}
	emitCopilotMultiReplaceStringEdits(json.RawMessage(args), "gpt-5-mini", 0, 0, "/tmp/t.jsonl", sink)
	if len(sink.events) != 2 {
		t.Fatalf("want 2 emitted edits, got %d", len(sink.events))
	}
	if sink.events[0].FilePath != "a.ts" || len(sink.events[0].Lines) != 1 || len(sink.events[0].RemovedLines) != 1 {
		t.Errorf("edit 0 wrong: %+v", sink.events[0])
	}
	if sink.events[1].FilePath != "b.ts" || len(sink.events[1].Lines) != 2 || len(sink.events[1].RemovedLines) != 1 {
		t.Errorf("edit 1 wrong: %+v", sink.events[1])
	}
	if !looksLikeMultiReplaceString("multi_replace_string_in_file") || looksLikeReplaceString("multi_replace_string_in_file") {
		t.Errorf("tool-name routing wrong")
	}
}
