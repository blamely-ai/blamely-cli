package authorship

import (
	"fmt"
	"testing"
)

// AddedLineNumbers names exactly the lines Attribute credits to the editing author
// because they changed: not unchanged lines, not lines moved within the file.
func TestAddedLineNumbers(t *testing.T) {
	tests := []struct {
		name, baseline, content string
		want                    []int
	}{
		{"one line changed", "a\nb\nc\n", "a\nB\nc\n", []int{2}},
		{"line appended", "a\nb\n", "a\nb\nc\n", []int{3}},
		{"nothing changed", "a\nb\n", "a\nb\n", nil},
		{"only CRLF differs", "a\nb\n", "a\r\nb\r\n", nil},
		{"line moved, one changed", "a\nb\nc\nd\n", "d\na\nB\nc\n", []int{3}},
		{"new file", "", "x\ny\n", []int{1, 2}},
		{"lines removed only", "a\nb\nc\n", "a\nc\n", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := AddedLineNumbers(tc.baseline, tc.content)
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("AddedLineNumbers = %v, want %v", got, tc.want)
			}
		})
	}
}

// The lines it names are the ones Attribute gives to the new author, and only
// those, when the prior log is all one other author.
func TestAddedLineNumbers_MatchesAttribute(t *testing.T) {
	baseline := "l1\nl2\nl3\nhuman4\nhuman5\n"
	content := "l1\nL2\nl3\nhuman4\nhuman5\nnew6\n"
	human := HumanAuthor()
	ai := Author{Type: AI, Tool: "copilot", GenType: "cli"}
	prior := Attribute(nil, "", baseline, human, 1)
	wl := Attribute(prior, baseline, content, ai, 2)
	var fromLog []int
	for _, r := range wl.Lines {
		if r.Author == ai {
			for n := r.Start; n <= r.End; n++ {
				fromLog = append(fromLog, n)
			}
		}
	}
	if got := AddedLineNumbers(baseline, content); fmt.Sprint(got) != fmt.Sprint(fromLog) {
		t.Errorf("AddedLineNumbers = %v, Attribute credited %v", got, fromLog)
	}
}
