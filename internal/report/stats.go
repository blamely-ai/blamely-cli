package report

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/blamely/blamely/internal/gitnotes"
	"github.com/blamely/blamely/internal/gitutil"
	"github.com/blamely/blamely/internal/store"
)

// RenderStats prints a deep single-commit view to stdout.
// It reads the blamely git note for sha and combines it with commit metadata.
func RenderStats(sha string) error {
	roots, err := currentRepoRoots()
	if err != nil {
		return err
	}
	// With several repos below the cwd, `sha` (usually HEAD) resolves in each of
	// them independently: render every repo that has a note for it, and only
	// complain when none does.
	var rendered int
	for _, root := range roots {
		ok, err := renderStatsIn(root, sha, len(roots) > 1)
		if err != nil {
			return err
		}
		if ok {
			rendered++
		}
	}
	if rendered == 0 {
		return fmt.Errorf("no blamely note for %s: run `blamely attribute %s %s` first", sha, roots[0], sha)
	}
	return nil
}

// renderStatsIn renders the stats dashboard for one repo, reporting false (with
// no error) when that repo has no note for `sha` — a normal outcome when the
// caller is iterating sibling repos.
func renderStatsIn(repoPath, sha string, multi bool) (bool, error) {
	db, err := store.Open()
	if err != nil {
		return false, err
	}
	defer db.Close()

	repoID, _ := gitutil.RepoID(repoPath)
	if repoID == "" {
		repoID = repoPath
	}

	noteBytes, err := readNote(repoPath, sha)
	if err != nil {
		return false, nil // no note here — the caller decides whether that's an error
	}
	var note gitnotes.Note
	if err := json.Unmarshal(noteBytes, &note); err != nil {
		return false, fmt.Errorf("parse note: %w", err)
	}

	// Commit metadata
	meta, err := commitMeta(repoPath, sha)
	if err != nil {
		meta = commitMeta_{"sha": sha}
	}

	// Session duration
	var commitNanos int64
	if ts, err := gitnotes.CommitTimestampNanos(repoPath, sha); err == nil {
		commitNanos = ts
	}
	sessionNanos := db.SessionDurationNanos(repoID, commitNanos)

	// stats is the dashboard "head" — the same cards as `report` but without the
	// per-file range detail. Fall back to the live session duration for older
	// notes that didn't bake in coding time.
	if note.CodingTimeNanos == 0 {
		note.CodingTimeNanos = sessionNanos
	}
	repoBanner(os.Stdout, repoPath, multi)
	renderDashboard(os.Stdout, &note, meta, false)
	return true, nil
}

// RenderCurrentStats prints the same deep view for the CURRENT uncommitted change
// (`blamely stats` with no argument): it attributes the working-tree diff against HEAD
// from the working logs, with no commit/note required.
func RenderCurrentStats() error {
	roots, err := currentRepoRoots()
	if err != nil {
		return err
	}
	var rendered int
	for _, root := range roots {
		ok, err := renderCurrentStatsIn(root, len(roots) > 1)
		if err != nil {
			return err
		}
		if ok {
			rendered++
		}
	}
	if rendered == 0 {
		fmt.Println("No uncommitted changes.")
	}
	return nil
}

// renderCurrentStatsIn renders the working-tree dashboard for one repo,
// reporting false when that repo has no uncommitted changes.
func renderCurrentStatsIn(repoPath string, multi bool) (bool, error) {
	note, err := gitnotes.AttributeWorkingTree(repoPath)
	if err != nil {
		return false, err
	}
	if note == nil || len(note.Files) == 0 {
		return false, nil
	}
	db, err := store.Open()
	if err != nil {
		return false, err
	}
	defer db.Close()
	repoID, _ := gitutil.RepoID(repoPath)
	if repoID == "" {
		repoID = repoPath
	}
	meta := commitMeta_{"sha": "working-tree", "subject": "uncommitted changes"}
	sessionNanos := db.SessionDurationNanos(repoID, time.Now().UnixNano())
	if note.CodingTimeNanos == 0 {
		note.CodingTimeNanos = sessionNanos
	}
	repoBanner(os.Stdout, repoPath, multi)
	renderDashboard(os.Stdout, note, meta, false)
	return true, nil
}

type commitMeta_ map[string]string

func commitMeta(repoPath, sha string) (commitMeta_, error) {
	out, err := exec.Command("git", "-C", repoPath, "show", "-s",
		"--format=%H|%s|%ae|%ci", sha).Output()
	if err != nil {
		return nil, err
	}
	parts := strings.SplitN(strings.TrimSpace(string(out)), "|", 4)
	m := commitMeta_{}
	keys := []string{"sha", "subject", "author", "date"}
	for i, k := range keys {
		if i < len(parts) {
			m[k] = parts[i]
		}
	}
	return m, nil
}

