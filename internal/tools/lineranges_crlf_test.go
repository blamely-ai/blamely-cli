package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Line-ending regression tests for LocateNewString and the edit tools built on
// it. Every file is written with explicit bytes, so these run the same on
// macOS, Linux and Windows — no git, no autocrlf, no platform newline.

func toCRLF(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }

const crlfFixture = "package x\n\nfunc a() {\n\treturn 1\n}\n\nfunc b() {}\n"

// TestLocateNewString_LineEndingAgnostic: the same edit must be located at the
// same lines with the same hashes whatever the line endings of the file on disk
// and of the tool's payload. Regression: an LF new_string in a CRLF file (an
// agent editing a core.autocrlf=true checkout) was never found, so the edit got
// no line hashes and its lines were committed as Human.
func TestLocateNewString_LineEndingAgnostic(t *testing.T) {
	const needle = "func a() {\n\treturn 1\n}"
	dir := t.TempDir()
	want, err := LocateNewString(writeFile(t, dir, "lf.go", crlfFixture), needle)
	if err != nil || want == nil {
		t.Fatalf("LF control: got %+v, %v", want, err)
	}
	if want.Start != 3 || want.End != 5 {
		t.Fatalf("LF control: want 3-5, got %d-%d", want.Start, want.End)
	}

	// First four lines CRLF, the rest LF.
	lines := strings.SplitAfter(crlfFixture, "\n")
	mixed := toCRLF(strings.Join(lines[:4], "")) + strings.Join(lines[4:], "")

	cases := []struct{ name, disk, payload string }{
		{"LF disk, CRLF payload", crlfFixture, toCRLF(needle)},
		{"CRLF disk, LF payload", toCRLF(crlfFixture), needle},
		{"CRLF disk, CRLF payload", toCRLF(crlfFixture), toCRLF(needle)},
		{"mixed disk, LF payload", mixed, needle},
		{"mixed disk, CRLF payload", mixed, toCRLF(needle)},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := writeFile(t, dir, "f"+string(rune('0'+i))+".go", tc.disk)
			got, err := LocateNewString(p, tc.payload)
			if err != nil {
				t.Fatal(err)
			}
			if got == nil {
				t.Fatal("not found")
			}
			if *got != *want {
				t.Errorf("want %+v, got %+v", *want, *got)
			}
		})
	}
}

// TestLocateNewString_TrailingNewlineCRLF: a single line ending in a newline
// covers just that line, and its ContentSHA is the bare line's hash — the
// per-line convention — whether the newline is \n or \r\n.
func TestLocateNewString_TrailingNewlineCRLF(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "f.go", toCRLF(crlfFixture))
	for _, payload := range []string{"\treturn 1\n", "\treturn 1\r\n"} {
		lr, err := LocateNewString(p, payload)
		if err != nil {
			t.Fatal(err)
		}
		if lr == nil || lr.Start != 4 || lr.End != 4 {
			t.Fatalf("%q: want 4-4, got %+v", payload, lr)
		}
		if lr.ContentSHA != sha256Hex([]byte("\treturn 1")) {
			t.Errorf("%q: ContentSHA must hash the line without its line ending", payload)
		}
	}
}

func TestLocateNewString_NotFoundCRLF(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "f.go", toCRLF(crlfFixture))
	lr, err := LocateNewString(p, "func a() {\n\treturn 2\n}")
	if err != nil {
		t.Fatal(err)
	}
	if lr != nil {
		t.Fatalf("expected nil, got %+v", lr)
	}
}

// TestExtractClaudeRanges_EditOnCRLFFile: Claude Edit / MultiEdit (and Cursor
// StrReplace, which shares the path) on a CRLF file with an LF payload must
// record the same per-line ranges and hashes as on an LF file.
func TestExtractClaudeRanges_EditOnCRLFFile(t *testing.T) {
	const (
		oldStr = "func a() {\n\treturn 1\n}"
		newStr = "func a() {\n\tx := 1\n\treturn x\n}"
	)
	post := strings.Replace(crlfFixture, oldStr, newStr, 1)

	run := func(t *testing.T, disk, tool string, input map[string]any) []LineRange {
		t.Helper()
		fp := filepath.Join(t.TempDir(), "a.go")
		if err := os.WriteFile(fp, []byte(disk), 0o644); err != nil {
			t.Fatal(err)
		}
		input["file_path"] = fp
		raw, _ := json.Marshal(input)
		_, ranges, _, _, _, err := extractClaudeRanges(claudeHookPayload{ToolName: tool, ToolInput: raw})
		if err != nil {
			t.Fatal(err)
		}
		return ranges
	}
	edit := func() map[string]any { return map[string]any{"old_string": oldStr, "new_string": newStr} }
	multi := func() map[string]any {
		return map[string]any{"edits": []map[string]any{{"old_string": oldStr, "new_string": newStr}}}
	}

	for _, tc := range []struct {
		tool  string
		input func() map[string]any
	}{{"Edit", edit}, {"MultiEdit", multi}} {
		t.Run(tc.tool, func(t *testing.T) {
			want := run(t, post, tc.tool, tc.input())
			if len(want) == 0 {
				t.Fatal("LF control recorded no ranges")
			}
			got := run(t, toCRLF(post), tc.tool, tc.input())
			if !reflect.DeepEqual(got, want) {
				t.Errorf("CRLF file:\n got %+v\nwant %+v", got, want)
			}
		})
	}
}
