package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func devinInput(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// A `write` carries the whole new file, so extractDevinRanges must hand back
// the full content — RecordDevinFromStdin uses it to ask the daemon for the
// previous snapshot and compute removed-line hashes.
func TestExtractDevinRanges_WriteReturnsFullContent(t *testing.T) {
	p := devinHookPayload{
		ToolName:  "write",
		ToolInput: devinInput(t, map[string]any{"path": "/tmp/does-not-exist.go", "content": "package main\n"}),
	}
	path, _, suggested, _, full := extractDevinRanges(p, "")
	if path != "/tmp/does-not-exist.go" {
		t.Errorf("path = %q", path)
	}
	if full == nil || *full != "package main\n" {
		t.Errorf("full content: want %q, got %v", "package main\n", full)
	}
	if suggested != 1 {
		t.Errorf("suggested = %d, want 1", suggested)
	}
}

// An old/new pair is a targeted replacement, not a whole-file write: the full
// content must stay nil so ResolveWholeFileWrite is never consulted.
func TestExtractDevinRanges_EditReturnsNilFullContent(t *testing.T) {
	p := devinHookPayload{
		ToolName:  "edit",
		ToolInput: devinInput(t, map[string]any{"path": "/tmp/x.go", "old_string": "a\nb\nc", "new_string": ""}),
	}
	_, _, _, removed, full := extractDevinRanges(p, "")
	if full != nil {
		t.Errorf("full content should be nil for an edit, got %q", *full)
	}
	if len(removed) == 0 {
		t.Error("expected removed-line hashes for a pure deletion")
	}
}

// Devin documents {path, content} but its Claude-compatible builds send
// {file_path, ...}. Both must resolve.
func TestExtractDevinRanges_AcceptsFilePathAlias(t *testing.T) {
	p := devinHookPayload{
		ToolName:  "write",
		ToolInput: devinInput(t, map[string]any{"file_path": "/tmp/aliased.go", "content": "x\n"}),
	}
	path, _, _, _, _ := extractDevinRanges(p, "")
	if path != "/tmp/aliased.go" {
		t.Errorf("file_path alias not honoured: got %q", path)
	}
}

// Devin sends no cwd, so a relative tool path must be resolved against the
// session cwd — otherwise it would be read against whatever directory the hook
// process happens to run in.
func TestExtractDevinRanges_ResolvesRelativePathAgainstCwd(t *testing.T) {
	dir := t.TempDir()
	p := devinHookPayload{
		ToolName:  "write",
		ToolInput: devinInput(t, map[string]any{"path": "src/main.go", "content": "package main\n"}),
	}
	path, _, _, _, _ := extractDevinRanges(p, dir)
	if want := filepath.Join(dir, "src/main.go"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
}

// A path with no content at all (a read, or an unknown tool shape) must not be
// reported as a whole-file write — doing so would tell the daemon the file was
// emptied and mark every existing line as removed.
func TestExtractDevinRanges_PathWithoutContentIsNotAWrite(t *testing.T) {
	p := devinHookPayload{
		ToolName:  "read",
		ToolInput: devinInput(t, map[string]any{"path": "/tmp/x.go"}),
	}
	_, ranges, suggested, _, full := extractDevinRanges(p, "")
	if full != nil {
		t.Errorf("expected nil full content, got %q", *full)
	}
	if ranges != nil || suggested != 0 {
		t.Errorf("expected no ranges/suggestion, got %v / %d", ranges, suggested)
	}
}

func TestExtractDevinRanges_NarrowsToChangedLines(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "main.go")
	// PostToolUse fires AFTER the write, so the file on disk already holds the
	// post-edit content — that is what LocateNewString has to match against.
	if err := os.WriteFile(file, []byte("package main\n\nfunc main() {\n\tprintln(\"hello\")\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := devinHookPayload{
		ToolName: "edit",
		ToolInput: devinInput(t, map[string]any{
			"path":       file,
			"old_string": "\tprintln(\"hi\")\n",
			"new_string": "\tprintln(\"hello\")\n",
		}),
	}
	_, ranges, _, removed, full := extractDevinRanges(p, "")
	if full != nil {
		t.Error("edit must not be treated as a whole-file write")
	}
	if len(ranges) == 0 {
		t.Fatal("expected the replacement to be located in the file")
	}
	if ranges[0].Start != 4 || ranges[0].End != 4 {
		t.Errorf("range = %d-%d, want 4-4", ranges[0].Start, ranges[0].End)
	}
	if len(removed) == 0 {
		t.Error("expected the replaced line to be fingerprinted as removed")
	}
}

func TestDevinToolFailed(t *testing.T) {
	for _, tc := range []struct {
		name string
		resp string
		want bool
	}{
		{"absent response is success", "", false},
		{"explicit success", `{"success":true,"output":"ok"}`, false},
		{"explicit failure", `{"success":false,"error":"permission denied"}`, true},
		{"unparseable is success", `not json`, false},
		{"missing success field is success", `{"output":"ok"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := devinToolFailed(json.RawMessage(tc.resp)); got != tc.want {
				t.Errorf("devinToolFailed(%s) = %v, want %v", tc.resp, got, tc.want)
			}
		})
	}
}

// Devin reads user-level hooks out of ~/.claude/settings.json too, so a payload
// arriving at `record claude` may actually be Devin's. These cases pin the
// discriminator down in both directions.
func TestIsDevinHookPayload(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want bool
	}{
		{
			"devin write",
			`{"hook_event_name":"PostToolUse","tool_name":"write","tool_input":{"path":"/a.go"},"session_id":"s","prompt_id":"p"}`,
			true,
		},
		{
			"devin exec",
			`{"tool_name":"exec","tool_input":{"command":"rm x"},"session_id":"s","prompt_id":"p"}`,
			true,
		},
		{
			"claude code has no prompt_id",
			`{"hook_event_name":"PostToolUse","tool_name":"Write","tool_input":{"file_path":"/a.go"},"session_id":"s","transcript_path":"/t.jsonl","cwd":"/repo"}`,
			false,
		},
		{
			"cursor is never devin",
			`{"tool_name":"write","tool_input":{"path":"/a.go"},"prompt_id":"p","cursor_version":"3.10.20"}`,
			false,
		},
		{
			"a transcript_path rules devin out",
			`{"tool_name":"write","tool_input":{"path":"/a.go"},"prompt_id":"p","transcript_path":"/t.jsonl"}`,
			false,
		},
		{
			"unknown tool name with a prompt_id is not rerouted",
			`{"tool_name":"SomethingElse","tool_input":{},"prompt_id":"p"}`,
			false,
		},
		{"garbage", `not json`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsDevinHookPayload([]byte(tc.raw)); got != tc.want {
				t.Errorf("IsDevinHookPayload = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDevinAbsPath(t *testing.T) {
	abs := filepath.Join(string(filepath.Separator), "tmp", "a.go")
	for _, tc := range []struct {
		name, path, cwd, want string
	}{
		{"absolute is untouched", abs, "/repo", abs},
		{"relative joins cwd", "src/a.go", "/repo", filepath.Join("/repo", "src/a.go")},
		{"no cwd leaves it alone", "src/a.go", "", "src/a.go"},
		{"empty path stays empty", "", "/repo", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := devinAbsPath(tc.path, tc.cwd); got != tc.want {
				t.Errorf("devinAbsPath(%q, %q) = %q, want %q", tc.path, tc.cwd, got, tc.want)
			}
		})
	}
}
