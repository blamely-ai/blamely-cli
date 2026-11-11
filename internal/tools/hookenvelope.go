package tools

import (
	"bytes"
	"encoding/json"
	"sort"
)

// hookEnvelope is the agent-agnostic reading of a tool hook's stdin.
//
// Why this exists
// ---------------
// Every agent pipes roughly the same facts into its tool hook — which tool ran,
// with what arguments, in which session and directory — but the field names drift
// between agents AND between versions of the same agent. Claude Code sends
// tool_name/tool_input; the GitHub Copilot agent runtime (Copilot CLI, and Copilot
// for JetBrains since it moved onto that runtime) sends toolName/toolArgs, may
// pass the arguments as a JSON-encoded string, and batches a PreToolUse into a
// toolCalls array. Each per-version spelling we failed to anticipate silently
// turned an AI edit into Human, because the hook found no tool and recorded
// nothing.
//
// So the envelope is read here, once, tolerating every spelling we have seen,
// and the per-tool handlers only deal with the tool's own arguments. Spellings
// are tried in a fixed order (snake_case first, the original Anthropic shape), so
// a payload carrying two spellings resolves the same way every time.
type hookEnvelope struct {
	SessionID      string
	TranscriptPath string
	Cwd            string
	Model          string
	ToolName       string
	ToolInput      json.RawMessage
	// Calls is set when the payload batches several tool calls (the Copilot
	// agent runtime's PreToolUse: {"toolCalls":[{"name","args"}, …]}).
	Calls []hookToolCall
	// Keys is the payload's sorted top-level key set — names only, never values —
	// so a payload we could not read can be reported without leaking its content.
	Keys []string
}

// hookToolCall is one tool invocation inside a hook payload.
type hookToolCall struct {
	Name  string
	Input json.RawMessage
}

var (
	hookSessionKeys    = []string{"session_id", "sessionId", "conversation_id", "conversationId"}
	hookTranscriptKeys = []string{"transcript_path", "transcriptPath"}
	hookCwdKeys        = []string{"cwd", "workingDirectory", "working_directory"}
	hookModelKeys      = []string{"model"}
	hookToolNameKeys   = []string{"tool_name", "toolName"}
	hookToolInputKeys  = []string{"tool_input", "toolInput", "toolArgs", "tool_args"}
	hookToolCallsKeys  = []string{"toolCalls", "tool_calls"}

	hookCallNameKeys  = []string{"name", "toolName", "tool_name"}
	hookCallInputKeys = []string{"args", "arguments", "input", "toolArgs", "tool_input"}
)

// parseHookEnvelope reads a hook payload. It fails only when the payload is not a
// JSON object; every missing field is simply left empty.
func parseHookEnvelope(raw []byte) (hookEnvelope, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return hookEnvelope{}, err
	}
	env := hookEnvelope{
		SessionID:      pickHookString(fields, hookSessionKeys),
		TranscriptPath: pickHookString(fields, hookTranscriptKeys),
		Cwd:            pickHookString(fields, hookCwdKeys),
		Model:          pickHookString(fields, hookModelKeys),
		ToolName:       pickHookString(fields, hookToolNameKeys),
		ToolInput:      normalizeToolInput(pickHookRaw(fields, hookToolInputKeys)),
	}
	for k := range fields {
		env.Keys = append(env.Keys, k)
	}
	sort.Strings(env.Keys)

	if calls := pickHookRaw(fields, hookToolCallsKeys); len(calls) > 0 {
		var items []map[string]json.RawMessage
		if json.Unmarshal(calls, &items) == nil {
			for _, it := range items {
				c := hookToolCall{
					Name:  pickHookString(it, hookCallNameKeys),
					Input: normalizeToolInput(pickHookRaw(it, hookCallInputKeys)),
				}
				if c.Name != "" || len(c.Input) > 0 {
					env.Calls = append(env.Calls, c)
				}
			}
		}
	}
	// A batch of one is just a tool call: lift it so single-call handlers see it.
	if env.ToolName == "" && len(env.ToolInput) == 0 && len(env.Calls) == 1 {
		env.ToolName, env.ToolInput = env.Calls[0].Name, env.Calls[0].Input
		env.Calls = nil
	}
	return env, nil
}

// toolCalls returns every tool call the payload describes: the batch when there
// is one, otherwise the single top-level call. Empty when the payload names no
// tool and carries no arguments.
func (e hookEnvelope) toolCalls() []hookToolCall {
	if len(e.Calls) > 0 {
		return e.Calls
	}
	if e.ToolName == "" && len(e.ToolInput) == 0 {
		return nil
	}
	return []hookToolCall{{Name: e.ToolName, Input: e.ToolInput}}
}

// pickHookRaw returns the first present, non-null value among keys.
func pickHookRaw(fields map[string]json.RawMessage, keys []string) json.RawMessage {
	for _, k := range keys {
		if v, ok := fields[k]; ok && len(v) > 0 && !bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return v
		}
	}
	return nil
}

// pickHookString returns the first non-empty string value among keys. A key whose
// value is not a string (a number, an object) is skipped, not an error.
func pickHookString(fields map[string]json.RawMessage, keys []string) string {
	for _, k := range keys {
		v, ok := fields[k]
		if !ok {
			continue
		}
		var s string
		if json.Unmarshal(v, &s) == nil && s != "" {
			return s
		}
	}
	return ""
}

// normalizeToolInput unwraps tool arguments that arrive JSON-encoded inside a
// string ("args":"{\"path\":…}") so every handler can unmarshal an object. A
// string that is not an encoded object — the bare patch an apply_patch call
// passes, for instance — is left exactly as it was, because those handlers read
// the string itself.
func normalizeToolInput(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return raw // already an object (or an array, number, …)
	}
	inner := bytes.TrimSpace([]byte(s))
	if len(inner) > 0 && inner[0] == '{' && json.Valid(inner) {
		return json.RawMessage(inner)
	}
	return raw
}
