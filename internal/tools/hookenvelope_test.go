package tools

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/blamely/blamely/internal/authorship"
	"github.com/blamely/blamely/internal/hookdrift"
)

// isolateHome points HOME at a temp dir so a test never posts to (or reads the
// drift tally of) a Blamely daemon running on the developer's machine.
func isolateHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseHookEnvelope_Spellings(t *testing.T) {
	cases := []struct {
		name, payload    string
		wantTool, wantID string
		wantPath         string
	}{
		{
			name:     "anthropic snake_case",
			payload:  `{"session_id":"s1","cwd":"/r","tool_name":"Edit","tool_input":{"file_path":"/r/a.go"}}`,
			wantTool: "Edit", wantID: "s1", wantPath: "/r/a.go",
		},
		{
			name:     "copilot agent runtime camelCase",
			payload:  `{"sessionId":"s2","cwd":"/r","timestamp":1,"toolName":"edit","toolArgs":{"path":"/r/a.go"},"toolResult":{"resultType":"success"}}`,
			wantTool: "edit", wantID: "s2", wantPath: "/r/a.go",
		},
		{
			name:     "arguments JSON-encoded in a string",
			payload:  `{"sessionId":"s3","toolName":"edit","toolArgs":"{\"path\":\"/r/a.go\"}"}`,
			wantTool: "edit", wantID: "s3", wantPath: "/r/a.go",
		},
		{
			name:     "batch of one is lifted",
			payload:  `{"sessionId":"s4","cwd":"/r","toolCalls":[{"id":"x","name":"edit","args":"{\"path\":\"/r/a.go\"}"}]}`,
			wantTool: "edit", wantID: "s4", wantPath: "/r/a.go",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env, err := parseHookEnvelope([]byte(c.payload))
			if err != nil {
				t.Fatal(err)
			}
			calls := env.toolCalls()
			if len(calls) != 1 {
				t.Fatalf("calls = %d, want 1", len(calls))
			}
			if calls[0].Name != c.wantTool || env.SessionID != c.wantID {
				t.Errorf("tool=%q session=%q, want %q %q", calls[0].Name, env.SessionID, c.wantTool, c.wantID)
			}
			var in struct {
				FilePath string `json:"file_path"`
				Path     string `json:"path"`
			}
			if err := json.Unmarshal(calls[0].Input, &in); err != nil {
				t.Fatalf("input not an object: %v (%s)", err, calls[0].Input)
			}
			if got := firstNonEmpty(in.FilePath, in.Path); got != c.wantPath {
				t.Errorf("path = %q, want %q", got, c.wantPath)
			}
		})
	}
}

