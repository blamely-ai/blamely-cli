package tools

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"strings"

	"github.com/blamely/blamely/internal/daemon"
	"github.com/blamely/blamely/internal/gitutil"
)

// codexHookPayload is the JSON Codex CLI sends on stdin for PostToolUse hooks.
type codexHookPayload struct {
	SessionID      string          `json:"session_id"`
	TranscriptPath string          `json:"transcript_path"`
	Cwd            string          `json:"cwd"`
	ToolName       string          `json:"tool_name"`
	ToolInput      json.RawMessage `json:"tool_input"`
	Model          string          `json:"model"`
}

// RecordCodexFromStdin handles `blamely record codex` (Codex CLI PostToolUse).
func RecordCodexFromStdin(r io.Reader) error {
	raw, err := readHookPayload(r)
	if err != nil {
		return err
	}
	var p codexHookPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		log.Printf("blamely record codex: parse hook payload: %v", err)
		return nil
	}
	// gen_type from the session's surface: the VS Code chat panel reports "chat",
	// the terminal CLI "cli" (see ReadCodexGenType / codexGenType).
	gt := ReadCodexGenType(p.TranscriptPath)

	// A single apply_patch can add/update some files and DELETE others. The
	// `*** Delete File:` directives produce no edit range, so handle them up
	// front — independent of whether the patch also has an add/update primary
	// path — otherwise an AI-deleted file falls through to Human at commit time.
	deletedViaPatch := parsePatchDeletedFiles(p.ToolInput)
	for _, dp := range deletedViaPatch {
		abs := dp
		if !filepath.IsAbs(abs) && p.Cwd != "" {
			abs = filepath.Join(p.Cwd, dp)
		}
		if err := recordToolDeletionPath(abs, p.Cwd, "codex", gt, p.Model, p.SessionID, p.TranscriptPath, "codex_delete"); err != nil {
			return err
		}
	}

	filePath, ranges, suggested, removed, newFullContent := extractCodexHookRanges(p)
	if filePath == "" {
		// No edit range and no structured delete directive — but Codex also
		// deletes via a shell command (`rm` on Unix, `Remove-Item`/`del` on
		// Windows, run through its shell_command/exec_command tool). Fingerprint
		// whatever git now reports as deleted and credit it to codex. Skipped
		// when the patch already named its deletions above (avoids recording the
		// same removal twice).
		name := strings.ToLower(p.ToolName)
		if len(deletedViaPatch) == 0 && (looksLikePatch(name) || codexShellNames[name]) {
			cmd := shellCommandFromInput(p.ToolInput)
			if codexShellNames[name] {
				// A real shell command also WRITES: a script, a heredoc or a formatter
				// run through it produces no edit range at all, so without the write
				// half everything Codex authors through the shell commits as Human.
				// recordShellWrites covers both (deletions first).
				return recordShellWrites(p.Cwd, cmd, shellWriteOpts{
					Tool:           "codex",
					GenType:        gt,
					Model:          p.Model,
					SessionID:      p.SessionID,
					TranscriptPath: p.TranscriptPath,
					WriteSource:    "codex_shell_fswrite",
					DeleteSource:   "codex_shell_delete",
				})
			}
			// apply_patch whose primary path we couldn't extract: the written paths
			// live in the patch BODY, not in tool_input, and the patch is not a shell
			// command — so only its deletions are recoverable here. Attributing every
			// changed file to a patch would over-claim; documented gap.
			return recordShellDeletionsFrom(p.Cwd, cmd, "codex", gt, p.Model, p.SessionID, p.TranscriptPath, "codex_shell_delete")
		}
		return nil
	}

	resolved := resolveSymlinks(filePath)
	repoPath, _ := gitutil.RepoID(resolved)
	if repoPath == "" && p.Cwd != "" {
		repoPath, _ = gitutil.RepoID(resolveSymlinks(p.Cwd))
	}
	wt, _ := gitutil.Toplevel(resolved)
	rel := resolved
	if wt != "" {
		if r, err := filepath.Rel(wt, resolved); err == nil && !strings.HasPrefix(r, "..") {
			rel = r
		}
	}

	// Claude-compatible Write shape overwrites the whole file with no "before"
	// content of its own — resolve it against the daemon's cached snapshot so
	// added lines are narrowed to what genuinely changed (and removed lines
	// detected), instead of crediting every re-emitted line to the AI. Same
	// shared rule every whole-file tool uses.
	if newFullContent != nil {
		var wfRemoved []DeletedLineHash
		ranges, wfRemoved = ResolveWholeFileWrite(wt, rel, *newFullContent, ranges)
		removed = append(removed, wfRemoved...)
	}

	payload := daemon.EditPayload{
		Tool:           "codex",
		Confidence:     "high",
		GenType:        gt,
		RepoPath:       repoPath,
		WorktreePath:   wt,
		FilePath:       rel,
		Model:          p.Model,
		SuggestedLines: suggested,
		Lines:          toDaemonRanges(ranges),
		RemovedLines:   toDaemonRemovedLines(removed),
		RawMeta: fmt.Sprintf(`{"session_id":%q,"tool":%q,"transcript_path":%q,"source":"codex_hook"}`,
			p.SessionID, p.ToolName, p.TranscriptPath),
	}
	applyHookUsage(&payload, hookUsageOptions{
		transcriptPath: p.TranscriptPath,
		sessionID:      p.SessionID,
		tool:           "codex",
	})
	// Attribution: mirror into the working log before the
	// daemon POST so capture is daemon-independent. No-op when the flag is off.
	captureAuthorship(wt, rel, "codex", gt, payload.Model)
	return postToDaemon(payload)
}

func extractCodexHookRanges(p codexHookPayload) (string, []LineRange, int64, []DeletedLineHash, *string) {
	name := strings.ToLower(p.ToolName)
	if looksLikePatch(name) {
		var suggested int64
		var out []LineRange
		var primary string
		for _, f := range parsePatchFiles(p.ToolInput) {
			if primary == "" {
				primary = f.Path
			}
			out = append(out, LineRange{Start: f.StartLine, End: f.EndLine, ContentSHA: f.ContentSHA, ContentSHANorm: f.ContentSHANorm})
			if f.EndLine >= f.StartLine {
				suggested += int64(f.EndLine - f.StartLine + 1)
			}
		}
		if primary == "" {
			return "", nil, 0, nil, nil
		}
		return primary, out, suggested, nil, nil
	}

	// Claude-compatible Edit/Write/MultiEdit shapes (some Codex versions).
	cl := claudeHookPayload{ToolName: p.ToolName, ToolInput: p.ToolInput}
	fp, ranges, suggested, removed, newFullContent, err := extractClaudeRanges(cl)
	if err != nil {
		return "", nil, 0, nil, nil
	}
	return fp, ranges, suggested, removed, newFullContent
}
