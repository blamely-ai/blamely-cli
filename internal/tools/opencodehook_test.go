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
		for _, key := range []string{"path", "filePath", "file_path"} {
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

func TestOpenCodeGoShellDoesNotClaimConcurrentHumanEdits(t *testing.T) {
	for _, tool := range []string{"shell", "bash"} {
		t.Run(tool, func(t *testing.T) {
			root := openCodeRepo(t)
			openCodeWrite(t, root, "edited.txt", "human\n")
			event := openCodeEvent(root, tool, map[string]any{"command": "sleep 1"})
			runOpenCodeHook(t, event, true)
			path, _ := openCodeCapturePath(event)
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("ambiguous shell snapshot created: %v", err)
			}
			openCodeWrite(t, root, "edited.txt", "human saved concurrently\n")
			openCodeWrite(t, root, "new.txt", "human created concurrently\n")
			runOpenCodeHook(t, event, false)
			if logs, err := authorship.ListWorkingLogs(root, "main", "INITIAL"); err != nil || len(logs) != 0 {
				t.Fatalf("human edits claimed: %+v %v", logs, err)
			}
		})
	}
}

func TestOpenCodeGoNativeEditAndWrite(t *testing.T) {
	for _, version := range []int{1, 2} {
		for _, tool := range []string{"edit", "write"} {
			t.Run(fmt.Sprintf("V%d/%s", version, tool), func(t *testing.T) {
				root := openCodeRepo(t)
				openCodeWrite(t, root, "file.txt", "human\n")
				key := "filePath"
				if version == 2 {
					key = "path"
				}
				event := openCodeEvent(root, tool, map[string]any{key: filepath.Join(root, "file.txt")})
				event.Version = version
				runOpenCodeHook(t, event, true)
				path, _ := openCodeCapturePath(event)
				if info, err := os.Stat(filepath.Join(path, "0")); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
					t.Fatalf("private snapshot: %v %v", info, err)
				}
				openCodeWrite(t, root, "file.txt", "human\nAI\n")
				// After callbacks need only identity; metadata comes from Go state.
				runOpenCodeHook(t, openCodeHook{SessionID: event.SessionID, CallID: event.CallID}, false)
				authors, ok := authorship.AuthorsForFile(root, "main", "INITIAL", "file.txt")
				if !ok || authors[1].Type != authorship.Human || authors[2].Tool != "opencode" || authors[2].Model != "openai/test" {
					t.Fatal(authors)
				}
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("capture not consumed")
				}
				runOpenCodeHook(t, event, false) // duplicate completion is a no-op
			})
		}
	}
}

