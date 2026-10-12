package tools

import (
	"bufio"
	"encoding/json"
	"github.com/blamely/blamely/internal/gitutil"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// CursorCommitFileOps scans Cursor's agent transcripts for repoRoot and returns
// the repo-relative file targets the assistant WROTE and DELETED via tool calls,
// for transcript files last modified at/after sinceNanos.
//
// It mirrors ClaudeCommitFileOps to attribute the atomic "rm f && git commit" /
// "git rm f && git commit" pattern a Cursor agent runs through its Shell tool:
// the deletion commits in one command, so the post-commit note is written before
// (or entirely without) the PostToolUse hook recording the edit, and afterwards
// the working tree is clean — but Cursor's agent transcript still records the
// Shell/Delete/Write tool call that proves the AI did it.
//
// Cursor's transcript lines carry NO per-line timestamp (only role+message), so —
// unlike Claude — the window is applied by transcript-file mtime, not line time.
// This is safe because the caller only flips files ACTUALLY added/deleted in the
// commit whose exact path the transcript named; an unrelated stale op in the same
// session file can't claim a file the commit didn't touch.
// cursorHomeDir resolves the user's home directory. It's a package var rather than
// a direct os.UserHomeDir call so tests can pin it to a temp dir and stay hermetic —
// independent of the process-global HOME env, which sibling tests in this package
// mutate via t.Setenv (a momentarily-empty HOME there made os.UserHomeDir error and
// this function return nothing, flaking the transcript tests under CI load).
var cursorHomeDir = os.UserHomeDir

func CursorCommitFileOps(repoRoot string, sinceNanos int64) (written, deleted []string) {
	home, err := cursorHomeDir()
	if err != nil {
		return nil, nil
	}
	// <cwd-encoded> is the repo root with the leading slash removed and / -> -,
	// matching cursorTranscriptPath in claude.go.
	proj := strings.ReplaceAll(strings.TrimPrefix(filepath.ToSlash(repoRoot), "/"), "/", "-")
	// Resolve the project dir tolerantly. On Windows the encoded name diverges from
	// what we compute here: git's toplevel gives a lowercase drive (c:/dev/proj)
	// while Cursor encodes the editor's uppercase-drive cwd, AND a Windows folder
	// can't contain ':' so the drive colon is substituted (e.g. C--dev-proj). An
	// exact Join would miss and the deletion would fall back to Human. So match
	// case-insensitively, and if nothing matches, fall back to comparing the
	// drive-stripped tail (the path portion both sides agree on). On macOS/Linux the
	// name is already exact and has no drive prefix, so both tiers are a no-op.
	projsBase := filepath.Join(home, ".cursor", "projects")
	dirName := proj
	if entries, err := os.ReadDir(projsBase); err == nil {
		wantTail := stripDrivePrefix(proj)
		var exact, tail string
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if strings.EqualFold(e.Name(), proj) {
				exact = e.Name()
				break
			}
			if tail == "" && wantTail != proj && strings.EqualFold(stripDrivePrefix(e.Name()), wantTail) {
				tail = e.Name()
			}
		}
		if exact != "" {
			dirName = exact
		} else if tail != "" {
			dirName = tail
		}
	}
	base := filepath.Join(projsBase, dirName, "agent-transcripts")
	sessDirs, err := os.ReadDir(base)
	if err != nil {
		return nil, nil
	}
	wset, dset := map[string]bool{}, map[string]bool{}
	for _, sd := range sessDirs {
		if !sd.IsDir() {
			continue
		}
		dir := filepath.Join(base, sd.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range files {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			// A transcript last written before the window opened can't hold a
			// deletion committed in it.
			if info, err := e.Info(); err == nil && info.ModTime().UnixNano() < sinceNanos {
				continue
			}
			scanCursorTranscriptOps(filepath.Join(dir, e.Name()), repoRoot, wset, dset)
		}
	}
	return mapKeys(wset), mapKeys(dset)
}

// cursorOpToolUse is one entry of a Cursor assistant message's content array.
// Cursor names the write/delete path field `path` (Claude uses `file_path`) and,
// like Claude, carries shell commands in `command`.
type cursorOpToolUse struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Input struct {
		Path    string `json:"path"`
		Command string `json:"command"`
	} `json:"input"`
}

func scanCursorTranscriptOps(path, repoRoot string, wset, dset map[string]bool) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	for sc.Scan() {
		// Cursor lines are {"role":..,"message":{"content":[...]}} with no timestamp.
		var ent struct {
			Message json.RawMessage `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &ent) != nil {
			continue
		}
		var msg struct {
			Content []cursorOpToolUse `json:"content"`
		}
		if json.Unmarshal(ent.Message, &msg) != nil {
			continue
		}
		for _, u := range msg.Content {
			if u.Type != "tool_use" {
				continue
			}
			switch u.Name {
			case "Write":
				// Whole-file writes only. Partial StrReplace/ApplyPatch edits are
				// recorded per-content by the hook and must not trigger a whole-file
				// flip here (they carry no `path`-only whole-file semantics anyway).
				if p := cursorRelTarget(repoRoot, u.Input.Path); p != "" {
					wset[p] = true
				}
			case "Delete":
				if p := cursorRelTarget(repoRoot, u.Input.Path); p != "" {
					dset[p] = true
				}
			case "Shell", "AwaitShell":
				for _, t := range bashRedirectTargets(u.Input.Command) {
					wset[t] = true
				}
				for _, t := range shellDeleteTargets(u.Input.Command) {
					dset[t] = true
				}
			}
		}
	}
}

// driveEncPrefixRe matches an encoded Windows drive prefix at the start of a
// project-dir name: a single letter followed by the run of ':'/'-' the encoding
// leaves in place of the drive colon and first separator (C:-, C--, c-, …).
var driveEncPrefixRe = regexp.MustCompile(`^[A-Za-z][:\-]+`)

// stripDrivePrefix removes an encoded Windows drive prefix so a folder Cursor
// named from an uppercase-drive, colon-substituted cwd can be matched against the
// lowercase-drive path git reports. Names without such a prefix (every macOS/Linux
// name, e.g. "Users-dev-proj") are returned unchanged.
func stripDrivePrefix(name string) string {
	return driveEncPrefixRe.ReplaceAllString(name, "")
}

// cursorRelTarget converts a Cursor tool path (usually absolute) to a repo-
// relative slash path so MatchesFileOp can match it exactly; a path outside the
// repo falls back to its cleaned literal (matched by basename downstream).
func cursorRelTarget(repoRoot, p string) string {
	if p == "" {
		return ""
	}
	if rel, ok := gitutil.Rel(repoRoot, p); ok {
		return filepath.ToSlash(rel)
	}
	return cleanTarget(p)
}
