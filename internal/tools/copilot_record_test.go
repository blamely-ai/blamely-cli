package tools

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestExtractCopilotRanges_GenericFallback(t *testing.T) {
	// Copilot's actual payload schema isn't fully documented; the generic
	// fallback should still pick up {file_path, new_string} regardless of the
	// tool_name we get.
	raw, _ := json.Marshal(map[string]any{
		"file_path":  "/tmp/foo.go",
		"new_string": "line1\nline2\nline3\n",
	})
	p := copilotHookPayload{ToolName: "MysteryTool", ToolInput: raw}
	file, _, suggested, _, _ := extractCopilotRanges(p)
	if file != "/tmp/foo.go" {
		t.Errorf("file_path: want /tmp/foo.go, got %q", file)
	}
	if suggested != 3 {
		t.Errorf("suggested: want 3, got %d", suggested)
	}
}

func TestExtractCopilotRanges_EditShape_NarrowsToNewLinesOnly(t *testing.T) {
	// old_string "a" appears unchanged at the top of new_string. Only the
	// two genuinely-new lines ("b","c") should be credited to the AI, not
	// the unchanged context line ("a").
	raw, _ := json.Marshal(map[string]any{
		"file_path":  "/tmp/x.go",
		"old_string": "a",
		"new_string": "a\nb\nc",
	})
	p := copilotHookPayload{ToolName: "Edit", ToolInput: raw}
	file, _, suggested, _, _ := extractCopilotRanges(p)
	if file != "/tmp/x.go" {
		t.Errorf("want /tmp/x.go, got %q", file)
	}
	if suggested != 2 {
		t.Errorf("suggested: want 2 (b + c, not the unchanged 'a'), got %d", suggested)
	}
}

func TestExtractCopilotRanges_MultiEditSums(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"file_path": "/tmp/y.go",
		"edits": []map[string]any{
			{"old_string": "a", "new_string": "x\ny"}, // 2 lines
			{"old_string": "b", "new_string": "z"},    // 1 line
		},
	})
	p := copilotHookPayload{ToolName: "MultiEdit", ToolInput: raw}
	file, _, suggested, _, _ := extractCopilotRanges(p)
	if file != "/tmp/y.go" {
		t.Errorf("want /tmp/y.go, got %q", file)
	}
	if suggested != 3 {
		t.Errorf("suggested: want 3 (2+1), got %d", suggested)
	}
}

func TestExtractCopilotRanges_EditDeletion_CountsSuggested(t *testing.T) {
	// new_string is empty (deletion), old_string had 3 lines → suggested=3,
	// no ranges to locate (the text is gone post-edit).
	raw, _ := json.Marshal(map[string]any{
		"file_path":  "/tmp/d.go",
		"old_string": "a\nb\nc",
		"new_string": "",
	})
	p := copilotHookPayload{ToolName: "Edit", ToolInput: raw}
	file, ranges, suggested, _, _ := extractCopilotRanges(p)
	if file != "/tmp/d.go" {
		t.Errorf("file: want /tmp/d.go, got %q", file)
	}
	if ranges != nil {
		t.Errorf("ranges should be nil for pure deletion, got %+v", ranges)
	}
	if suggested != 3 {
		t.Errorf("suggested: want 3 (deleted lines), got %d", suggested)
	}
}

func TestExtractCopilotRanges_MultiEditMixedDeletion(t *testing.T) {
	// Two sub-edits: one adds 2 lines, one deletes 4 lines.
	raw, _ := json.Marshal(map[string]any{
		"file_path": "/tmp/m.go",
		"edits": []map[string]any{
			{"old_string": "x", "new_string": "x1\nx2"},                // +2
			{"old_string": "del1\ndel2\ndel3\ndel4", "new_string": ""}, // -4
		},
	})
	p := copilotHookPayload{ToolName: "MultiEdit", ToolInput: raw}
	_, _, suggested, _, _ := extractCopilotRanges(p)
	if suggested != 6 {
		t.Errorf("suggested: want 6 (2 added + 4 deleted), got %d", suggested)
	}
}

