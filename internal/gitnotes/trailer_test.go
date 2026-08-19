package gitnotes

import "testing"

func TestToolFromCoAuthorTrailer(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  string
		want string
	}{
		{
			"devin bot trailer",
			"feat: add auth\n\nCo-Authored-By: Devin <158243242+devin-ai-integration[bot]@users.noreply.github.com>",
			"devin",
		},
		{
			"case insensitive",
			"fix\n\nco-authored-by: devin <devin-ai-integration[bot]@users.noreply.github.com>",
			"devin",
		},
		{
			"different numeric id still matches",
			"fix\n\nCo-Authored-By: Devin <999+devin-ai-integration[bot]@users.noreply.github.com>",
			"devin",
		},
		{
			"renamed display name still matches on the app slug",
			"fix\n\nCo-Authored-By: Our Robot <158243242+devin-ai-integration[bot]@users.noreply.github.com>",
			"devin",
		},
		// Claude is observed locally by its own hook; reading its trailer would
		// double-attribute lines the precise path already handled.
		{
			"claude trailer is not claimed",
			"fix\n\nCo-Authored-By: Claude <noreply@anthropic.com>",
			"",
		},
		{"no trailer", "just a normal commit", ""},
		{"empty", "", ""},
		{
			"the word devin in prose is not a trailer",
			"refactor the devin integration module",
			"",
		},
		{
			"a mention outside a Co-Authored-By line does not count",
			"fix\n\nSee-Also: devin-ai-integration",
			"",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := toolFromCoAuthorTrailer(tc.msg); got != tc.want {
				t.Errorf("toolFromCoAuthorTrailer = %q, want %q", got, tc.want)
			}
		})
	}
}

// noteWithHumanAdds builds a minimal note whose single file has n Human-added
// lines, matching what buildNote produces for a commit with no edit records.
func noteWithHumanAdds(n int) *Note {
	return &Note{
		ByTool:    map[string]Tool{},
		ByGenType: ByGenType{Human: n},
		Totals:    Totals{AddedLines: n, HumanLines: n},
		Files: []FileEntry{{
			Path:  "f.go",
			Lines: []RangeEntry{{Start: 1, End: n, Type: "add", AuthorType: "Human"}},
		}},
	}
}

func TestBackfillFromCommitTrailer_CreditsDevin(t *testing.T) {
	note := noteWithHumanAdds(3)
	backfillFromCommitTrailer(note, "feat\n\nCo-Authored-By: Devin <1+devin-ai-integration[bot]@users.noreply.github.com>")

	if note.Totals.AILines != 3 || note.Totals.HumanLines != 0 {
		t.Errorf("totals: ai=%d human=%d, want 3/0", note.Totals.AILines, note.Totals.HumanLines)
	}
	if got := note.ByTool["devin"].Lines; got != 3 {
		t.Errorf("by_tool devin lines = %d, want 3", got)
	}
	line := note.Files[0].Lines[0]
	if line.AuthorType != "AI" || line.Tool != "devin" {
		t.Errorf("line not flipped: author=%q tool=%q", line.AuthorType, line.Tool)
	}
	if line.GenType == nil || *line.GenType != "chat" {
		t.Errorf("gen_type = %v, want chat", line.GenType)
	}
}

// An observed edit is line-level evidence; a trailer is only commit-level.
// When any AI line is already attributed, the trailer must stay out of it.
func TestBackfillFromCommitTrailer_YieldsToObservedEdits(t *testing.T) {
	note := noteWithHumanAdds(3)
	// Simulate one line already claimed by a locally-recorded claude edit.
	note.Totals.AILines = 1
	note.Totals.HumanLines = 2
	note.ByTool["claude"] = Tool{Lines: 1}

	backfillFromCommitTrailer(note, "feat\n\nCo-Authored-By: Devin <1+devin-ai-integration[bot]@users.noreply.github.com>")

	if _, ok := note.ByTool["devin"]; ok {
		t.Error("trailer overrode an observed edit")
	}
	if note.Totals.AILines != 1 || note.Totals.HumanLines != 2 {
		t.Errorf("totals changed: ai=%d human=%d, want 1/2", note.Totals.AILines, note.Totals.HumanLines)
	}
}

func TestBackfillFromCommitTrailer_NoTrailerIsNoOp(t *testing.T) {
	note := noteWithHumanAdds(4)
	backfillFromCommitTrailer(note, "chore: bump deps")

	if note.Totals.AILines != 0 || note.Totals.HumanLines != 4 {
		t.Errorf("totals: ai=%d human=%d, want 0/4", note.Totals.AILines, note.Totals.HumanLines)
	}
	if len(note.ByTool) != 0 {
		t.Errorf("by_tool should stay empty, got %v", note.ByTool)
	}
}

func TestBackfillFromCommitTrailer_NilNoteIsSafe(t *testing.T) {
	backfillFromCommitTrailer(nil, "Co-Authored-By: Devin <1+devin-ai-integration[bot]@x>")
}