func TestOpenCodeGoDiscardsLegacyShellSnapshot(t *testing.T) {
	root := openCodeRepo(t)
	event := openCodeEvent(root, "shell", nil)
	path, _ := openCodeCapturePath(event)
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest, _ := json.Marshal(map[string]any{"event": event, "revision": openCodeRevision(root), "shell": true, "known": []string{"file.txt"}, "files": []string{"file.txt"}})
	if err := os.WriteFile(filepath.Join(path, "capture.json"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "0"), []byte("human\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	openCodeWrite(t, root, "file.txt", "human saved concurrently\n")
	runOpenCodeHook(t, event, false)
	if logs, err := authorship.ListWorkingLogs(root, "main", "INITIAL"); err != nil || len(logs) != 0 {
		t.Fatalf("legacy shell snapshot claimed human edit: %+v %v", logs, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("legacy snapshot not consumed")
	}
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
	if logs, err := authorship.ListWorkingLogs(root, "main", "INITIAL"); err != nil || len(logs) != 2 {
		t.Fatalf("rename/create: %v %v", logs, err)
	}
	authors, ok := authorship.AuthorsForFile(root, "main", "INITIAL", "new/renamed.txt")
	if !ok || authors[1].Type != authorship.Human {
		t.Fatalf("renamed human content claimed: %+v", authors)
	}
}

func TestOpenCodeGoEmptyCheckoutAndBranchChange(t *testing.T) {
	root := openCodeRepo(t)
	event := openCodeEvent(root, "write", map[string]any{"path": "new.txt"})
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

func TestOpenCodeGoPatchRenamePreservesPreviousAI(t *testing.T) {
	for _, edited := range []bool{false, true} {
		t.Run(fmt.Sprintf("edited=%v", edited), func(t *testing.T) {
			root := openCodeRepo(t)
			previousAI := authorship.Author{Type: authorship.AI, Tool: "claude", Model: "previous/model"}
			before := "human\nprevious AI\nhuman later\n"
			if _, err := authorship.Update(root, "main", "INITIAL", "old.txt", "human\nprevious AI\n", "human\n", previousAI, 1); err != nil {
				t.Fatal(err)
			}
			openCodeWrite(t, root, "old.txt", before)
			event := openCodeEvent(root, "patch", map[string]any{"patchText": "*** Update File: old.txt\n*** Move to: new.txt\n"})
			runOpenCodeHook(t, event, true)
			if err := os.Rename(filepath.Join(root, "old.txt"), filepath.Join(root, "new.txt")); err != nil {
				t.Fatal(err)
			}
			if edited {
				openCodeWrite(t, root, "new.txt", before+"new AI\n")
			}
			runOpenCodeHook(t, event, false)
			authors, ok := authorship.AuthorsForFile(root, "main", "INITIAL", "new.txt")
			if !ok || authors[1].Type != authorship.Human || authors[2] != previousAI || authors[3].Type != authorship.Human || (edited && authors[4].Tool != "opencode") {
				t.Fatalf("lost rename authorship: %+v", authors)
			}
			wl, err := authorship.LoadWorkingLog(root, "main", "INITIAL", "old.txt")
			if err != nil || wl != nil {
				t.Fatalf("stale source log: %+v %v", wl, err)
			}
			if _, err := os.Stat(authorship.BaselinePath(root, "main", "INITIAL", "old.txt")); !os.IsNotExist(err) {
				t.Fatalf("stale source baseline: %v", err)
			}
			if deletions, err := authorship.LoadDeletions(root, "main", "INITIAL"); err != nil || len(deletions) != 0 {
				t.Fatalf("pure move recorded deletions: %+v %v", deletions, err)
			}
		})
	}
}

func TestOpenCodeGoFailedPatchDoesNotTransferAuthorship(t *testing.T) {
	root := openCodeRepo(t)
	openCodeWrite(t, root, "old.txt", "human\n")
	event := openCodeEvent(root, "patch", map[string]any{"patchText": "*** Update File: old.txt\n*** Move to: new.txt\n"})
	runOpenCodeHook(t, event, true)
	runOpenCodeHook(t, event, false)
	if logs, err := authorship.ListWorkingLogs(root, "main", "INITIAL"); err != nil || len(logs) != 0 {
		t.Fatalf("failed rename changed authorship: %+v %v", logs, err)
	}
}

func TestOpenCodeGoRenameWithoutSourceBaselineIsSkipped(t *testing.T) {
	root := openCodeRepo(t)
	openCodeWrite(t, root, "old.txt", strings.Repeat("x", openCodeMaxFileBytes+1))
	event := openCodeEvent(root, "patch", map[string]any{"patchText": "*** Update File: old.txt\n*** Move to: new.txt\n"})
	runOpenCodeHook(t, event, true)
	if err := os.Rename(filepath.Join(root, "old.txt"), filepath.Join(root, "new.txt")); err != nil {
		t.Fatal(err)
	}
	openCodeWrite(t, root, "new.txt", "now small\n")
	runOpenCodeHook(t, event, false)
	if logs, err := authorship.ListWorkingLogs(root, "main", "INITIAL"); err != nil || len(logs) != 0 {
		t.Fatalf("uncaptured rename claimed: %+v %v", logs, err)
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
	var patch strings.Builder
	for index := 0; index < 65; index++ {
		openCodeWrite(t, root, fmt.Sprintf("f%02d.txt", index), strings.Repeat("x", openCodeMaxFileBytes))
		fmt.Fprintf(&patch, "*** Update File: f%02d.txt\n", index)
	}
	event := openCodeEvent(root, "patch", map[string]any{"patchText": patch.String()})
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
	if len(state.Files) != 64 {
		t.Fatalf("budget: %d captured", len(state.Files))
	}
	openCodeWrite(t, root, "f64.txt", "excluded file changed\n")
	runOpenCodeHook(t, event, false)
	if logs, err := authorship.ListWorkingLogs(root, "main", "INITIAL"); err != nil || len(logs) != 0 {
		t.Fatalf("uncaptured existing file claimed as new: %v %v", logs, err)
	}
}
