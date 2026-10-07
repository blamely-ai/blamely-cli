package tools

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/blamely/blamely/internal/config"
	"github.com/blamely/blamely/internal/gitutil"
)

const (
	openCodeMaxFileBytes     = 1 << 20
	openCodeMaxLines         = 4000
	openCodeMaxSnapshotBytes = 64 << 20
	openCodeMaxManifestBytes = 16 << 20
	openCodeCaptureAge       = 24 * time.Hour
)

// Plugins only forward events. Both lifecycle phases run in separate Go
// processes, with private per-session/per-call snapshots connecting them.
type openCodeHook struct {
	Cwd       string          `json:"cwd"`
	SessionID string          `json:"session_id"`
	CallID    string          `json:"call_id"`
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input,omitempty"`
	Version   int             `json:"version"`
	Model     string          `json:"model"`
	Phase     string          `json:"phase,omitempty"`
}

type openCodeCapture struct {
	Event    openCodeHook      `json:"event"`
	Revision string            `json:"revision"`
	Moves    map[string]string `json:"moves,omitempty"` // destination → source
	Files    []string          `json:"files"`
}

func decodeOpenCodeHook(r io.Reader) (openCodeHook, error) {
	var event openCodeHook
	raw, err := readHookPayload(r)
	if err != nil {
		return event, err
	}
	if err := json.Unmarshal(raw, &event); err != nil {
		return event, fmt.Errorf("parse OpenCode hook: %w", err)
	}
	if event.SessionID == "" || (event.CallID == "" && event.Phase != "discard") {
		return event, fmt.Errorf("session_id, call_id required")
	}
	return event, nil
}

func CaptureOpenCodeFromStdin(r io.Reader) error {
	event, err := decodeOpenCodeHook(r)
	if err != nil {
		return err
	}
	return captureOpenCodeHook(event)
}

func RecordOpenCodeFromStdin(r io.Reader) error {
	event, err := decodeOpenCodeHook(r)
	if err != nil {
		return err
	}
	path, err := openCodeCapturePath(event)
	if err != nil {
		return err
	}
	if event.Phase == "discard" {
		return os.RemoveAll(filepath.Dir(path))
	}
	return finishOpenCodeHook(path)
}

func openCodeHash(value string) string {
	hash := sha256.Sum256([]byte(value))
	return hex.EncodeToString(hash[:])
}

func openCodeCapturePath(event openCodeHook) (string, error) {
	dir, err := config.BlamelyDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "opencode", openCodeHash(event.SessionID), openCodeHash(event.CallID)), nil
}

var openCodePatchHeader = regexp.MustCompile(`(?m)^\*\*\* ((?:Add|Update|Delete) File|Move to): ([^\r\n]+)\r?$`)

func openCodeTargets(event openCodeHook) []string {
	paths, _ := openCodeTargetDetails(event)
	return paths
}

func openCodeTargetDetails(event openCodeHook) ([]string, map[string]string) {
	var args struct {
		Path          string `json:"path"`
		FilePath      string `json:"filePath"`
		FilePathSnake string `json:"file_path"`
		PatchText     string `json:"patchText"`
		Patch         string `json:"patch"`
		Input         string `json:"input"`
	}
	if json.Unmarshal(event.ToolInput, &args) != nil {
		return nil, nil
	}
	switch event.ToolName {
	case "write", "edit", "multiedit":
		if path := firstNonEmpty(args.Path, args.FilePath, args.FilePathSnake); path != "" {
			return []string{path}, nil
		}
	case "patch", "apply_patch":
		var paths []string
		seen := map[string]bool{}
		moves := map[string]string{}
		source := ""
		for _, match := range openCodePatchHeader.FindAllStringSubmatch(firstNonEmpty(args.PatchText, args.Patch, args.Input), -1) {
			if match[1] == "Move to" {
				if source != "" && source != match[2] {
					moves[match[2]] = source
				}
				source = ""
			} else if match[1] == "Update File" {
				source = match[2]
			} else {
				source = ""
			}
			if !seen[match[2]] {
				paths = append(paths, match[2])
				seen[match[2]] = true
			}
		}
		return paths, moves
	}
	return nil, nil
}

func openCodeRevision(root string) string {
	head, _ := gitutil.OutputTimeout(5*time.Second, root, "rev-parse", "--verify", "HEAD")
	branch, _ := gitutil.OutputTimeout(5*time.Second, root, "symbolic-ref", "--quiet", "HEAD")
	return string(head) + "\x00" + string(branch)
}

func openCodeContent(root, path string) (string, bool) {
	abs := filepath.Join(root, path)
	if filepath.IsAbs(path) {
		abs = path
	}
	resolved := openCodeResolvedPath(abs)
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) || rel == ".git" || strings.HasPrefix(rel, ".git"+string(filepath.Separator)) {
		return "", false
	}
	info, err := os.Lstat(abs)
	if os.IsNotExist(err) {
		return "", true
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > openCodeMaxFileBytes {
		return "", false
	}
	file, err := os.Open(abs)
	if err != nil {
		return "", false
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, openCodeMaxFileBytes+1))
	if err != nil || len(data) > openCodeMaxFileBytes || bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) || bytes.Count(data, []byte{'\n'})+1 > openCodeMaxLines {
		return "", false
	}
	return string(data), true
}

