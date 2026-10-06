package tools

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blamely/blamely/internal/authorship"
	"github.com/blamely/blamely/internal/daemon"
)

func openCodeRepo(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	root := t.TempDir()
	cmd := exec.Command("git", "-C", root, "init", "-qb", "main")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func recordOpenCode(t *testing.T, payload openCodePayload) {
	t.Helper()
	if err := recordOpenCodeEdit(payload); err != nil {
		t.Fatal(err)
	}
}

func TestOpenCodeCapturePreservesHumanAndPreviousAI(t *testing.T) {
	root := openCodeRepo(t)
	p := openCodePayload{Cwd: root, FilePath: "new/file.txt", Version: 2, SessionID: "opencode:session", Model: "openai/test", Before: "human\n", After: "human\nAI\n"}
	recordOpenCode(t, p) // Captured content, not a re-read of potentially newer disk content.
	wl, err := authorship.LoadWorkingLog(root, "main", "INITIAL", "new/file.txt")
	if err != nil || wl == nil {
		t.Fatalf("working log: %v %v", wl, err)
	}
	if wl.Lines[0].Author.Type != authorship.Human || wl.Lines[len(wl.Lines)-1].Author.Tool != "opencode" {
		t.Fatalf("incorrect first capture: %+v", wl.Lines)
	}
	// A human edit between tool calls must not be claimed by the next AI write.
	p.Before = "human\nAI\nhuman later\n"
	p.After = "human\nAI\nhuman later\nAI later\n"
	recordOpenCode(t, p)
	authors, ok := authorship.AuthorsForFile(root, "main", "INITIAL", "new/file.txt")
	if !ok || authors[1].Type != authorship.Human || authors[2].Tool != "opencode" || authors[3].Type != authorship.Human || authors[4].Tool != "opencode" || authors[4].Model != "openai/test" {
		t.Fatalf("incorrect subsequent capture: %+v", authors)
	}
	p.Before, p.After = p.After, ""
	recordOpenCode(t, p)
	wl, err = authorship.LoadWorkingLog(root, "main", "INITIAL", "new/file.txt")
	if err != nil || wl == nil || len(wl.Lines) != 0 {
		t.Fatalf("deleted file log: %v %v", wl, err)
	}
}

func TestOpenCodePayloadCarriesNativeIdentityAndChangedLines(t *testing.T) {
	root := openCodeRepo(t)
	received := make(chan daemon.EditPayload, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p daemon.EditPayload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Errorf("decode: %v", err)
		}
		received <- p
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	if err := os.MkdirAll(filepath.Join(os.Getenv("HOME"), ".blamely"), 0o755); err != nil {
		t.Fatal(err)
	}
	port := server.URL[strings.LastIndex(server.URL, ":")+1:]
	if err := os.WriteFile(filepath.Join(os.Getenv("HOME"), ".blamely", "daemon.port"), []byte(port), 0o644); err != nil {
		t.Fatal(err)
	}
	recordOpenCode(t, openCodePayload{Cwd: root, FilePath: "file.txt", Before: "human\nold\n", After: "human\nAI\n", Version: 1, Model: "anthropic/test", SessionID: "opencode:v1", CallID: "call", ToolName: "bash"})
	p := <-received
	if p.Tool != "opencode" || p.GenType != "chat" || p.Model != "anthropic/test" || p.WorktreePath != root || p.Branch != "main" {
		t.Fatalf("incorrect metadata: %+v", p)
	}
	if len(p.Lines) != 1 || p.Lines[0].Start != 2 || len(p.RemovedLines) != 1 {
		t.Fatalf("incorrect changed lines: %+v", p)
	}
	if !strings.Contains(p.RawMeta, "opencode:v1") || !strings.Contains(p.RawMeta, `"version":1`) {
		t.Fatal(p.RawMeta)
	}
}

func TestOpenCodeRejectsEscapedPathsAndMalformedPayload(t *testing.T) {
	root := openCodeRepo(t)
	for _, input := range []string{"{", `{"cwd":"` + filepath.ToSlash(root) + `","file_path":"../outside.txt","before":"old","after":"new"}`} {
		if err := RecordOpenCodeFromStdin(strings.NewReader(input)); err == nil {
			t.Fatalf("accepted invalid input: %s", input)
		}
	}
}
