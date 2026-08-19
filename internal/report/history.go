package report

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/blamely/blamely/internal/gitnotes"
	"github.com/blamely/blamely/internal/gitutil"
	"github.com/blamely/blamely/internal/store"
)

// HistoryOptions controls what RenderHistory aggregates.
type HistoryOptions struct {
	Since    time.Duration // 0 = all time
	AllRepos bool          // false = current repo only
}

// histToolStat holds per-tool aggregate stats across all commits.
type histToolStat struct {
	Lines        int
	InputTokens  int64
	OutputTokens int64
	CacheRead    int64
	Model        string
}

// histStats is the aggregate accumulated across all noted commits.
type histStats struct {
	TotalAdded        int
	TotalDeleted      int
	AILines           int
	HumanLines        int
	Chat              int
	CLI               int
	Completion        int
	TotalSessionNanos int64
	FirstTS           int64
	LastTS            int64
	ByTool            map[string]*histToolStat
}

// RenderHistory prints an aggregate report across all noted commits.
func RenderHistory(opts HistoryOptions) error {
	db, err := store.Open()
	if err != nil {
		return err
	}
	defer db.Close()

	var repos []string
	if opts.AllRepos {
		repos, err = db.KnownRepoPaths()
		if err != nil {
			return err
		}
	} else {
		// Every repo at or below the cwd — one when we're inside a work tree,
		// each nested clone when the cwd is the workspace dir above them.
		repos, err = currentRepoIDs()
		if err != nil {
			return fmt.Errorf("%w; use --all to query all repos", err)
		}
	}
	if len(repos) == 0 {
		fmt.Println("No repos with blamely data found.")
		return nil
	}

	var sinceNanos int64
	if opts.Since > 0 {
		sinceNanos = time.Now().Add(-opts.Since).UnixNano()
	}

	commits, err := db.KnownCommits(repos, sinceNanos)
	if err != nil {
		return err
	}
	if len(commits) == 0 {
		fmt.Println("No commits with blamely notes found in the specified range.")
		return nil
	}

	s := &histStats{ByTool: map[string]*histToolStat{}}
	if len(commits) > 0 {
		s.LastTS = commits[0].TS
		s.FirstTS = commits[len(commits)-1].TS
	}

	for _, c := range commits {
		noteBytes, err := exec.Command("git", "-C", c.RepoPath,
			"notes", "--ref="+gitnotes.NotesRef, "show", c.SHA).Output()
		if err != nil {
			continue
		}
		var note gitnotes.Note
		if err := json.Unmarshal(noteBytes, &note); err != nil {
			continue
		}
		s.TotalAdded += note.Totals.AILines + note.Totals.HumanLines
		s.TotalDeleted += note.Totals.DeletedLines
		s.AILines += note.Totals.AILines
		s.HumanLines += note.Totals.HumanLines
		s.Chat += note.ByGenType.Chat
		s.CLI += note.ByGenType.CLI
		s.Completion += note.ByGenType.Completion

		for name, t := range note.ByTool {
			if t.Lines == 0 {
				continue
			}
			ts := s.ByTool[name]
			if ts == nil {
				ts = &histToolStat{}
				s.ByTool[name] = ts
			}
			ts.Lines += t.Lines
			if t.Tokens != nil {
				ts.InputTokens += t.Tokens.Input
				ts.OutputTokens += t.Tokens.Output
				ts.CacheRead += t.Tokens.CacheRead
			}
			if t.Model != nil && *t.Model != "" && ts.Model == "" {
				ts.Model = *t.Model
			}
		}

		// Estimate session duration for this commit.
		repoID, _ := gitutil.RepoID(c.RepoPath)
		s.TotalSessionNanos += db.SessionDurationNanos(repoID, c.TS)
	}

	printHistory(os.Stdout, s, len(commits), repos, opts)
	return nil
}

