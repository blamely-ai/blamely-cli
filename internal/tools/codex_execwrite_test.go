package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blamely/blamely/internal/authorship"
	"github.com/blamely/blamely/internal/daemon"
)

// codexLogLine wraps a payload in the session-log envelope, stamped at when.
func codexLogLine(t *testing.T, typ string, when time.Time, payload any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"timestamp": when.UTC().Format("2006-01-02T15:04:05.000Z"),
		"type":      typ,
		"payload":   payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// codexExecCallLine is a code-mode exec call as Codex 0.167 logs it on Windows:
// the shell command is a JS string inside the script, with no workdir.
func codexExecCallLine(t *testing.T, callID, cmd string, when time.Time) []byte {
	return codexLogLine(t, "response_item", when, map[string]any{
		"type":    "custom_tool_call",
		"status":  "completed",
		"call_id": callID,
		"name":    "exec",
		"input":   `text(await tools.exec_command({cmd:"` + jsEscapeForTest(cmd) + `",yield_time_ms:30000}));`,
	})
}

func codexExecOutputLine(t *testing.T, callID string, when time.Time) []byte {
	return codexLogLine(t, "response_item", when, map[string]any{
		"type":    "custom_tool_call_output",
		"call_id": callID,
		"output":  []map[string]any{{"type": "input_text", "text": "Script completed"}},
	})
}

// newCodexExecSession starts a tailer state on a VS Code session in repo.
func newCodexExecSession(t *testing.T, repo string) *codexState {
	t.Helper()
	st := &codexState{sink: &mockSink{}, transcriptPath: filepath.Join(t.TempDir(), "rollout.jsonl")}
	processCodexLine(codexLogLine(t, "session_meta", time.Now(), map[string]any{
		"id": "sess-exec", "cwd": repo, "originator": "codex_vscode", "source": "vscode",
	}), st)
	processCodexLine(codexLogLine(t, "turn_context", time.Now(), map[string]any{
		"cwd": repo, "model": "gpt-6-astra",
	}), st)
	return st
}

func codexPosts(posts []daemon.EditPayload, file string) []daemon.EditPayload {
	var out []daemon.EditPayload
	for _, p := range posts {
		if p.Tool == "codex" && filepath.Base(p.FilePath) == file {
			out = append(out, p)
		}
	}
	return out
}

// Codex in code mode runs no hooks, so a file its exec script writes was
// committed as Human. The session log's call and output now stand in for the
// hooks: only the line the script changed is Codex's, and the lines the human
// typed before stay Human.
func TestCodexExecWrite_CreditsOnlyChangedLines(t *testing.T) {
	isolateHome(t)
	posts := captureDaemonPosts(t)
	repo, abs := gitRepoWithFile(t, "a.ts", "l1\nl2\nl3\n")
	if err := os.WriteFile(abs, []byte("l1\nl2\nl3\nhuman4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := newCodexExecSession(t, repo)

	cmd := "$c = [IO.File]::ReadAllText('a.ts') -replace 'l2','L2'; [IO.File]::WriteAllText('a.ts', $c)"
	processCodexLine(codexExecCallLine(t, "call_1", cmd, time.Now()), st)
	if err := os.WriteFile(abs, []byte("l1\nL2\nl3\nhuman4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	processCodexLine(codexExecOutputLine(t, "call_1", time.Now()), st)

	edits := codexPosts(posts(), "a.ts")
	if len(edits) != 1 {
		t.Fatalf("want one codex edit for a.ts, got %d: %+v", len(edits), edits)
	}
	p := edits[0]
	if p.SuggestedLines != 1 || len(p.Lines) != 1 || p.Lines[0].Start != 2 {
		t.Fatalf("payload lines = %+v (suggested %d), want only line 2", p.Lines, p.SuggestedLines)
	}
	if p.GenType != "chat" || p.Model != "gpt-6-astra" {
		t.Errorf("gen_type %q model %q, want chat / gpt-6-astra", p.GenType, p.Model)
	}

	authors := authorsByLine(t, abs)
	if a := authors[2]; a.Type != authorship.AI || a.Tool != "codex" {
		t.Errorf("line 2 author = %+v, want codex", a)
	}
	if a := authors[4]; a.Type != authorship.Human {
		t.Errorf("line 4 author = %+v, want Human", a)
	}
	if len(st.execCalls) != 0 {
		t.Errorf("call still pending after its output: %+v", st.execCalls)
	}
}

// A call that only reads records nothing.
func TestCodexExecWrite_ReadOnlyCallRecordsNothing(t *testing.T) {
	isolateHome(t)
	posts := captureDaemonPosts(t)
	repo, abs := gitRepoWithFile(t, "a.ts", "l1\nl2\n")
	if err := os.WriteFile(abs, []byte("l1\nl2\nhuman3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The human saved before Codex started.
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(abs, old, old); err != nil {
		t.Fatal(err)
	}
	st := newCodexExecSession(t, repo)
	processCodexLine(codexExecCallLine(t, "call_r", "Get-Content -LiteralPath a.ts", time.Now()), st)
	processCodexLine(codexExecOutputLine(t, "call_r", time.Now()), st)

	if got := codexPosts(posts(), "a.ts"); len(got) != 0 {
		t.Fatalf("read-only call recorded %+v", got)
	}
}

// A replayed log, read after a daemon restart, describes writes long done. The
// disk no longer shows them, so acting on it would credit whatever the human
// changed since.
func TestCodexExecWrite_IgnoresReplayedCalls(t *testing.T) {
	isolateHome(t)
	posts := captureDaemonPosts(t)
	repo, abs := gitRepoWithFile(t, "a.ts", "l1\nl2\n")
	st := newCodexExecSession(t, repo)

	then := time.Now().Add(-time.Hour)
	processCodexLine(codexExecCallLine(t, "call_old", "Set-Content a.ts x", then), st)
	if err := os.WriteFile(abs, []byte("l1\nhuman\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	processCodexLine(codexExecOutputLine(t, "call_old", then.Add(time.Second)), st)

	if got := codexPosts(posts(), "a.ts"); len(got) != 0 {
		t.Fatalf("replayed call recorded %+v", got)
	}
}

// A direct shell tool call (not code mode) runs the hooks, and `record codex`
// records its writes. The tailer must leave it alone.
func TestCodexExecWrite_LeavesDirectShellCallsToTheHook(t *testing.T) {
	isolateHome(t)
	posts := captureDaemonPosts(t)
	repo, abs := gitRepoWithFile(t, "a.ts", "l1\nl2\n")
	st := newCodexExecSession(t, repo)

	args, _ := json.Marshal(map[string]any{"command": "Set-Content a.ts x", "workdir": repo})
	processCodexLine(codexLogLine(t, "response_item", time.Now(), map[string]any{
		"type": "function_call", "name": "shell_command", "call_id": "call_s", "arguments": string(args),
	}), st)
	if err := os.WriteFile(abs, []byte("l1\nL2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	processCodexLine(codexLogLine(t, "response_item", time.Now(), map[string]any{
		"type": "function_call_output", "call_id": "call_s", "output": "ok",
	}), st)

	if got := codexPosts(posts(), "a.ts"); len(got) != 0 {
		t.Fatalf("direct shell call recorded by the tailer: %+v", got)
	}
}

// When Codex does run its hooks for a code-mode exec call, it names the tool
// "Bash" with a Claude-style {"command": ...} input. `record codex` did not know
// the name and recorded nothing. It now takes the shell-write path: only the
// changed line is Codex's.
func TestRecordCodex_BashHookFromExec(t *testing.T) {
	isolateHome(t)
	posts := captureDaemonPosts(t)
	repo, abs := gitRepoWithFile(t, "a.ts", "l1\nl2\nl3\n")
	if err := os.WriteFile(abs, []byte("l1\nl2\nl3\nhuman4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hook := func(event string) string {
		return mustJSON(t, map[string]any{
			"session_id":      "sess-bash",
			"cwd":             repo,
			"hook_event_name": event,
			"model":           "gpt-6-astra",
			"tool_name":       "Bash",
			"tool_use_id":     "exec-1",
			"tool_input":      map[string]any{"command": "(Get-Content a.ts) -replace 'l2','L2' | Set-Content a.ts"},
		})
	}
	if err := CaptureBaselineFromStdin(strings.NewReader(hook("PreToolUse"))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte("l1\nL2\nl3\nhuman4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RecordCodexFromStdin(strings.NewReader(hook("PostToolUse"))); err != nil {
		t.Fatal(err)
	}

	edits := codexPosts(posts(), "a.ts")
	if len(edits) != 1 {
		t.Fatalf("want one codex edit for a.ts, got %d: %+v", len(edits), edits)
	}
	if p := edits[0]; len(p.Lines) != 1 || p.Lines[0].Start != 2 || p.SuggestedLines != 1 {
		t.Fatalf("payload lines = %+v (suggested %d), want only line 2", p.Lines, p.SuggestedLines)
	}
	if a := authorsByLine(t, abs)[4]; a.Type != authorship.Human {
		t.Errorf("line 4 author = %+v, want Human", a)
	}
}

// When both the hook and the session log report the same exec call, the change
// is recorded once.
func TestCodexExecWrite_HookAndLogRecordOnce(t *testing.T) {
	isolateHome(t)
	posts := captureDaemonPosts(t)
	repo, abs := gitRepoWithFile(t, "a.ts", "l1\nl2\n")
	st := newCodexExecSession(t, repo)
	cmd := "(Get-Content a.ts) -replace 'l2','L2' | Set-Content a.ts"
	processCodexLine(codexExecCallLine(t, "call_b", cmd, time.Now()), st)
	if err := os.WriteFile(abs, []byte("l1\nL2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	post := mustJSON(t, map[string]any{
		"session_id": "sess-exec", "cwd": repo, "hook_event_name": "PostToolUse",
		"tool_name": "Bash", "tool_input": map[string]any{"command": cmd},
	})
	if err := RecordCodexFromStdin(strings.NewReader(post)); err != nil {
		t.Fatal(err)
	}
	processCodexLine(codexExecOutputLine(t, "call_b", time.Now()), st)

	if got := codexPosts(posts(), "a.ts"); len(got) != 1 {
		t.Fatalf("want the change recorded once, got %d: %+v", len(got), got)
	}
}