func captureOpenCodeHook(event openCodeHook) error {
	// A repository-wide shell diff cannot distinguish a tool write from a
	// concurrent human save. Only capture tools with explicit file targets.
	paths, moves := openCodeTargetDetails(event)
	if len(paths) == 0 {
		return nil
	}
	if event.Cwd == "" {
		return fmt.Errorf("cwd required")
	}
	event.Cwd = resolveSymlinks(event.Cwd)
	root, ok := gitutil.Toplevel(event.Cwd)
	if !ok {
		return nil
	}
	resolve := func(path string) string {
		if !filepath.IsAbs(path) {
			path = filepath.Join(event.Cwd, path)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return ""
		}
		return rel
	}
	resolved := []string{}
	seen := map[string]bool{}
	for _, path := range paths {
		if rel := resolve(path); rel != "" && !seen[rel] {
			resolved = append(resolved, rel)
			seen[rel] = true
		}
	}
	resolvedMoves := map[string]string{}
	for dest, source := range moves {
		if dest, source = resolve(dest), resolve(source); dest != "" && source != "" && dest != source {
			resolvedMoves[dest] = source
		}
	}
	paths = resolved
	event.Cwd, event.ToolInput, event.Phase = root, nil, ""
	state := openCodeCapture{Event: event, Moves: resolvedMoves, Revision: openCodeRevision(root)}
	path, err := openCodeCapturePath(event)
	if err != nil {
		return err
	}
	pruneOpenCodeCaptures(filepath.Dir(filepath.Dir(path)))
	if _, err := os.Stat(path); err == nil {
		return nil
	} // duplicate pre-hook: keep the original before-image
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(path), ".capture-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	bytes := 0
	for _, rel := range paths {
		text, ok := openCodeContent(root, rel)
		if !ok {
			continue
		}
		bytes += len(text)
		if bytes > openCodeMaxSnapshotBytes {
			break
		}
		if err := os.WriteFile(filepath.Join(tmp, strconv.Itoa(len(state.Files))), []byte(text), 0o600); err != nil {
			return err
		}
		state.Files = append(state.Files, rel)
	}
	manifest, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if len(manifest) > openCodeMaxManifestBytes {
		return fmt.Errorf("OpenCode capture manifest exceeds limit")
	}
	if err := os.WriteFile(filepath.Join(tmp, "capture.json"), manifest, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func finishOpenCodeHook(path string) error {
	// Atomically claim the capture so duplicate after/error callbacks cannot
	// record the same edit twice, even across separate CLI processes.
	claimed := path + ".processing"
	if err := os.Rename(path, claimed); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer os.RemoveAll(claimed)
	info, err := os.Stat(claimed)
	if err != nil {
		return err
	}
	if time.Since(info.ModTime()) > openCodeCaptureAge {
		return nil
	}
	file, err := os.Open(filepath.Join(claimed, "capture.json"))
	if err != nil {
		return err
	}
	defer file.Close()
	var state openCodeCapture
	if err := json.NewDecoder(io.LimitReader(file, openCodeMaxManifestBytes+1)).Decode(&state); err != nil {
		return err
	}
	// Also discard shell snapshots created by older plugin/CLI versions.
	if state.Event.ToolName == "shell" || state.Event.ToolName == "bash" {
		return nil
	}
	if openCodeRevision(state.Event.Cwd) != state.Revision {
		return nil
	}
	indices := map[string]int{}
	for index, rel := range state.Files {
		indices[rel] = index
	}
	moved := map[string]string{}
	skip := map[string]bool{}
	for dest, source := range state.Moves {
		_, sourceErr := os.Lstat(filepath.Join(state.Event.Cwd, source))
		destInfo, destErr := os.Lstat(filepath.Join(state.Event.Cwd, dest))
		if !os.IsNotExist(sourceErr) || destErr != nil || !destInfo.Mode().IsRegular() {
			continue // failed or partial patch: do not transfer authorship
		}
		skip[source] = true
		if _, ok := indices[source]; !ok {
			skip[dest] = true // no source baseline: never claim moved content as new
			continue
		}
		moved[dest] = source
	}
	bytes := 0
	for index, rel := range state.Files {
		if skip[rel] {
			continue
		}
		after, ok := openCodeContent(state.Event.Cwd, rel)
		if !ok {
			continue
		}
		bytes += len(after)
		if bytes > openCodeMaxSnapshotBytes {
			break
		}
		previousPath := moved[rel]
		if previousPath != "" {
			index = indices[previousPath]
		}
		data, err := os.ReadFile(filepath.Join(claimed, strconv.Itoa(index)))
		if err != nil {
			return err // missing baseline is not an empty/new file
		}
		before := string(data)
		if before == after && previousPath == "" {
			continue
		}
		event := state.Event
		if err := recordOpenCodeEdit(openCodePayload{Cwd: event.Cwd, SessionID: event.SessionID, CallID: event.CallID, ToolName: event.ToolName, Version: event.Version, Model: event.Model, FilePath: rel, PreviousPath: previousPath, Before: before, After: after}); err != nil {
			return err
		}
	}
	return nil
}

// Interrupted tools can leave private snapshots behind. Expire them lazily,
// at most once per hour; session teardown can discard them immediately.
func pruneOpenCodeCaptures(root string) {
	marker := filepath.Join(root, ".gc")
	if info, err := os.Stat(marker); err == nil && time.Since(info.ModTime()) < time.Hour {
		return
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return
	}
	_ = os.WriteFile(marker, nil, 0o600)
	sessions, _ := os.ReadDir(root)
	for _, session := range sessions {
		if !session.IsDir() {
			continue
		}
		parent := filepath.Join(root, session.Name())
		captures, _ := os.ReadDir(parent)
		for _, capture := range captures {
			if info, err := capture.Info(); err == nil && time.Since(info.ModTime()) > openCodeCaptureAge {
				_ = os.RemoveAll(filepath.Join(parent, capture.Name()))
			}
		}
		_ = os.Remove(parent) // only empty session directories
	}
}