func readNote(repoPath, sha string) ([]byte, error) {
	return exec.Command("git", "-C", repoPath, "notes", "--ref="+gitnotes.NotesRef, "show", sha).Output()
}

func renderStats(w io.Writer, note *gitnotes.Note, meta commitMeta_, sessionNanos int64) {
	sha := meta["sha"]
	if len(sha) > 12 {
		sha = sha[:12]
	}
	subject := meta["subject"]
	author := meta["author"]
	dateStr := meta["date"]

	// Title bar + commit identity block.
	titleBar(w, "stats", sha)
	if subject != "" {
		fmt.Fprintf(w, "\n%s%s\n", gutter, bold("\""+subject+"\""))
	}
	metaRow(w, "author", author)
	when := ""
	if t, err := time.Parse("2006-01-02 15:04:05 -0700", dateStr); err == nil {
		when = sep(t.Format("2006-01-02 15:04"), humanDuration(time.Since(t)))
	}
	metaRow(w, "branch", sep(note.Branch, when))
	metaRow(w, "date", whenOnly(note, when))

	// Changes — added and deleted, each split by author (AI vs Human). Computed
	// from the always-present counters so the split is correct for older notes
	// too (which predate the ai_added/human_deleted fields).
	aiAdded, humanAdded := note.Totals.AILines, note.Totals.HumanLines
	aiDeleted := note.Totals.AIDeletedLines
	humanDeleted := note.Totals.DeletedLines - aiDeleted
	added := aiAdded + humanAdded
	deleted := note.Totals.DeletedLines
	net := added - deleted
	sectionHead(w, "Changes")
	fmt.Fprintf(w, "%s  %s   %s\n", gutter,
		green(fmt.Sprintf("+%-4d", added)),
		dim(fmt.Sprintf("added    ·  AI %d · Human %d", aiAdded, humanAdded)))
	if deleted > 0 {
		fmt.Fprintf(w, "%s  %s   %s\n", gutter,
			red(fmt.Sprintf("-%-4d", deleted)),
			dim(fmt.Sprintf("deleted  ·  AI %d · Human %d", aiDeleted, humanDeleted)))
	}
	fmt.Fprintf(w, "%s  %s\n", gutter, dim("─────"))
	fmt.Fprintf(w, "%s  %s   %s\n", gutter, bold(fmt.Sprintf("%-4d", net)), dim("net"))

	// Attribution — one block per contributing AI tool.
	if note.Totals.AILines > 0 {
		sectionHead(w, "Attribution")
		for _, name := range []string{"claude", "cursor", "codex", "copilot", "gemini", "devin"} {
			t, ok := note.ByTool[name]
			if !ok || t.Lines == 0 {
				continue
			}
			genType := toolGenType(note, name)
			if genType == "" {
				genType = legacyToolGenType(name)
			}
			model := ""
			if t.Model != nil {
				model = *t.Model
			}
			head := fmt.Sprintf("%s   %s",
				green(fmt.Sprintf("%-9s", name)),
				dim(fmt.Sprintf("%d lines", t.Lines)))
			fmt.Fprintf(w, "%s  %s   %s\n", gutter, head, sep(genType, model))
			if t.Tokens != nil {
				fmt.Fprintf(w, "%s  %s\n", gutter+"           ", dim(fmt.Sprintf("tokens   in %s · out %s · cache %s",
					formatK(t.Tokens.Input), formatK(t.Tokens.Output), formatK(t.Tokens.CacheRead))))
			}
			if t.SuggestedLines > 0 {
				pct := int64(t.AcceptedLines) * 100 / t.SuggestedLines
				fmt.Fprintf(w, "%s  %s\n", gutter+"           ", dim(fmt.Sprintf("accepted %d%%   (%d suggested %s %d kept)",
					pct, t.SuggestedLines, glyphArr, t.AcceptedLines)))
			}
		}
	}

	// Generation — proportional mini-bars across the four modes.
	g := note.ByGenType
	if g.Chat+g.CLI+g.Completion+g.Human > 0 {
		sectionHead(w, "Generation")
		renderGenRows(w, g)
	}

	// Files — per-file +/- and the tool breakdown.
	if len(note.Files) > 0 {
		sectionHead(w, "Files")
		nameW := 0
		for _, f := range note.Files {
			if len(f.Path) > nameW {
				nameW = len(f.Path)
			}
		}
		if nameW > 44 {
			nameW = 44
		}
		for _, f := range note.Files {
			fmt.Fprintf(w, "%s  %-*s  %s %s   %s\n", gutter, nameW, f.Path,
				green(fmt.Sprintf("+%-4d", f.Added)),
				red(fmt.Sprintf("-%-4d", f.Deleted)),
				dim(fileToolBreakdown(f)))
		}
	}

	// Tokens + coding-time one-liners.
	if note.Totals.Tokens != nil || codingNanos(note, sessionNanos) > 0 {
		fmt.Fprintln(w)
	}
	if note.Totals.Tokens != nil {
		t := note.Totals.Tokens
		inlineMeta(w, "Tokens", dim(fmt.Sprintf("in %s · out %s · cache_read %s · cache_write %s",
			formatK(t.Input), formatK(t.Output), formatK(t.CacheRead), formatK(t.CacheWrite))))
	}
	if c := codingNanos(note, sessionNanos); c > 0 {
		mins := c / int64(time.Minute)
		inlineMeta(w, "Coding", dim(fmt.Sprintf("~%d min   first edit %s commit", mins, glyphArr)))
	}
	fmt.Fprintln(w)
	versionLine(w, noteVersion(note))
	fmt.Fprintln(w)
}