func printHistory(w *os.File, s *histStats, commitCount int, repos []string, opts HistoryOptions) {
	color := colorEnabled()
	bold := func(v string) string {
		if color {
			return ansiBold + v + ansiReset
		}
		return v
	}
	dim := func(v string) string {
		if color {
			return ansiDim + v + ansiReset
		}
		return v
	}
	colorFn := func(c, v string) string {
		if color {
			return c + v + ansiReset
		}
		return v
	}

	sinceLabel := "all time"
	if opts.Since > 0 {
		sinceLabel = "last " + opts.Since.String()
	}
	repoLabel := repos[0]
	if len(repos) > 1 {
		repoLabel = fmt.Sprintf("%d repos", len(repos))
	}
	fmt.Fprintf(w, "%s · %s  (%s · %d commits)\n\n",
		bold("Blamely history"), repoLabel, sinceLabel, commitCount)

	// Changes
	net := s.TotalAdded - s.TotalDeleted
	fmt.Fprintf(w, "%s\n", bold("Changes:"))
	fmt.Fprintf(w, "  %s\n", colorFn(ansiGreen, fmt.Sprintf("+%-6d added   (AI %d · human %d)", s.TotalAdded, s.AILines, s.HumanLines)))
	if s.TotalDeleted > 0 {
		fmt.Fprintf(w, "  %s\n", colorFn("\x1b[31m", fmt.Sprintf("-%-6d deleted", s.TotalDeleted)))
	}
	fmt.Fprintf(w, "  ────────────\n")
	fmt.Fprintf(w, "  %s net\n\n", bold(fmt.Sprintf("%-6d", net)))

	// By tool
	total := s.AILines + s.HumanLines
	if total == 0 {
		total = 1
	}
	const barWidth = 20
	fmt.Fprintf(w, "%s\n", bold("By tool:"))
	for _, name := range []string{"claude", "cursor", "codex", "copilot", "gemini", "devin", "human"} {
		ts := s.ByTool[name]
		lines := 0
		if ts != nil {
			lines = ts.Lines
		}
		pct := float64(lines) * 100 / float64(total)
		barCells := int(float64(barWidth)*float64(lines)/float64(total) + 0.5)
		bar := strings.Repeat("█", barCells) + strings.Repeat("░", barWidth-barCells)
		tokStr := ""
		if ts != nil && ts.InputTokens > 0 {
			tokStr = dim(fmt.Sprintf("  in=%s out=%s cache=%s",
				formatK(ts.InputTokens), formatK(ts.OutputTokens), formatK(ts.CacheRead)))
		}
		modelStr := ""
		if ts != nil && ts.Model != "" {
			modelStr = "  " + dim(ts.Model)
		}
		barColor := ansiGreen
		if name == "human" {
			barColor = ansiBlue
		} else if name == "copilot" {
			barColor = "\x1b[35m"
		} else if name == "cursor" {
			barColor = "\x1b[36m"
		}
		if color {
			bar = barColor + bar + ansiReset
		}
		fmt.Fprintf(w, "  %-10s %6d  %s  %5.1f%%%s%s\n",
			name, lines, bar, pct, modelStr, tokStr)
	}
	fmt.Fprintln(w)

	// By generation type
	fmt.Fprintf(w, "%s\n", bold("By generation:"))
	for _, row := range []struct {
		name  string
		lines int
	}{
		{"chat", s.Chat},
		{"cli", s.CLI},
		{"completion", s.Completion},
	} {
		pct := float64(row.lines) * 100 / float64(total)
		fmt.Fprintf(w, "  %-12s %6d  %5.1f%%\n", row.name, row.lines, pct)
	}
	fmt.Fprintln(w)

	// Tokens total
	var totalIn, totalOut, totalCache int64
	for _, ts := range s.ByTool {
		totalIn += ts.InputTokens
		totalOut += ts.OutputTokens
		totalCache += ts.CacheRead
	}
	if totalIn > 0 {
		fmt.Fprintf(w, "%s  in=%s  out=%s  cache_read=%s\n\n",
			bold("Total tokens:"), formatK(totalIn), formatK(totalOut), formatK(totalCache))
	}

	// Session time
	sessionMins := s.TotalSessionNanos / int64(time.Minute)
	if sessionMins > 0 {
		h := sessionMins / 60
		m := sessionMins % 60
		sessionStr := fmt.Sprintf("%dm", m)
		if h > 0 {
			sessionStr = fmt.Sprintf("%dh %dm", h, m)
		}
		fmt.Fprintf(w, "%s  ~%s\n", bold("Coding sessions:"), sessionStr)
	}

	// Time span
	if s.FirstTS > 0 && s.LastTS > 0 {
		first := time.Unix(0, s.FirstTS).Format("2006-01-02")
		last := time.Unix(0, s.LastTS).Format("2006-01-02")
		fmt.Fprintf(w, "%s  %s → %s\n", bold("Span:"), first, last)
	}
}
