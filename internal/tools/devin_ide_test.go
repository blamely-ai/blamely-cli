package tools

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// The payloads below are the real shapes observed in Devin IDE session
// databases, trimmed to the fields blamely reads.

// A LOCAL session: absolute host path, newText carries the actual content.
const devinLocalWritePayload = `{"kind":"tool_call","content":{
  "_meta":{"cognition.ai/inferenceToolName":"write","cognition.ai/timestamp":"2026-08-19T15:18:17.495829+00:00"},
  "content":[{"newText":"line one\nline two\n","path":"%PATH%","type":"diff"}],
  "kind":"edit",
  "rawInput":{"file_path":"%PATH%"},
  "title":"Wrote %PATH%",
  "toolCallId":"functions.write:1",
  "status":"completed"}}`

// A CLOUD session: container path, and newText/oldText EMPTY because the real
// content lives in a remote blob behind cognition.ai/contentsKey.
const devinCloudEditPayload = `{"kind":"tool_call","content":{
  "_meta":{"cognition.ai/eventType":"multi_edit_result","cognition.ai/timestamp":"2026-08-19T15:06:44.920606738+00:00",
           "cognition.ai/fileUpdates":[{"end_line":71,"file_path":"/home/ubuntu/repos/vue-project/src/App.vue","start_line":2}]},
  "content":[{"_meta":{"cognition.ai/contentsKey":"editor_files/devin-abc/x.txt"},
              "newText":"","oldText":"","path":"/home/ubuntu/repos/vue-project/src/App.vue","type":"diff"}],
  "kind":"edit",
  "rawInput":null,
  "title":"Edit /home/ubuntu/repos/vue-project/src/App.vue",
  "status":"completed"}}`

func TestParseDevinACPEdit_LocalWrite(t *testing.T) {
	payload := replaceAll(devinLocalWritePayload, "%PATH%", "/tmp/demo.html")
	e, ok := parseDevinACPEdit(payload)
	if !ok {
		t.Fatal("expected a local write to parse as an edit")
	}
	if e.Path != "/tmp/demo.html" {
		t.Errorf("path = %q", e.Path)
	}
	if e.NewText != "line one\nline two\n" {
		t.Errorf("newText = %q", e.NewText)
	}
	if e.Tool != "write" {
		t.Errorf("tool = %q, want write", e.Tool)
	}
	if e.Timestamp.IsZero() {
		t.Error("timestamp not parsed")
	}
}

// The whole point of the local/cloud split: a cloud row carries no usable text,
// so it must not produce an edit. Its container path would resolve to nothing
// here anyway.
func TestParseDevinACPEdit_CloudRowIsSkipped(t *testing.T) {
	if _, ok := parseDevinACPEdit(devinCloudEditPayload); ok {
		t.Error("cloud session row was parsed as a local edit")
	}
}

func TestParseDevinACPEdit_NonEdits(t *testing.T) {
	for _, tc := range []struct{ name, payload string }{
		{"read", `{"kind":"tool_call","content":{"kind":"read","status":"completed","content":[]}}`},
		{"shell exec", `{"kind":"tool_call","content":{"kind":"execute","status":"completed","content":[]}}`},
		{"failed edit", `{"kind":"tool_call","content":{"kind":"edit","status":"failed","content":[{"type":"diff","path":"/tmp/x","newText":"a\n"}]}}`},
		{"edit with no path", `{"kind":"tool_call","content":{"kind":"edit","status":"completed","content":[{"type":"diff","newText":"a\n"}]}}`},
		{"garbage", `not json`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := parseDevinACPEdit(tc.payload); ok {
				t.Errorf("%s was parsed as an edit", tc.name)
			}
		})
	}
}

// newDevinSessionDB builds a session database in the real schema.
func newDevinSessionDB(t *testing.T, dir string, rows []struct {
	Kind    string
	Payload string
}) string {
	t.Helper()
	path := filepath.Join(dir, "session.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE messages (position INTEGER PRIMARY KEY, kind TEXT NOT NULL, payload TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for i, r := range rows {
		if _, err := db.Exec(`INSERT INTO messages (position, kind, payload) VALUES (?,?,?)`, i, r.Kind, r.Payload); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func TestReadDevinACPRows_ReadsEditsAndAdvancesCursor(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "demo.html")
	if err := os.WriteFile(target, []byte("line one\nline two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := newDevinSessionDB(t, dir, []struct{ Kind, Payload string }{
		{"user_message", `{"kind":"user_message"}`},
		{"tool_call", replaceAll(devinLocalWritePayload, "%PATH%", target)},
		{"agent_thought", `{"kind":"agent_thought"}`},
		{"tool_call", devinCloudEditPayload},
	})

	edits, maxPos, err := readDevinACPRows(path, -1, 100)
	if err != nil {
		t.Fatalf("readDevinACPRows: %v", err)
	}
	// Only the local write is an edit; the cloud row is filtered out.
	if len(edits) != 1 {
		t.Fatalf("edits = %d, want 1", len(edits))
	}
	if edits[0].Path != target {
		t.Errorf("path = %q, want %q", edits[0].Path, target)
	}
	// The cursor must advance past the LAST tool_call row (position 3), not
	// just the last edit — otherwise the cloud row is re-scanned every poll.
	if maxPos != 3 {
		t.Errorf("maxPos = %d, want 3", maxPos)
	}
}

func TestReadDevinACPRows_ResumesFromCursor(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "demo.html")
	if err := os.WriteFile(target, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	payload := replaceAll(devinLocalWritePayload, "%PATH%", target)
	path := newDevinSessionDB(t, dir, []struct{ Kind, Payload string }{
		{"tool_call", payload},
		{"tool_call", payload},
	})

	// Cursor already past row 0 — only row 1 should come back.
	edits, maxPos, err := readDevinACPRows(path, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(edits) != 1 {
		t.Fatalf("edits = %d, want 1 (row 0 already consumed)", len(edits))
	}
	if edits[0].Position != 1 || maxPos != 1 {
		t.Errorf("position = %d, maxPos = %d, want 1/1", edits[0].Position, maxPos)
	}
}

func TestReadDevinACPRows_RespectsLimit(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "demo.html")
	if err := os.WriteFile(target, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	payload := replaceAll(devinLocalWritePayload, "%PATH%", target)
	var rows []struct{ Kind, Payload string }
	for i := 0; i < 10; i++ {
		rows = append(rows, struct{ Kind, Payload string }{"tool_call", payload})
	}
	path := newDevinSessionDB(t, dir, rows)

	edits, maxPos, err := readDevinACPRows(path, -1, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(edits) != 3 || maxPos != 2 {
		t.Errorf("edits = %d maxPos = %d, want 3 / 2", len(edits), maxPos)
	}
}

func TestReadDevinACPRows_MissingDBIsAnError(t *testing.T) {
	if _, _, err := readDevinACPRows(filepath.Join(t.TempDir(), "nope.db"), -1, 10); err == nil {
		t.Error("expected an error for a missing database")
	}
}

func TestDefaultDevinACPRootsIsNonEmpty(t *testing.T) {
	if got := defaultDevinACPRoots(); len(got) == 0 {
		t.Error("expected at least one acp-messages root for this platform")
	}
}

// replaceAll avoids importing strings just for the fixtures.
func replaceAll(s, old, new string) string {
	out := ""
	for {
		i := indexOf(s, old)
		if i < 0 {
			return out + s
		}
		out += s[:i] + new
		s = s[i+len(old):]
	}
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
