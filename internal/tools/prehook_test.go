package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blamely/blamely/internal/authorship"
)

func TestIsReadOnlyToolCall(t *testing.T) {
	for _, name := range []string{"read_file", "list_dir", "view", "Read", "read_many_files", "list_directory"} {
		if !isReadOnlyToolCall(name, "") {
			t.Errorf("%s should be read-only", name)
		}
	}
	for _, name := range []string{"create_file", "replace_string_in_file", "apply_patch", "edit", "run_in_terminal", "MysteryTool"} {
		if isReadOnlyToolCall(name, "") {
			t.Errorf("%s must keep its baseline", name)
		}
	}
	if !isReadOnlyToolCall("str_replace_editor", "view") || isReadOnlyToolCall("str_replace_editor", "str_replace") {
		t.Errorf("str_replace_editor: view reads, str_replace writes")
	}
}

func TestMultiFileTargets(t *testing.T) {
	patch := "*** Begin Patch\n*** Update File: /r/a.go\n@@\n-x\n+y\n*** Add File: /r/new.go\n+z\n*** Delete File: /r/old.go\n*** End Patch"
	raw, _ := json.Marshal(map[string]any{"input": patch})
	got := multiFileTargets("apply_patch", raw)
	if strings.Join(got, ",") != "/r/a.go,/r/old.go" {
		t.Errorf("apply_patch targets: got %v (an Add File has nothing to snapshot)", got)
	}
	raw, _ = json.Marshal(map[string]any{"replacements": []map[string]string{
		{"filePath": "/r/a.ts"}, {"filePath": "/r/b.ts"}, {"filePath": "/r/a.ts"},
	}})
	got = multiFileTargets("multi_replace_string_in_file", raw)
	if strings.Join(got, ",") != "/r/a.ts,/r/b.ts" {
		t.Errorf("multi_replace targets: got %v", got)
	}
	if multiFileTargets("create_file", raw) != nil {
		t.Errorf("single-file tools have no multi-file targets")
	}
	// VS Code's agent host: tool_name "Edit", tool_input the bare patch string.
	raw, _ = json.Marshal(patch)
	got = multiFileTargets("Edit", raw)
	if strings.Join(got, ",") != "/r/a.go,/r/old.go" {
		t.Errorf("agent-host Edit patch targets: got %v", got)
	}
}

// committedRepoWithEdit returns a repo whose committed a.ts the human has since
// extended with an uncommitted line, and the file's absolute path.
func committedRepoWithEdit(t *testing.T) (string, string) {
	t.Helper()
	root := initRepoWithFile(t, "a.ts", "committed\n")
	abs := filepath.Join(root, "a.ts")
	if err := os.WriteFile(abs, []byte("committed\nhuman, uncommitted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, abs
}

func storedBaseline(t *testing.T, abs string) (string, bool) {
	t.Helper()
	ctx, ok := authorship.ResolveContext(abs)
	if !ok {
		t.Fatalf("no context for %s", abs)
	}
	data, err := os.ReadFile(authorship.BaselinePath(ctx.RepoRoot, ctx.Branch, ctx.BaseSHA, ctx.RelPath))
	return string(data), err == nil
}

// The pre-hook used to find no path in a multi_replace_string_in_file call, so
// its first record diffed against HEAD and claimed the human's uncommitted line.
func TestCaptureBaselineFromStdin_MultiReplaceSnapshotsEachFile(t *testing.T) {
	root, abs := committedRepoWithEdit(t)
	payload, _ := json.Marshal(map[string]any{
		"tool_name": "multi_replace_string_in_file",
		"cwd":       root,
		"tool_input": map[string]any{"replacements": []map[string]string{
			{"filePath": abs, "oldString": "committed", "newString": "changed"},
		}},
	})
	if err := CaptureBaselineFromStdin(strings.NewReader(string(payload))); err != nil {
		t.Fatal(err)
	}
	got, ok := storedBaseline(t, abs)
	if !ok || !strings.Contains(got, "human, uncommitted") {
		t.Fatalf("want a baseline holding the human's line, got ok=%v %q", ok, got)
	}
}

// The agent host's pre-hook names an apply_patch "Edit" and passes the bare
// patch: without a baseline the record diffs against HEAD and claims the
// human's uncommitted line for Copilot.
func TestCaptureBaselineFromStdin_AgentHostEditPatchSnapshotsEachFile(t *testing.T) {
	root, abs := committedRepoWithEdit(t)
	payload, _ := json.Marshal(map[string]any{
		"hook_event_name": "PreToolUse",
		"tool_name":       "Edit",
		"cwd":             root,
		"tool_input":      "*** Begin Patch\n*** Update File: " + abs + "\n@@\n committed\n+agent line\n*** End Patch\n",
	})
	if err := CaptureBaselineFromStdin(strings.NewReader(string(payload))); err != nil {
		t.Fatal(err)
	}
	got, ok := storedBaseline(t, abs)
	if !ok || !strings.Contains(got, "human, uncommitted") {
		t.Fatalf("want a baseline holding the human's line, got ok=%v %q", ok, got)
	}
}

// A tracked file the user then edited without anything recording it (the editor
// plugin missed it, or wrote it under an alias path): the agent's pre-hook must
// credit that line to the human before the agent writes, or the agent's record
// diffs from the stale log and claims it.
func TestCaptureBaselineFromStdin_TrackedFileUnobservedLinesStayHuman(t *testing.T) {
	root := initRepoWithFile(t, "a.ts", "committed\n")
	abs := filepath.Join(root, "a.ts")
	ctx, ok := authorship.ResolveContext(abs)
	if !ok {
		t.Fatalf("no context for %s", abs)
	}
	// Tracked: the post-commit seed leaves a log describing the committed content.
	if err := authorship.SeedWorkingLog(ctx.RepoRoot, ctx.Branch, ctx.BaseSHA, ctx.RelPath, "committed\n",
		[]authorship.LineAttribution{{Start: 1, End: 1, Author: authorship.HumanAuthor()}}, 0); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte("committed\nby hand\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{
		"tool_name":  "Edit",
		"cwd":        root,
		"tool_input": "*** Begin Patch\n*** Update File: " + abs + "\n@@\n by hand\n+by agent\n*** End Patch\n",
	})
	if err := CaptureBaselineFromStdin(strings.NewReader(string(payload))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte("committed\nby hand\nby agent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	captureAuthorship(root, "a.ts", "copilot", "cli", "")

	got, ok := authorship.AuthorTypesForFile(ctx.RepoRoot, ctx.Branch, ctx.BaseSHA, ctx.RelPath)
	if !ok {
		t.Fatal("no working log")
	}
	if got[2] != authorship.Human || got[3] != authorship.AI {
		t.Errorf("line 2 (by hand) = %v, line 3 (by agent) = %v; want Human, AI", got[2], got[3])
	}
}

func TestCaptureBaselineFromStdin_ReadTakesNoBaseline(t *testing.T) {
	root, abs := committedRepoWithEdit(t)
	payload, _ := json.Marshal(map[string]any{
		"tool_name":  "read_file",
		"cwd":        root,
		"tool_input": map[string]any{"filePath": abs, "startLine": 1, "endLine": 50},
	})
	if err := CaptureBaselineFromStdin(strings.NewReader(string(payload))); err != nil {
		t.Fatal(err)
	}
	if _, ok := storedBaseline(t, abs); ok {
		t.Fatalf("a read must not take a baseline")
	}
}
