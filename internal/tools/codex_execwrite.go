package tools

import (
	"encoding/json"
	"log"
	"strings"
	"time"
)

// codex_execwrite.go covers files Codex writes through its code-mode `exec` tool.
//
// In code mode Codex sends every tool through one `exec` call. The call's input is
// a JavaScript body such as `text(await tools.exec_command({cmd:"..."}))`, and
// Codex runs no PreToolUse or PostToolUse hook for it. So `blamely record codex`
// never hears about a file the script writes, and the change commits as Human.
// Seen on Windows with the VS Code extension (Codex 0.167): a PowerShell rewrite
// of one line, no hook run, nothing in daemon.log.
//
// The session log still records the call and, later, its output. The tailer does
// what the two hooks would have done:
//   - when the call appears, it snapshots the dirty files the working log does
//     not describe yet (the pre-hook's job);
//   - when the output appears, it credits what changed on disk since the call
//     started, through the shell-write path every agent's shell tool uses (the
//     post-hook's job).

// codexExecLiveWindow is how old a call may be and still be acted on. An older
// call comes from a replayed log, after a daemon restart or on first start, and
// the disk no longer shows what that call did.
const codexExecLiveWindow = 2 * time.Minute

// codexExecSlack widens the write window past the call's own timestamp, to
// cover clock rounding between Codex's log and the file's mtime.
const codexExecSlack = 2 * time.Second

// codexExecCall is a code-mode exec call waiting for its output.
type codexExecCall struct {
	when     time.Time
	workdirs []string
}

// trackCodexExecWrites handles one response_item. It remembers a code-mode exec
// call that runs shell commands, and attributes that call's writes when its
// output arrives.
func trackCodexExecWrites(payload json.RawMessage, when time.Time, st *codexState) {
	var p struct {
		Type   string `json:"type"`
		Name   string `json:"name"`
		CallID string `json:"call_id"`
	}
	if json.Unmarshal(payload, &p) != nil || p.CallID == "" {
		return
	}
	switch p.Type {
	case "custom_tool_call", "function_call":
		// A direct shell tool call runs the hooks, and `record codex` records
		// its writes.
		if codexShellNames[strings.ToLower(p.Name)] {
			return
		}
		if time.Since(when) > codexExecLiveWindow {
			return
		}
		var dirs []string
		seen := map[string]bool{}
		for _, inv := range codexShellInvocations(payload) {
			wd := firstNonEmpty(inv.Workdir, st.cwd)
			if wd == "" || seen[wd] {
				continue
			}
			seen[wd] = true
			dirs = append(dirs, wd)
		}
		if len(dirs) == 0 {
			return
		}
		st.pruneExecCalls()
		if st.execCalls == nil {
			st.execCalls = map[string]codexExecCall{}
		}
		st.execCalls[p.CallID] = codexExecCall{when: when, workdirs: dirs}
		for _, wd := range dirs {
			captureShellBaselines(wd)
		}
	case "custom_tool_call_output", "function_call_output":
		call, ok := st.execCalls[p.CallID]
		if !ok {
			return
		}
		delete(st.execCalls, p.CallID)
		window := time.Since(call.when) + codexExecSlack
		if window > maxBashWriteWindow {
			window = maxBashWriteWindow
		}
		for _, wd := range call.workdirs {
			// No command: the script's deletions are recorded when the call
			// appears (emitCodexShellDeletions), so they are not recorded twice.
			err := recordShellWrites(wd, "", shellWriteOpts{
				Tool:           "codex",
				GenType:        st.gen(),
				Model:          st.model,
				SessionID:      st.sessionID,
				TranscriptPath: st.transcriptPath,
				WriteSource:    "codex_exec_fswrite",
				DeleteSource:   "codex_shell_delete",
				Window:         window,
			})
			if err != nil {
				log.Printf("codex exec write: %v", err)
			}
		}
	}
}

// pruneExecCalls drops calls whose output never came (an interrupted turn), so
// the map does not grow over a long session.
func (st *codexState) pruneExecCalls() {
	for id, c := range st.execCalls {
		if time.Since(c.when) > maxBashWriteWindow {
			delete(st.execCalls, id)
		}
	}
}
