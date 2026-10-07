package report

import (
	"strings"
	"testing"

	"github.com/blamely/blamely/internal/gitnotes"
)

func TestOpenCodeReportIdentity(t *testing.T) {
	file := gitnotes.FileEntry{Lines: []gitnotes.RangeEntry{{Start: 1, End: 3, Type: "add", AuthorType: "AI", Tool: "opencode"}}}
	if got := fileToolBreakdown(file); !strings.Contains(got, "opencode 3") {
		t.Fatalf("missing native tool in report: %q", got)
	}
	if _, ok := toolGlyphs["opencode"]; !ok {
		t.Fatal("missing OpenCode HTML glyph")
	}
}
