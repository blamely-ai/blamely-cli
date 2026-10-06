package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/blamely/blamely/internal/authorship"
	"github.com/blamely/blamely/internal/daemon"
	"github.com/blamely/blamely/internal/gitutil"
)

// openCodePayload is an internal captured edit, not the plugin wire protocol.
// Before/after content is discovered and read by the Go hook lifecycle.
type openCodePayload struct {
	Cwd       string `json:"cwd"`
	SessionID string `json:"session_id"`
	CallID    string `json:"call_id"`
	ToolName  string `json:"tool_name"`
	Version   int    `json:"version"`
	Model     string `json:"model"`
	FilePath  string `json:"file_path"`
	Before    string `json:"before"`
	After     string `json:"after"`
}

func recordOpenCodeEdit(p openCodePayload) error {
	if p.Before == p.After {
		return nil
	}
	if p.Cwd == "" || p.FilePath == "" {
		return fmt.Errorf("cwd, file_path required")
	}
	top, ok := gitutil.Toplevel(p.Cwd)
	if !ok {
		return nil
	}
	abs := p.FilePath
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(top, abs)
	}
	abs = openCodeResolvedPath(abs)
	rel, err := filepath.Rel(top, abs)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) || rel == ".git" || strings.HasPrefix(rel, ".git"+string(filepath.Separator)) {
		return fmt.Errorf("file outside checkout")
	}
	// Resolve from the checkout so removed files/new directories also work.
	ctx, ok := authorship.ResolveContext(filepath.Join(top, ".blamely-context"))
	if !ok {
		return nil
	}
	ctx.RelPath = filepath.ToSlash(rel)
	// Until checkout-local storage is available, fail closed rather than send a
	// worktree edit under the main checkout's HEAD. The worktree fix makes the
	// working-log path reside in the actual gitdir, so this guard stops applying.
	if info, err := os.Stat(filepath.Join(top, ".git")); err == nil && !info.IsDir() && strings.HasPrefix(authorship.WorkingLogPath(top, ctx.Branch, ctx.BaseSHA, ctx.RelPath), filepath.Join(top, ".git")+string(filepath.Separator)) {
		return fmt.Errorf("linked worktree recording requires checkout-local working-log support")
	}
	if authorship.SeedHook != nil {
		authorship.SeedHook(top, ctx.Branch, ctx.BaseSHA, ctx.RelPath)
	}
	// First fold any unobserved pre-existing edits as Human, preserving previous
	// AI attribution. Then fold ONLY this tool's actual changes as OpenCode.
	if _, err := authorship.Update(top, ctx.Branch, ctx.BaseSHA, ctx.RelPath, p.Before, p.Before, authorship.HumanAuthor(), 0); err != nil {
		return err
	}
	if _, err := authorship.Update(top, ctx.Branch, ctx.BaseSHA, ctx.RelPath, p.After, p.Before, authorship.Author{Type: authorship.AI, Tool: "opencode", GenType: "chat", Model: p.Model}, 0); err != nil {
		return err
	}
	repoID, _ := gitutil.RepoID(top)
	meta, _ := json.Marshal(map[string]any{"source": "opencode_plugin", "session_id": p.SessionID, "call_id": p.CallID, "tool": p.ToolName, "version": p.Version})
	return postToDaemon(daemon.EditPayload{
		Tool: "opencode", Confidence: "high", GenType: "chat", Model: p.Model,
		RepoPath: repoID, WorktreePath: top, Branch: ctx.Branch, FilePath: ctx.RelPath,
		Lines:          toDaemonRanges(narrowWholeFileAddedLines(p.Before, p.After)),
		RemovedLines:   toDaemonRemovedLines(RemovedLineHashes(p.Before, p.After)),
		SuggestedLines: int64(countLines(p.After)), RawMeta: string(meta),
	})
}

// Resolve symlinked existing ancestors even when the file/new directories have
// not been written yet, so a missing leaf cannot bypass the checkout boundary.
func openCodeResolvedPath(path string) string {
	ancestor := path
	for {
		if resolved, err := filepath.EvalSymlinks(ancestor); err == nil {
			rel, _ := filepath.Rel(ancestor, path)
			return filepath.Join(resolved, rel)
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return path
		}
		ancestor = parent
	}
}
