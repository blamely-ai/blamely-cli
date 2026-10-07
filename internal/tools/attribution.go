package tools

import (
	"encoding/json"
	"io"
	"path/filepath"
	"strings"

	"github.com/blamely/blamely/internal/authorship"
	"github.com/blamely/blamely/internal/gitutil"
)

// CaptureBaselineFromStdin handles a PreToolUse hook (`blamely record <tool> --pre`):
// it snapshots the target file's CURRENT content as the pre-edit baseline, so the
// matching PostToolUse `record` diffs the agent's write against the true pre-edit
// state even for a file the editor never had open (Decision B fallback). Flag-gated
// and best-effort; tool-agnostic (reads the file path from the common hook shapes).
// Tools that name their files elsewhere — apply_patch in the patch body, VS Code's
// multi_replace_string_in_file in replacements[] — get a baseline per named file.
func CaptureBaselineFromStdin(r io.Reader) error {
	if !authorship.Enabled() {
		return nil
	}
	raw, err := readHookPayload(r)
	if err != nil {
		return nil // best-effort: a pre-hook must never block the tool
	}
	var p struct {
		Cwd       string          `json:"cwd"`
		ToolName  string          `json:"tool_name"`
		ToolInput json.RawMessage `json:"tool_input"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return nil
	}
	var in struct {
		FilePathSnake string `json:"file_path"`
		FilePathCamel string `json:"filePath"`
		Path          string `json:"path"`
		Command       string `json:"command"`
	}
	_ = json.Unmarshal(p.ToolInput, &in)
	if isReadOnlyToolCall(p.ToolName, in.Command) {
		// Nothing will be written. A baseline taken now is stale by the time
		// the agent does write — the write's own pre-hook takes a fresh one —
		// and resolving it costs git processes on every read the agent makes.
		return nil
	}
	if paths := multiFileTargets(p.ToolName, p.ToolInput); len(paths) > 0 {
		for _, fp := range paths {
			if !filepath.IsAbs(fp) && p.Cwd != "" {
				fp = filepath.Join(p.Cwd, fp)
			}
			captureBaselineIfUntracked(fp)
		}
		return nil
	}
	fp := firstNonEmpty(in.FilePathSnake, in.FilePathCamel, in.Path)
	if fp == "" {
		// A shell command names no target file — it may write anything — so snapshot
		// the repo's untracked-by-Attribution files instead. apply_patch still falls
		// back to HEAD at record time (paths live in the patch body): documented gap.
		if isShellToolName(p.ToolName) {
			captureShellBaselines(p.Cwd)
		}
		return nil
	}
	if !filepath.IsAbs(fp) && p.Cwd != "" {
		fp = filepath.Join(p.Cwd, fp)
	}
	captureBaselineIfUntracked(fp)
	return nil
}

// readOnlyToolNames are agent tools that name a file or directory but never
// write it: VS Code Copilot's read_file/list_dir, the Copilot CLI's view, Gemini's
// read_file/read_many_files/list_directory, and the generic read/ls shapes. Only
// names known to read are listed — an unknown tool keeps taking its baseline.
var readOnlyToolNames = map[string]bool{
	"read_file": true, "list_dir": true, "view": true, "read": true, "ls": true,
	"read_many_files": true, "list_directory": true,
}

// isReadOnlyToolCall reports whether a tool call only reads. str_replace_editor is
// both an editor and a viewer; its command says which.
func isReadOnlyToolCall(toolName, command string) bool {
	n := strings.ToLower(toolName)
	if n == "str_replace_editor" {
		return strings.EqualFold(command, "view")
	}
	return readOnlyToolNames[n]
}

// multiFileTargets returns the files a tool call will write when they are not in
// a top-level path field: every `*** Update/Delete File:` of an apply_patch body,
// and every replacements[].filePath of multi_replace_string_in_file. Without a
// baseline for each, their first record diffs against HEAD and claims every
// uncommitted line in the file — the human's included — for the agent.
// (*** Add File targets are new, so there is nothing to snapshot.)
func multiFileTargets(toolName string, input json.RawMessage) []string {
	n := strings.ToLower(toolName)
	switch {
	case n == "multi_replace_string_in_file":
		var in struct {
			Replacements []struct {
				FilePath string `json:"filePath"`
			} `json:"replacements"`
		}
		if json.Unmarshal(input, &in) != nil {
			return nil
		}
		seen := map[string]bool{}
		var out []string
		for _, r := range in.Replacements {
			if r.FilePath != "" && !seen[r.FilePath] {
				seen[r.FilePath] = true
				out = append(out, r.FilePath)
			}
		}
		return out
	case strings.Contains(n, "patch"):
		body, _ := patchEnvelope(input)
		var out []string
		for _, line := range strings.Split(body, "\n") {
			line = strings.TrimRight(line, "\r")
			for _, prefix := range []string{"*** Update File: ", "*** Delete File: "} {
				if strings.HasPrefix(line, prefix) {
					if p := strings.TrimSpace(strings.TrimPrefix(line, prefix)); p != "" {
						out = append(out, p)
					}
				}
			}
		}
		return out
	}
	return nil
}

// captureBaselineIfUntracked snapshots absPath's current content as its pre-edit
// baseline, but only while Attribution isn't tracking the file yet — which is
// exactly the "a file the editor never had open" case the pre-hook exists for.
//
// The guard matters: for a file that DOES have a working log, the stored baseline
// is what its current line attributions describe. Overwriting it with today's
// content strands those line numbers, and a subsequent commit can then read the
// user's own lines off a shifted mapping and mark them AI. That failure is why
// PreToolUse was switched off for Claude (see install.claudeHookEvents); the guard
// removes the hazard rather than the hook.
func captureBaselineIfUntracked(absPath string) {
	ctx, ok := authorship.ResolveContext(absPath)
	if !ok {
		return
	}
	if wl, err := authorship.LoadWorkingLog(ctx.RepoRoot, ctx.Branch, ctx.BaseSHA, ctx.RelPath); err == nil && wl != nil {
		return
	}
	_ = authorship.CaptureBaselineIn(ctx, absPath)
}

// maxShellBaselineFiles caps how many pre-command baselines one shell command
// snapshots. Past that the working tree is mid-rebase / mid-checkout rather than
// mid-authoring, and the reads would cost more than the attribution is worth.
const maxShellBaselineFiles = 60

// captureShellBaselines snapshots the pre-command content of every changed source
// file that Attribution is NOT yet tracking, for each repo under cwd.
//
// Without it, the FIRST observation of a file diffs against HEAD — so a shell
// write claims every uncommitted change the file had accumulated, including lines
// the user typed by hand, and the user's own work commits as AI. That is the exact
// inversion of the product's rule (an unobserved line is Human), so the pre-edit
// snapshot is what keeps a shell write's claim honest.
//
// A file that already HAS a working log is skipped: its stored baseline is what
// its current attributions describe, and overwriting that would strand their line
// numbers. Committed files are covered by the post-commit seed, which writes both
// a log and a baseline — so in practice this fills the gap for files no commit has
// noted yet (new files, and files whose uncommitted edits nobody observed).
func captureShellBaselines(cwd string) {
	if !authorship.Enabled() || cwd == "" {
		return
	}
	for _, root := range gitutil.DiscoverRepos(cwd) {
		files := changedSourceFiles(root)
		if len(files) == 0 || len(files) > maxShellBaselineFiles {
			continue
		}
		for _, rel := range files {
			captureBaselineIfUntracked(filepath.Join(root, rel))
		}
	}
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// captureAuthorship mirrors a recorded edit into the Attribution working log (the
// pre-edit-baseline + diff engine in internal/authorship). It is:
//   - gated behind authorship.Enabled() (a no-op when disabled);
//   - best-effort and silent — a working-log failure must never affect the host
//     tool (this runs inside a PostToolUse hook);
//   - daemon-independent — it writes the working-log file directly, so call it
//     BEFORE any daemon POST so capture survives a down/unreachable daemon.
//
// author is derived from (tool, genType): an empty tool or a human gen_type
// (typing, copypaste) is Human; anything else is the AI tool.
func captureAuthorship(repoPath, rel, tool, genType, model string) {
	if !authorship.Enabled() || repoPath == "" || rel == "" {
		return
	}
	_, _ = authorship.RecordEdit(filepath.Join(repoPath, rel), captureAuthor(tool, genType, model))
}

// captureAuthorshipAt is captureAuthorship for a hook that already located the
// file (gitutil.Locate), sparing the git calls RecordEdit would repeat. It takes
// the shortcut only when the repo IS the work tree (RepoID == Toplevel, i.e. not a
// linked worktree); otherwise it calls captureAuthorship unchanged, so the file
// the working log describes is the same either way.
func captureAuthorshipAt(loc gitutil.Location, absPath, repoPath, rel, tool, genType, model string) {
	if !authorship.Enabled() || repoPath == "" || rel == "" {
		return
	}
	if loc.Toplevel == "" || loc.RepoID != repoPath || filepath.Clean(loc.Toplevel) != filepath.Clean(repoPath) {
		captureAuthorship(repoPath, rel, tool, genType, model)
		return
	}
	ctx, ok := authorship.ContextAt(loc, absPath)
	if !ok {
		return
	}
	_, _ = authorship.RecordEditIn(ctx, absPath, captureAuthor(tool, genType, model))
}

// captureAuthor is the working-log author for (tool, genType): an empty tool or
// a human gen_type (typing, copypaste) is Human; anything else is the AI tool.
func captureAuthor(tool, genType, model string) authorship.Author {
	if tool == "" || genType == "human" {
		return authorship.HumanAuthor()
	}
	return authorship.Author{Type: authorship.AI, Tool: tool, GenType: genType, Model: model}
}