// A Copilot CLI deletion (str_replace_editor with old_str → empty new_str) must
// record removed-line hashes so the deletion attributes to copilot, not Human.
// Regression for commit 08c8b5b5 (CLI delete recorded lines=0 removed=0 → Human).
func TestExtractCopilotRanges_DeletionRecordsRemoved(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"path":    "/tmp/index.html",
		"old_str": "  <p>cimbom</p>\n  <p>Love you so much</p>",
		"new_str": "",
	})
	p := copilotHookPayload{ToolName: "str_replace_editor", ToolInput: raw}
	file, _, suggested, removed, _ := extractCopilotRanges(p)
	if file != "/tmp/index.html" {
		t.Fatalf("file: got %q", file)
	}
	if len(removed) != 2 {
		t.Fatalf("removed: want 2 deleted-line hashes, got %d (%+v)", len(removed), removed)
	}
	if suggested != 2 {
		t.Errorf("suggested: want 2, got %d", suggested)
	}
	// Edit shape too.
	raw2, _ := json.Marshal(map[string]any{"file_path": "/tmp/a.go", "old_string": "a\nb\nc", "new_string": ""})
	_, _, _, removed2, _ := extractCopilotRanges(copilotHookPayload{ToolName: "Edit", ToolInput: raw2})
	if len(removed2) != 3 {
		t.Fatalf("Edit deletion: want 3 removed, got %d", len(removed2))
	}
}

func TestExtractCopilotRanges_EmptyPayload(t *testing.T) {
	p := copilotHookPayload{ToolName: "Edit", ToolInput: json.RawMessage(`{}`)}
	file, ranges, suggested, _, _ := extractCopilotRanges(p)
	if file != "" || ranges != nil || suggested != 0 {
		t.Errorf("empty payload should return zero values, got (%q, %v, %d)", file, ranges, suggested)
	}
}