func TestParseHookEnvelope_BatchKeepsEveryCall(t *testing.T) {
	env, err := parseHookEnvelope([]byte(`{"toolCalls":[{"name":"edit","args":"{}"},{"name":"powershell","args":{"command":"x"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	calls := env.toolCalls()
	if len(calls) != 2 || calls[0].Name != "edit" || calls[1].Name != "powershell" {
		t.Fatalf("calls = %+v", calls)
	}
}

// A bare apply_patch body is a string that is NOT an encoded object: the patch
// handlers read the string itself, so it must reach them untouched.
func TestParseHookEnvelope_BarePatchStringUntouched(t *testing.T) {
	patch := "*** Begin Patch\n*** Update File: a.go\n*** End Patch\n"
	env, err := parseHookEnvelope([]byte(mustJSON(t, map[string]any{"tool_name": "Edit", "tool_input": patch})))
	if err != nil {
		t.Fatal(err)
	}
	var got string
	if err := json.Unmarshal(env.ToolInput, &got); err != nil || got != patch {
		t.Fatalf("tool_input = %s, want the patch string", env.ToolInput)
	}
}

func TestParseHookEnvelope_KeysOnlyNoValues(t *testing.T) {
	env, err := parseHookEnvelope([]byte(`{"toolName":"x","toolArgs":{"secret":"code"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(env.Keys, ",") != "toolArgs,toolName" {
		t.Errorf("keys = %v", env.Keys)
	}
}

func TestParseHookEnvelope_NotAnObject(t *testing.T) {
	if _, err := parseHookEnvelope([]byte(`[1,2]`)); err == nil {
		t.Fatal("want an error for a non-object payload")
	}
}

// ── Copilot agent runtime (Copilot CLI, Copilot for JetBrains 1.18+) ─────────
//
// The shapes below are the ones Copilot for JetBrains 1.18 was observed sending.
// Before the envelope was read tolerantly, every `edit` call in them recorded
// nothing and its lines committed as Human.

// copilotAgentPre is the agent runtime's batched PreToolUse payload.
func copilotAgentPre(t *testing.T, cwd, tool string, args map[string]any) string {
	return mustJSON(t, map[string]any{
		"sessionId": "sess-jb", "cwd": cwd,
		"toolCalls": []map[string]any{{"id": "toolu_1", "name": tool, "args": args}},
	})
}

// copilotAgentPost is the agent runtime's PostToolUse payload.
func copilotAgentPost(t *testing.T, cwd, tool string, args map[string]any) string {
	return mustJSON(t, map[string]any{
		"sessionId": "sess-jb", "cwd": cwd, "timestamp": 1794311492159,
		"toolName": tool, "toolArgs": args,
		"toolResult": map[string]any{"resultType": "success", "textResultForLlm": "ok"},
	})
}

func runCopilotPre(t *testing.T, payload string) {
	t.Helper()
	if err := CaptureBaselineFromStdin(strings.NewReader(payload)); err != nil {
		t.Fatalf("pre: %v", err)
	}
}

func runCopilotPost(t *testing.T, payload string) {
	t.Helper()
	// The error is the daemon POST failing (no daemon under the isolated HOME);
	// the working log, which is what attribution reads, is written before it.
	_ = RecordCopilotFromStdin(strings.NewReader(payload))
}

func TestRecordCopilot_AgentRuntimeEdit_CreditsOnlyChangedLine(t *testing.T) {
	isolateHome(t)
	repo, abs := gitRepoWithFile(t, "Page.tsx", "const a = 1;\nconst isListVisible = true;\nexport {};\n")
	// The human's own uncommitted line, before Copilot touches the file.
	if err := os.WriteFile(abs, []byte("const a = 1;\nconst isListVisible = true;\nexport {};\n// human note\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"path": abs, "old_str": "const isListVisible = true;", "new_str": "const isVisible = true;"}
	runCopilotPre(t, copilotAgentPre(t, repo, "edit", args))
	if err := os.WriteFile(abs, []byte("const a = 1;\nconst isVisible = true;\nexport {};\n// human note\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCopilotPost(t, copilotAgentPost(t, repo, "edit", args))

	got := authorsByLine(t, abs)
	if a := got[2]; a.Type != authorship.AI || a.Tool != "copilot" {
		t.Errorf("line 2 = %+v, want AI/copilot", a)
	}
	if a := got[4]; a.Type == authorship.AI {
		t.Errorf("line 4 = %+v, want Human: the user wrote it", a)
	}
	if n := len(hookdrift.Read()); n != 0 {
		t.Errorf("a readable edit must not be reported as drift, got %v", hookdrift.Read())
	}
}

// A Claude tool name over Copilot's own argument names.
func TestRecordCopilot_ClaudeNameNativeArgs(t *testing.T) {
	isolateHome(t)
	repo, abs := gitRepoWithFile(t, "a.ts", "one\ntwo\n")
	if err := os.WriteFile(abs, []byte("one\nTWO\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCopilotPost(t, mustJSON(t, map[string]any{
		"session_id": "s", "cwd": repo, "tool_name": "Edit",
		"tool_input": map[string]any{"path": abs, "old_str": "two", "new_str": "TWO"},
	}))
	if a := authorsByLine(t, abs)[2]; a.Type != authorship.AI || a.Tool != "copilot" {
		t.Errorf("line 2 = %+v, want AI/copilot", a)
	}
}

// A terminal write must claim what the command changed, not the whole file.
// Copilot 1.18 names this tool "powershell"; before it was recognised the
// pre-hook took no baseline and a one-identifier rename credited ~160 lines.
func TestRecordCopilot_AgentRuntimePowershell_CreditsOnlyChangedLine(t *testing.T) {
	isolateHome(t)
	repo, abs := gitRepoWithFile(t, "Page.tsx", "l1\nl2\nl3\n")
	// Uncommitted human work in the same file.
	if err := os.WriteFile(abs, []byte("l1\nl2\nl3\nhuman4\nhuman5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"command": "(Get-Content Page.tsx) -replace 'l2','L2' | Set-Content Page.tsx", "description": "rename"}
	runCopilotPre(t, copilotAgentPre(t, repo, "powershell", args))
	if err := os.WriteFile(abs, []byte("l1\nL2\nl3\nhuman4\nhuman5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCopilotPost(t, copilotAgentPost(t, repo, "powershell", args))

	got := authorsByLine(t, abs)
	if a := got[2]; a.Type != authorship.AI || a.Tool != "copilot" {
		t.Errorf("line 2 = %+v, want AI/copilot", a)
	}
	for _, ln := range []int{4, 5} {
		if a := got[ln]; a.Type == authorship.AI {
			t.Errorf("line %d = %+v, want Human: the user wrote it before the command", ln, a)
		}
	}
}

// The version-proofing itself: a tool this Blamely has never seen, with argument
// names it cannot read, is still credited — by observing the disk — and is
// reported for `blamely doctor`.
func TestRecordCopilot_UnknownTool_AttributedFromDisk(t *testing.T) {
	isolateHome(t)
	repo, abs := gitRepoWithFile(t, "svc.go", "a\nb\n")
	if err := os.WriteFile(abs, []byte("a\nb\nhuman\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"target": "svc.go", "patchset": []string{"x"}}
	runCopilotPre(t, copilotAgentPre(t, repo, "rewrite_v9", args))
	if err := os.WriteFile(abs, []byte("a\nb\nhuman\nagent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCopilotPost(t, copilotAgentPost(t, repo, "rewrite_v9", args))

	got := authorsByLine(t, abs)
	if a := got[4]; a.Type != authorship.AI || a.Tool != "copilot" {
		t.Errorf("line 4 = %+v, want AI/copilot", a)
	}
	if a := got[3]; a.Type == authorship.AI {
		t.Errorf("line 3 = %+v, want Human", a)
	}
	e, ok := hookdrift.Read()["copilot"]
	if !ok || e.Count != 1 || e.LastToolName != "rewrite_v9" {
		t.Fatalf("drift = %+v, want one rewrite_v9 entry", hookdrift.Read())
	}
	for _, k := range e.LastKeys {
		if k == "target" || k == "patchset" {
			t.Errorf("drift must record top-level keys only, got %v", e.LastKeys)
		}
	}
}

// A read is neither attributed nor reported, even when a file is dirty.
func TestRecordCopilot_ReadToolRecordsNothing(t *testing.T) {
	isolateHome(t)
	repo, abs := gitRepoWithFile(t, "a.go", "a\n")
	if err := os.WriteFile(abs, []byte("a\nhuman\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"pattern": "human", "paths": abs}
	runCopilotPre(t, copilotAgentPre(t, repo, "grep", args))
	runCopilotPost(t, copilotAgentPost(t, repo, "grep", args))
	if a := authorsByLine(t, abs)[2]; a.Type == authorship.AI {
		t.Errorf("line 2 = %+v, want Human", a)
	}
	if len(hookdrift.Read()) != 0 {
		t.Errorf("a read must not be reported as drift: %v", hookdrift.Read())
	}
}

// VS Code's agent host names every new chat with rename_chat, through both the
// runtime's own hook (camelCase) and VS Code's hook service (snake_case). It
// writes no file, so the human's dirty line must stay Human and no drift may be
// reported. Before it was listed, every Copilot Chat user saw a doctor warning.
func TestRecordCopilot_RenameChatRecordsNothing(t *testing.T) {
	isolateHome(t)
	repo, abs := gitRepoWithFile(t, "a.ts", "a\n")
	if err := os.WriteFile(abs, []byte("a\nhuman\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"title": "Shorten longMessage", "automatic": true}
	runCopilotPre(t, copilotAgentPre(t, repo, "rename_chat", args))
	runCopilotPost(t, copilotAgentPost(t, repo, "rename_chat", args))
	runCopilotPost(t, mustJSON(t, map[string]any{
		"session_id": "s", "cwd": repo, "hook_event_name": "PostToolUse", "timestamp": "2026-11-11T15:50:41Z",
		"tool_name": "rename_chat", "tool_input": args, "tool_result": "ok",
	}))
	if a := authorsByLine(t, abs)[2]; a.Type == authorship.AI {
		t.Errorf("line 2 = %+v, want Human", a)
	}
	if len(hookdrift.Read()) != 0 {
		t.Errorf("rename_chat must not be reported as drift: %v", hookdrift.Read())
	}
}

// The batched PreToolUse snapshots every file the batch will edit.
func TestCaptureBaselineFromStdin_AgentRuntimeBatch(t *testing.T) {
	root, abs := committedRepoWithEdit(t)
	payload := mustJSON(t, map[string]any{
		"sessionId": "s", "cwd": root,
		"toolCalls": []map[string]any{
			{"name": "view", "args": mustJSON(t, map[string]any{"path": abs})},
			{"name": "edit", "args": mustJSON(t, map[string]any{"path": abs, "old_str": "committed", "new_str": "x"})},
		},
	})
	if err := CaptureBaselineFromStdin(bytes.NewReader([]byte(payload))); err != nil {
		t.Fatal(err)
	}
	got, ok := storedBaseline(t, abs)
	if !ok || !strings.Contains(got, "human, uncommitted") {
		t.Fatalf("want a baseline holding the human's line, got ok=%v %q", ok, got)
	}
}