// whenOnly returns the timestamp half of the header when there's no branch to
// pair it with, so the date still shows on its own row.
func whenOnly(note *gitnotes.Note, when string) string {
	if note.Branch != "" {
		return "" // already shown on the branch row
	}
	return when
}

// codingNanos prefers the value baked into the note (captured at attribution
// time) and falls back to the live DB session duration for older notes.
func codingNanos(note *gitnotes.Note, sessionNanos int64) int64 {
	if note.CodingTimeNanos > 0 {
		return note.CodingTimeNanos
	}
	return sessionNanos
}

// renderGenRows prints the four generation modes as labelled proportional
// mini-bars, busiest-first ordering preserved by the fixed mode list.
func renderGenRows(w io.Writer, g gitnotes.ByGenType) {
	total := g.Chat + g.CLI + g.Completion + g.Human
	if total == 0 {
		total = 1
	}
	const barW = 20
	rows := []struct {
		name  string
		lines int
		clr   string
	}{
		{"chat", g.Chat, ansiGreen},
		{"cli", g.CLI, ansiCyan},
		{"completion", g.Completion, ansiMagenta},
		{"human", g.Human, ansiBlue},
	}
	for _, r := range rows {
		filled := int(float64(barW)*float64(r.lines)/float64(total) + 0.5)
		if filled > barW {
			filled = barW
		}
		var bar string
		if colorEnabled() {
			if r.lines > 0 {
				bar = r.clr + strings.Repeat("█", filled) + ansiReset + dim(strings.Repeat("░", barW-filled))
			} else {
				bar = dim(strings.Repeat("░", barW))
			}
		} else {
			bar = strings.Repeat("█", filled) + strings.Repeat("░", barW-filled)
		}
		pct := ""
		if r.lines > 0 {
			pct = fmt.Sprintf("   %5.1f%%", float64(r.lines)*100/float64(total))
		}
		fmt.Fprintf(w, "%s  %-11s %4d  %s%s\n", gutter, r.name, r.lines, bar, dim(pct))
	}
}

// toolGenType returns the dominant generation type for a tool across the note,
// derived from per-line gen_type tags. Falls back to "" when the tool didn't
// produce any classified lines.
func toolGenType(note *gitnotes.Note, tool string) string {
	counts := map[string]int{}
	for _, f := range note.Files {
		for _, l := range f.Lines {
			if l.Tool != tool || l.GenType == nil {
				continue
			}
			counts[*l.GenType] += l.NumLines()
		}
	}
	var best string
	var bestN int
	for k, n := range counts {
		if n > bestN {
			best = k
			bestN = n
		}
	}
	return best
}

// legacyToolGenType is the pre-per-line-gen_type fallback used only when the
// note doesn't have any classified lines for the tool (i.e. older notes
// produced before the schema upgrade). Avoid in new code; per-line gen_type
// is authoritative.
func legacyToolGenType(tool string) string {
	switch tool {
	case "codex", "devin":
		return "cli"
	case "copilot":
		return "completion"
	default:
		return "chat"
	}
}

// fileToolBreakdown summarises which tools contributed to a file. Deletions
// (Tool == "") are ignored — they don't carry attribution.
func fileToolBreakdown(f gitnotes.FileEntry) string {
	counts := map[string]int{}
	for _, l := range f.Lines {
		if l.Type != "add" || l.Tool == "" {
			continue
		}
		counts[l.Tool] += l.NumLines()
	}
	var parts []string
	for _, name := range []string{"claude", "cursor", "codex", "copilot", "gemini", "devin", "human"} {
		if c := counts[name]; c > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", name, c))
		}
	}
	return strings.Join(parts, " · ")
}

func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func formatK(n int64) string {
	if n == 0 {
		return "0"
	}
	if n >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
	if n >= 1_000 {
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	}
	return fmt.Sprintf("%d", n)
}