func TestCopilotGenType(t *testing.T) {
	cases := map[string]string{
		// The Copilot CLI hook's edits are command-line agent actions → "cli".
		"Edit":               "cli",
		"Write":              "cli",
		"Apply":              "cli",
		"str_replace_editor": "cli",
		"apply_patch":        "cli",
		// Explicit chat-panel tool names still map to chat.
		"chat-reply": "chat",
		"AskAgent":   "chat",
		"ChatPanel":  "chat",
	}
	for in, want := range cases {
		if got := copilotGenType(in); got != want {
			t.Errorf("copilotGenType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCountLines(t *testing.T) {
	cases := map[string]int{
		"":             0,
		"a":            1,
		"a\n":          1,
		"a\nb":         2,
		"a\nb\n":       2,
		"a\nb\nc":      3,
		"\n":           1, // empty line is still a line
		"a\nb\nc\nd\n": 4,
	}
	for in, want := range cases {
		if got := countLines(in); got != want {
			t.Errorf("countLines(%q) = %d, want %d", in, got, want)
		}
	}
}

// An add via the Copilot CLI "edit" tool (generic fallback) must carry per-line
// content_sha so the added lines attribute to copilot, not Human (regression for
// commit 579e3dc: 5 added lines showed Human).
func TestExtractCopilotRanges_AddCarriesPerLineSha(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"path":       "/tmp/index.html",
		"new_string": "  <p>1</p>\n  <p>2</p>\n  <p>3</p>",
	})
	_, ranges, suggested, _, _ := extractCopilotRanges(copilotHookPayload{ToolName: "edit", ToolInput: raw})
	if len(ranges) != 3 || suggested != 3 {
		t.Fatalf("want 3 ranges/suggested, got %d ranges suggested=%d", len(ranges), suggested)
	}
	for i, r := range ranges {
		if r.ContentSHA == "" || r.ContentSHANorm == "" {
			t.Fatalf("range %d missing per-line content_sha: %+v", i, r)
		}
	}
	if ranges[0].ContentSHA != sha256Hex([]byte("  <p>1</p>")) {
		t.Errorf("range[0] content_sha mismatch")
	}
}

// VS Code's agent hooks send camelCase field names. This is the create_file
// payload from a live session whose new test file committed as Human: the tool
// branch read only "path", found nothing, and the edit was never recorded.
func TestExtractCopilotRanges_VSCodeCreateFile(t *testing.T) {
	raw := json.RawMessage(`{"filePath":"c:\\Users\\dev\\git\\worker\\tests\\basic.spec.ts","content":"import { describe } from '@jest/globals';\n\ndescribe('x', () => {});\n"}`)
	file, ranges, suggested, _, _ := extractCopilotRanges(copilotHookPayload{ToolName: "create_file", ToolInput: raw})
	if file != `c:\Users\dev\git\worker\tests\basic.spec.ts` {
		t.Fatalf("file: got %q", file)
	}
	if suggested != 3 || len(ranges) != 3 {
		t.Fatalf("want suggested=3 and 3 ranges, got suggested=%d ranges=%d", suggested, len(ranges))
	}
	// The blank line carries no content_sha; the two code lines do.
	if ranges[1].ContentSHA != "" || ranges[2].ContentSHA == "" {
		t.Errorf("blank/non-blank content_sha mismatch: %+v", ranges)
	}
	if ranges[0].ContentSHA != sha256Hex([]byte("import { describe } from '@jest/globals';")) {
		t.Errorf("range[0] content_sha mismatch")
	}
}

func TestExtractCopilotRanges_VSCodeInsertEdit(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"filePath": "/tmp/a.ts", "code": "x()\ny()", "explanation": "add calls"})
	file, ranges, suggested, _, _ := extractCopilotRanges(copilotHookPayload{ToolName: "insert_edit_into_file", ToolInput: raw})
	if file != "/tmp/a.ts" || len(ranges) != 2 || suggested != 2 {
		t.Fatalf("got file=%q ranges=%d suggested=%d", file, len(ranges), suggested)
	}
}

func TestExtractCopilotRanges_VSCodeReplaceString(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"filePath":  "/tmp/r.ts",
		"oldString": "a\nb",
		"newString": "a\nB\nc",
	})
	file, ranges, suggested, removed, _ := extractCopilotRanges(copilotHookPayload{ToolName: "replace_string_in_file", ToolInput: raw})
	if file != "/tmp/r.ts" {
		t.Fatalf("file: got %q", file)
	}
	// "a" is unchanged context; only B and c are the AI's.
	if suggested != 2 || len(ranges) != 2 {
		t.Fatalf("want 2 changed lines, got suggested=%d ranges=%d", suggested, len(ranges))
	}
	if len(removed) != 1 || removed[0].ContentSHA != sha256Hex([]byte("b")) {
		t.Errorf("want removed [b], got %+v", removed)
	}
}

func TestExtractCopilotRanges_GenericFallbackCamelCase(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"filePath": "/tmp/g.ts", "oldString": "", "newString": "one\ntwo"})
	file, ranges, suggested, _, _ := extractCopilotRanges(copilotHookPayload{ToolName: "some_future_edit_tool", ToolInput: raw})
	if file != "/tmp/g.ts" || len(ranges) != 2 || suggested != 2 {
		t.Fatalf("got file=%q ranges=%d suggested=%d", file, len(ranges), suggested)
	}
}

