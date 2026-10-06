package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/blamely/blamely/internal/authorship"
)

func openCodeEvent(root, tool string, input any) openCodeHook {
	raw, _ := json.Marshal(input)
	return openCodeHook{Cwd: root, ToolName: tool, ToolInput: raw, SessionID: "opencode:session", CallID: "call", Version: 2, Model: "openai/test"}
}

func runOpenCodeHook(t *testing.T, event openCodeHook, pre bool) {
	t.Helper()
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if pre {
		err = CaptureOpenCodeFromStdin(strings.NewReader(string(raw)))
	} else {
		err = RecordOpenCodeFromStdin(strings.NewReader(string(raw)))
	}
	if err != nil {
		t.Fatal(err)
	}
}

func openCodeWrite(t *testing.T, root, path, text string) {
	t.Helper()
	file := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestOpenCodeTargets(t *testing.T) {
	patch := "*** Update File: old.txt\r\n*** Move to: new ş.txt\r\n*** Add File: a.txt\r\n*** Delete File: b.txt\r\n*** Update File: a.txt\r\n"
	for _, tool := range []string{"patch", "apply_patch"} {
		if got := openCodeTargets(openCodeEvent("", tool, map[string]any{"patchText": patch})); !reflect.DeepEqual(got, []string{"old.txt", "new ş.txt", "a.txt", "b.txt"}) {
			t.Fatal(got)
		}
	}
	for _, tool := range []string{"write", "edit", "multiedit"} {
		for _, key := range []string{"filePath", "file_path"} {
			if got := openCodeTargets(openCodeEvent("", tool, map[string]any{key: "file.txt"})); !reflect.DeepEqual(got, []string{"file.txt"}) {
				t.Fatal(got)
			}
		}
	}
	if got := openCodeTargets(openCodeEvent("", "read", map[string]any{"filePath": "file.txt"})); len(got) != 0 {
		t.Fatal(got)
	}
	if got := openCodeTargets(openCodeEvent("", "edit", nil)); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestOpenCodeGoShellCapture(t *testing.T) {
	root := openCodeRepo(t)
	openCodeWrite(t, root, ".gitignore", "ignored/\n")
	openCodeWrite(t, root, "edited.txt", "human\n")
	openCodeWrite(t, root, "untouched.txt", "dirty human\n")
	openCodeWrite(t, root, "deleted.txt", "deleted\n")
	openCodeWrite(t, root, "binary.txt", "\x00")
	event := openCodeEvent(root, "shell", map[string]any{"command": "script"})
	runOpenCodeHook(t, event, true)
	path, _ := openCodeCapturePath(event)
	if info, err := os.Stat(filepath.Join(path, "0")); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Fatalf("private snapshot: %v %v", info, err)
	}
	openCodeWrite(t, root, "edited.txt", "human\nAI\n")
	openCodeWrite(t, root, "new ş.txt", "new\n")
	openCodeWrite(t, root, "binary.txt", "now text\n")
	openCodeWrite(t, root, "ignored/output.txt", "build\n")
	if err := os.Remove(filepath.Join(root, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	// After callbacks need only identity: directory/tool/model come from Go state.
	runOpenCodeHook(t, openCodeHook{SessionID: event.SessionID, CallID: event.CallID}, false)
	logs, err := authorship.ListWorkingLogs(root, "main", "INITIAL")
	if err != nil || len(logs) != 3 {
		t.Fatalf("changed files: %+v %v", logs, err)
	}
	authors, ok := authorship.AuthorsForFile(root, "main", "INITIAL", "edited.txt")
	if !ok || authors[1].Type != authorship.Human || authors[2].Tool != "opencode" || authors[2].Model != "openai/test" {
		t.Fatal(authors)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("capture not consumed")
	}
	runOpenCodeHook(t, event, false) // duplicate completion/error callback is a no-op
}

func TestOpenCodeGoPatchRenameAndNestedCreation(t *testing.T) {
	root := openCodeRepo(t)
	openCodeWrite(t, root, "old.txt", "old\n")
	event := openCodeEvent(root, "patch", map[string]any{"patchText": "*** Update File: old.txt\n*** Move to: new/renamed.txt\n*** Add File: new/created.txt\n"})
	runOpenCodeHook(t, event, true)
	if err := os.MkdirAll(filepath.Join(root, "new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "old.txt"), filepath.Join(root, "new", "renamed.txt")); err != nil {
		t.Fatal(err)
	}
	openCodeWrite(t, root, "new/created.txt", "created\n")
	runOpenCodeHook(t, event, false)
	if logs, err := authorship.ListWorkingLogs(root, "main", "INITIAL"); err != nil || len(logs) != 3 {
		t.Fatalf("rename/create: %v %v", logs, err)
	}
}

func TestOpenCodeGoEmptyCheckoutAndBranchChange(t *testing.T) {
	root := openCodeRepo(t)
	event := openCodeEvent(root, "bash", map[string]any{"command": "script"})
	runOpenCodeHook(t, event, true)
	openCodeWrite(t, root, "new.txt", "created\n")
	runOpenCodeHook(t, event, false)
	if wl, err := authorship.LoadWorkingLog(root, "main", "INITIAL", "new.txt"); err != nil || wl == nil {
		t.Fatalf("empty checkout: %v", err)
	}
	runOpenCodeHook(t, event, true)
	if out, err := exec.Command("git", "-C", root, "symbolic-ref", "HEAD", "refs/heads/other").CombinedOutput(); err != nil {
		t.Fatalf("git: %v %s", err, out)
	}
	openCodeWrite(t, root, "new.txt", "new branch\n")
	runOpenCodeHook(t, event, false)
	if logs, err := authorship.ListWorkingLogs(root, "other", "INITIAL"); err != nil || len(logs) != 0 {
		t.Fatalf("branch change claimed: %v %v", logs, err)
	}
}

func TestOpenCodeGoSkipsInvalidPathsAndFiles(t *testing.T) {
	root := openCodeRepo(t)
	for _, path := range []string{"large.txt", "binary.txt", "invalid.txt", "long.txt", "../escape.txt", ".git/config"} {
		switch path {
		case "large.txt":
			openCodeWrite(t, root, path, strings.Repeat("x", openCodeMaxFileBytes+1))
		case "binary.txt":
			openCodeWrite(t, root, path, "\x00")
		case "invalid.txt":
			openCodeWrite(t, root, path, string([]byte{255}))
		case "long.txt":
			openCodeWrite(t, root, path, strings.Repeat("line\n", openCodeMaxLines+1))
		}
		event := openCodeEvent(root, "write", map[string]any{"filePath": path})
		runOpenCodeHook(t, event, true)
		if !strings.HasPrefix(path, "..") && !strings.HasPrefix(path, ".git") {
			openCodeWrite(t, root, path, "now text\n")
		}
		runOpenCodeHook(t, event, false)
	}
	if runtime.GOOS != "windows" {
		if err := os.Symlink(t.TempDir(), filepath.Join(root, "escape")); err != nil {
			t.Fatal(err)
		}
		event := openCodeEvent(root, "write", map[string]any{"filePath": "escape/new/file.txt"})
		runOpenCodeHook(t, event, true)
		openCodeWrite(t, root, "escape/new/file.txt", "outside\n")
		runOpenCodeHook(t, event, false)
	}
	if logs, err := authorship.ListWorkingLogs(root, "main", "INITIAL"); err != nil || len(logs) != 0 {
		t.Fatalf("invalid file claimed: %v %v", logs, err)
	}
}

func TestOpenCodeGoSessionIsolationCleanupAndExpiry(t *testing.T) {
	root := openCodeRepo(t)
	openCodeWrite(t, root, "file.txt", "before\n")
	event := openCodeEvent(root, "edit", map[string]any{"filePath": "file.txt"})
	runOpenCodeHook(t, event, true)
	other := event
	other.SessionID = "../other/session"
	path, _ := openCodeCapturePath(event)
	otherPath, _ := openCodeCapturePath(other)
	if path == otherPath || strings.Contains(otherPath, "other/session") {
		t.Fatal("unsafe session key")
	}
	runOpenCodeHook(t, other, false)
	if _, err := os.Stat(path); err != nil {
		t.Fatal("other session consumed capture")
	}
	openCodeWrite(t, root, "file.txt", "after\n")
	runOpenCodeHook(t, event, true) // a duplicate pre-hook must not overwrite before
	data, err := os.ReadFile(filepath.Join(path, "0"))
	if err != nil || string(data) != "before\n" {
		t.Fatalf("duplicate pre: %q %v", data, err)
	}
	runOpenCodeHook(t, openCodeHook{SessionID: event.SessionID, Phase: "discard"}, false)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("discard did not clean state")
	}
	runOpenCodeHook(t, event, true)
	old := time.Now().Add(-2 * openCodeCaptureAge)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	runOpenCodeHook(t, event, false)
	if logs, err := authorship.ListWorkingLogs(root, "main", "INITIAL"); err != nil || len(logs) != 0 {
		t.Fatalf("expired capture claimed: %v %v", logs, err)
	}
}

func TestOpenCodeGoSnapshotBudget(t *testing.T) {
	root := openCodeRepo(t)
	for index := 0; index < 65; index++ {
		openCodeWrite(t, root, fmt.Sprintf("f%02d.txt", index), strings.Repeat("x", openCodeMaxFileBytes))
	}
	event := openCodeEvent(root, "shell", map[string]any{"command": "script"})
	runOpenCodeHook(t, event, true)
	path, _ := openCodeCapturePath(event)
	data, err := os.ReadFile(filepath.Join(path, "capture.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state openCodeCapture
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Files) != 64 || len(state.Known) != 65 {
		t.Fatalf("budget: %d captured, %d known", len(state.Files), len(state.Known))
	}
	openCodeWrite(t, root, "f64.txt", "excluded file changed\n")
	runOpenCodeHook(t, event, false)
	if logs, err := authorship.ListWorkingLogs(root, "main", "INITIAL"); err != nil || len(logs) != 0 {
		t.Fatalf("uncaptured existing file claimed as new: %v %v", logs, err)
	}
}
