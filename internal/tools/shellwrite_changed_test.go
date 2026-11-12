package tools

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/blamely/blamely/internal/daemon"
)

// captureDaemonPosts starts a fake daemon under the isolated HOME and returns a
// function listing every /edit payload it received so far.
func captureDaemonPosts(t *testing.T) func() []daemon.EditPayload {
	t.Helper()
	var mu sync.Mutex
	var got []daemon.EditPayload
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p daemon.EditPayload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Errorf("decode: %v", err)
		}
		mu.Lock()
		got = append(got, p)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	dir := filepath.Join(os.Getenv("HOME"), ".blamely")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	port := server.URL[strings.LastIndex(server.URL, ":")+1:]
	if err := os.WriteFile(filepath.Join(dir, "daemon.port"), []byte(port), 0o644); err != nil {
		t.Fatal(err)
	}
	return func() []daemon.EditPayload {
		mu.Lock()
		defer mu.Unlock()
		return append([]daemon.EditPayload(nil), got...)
	}
}

// A shell command that changes one line must report that line to the daemon, not
// the whole file. The commit-time reconcile credits a Human line whose content an
// AI edit recorded, so a whole-file report handed the agent the human's own
// uncommitted lines in the same file at commit, although the working log had them
// right. Reproduced on Windows with Copilot's powershell tool in VS Code: a line
// typed by hand committed as Copilot's, and "16 suggested -> 2 kept".
func TestRecordCopilot_ShellWriteReportsOnlyChangedLines(t *testing.T) {
	isolateHome(t)
	posts := captureDaemonPosts(t)
	repo, abs := gitRepoWithFile(t, "Page.tsx", "l1\nl2\nl3\n")
	if err := os.WriteFile(abs, []byte("l1\nl2\nl3\nhuman4\nhuman5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"command": "(Get-Content Page.tsx) -replace 'l2','L2' | Set-Content Page.tsx", "description": "rename"}
	runCopilotPre(t, copilotAgentPre(t, repo, "powershell", args))
	if err := os.WriteFile(abs, []byte("l1\nL2\nl3\nhuman4\nhuman5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCopilotPost(t, copilotAgentPost(t, repo, "powershell", args))

	var edits []daemon.EditPayload
	for _, p := range posts() {
		if p.Tool == "copilot" && filepath.Base(p.FilePath) == "Page.tsx" {
			edits = append(edits, p)
		}
	}
	if len(edits) != 1 {
		t.Fatalf("want one copilot edit for Page.tsx, got %d: %+v", len(edits), edits)
	}
	p := edits[0]
	if p.SuggestedLines != 1 || len(p.Lines) != 1 || p.Lines[0].Start != 2 {
		t.Fatalf("payload lines = %+v (suggested %d), want only line 2", p.Lines, p.SuggestedLines)
	}
	if p.Lines[0].ContentSHA != sha256Hex([]byte("L2")) {
		t.Errorf("line 2 content_sha = %s, want the sha of %q", p.Lines[0].ContentSHA, "L2")
	}
}

// keepPayloadLines keeps only the named lines and recounts SuggestedLines; with
// no changed lines nothing is left, and the caller then skips the POST.
func TestKeepPayloadLines(t *testing.T) {
	p := daemon.EditPayload{SuggestedLines: 3, Lines: []daemon.Range{{Start: 1, End: 1}, {Start: 2, End: 2}, {Start: 4, End: 4}}}
	keepPayloadLines(&p, []int{2, 3})
	if len(p.Lines) != 1 || p.Lines[0].Start != 2 || p.SuggestedLines != 1 {
		t.Fatalf("got %+v", p)
	}
	keepPayloadLines(&p, nil)
	if len(p.Lines) != 0 || p.SuggestedLines != 0 {
		t.Fatalf("got %+v", p)
	}
}
