package tools

import "github.com/blamely/blamely/internal/daemon"

type hookUsageOptions struct {
	transcriptPath string
	sessionID      string
	tool           string // claude | cursor | copilot | codex | gemini | devin
}

// applyHookUsage enriches a record-hook payload with model + token counts from
// the tool's on-disk session/transcript files (best-effort).
func applyHookUsage(p *daemon.EditPayload, opt hookUsageOptions) {
	if u := readHookUsage(opt); u != nil {
		applyUsageToPayload(p, u)
	}
}

func readHookUsage(opt hookUsageOptions) *TranscriptUsage {
	switch opt.tool {
	case "codex":
		if u, _ := ReadCodexSessionUsage(opt.transcriptPath); u != nil {
			return u
		}
	case "gemini":
		if u, _ := ReadGeminiTranscriptUsage(opt.transcriptPath); u != nil {
			return u
		}
	case "copilot":
		// Copilot CLI: model + output tokens come from its own session events
		// log (~/.copilot/session-state/<id>/events.jsonl), which the generic
		// transcript/chat readers below can't parse.
		if u, _ := ReadCopilotCliUsage(opt.sessionID); u != nil {
			return u
		}
		if u, _ := ReadTranscriptUsage(opt.transcriptPath); u != nil {
			return u
		}
		if path := findChatSessionPath(opt.sessionID, copilotChatSearchRoots()); path != "" {
			if u, _ := ReadChatSessionLatestUsage(path); u != nil {
				return u
			}
		}
	case "cursor":
		if u, _ := ReadTranscriptUsage(opt.transcriptPath); u != nil {
			return u
		}
		if path := findChatSessionPath(opt.sessionID, defaultCursorChatRoots()); path != "" {
			if u, _ := ReadChatSessionLatestUsage(path); u != nil {
				return u
			}
		}
	case "devin":
		// Devin CLI's hook payload carries no transcript_path and it writes no
		// session transcript we can read, so there is no model or token usage to
		// recover. This case exists to keep devin OUT of the claude default
		// below — falling through would parse an unrelated Claude transcript and
		// attach another session's token counts to a Devin edit.
		//
		// If a future Devin release starts sending transcript_path, wire the
		// reader in here rather than removing the case.
		return nil
	default: // claude
		if u, _ := ReadTranscriptUsage(opt.transcriptPath); u != nil {
			return u
		}
	}
	return nil
}