func TestExtractCopilotMultiFileEdits_MultiReplace(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"explanation": "rename",
		"replacements": []map[string]any{
			{"filePath": "/tmp/one.ts", "oldString": "old1", "newString": "new1"},
			{"filePath": "/tmp/two.ts", "oldString": "gone1\ngone2", "newString": ""},
		},
	})
	edits, ok := extractCopilotMultiFileEdits(copilotHookPayload{ToolName: "multi_replace_string_in_file", ToolInput: raw})
	if !ok || len(edits) != 2 {
		t.Fatalf("want 2 edits, got ok=%v %d", ok, len(edits))
	}
	if edits[0].path != "/tmp/one.ts" || len(edits[0].ranges) != 1 || edits[0].suggested != 1 {
		t.Errorf("edit 0: %+v", edits[0])
	}
	// Second replacement is a pure deletion: nothing added, both removals kept.
	if edits[1].path != "/tmp/two.ts" || len(edits[1].ranges) != 0 || len(edits[1].removed) != 2 || edits[1].suggested != 2 {
		t.Errorf("edit 1: %+v", edits[1])
	}
}

func TestExtractCopilotMultiFileEdits_ApplyPatch(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.go")
	b := filepath.Join(dir, "b.go")
	patch := "*** Begin Patch\n*** Update File: " + a + "\n@@\n-old\n+new\n*** Add File: " + b + "\n+package b\n+\n+func B() {}\n*** End Patch"
	raw, _ := json.Marshal(map[string]any{"input": patch, "explanation": "edit two files"})
	edits, ok := extractCopilotMultiFileEdits(copilotHookPayload{ToolName: "apply_patch", ToolInput: raw})
	if !ok || len(edits) != 2 {
		t.Fatalf("want 2 edits, got ok=%v %d", ok, len(edits))
	}
	if edits[0].path != a || len(edits[0].ranges) != 1 || len(edits[0].removed) != 1 {
		t.Errorf("edit 0: %+v", edits[0])
	}
	if edits[1].path != b || len(edits[1].ranges) != 2 || edits[1].suggested != 2 {
		t.Errorf("edit 1: %+v", edits[1])
	}
}

func TestExtractCopilotMultiFileEdits_OtherToolsFallThrough(t *testing.T) {
	for _, name := range []string{"create_file", "replace_string_in_file", "run_in_terminal", "shell", "read_file"} {
		if _, ok := extractCopilotMultiFileEdits(copilotHookPayload{ToolName: name, ToolInput: json.RawMessage(`{"command":"ls"}`)}); ok {
			t.Errorf("%s: must fall through to the single-file path", name)
		}
	}
}

func TestCopilotHookGenType(t *testing.T) {
	vscode := `c:\Users\dev\AppData\Roaming\Code\User\workspaceStorage\7d91\GitHub.copilot-chat\transcripts\8a17.jsonl`
	if got := copilotHookGenType(copilotHookPayload{ToolName: "create_file", TranscriptPath: vscode}); got != "chat" {
		t.Errorf("VS Code hook: want chat, got %q", got)
	}
	cli := `/home/dev/.copilot/session-state/8a17/events.jsonl`
	if got := copilotHookGenType(copilotHookPayload{ToolName: "create_file", TranscriptPath: cli}); got != "cli" {
		t.Errorf("Copilot CLI hook: want cli, got %q", got)
	}
}

// A tool call that names a file but carries no content either way is a READ.
// Recording it ran captureAuthorship, which credited every uncommitted line in
// the file — including the human's — to Copilot (reproduced with the CLI's
// view tool on v1.8.4, and VS Code's read_file once filePath was understood).
func TestExtractCopilotRanges_ReadsAreNotEdits(t *testing.T) {
	reads := []struct {
		tool  string
		input map[string]any
	}{
		{"read_file", map[string]any{"filePath": "/tmp/a.ts", "startLine": 1, "endLine": 200}},
		{"list_dir", map[string]any{"path": "/tmp"}},
		{"view", map[string]any{"path": "/tmp/a.ts"}},
		{"str_replace_editor", map[string]any{"command": "view", "path": "/tmp/a.ts"}},
	}
	for _, r := range reads {
		raw, _ := json.Marshal(r.input)
		if file, _, _, _, _ := extractCopilotRanges(copilotHookPayload{ToolName: r.tool, ToolInput: raw}); file != "" {
			t.Errorf("%s: a read must not be recorded as an edit, got file %q", r.tool, file)
		}
	}
}
