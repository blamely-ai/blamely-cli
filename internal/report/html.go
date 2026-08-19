package report

import (
	"bytes"
	"fmt"
	"html/template"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/blamely/blamely/internal/gitnotes"
)

// RenderHTML builds a self-contained dark-dashboard HTML report for a commit's
// blamely note — the "stats head" panel (AI/Human donut, changes, generation,
// files, leaderboard, tokens) rendered as static HTML+SVG with no JS framework
// or network dependency, so the file opens and prints anywhere. meta carries the
// git commit identity (subject/author/date) the note doesn't itself store.
func RenderHTML(note *gitnotes.Note, meta commitMeta_) (string, error) {
	vm := buildHTMLModel(note, meta)
	var buf bytes.Buffer
	if err := htmlTmpl.Execute(&buf, vm); err != nil {
		return "", fmt.Errorf("render html: %w", err)
	}
	return buf.String(), nil
}

// noteVersion returns the blamely version that GENERATED this commit's
// attribution, read from the note's `generated_by` ("blamely <version>"). The
// footer should credit the version that wrote the note, not the (possibly newer)
// version rendering the report. Falls back to the running version when the note
// predates generated_by or carries an unexpected shape.
func noteVersion(note *gitnotes.Note) string {
	if note != nil {
		v := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(note.GeneratedBy), "blamely"))
		if v != "" {
			return v
		}
	}
	return Version
}

// ---- view model ---------------------------------------------------------

type htmlVM struct {
	Subject, ShortHash, Branch, Author, Ago, Date string
	Version                                       string

	AIPct, HumanPct                            float64
	AILines, HumanLines                        int
	TopModel                                   string
	DonutAIDash, DonutHumanDash, DonutHumanOff string

	Added, Deleted, Net                          int
	AIAdded, HumanAdded, AIDeleted, HumanDeleted int
	AddedPct, DeletedPct                         float64

	Gen      []htmlGen
	Accepted *htmlAccepted

	TokIn, TokOut, TokCacheR, TokCacheW string
	Coding                              string

	FilesChanged int
	Files        []htmlFile
	Leaders      []htmlLeader
	Contributors int

	Tools []htmlTool
}

// htmlTool is one contributing AI tool in the "Tools" usage card.
type htmlTool struct {
	Name      string
	Icon      template.HTML // brand-colored glyph (original mark, not a trademark logo)
	Color     string        // hex tint for the icon + bar
	Lines     int
	Deleted   int
	WidthPct  float64 // bar width relative to the busiest tool
	Model     string
	HasAccept bool
	AcceptPct int
	Suggested int64
	Kept      int64
	HasTokens bool
	TokIn     string
	TokOut    string
	TokCacheR string
	TokCacheW string
}

type htmlGen struct {
	Label    string // also the CSS color class suffix (g-chat, g-cli, …)
	Value    int
	Pct      string
	WidthPct float64
	Zero     bool
}

type htmlAccepted struct {
	Pct             int
	Suggested, Kept int64
}

type htmlFile struct {
	Name             string
	Added, Deleted   int
	Attr             string // "" when no AI tool contributed
	AttrLines        int
	AddedW, DeletedW float64
	Ranges           []htmlRange
}

// htmlRange is one attributed line-range in a file's detail section.
type htmlRange struct {
	Loc  string // "L12" or "L12-40"
	Type string // "add" | "delete"
	Attr string // "claude · model · chat" | "human" | "—"
	IsAI bool
}

type htmlLeader struct {
	Rank     int
	Name     string
	Lines    int
	Pct      string
	WidthPct float64
	IsModel  bool
	Initial  string
	Icon     template.HTML // tool glyph for model rows (empty for human / unknown)
	Color    string
}

// toolGlyphs holds an original brand-colored mark per AI tool. The SVGs are
// simple geometric glyphs of our own design (a spark, a caret, a cursor, …),
// not reproductions of the tools' trademarked logos — they just give each tool
// a recognizable color + shape in the report.
var toolGlyphs = map[string]struct {
	color string
	icon  template.HTML
}{
	"claude":  {"#d97757", template.HTML(`<svg viewBox="0 0 24 24" fill="currentColor"><path d="M12 1.6l1.7 6.3a2 2 0 0 0 1.4 1.4L21.4 11l-6.3 1.7a2 2 0 0 0-1.4 1.4L12 20.4l-1.7-6.3a2 2 0 0 0-1.4-1.4L2.6 11l6.3-1.7a2 2 0 0 0 1.4-1.4z"/></svg>`)},
	"cursor":  {"#cdd3da", template.HTML(`<svg viewBox="0 0 24 24" fill="currentColor"><path d="M5 2.6l13.8 7.9a.7.7 0 0 1-.1 1.3l-5.6 1.6a1 1 0 0 0-.7.7l-1.6 5.6a.7.7 0 0 1-1.3.1z"/></svg>`)},
	"codex":   {"#19c37d", template.HTML(`<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.1" stroke-linecap="round" stroke-linejoin="round"><path d="M9.5 7.5 5 12l4.5 4.5M14.5 7.5 19 12l-4.5 4.5"/></svg>`)},
	"copilot": {"#a371f7", template.HTML(`<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9"><path d="M12 7c1.6-2 3.4-2.6 5-2 1.4.5 1.6 2.3 1.4 4M12 7c-1.6-2-3.4-2.6-5-2C5.6 5.5 5.4 7.3 5.6 9.2"/><rect x="3.2" y="9" width="17.6" height="8.4" rx="4.2"/><circle cx="9" cy="13.2" r="1.25" fill="currentColor" stroke="none"/><circle cx="15" cy="13.2" r="1.25" fill="currentColor" stroke="none"/></svg>`)},
	"gemini":  {"#4f9cf0", template.HTML(`<svg viewBox="0 0 24 24" fill="currentColor"><path d="M12 2c.4 5.4 4.2 9.2 9.6 9.6C16.2 12 12.4 15.8 12 21.2 11.6 15.8 7.8 12 2.4 11.6 7.8 11.2 11.6 7.4 12 2z"/></svg>`)},
	"devin":   {"#4b8dd6", template.HTML(`<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linejoin="round"><path d="M12 2.8 20.2 7v10L12 21.2 3.8 17V7z"/><path d="M12 8.2 16 10.5v4L12 16.8 8 14.5v-4z" fill="currentColor" stroke="none"/></svg>`)},
}

const (
	hueAcc    = "var(--acc)"
	hueRed    = "#e06c75"
	hueAmber  = "#e0a33a"
	hueBlue   = "#4f9cf0"
	hueViolet = "#b58df0"
)

func buildHTMLModel(note *gitnotes.Note, meta commitMeta_) htmlVM {
	t := note.Totals
	aiAdded, humanAdded := t.AILines, t.HumanLines
	aiDel := t.AIDeletedLines
	humanDel := t.DeletedLines - aiDel
	added := aiAdded + humanAdded
	deleted := t.DeletedLines

	short := note.Commit
	if len(short) > 8 {
		short = short[:8]
	}

	// The Attribution headline counts ALL attributed lines — additions AND
	// deletions — so a commit where a human deletes code isn't headlined "100%
	// AI" just because the few added lines were AI. This matches the Generation
	// panel (which already counts deletions).
	aiTouched := aiAdded + aiDel
	humanTouched := humanAdded + humanDel

	vm := htmlVM{
		Subject:      firstLine(meta["subject"]),
		ShortHash:    short,
		Branch:       note.Branch,
		Author:       meta["author"],
		Ago:          agoFromDate(meta["date"]),
		Date:         shortDate(meta["date"]),
		Version:      noteVersion(note),
		AILines:      aiTouched,
		HumanLines:   humanTouched,
		Added:        added,
		Deleted:      deleted,
		Net:          added - deleted,
		AIAdded:      aiAdded,
		HumanAdded:   humanAdded,
		AIDeleted:    aiDel,
		HumanDeleted: humanDel,
	}

	if tot := aiTouched + humanTouched; tot > 0 {
		vm.AIPct = round1(float64(aiTouched) * 100 / float64(tot))
		vm.HumanPct = round1(100 - vm.AIPct)
	}
	if tot := added + deleted; tot > 0 {
		vm.AddedPct = round1(float64(added) * 100 / float64(tot))
		vm.DeletedPct = round1(float64(deleted) * 100 / float64(tot))
	}
	vm.DonutAIDash, vm.DonutHumanDash, vm.DonutHumanOff = donutArcs(vm.AIPct, vm.HumanPct)

	// Generation rows.
	g := note.ByGenType
	genTotal := g.Chat + g.CLI + g.Completion + g.Human
	if genTotal == 0 {
		genTotal = 1
	}
	maxGen := maxInt(g.Chat, g.CLI, g.Completion, g.Human, 1)
	for _, r := range []struct {
		label string
		v     int
	}{
		{"chat", g.Chat}, {"cli", g.CLI},
		{"completion", g.Completion}, {"human", g.Human},
	} {
		vm.Gen = append(vm.Gen, htmlGen{
			Label: r.label, Value: r.v, Zero: r.v == 0,
			Pct:      fmt.Sprintf("%.1f", float64(r.v)*100/float64(genTotal)),
			WidthPct: round1(float64(r.v) * 100 / float64(maxGen)),
		})
	}

	// Accepted (dominant AI tool with suggested>0).
	vm.TopModel, vm.Accepted = topModelAndAccepted(note)

	// Tokens + coding.
	if t.Tokens != nil {
		vm.TokIn, vm.TokOut = formatK(t.Tokens.Input), formatK(t.Tokens.Output)
		vm.TokCacheR, vm.TokCacheW = formatK(t.Tokens.CacheRead), formatK(t.Tokens.CacheWrite)
	} else {
		vm.TokIn, vm.TokOut, vm.TokCacheR, vm.TokCacheW = "0", "0", "0", "0"
	}
	if note.CodingTimeNanos > 0 {
		vm.Coding = fmt.Sprintf("~%d min", note.CodingTimeNanos/int64(60*1e9))
	} else {
		vm.Coding = "~0 min"
	}

	// Files.
	vm.FilesChanged = len(note.Files)
	for _, f := range note.Files {
		tot := f.Added + f.Deleted
		if tot == 0 {
			tot = 1
		}
		name, lines := fileTopAITool(f)
		vm.Files = append(vm.Files, htmlFile{
			Name: f.Path, Added: f.Added, Deleted: f.Deleted,
			Attr: name, AttrLines: lines,
			AddedW:   round1(float64(f.Added) * 100 / float64(tot)),
			DeletedW: round1(float64(f.Deleted) * 100 / float64(tot)),
			Ranges:   fileRanges(f),
		})
	}

	// Leaderboard: each model + a human row (counting additions AND deletions),
	// by lines desc.
	vm.Leaders, vm.Contributors = buildLeaders(note, added+deleted, firstNonEmpty(meta["author"], "human"))

	// Per-tool usage rows (icon · lines · model · acceptance · tokens).
	vm.Tools = buildTools(note)
	return vm
}

// donutArcs computes the SVG stroke-dasharray/offset for the AI and Human arcs
// of the attribution ring (r=52). Mirrors the JSX donut geometry.
func donutArcs(aiPct, humanPct float64) (aiDash, humanDash, humanOff string) {
	const r = 52.0
	c := 2 * math.Pi * r
	const gap = 2.0
	aiLen := aiPct / 100 * c
	humanLen := humanPct / 100 * c
	aiDash = fmt.Sprintf("%.2f %.2f", math.Max(aiLen-gap, 0), c)
	humanDash = fmt.Sprintf("%.2f %.2f", math.Max(humanLen-gap, 2), c)
	humanOff = fmt.Sprintf("%.2f", -(aiLen + gap))
	return
}

// topModelAndAccepted returns the busiest AI tool's model name and its
// suggested/accepted acceptance summary (nil when no AI tool reports one).
func topModelAndAccepted(note *gitnotes.Note) (string, *htmlAccepted) {
	var bestName string
	var best gitnotes.Tool
	for name, tl := range note.ByTool {
		if name == "human" || name == "copypaste" {
			continue
		}
		if tl.Lines > best.Lines {
			best, bestName = tl, name
		}
	}
	_ = bestName
	model := ""
	if best.Model != nil {
		model = *best.Model
	}
	var acc *htmlAccepted
	if best.SuggestedLines > 0 {
		// Clamp at 100%: suggested_lines is the watcher's measured proposal, but a
		// tool can also have committed lines from edits that didn't measure one, so
		// accepted (all kept lines) can exceed it — show "100%", not ">100%".
		acc = &htmlAccepted{
			Pct:       min(int(int64(best.AcceptedLines)*100/best.SuggestedLines), 100),
			Suggested: best.SuggestedLines,
			Kept:      int64(best.AcceptedLines),
		}
	}
	return model, acc
}

// fileTopAITool returns the AI tool that contributed the most added lines to f
// and that line count, or "" when only human/deletion lines are present.
func fileTopAITool(f gitnotes.FileEntry) (string, int) {
	counts := map[string]int{}
	for _, l := range f.Lines {
		if l.Type != "add" || l.Tool == "" || l.Tool == "human" || l.Tool == "copypaste" {
			continue // copypaste is human-authored, not an AI tool
		}
		counts[l.Tool] += l.NumLines()
	}
	var name string
	var best int
	for n, c := range counts {
		if c > best {
			name, best = n, c
		}
	}
	return name, best
}

// buildLeaders ranks contributors by total lines touched — additions AND
// deletions — so a commit where a human only deletes code still lists that
// human (rather than "no contributors"), and AI deletions count toward the
// model that made them. `total` is the added+deleted denominator for the
// percentage. Deletions aren't tracked per-model in the note, so AI deletions
// are apportioned across models by their added-line share (or shown as a
// generic "AI" entry when the AI added nothing).
func buildLeaders(note *gitnotes.Note, total int, authorName string) ([]htmlLeader, int) {
	type entry struct {
		name    string
		lines   int
		isModel bool
	}
	t := note.Totals
	aiDeleted := t.AIDeletedLines
	humanDeleted := t.DeletedLines - aiDeleted

	var entries []entry
	var modelAddedTotal int
	models := make([]string, 0, len(t.Models))
	for m, added := range t.Models {
		models = append(models, m)
		modelAddedTotal += added
	}
	sort.Strings(models) // deterministic distribution order
	if len(models) > 0 {
		remaining := aiDeleted
		for i, m := range models {
			share := 0
			if aiDeleted > 0 {
				if i == len(models)-1 || modelAddedTotal == 0 {
					share = remaining // last model soaks up the rounding remainder
				} else {
					share = aiDeleted * t.Models[m] / modelAddedTotal
					remaining -= share
				}
			}
			entries = append(entries, entry{m, t.Models[m] + share, true})
		}
	} else if aiByTool := buildLeaderToolEntries(note); len(aiByTool) > 0 {
		// No per-model rollup (e.g. an Antigravity edit recorded before its model
		// was resolvable). Fall back to by_tool so the AI still appears, labeled
		// by its model if known, else the tool name.
		for _, e := range aiByTool {
			entries = append(entries, entry{e.name, e.lines, true})
		}
	} else if aiDeleted > 0 {
		// AI removed lines but recorded no per-model additions (deletion-only).
		entries = append(entries, entry{"AI", aiDeleted, true})
	}
	if h := t.HumanLines + humanDeleted; h > 0 {
		entries = append(entries, entry{authorName, h, false})
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].lines > entries[j].lines })
	totalAdded := total
	if totalAdded == 0 {
		totalAdded = 1
	}
	out := make([]htmlLeader, 0, len(entries))
	for i, e := range entries {
		pct := round1(float64(e.lines) * 100 / float64(totalAdded))
		initial := ""
		if !e.isModel {
			initial = strings.ToLower(firstRune(e.name))
		}
		l := htmlLeader{
			Rank: i + 1, Name: e.name, Lines: e.lines,
			Pct: fmt.Sprintf("%.1f", pct), WidthPct: pct,
			IsModel: e.isModel, Initial: initial,
		}
		if e.isModel {
			if tool := toolForModel(e.name); tool != "" {
				if g, ok := toolGlyphs[tool]; ok {
					l.Icon, l.Color = g.icon, g.color
				}
			}
		}
		out = append(out, l)
	}
	return out, len(entries)
}

// buildLeaderToolEntries derives leaderboard AI entries from by_tool when the
// per-model rollup is empty: one entry per contributing AI tool, labeled by its
// model when known else the tool name (which toolForModel maps back to the same
// tool glyph), with the commit's AI deletions apportioned by added-line share.
func buildLeaderToolEntries(note *gitnotes.Note) []struct {
	name  string
	lines int
} {
	out := make([]struct {
		name  string
		lines int
	}, 0, 5)
	for _, name := range []string{"claude", "cursor", "codex", "copilot", "gemini", "devin"} {
		tl, ok := note.ByTool[name]
		if !ok || (tl.Lines == 0 && tl.DeletedLines == 0) {
			continue
		}
		label := name
		if tl.Model != nil && *tl.Model != "" {
			label = *tl.Model
		}
		// Count added + deleted so a tool that only removed code still ranks. by_tool
		// already includes THIS tool's deletions, so we must NOT also distribute the
		// commit's aiDeleted total on top — that double-counted (a 12-line deletion
		// showed as 24 in the leaderboard).
		out = append(out, struct {
			name  string
			lines int
		}{label, tl.Lines + tl.DeletedLines})
	}
	return out
}

// toolForModel maps a concrete model identifier to the AI tool that runs it, so
// the leaderboard can show a real tool glyph next to a model name.
func toolForModel(model string) string {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "claude"), strings.Contains(m, "opus"),
		strings.Contains(m, "sonnet"), strings.Contains(m, "haiku"):
		return "claude"
	case strings.Contains(m, "composer"), strings.Contains(m, "cursor"):
		return "cursor"
	case strings.Contains(m, "gemini"):
		return "gemini"
	case strings.Contains(m, "copilot"):
		return "copilot"
	case strings.Contains(m, "gpt"), strings.HasPrefix(m, "o1"),
		strings.HasPrefix(m, "o3"), strings.HasPrefix(m, "o4"), strings.Contains(m, "codex"):
		return "codex"
	}
	return ""
}

// buildTools assembles the per-tool usage rows: each contributing AI tool with
// its authored lines, model, acceptance, and tokens. Bar widths are relative to
// the busiest tool.
func buildTools(note *gitnotes.Note) []htmlTool {
	order := []string{"claude", "cursor", "codex", "copilot", "gemini", "devin"}
	maxLines := 1
	for _, name := range order {
		if tl, ok := note.ByTool[name]; ok && tl.Lines+tl.DeletedLines > maxLines {
			maxLines = tl.Lines + tl.DeletedLines
		}
	}
	var out []htmlTool
	for _, name := range order {
		tl, ok := note.ByTool[name]
		if !ok || (tl.Lines == 0 && tl.DeletedLines == 0 && tl.SuggestedLines == 0) {
			continue
		}
		g := toolGlyphs[name]
		ht := htmlTool{
			Name: name, Icon: g.icon, Color: g.color,
			Lines:    tl.Lines,
			Deleted:  tl.DeletedLines,
			WidthPct: round1(float64(tl.Lines+tl.DeletedLines) * 100 / float64(maxLines)),
		}
		if tl.Model != nil {
			ht.Model = *tl.Model
		}
		if tl.SuggestedLines > 0 {
			ht.HasAccept = true
			ht.Suggested = tl.SuggestedLines
			ht.Kept = int64(tl.AcceptedLines)
			ht.AcceptPct = min(int(int64(tl.AcceptedLines)*100/tl.SuggestedLines), 100)
		}
		if tk := tl.Tokens; tk != nil && (tk.Input|tk.Output|tk.CacheRead|tk.CacheWrite) != 0 {
			ht.HasTokens = true
			ht.TokIn, ht.TokOut = formatK(tk.Input), formatK(tk.Output)
			ht.TokCacheR, ht.TokCacheW = formatK(tk.CacheRead), formatK(tk.CacheWrite)
		}
		out = append(out, ht)
	}
	return out
}

// fileRanges builds the per-range attribution detail for a file: each line span
// with its tool · model · gen_type (AI) or "human" / "—" (deletions).
func fileRanges(f gitnotes.FileEntry) []htmlRange {
	out := make([]htmlRange, 0, len(f.Lines))
	for _, l := range f.Lines {
		loc := fmt.Sprintf("L%d", l.Start)
		if l.End > l.Start {
			loc = fmt.Sprintf("L%d-%d", l.Start, l.End)
		}
		attr, isAI := rangeAttr(l)
		out = append(out, htmlRange{Loc: loc, Type: l.Type, Attr: attr, IsAI: isAI})
	}
	return out
}

// rangeAttr renders one range's authorship and whether it's AI-attributed.
func rangeAttr(l gitnotes.RangeEntry) (string, bool) {
	// copypaste is a Human-side tool tag (author_type stays Human): render it as
	// human with a copy-paste note, never as an AI generator.
	if l.Tool == "copypaste" {
		return "human · copy-paste", false
	}
	if l.Tool != "" && l.Tool != "human" {
		s := toolLabel(l.Tool)
		var extra []string
		if l.Model != nil && *l.Model != "" {
			extra = append(extra, *l.Model)
		}
		if l.GenType != nil && *l.GenType != "" {
			extra = append(extra, *l.GenType)
		}
		if len(extra) > 0 {
			s += " · " + strings.Join(extra, " · ")
		}
		return s, true
	}
	if l.AuthorType != "" {
		return "human", false
	}
	return "—", false
}

// ---- small format helpers ----------------------------------------------

func round1(f float64) float64 { return math.Round(f*10) / 10 }

func maxInt(xs ...int) int {
	m := xs[0]
	for _, x := range xs[1:] {
		if x > m {
			m = x
		}
	}
	return m
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func firstNonEmpty(parts ...string) string {
	for _, p := range parts {
		if p != "" {
			return p
		}
	}
	return ""
}

func firstRune(s string) string {
	for _, r := range s {
		return string(r)
	}
	return "?"
}

// gitDateLayout matches `git show --format=%ci` ("2006-01-02 15:04:05 -0700").
const gitDateLayout = "2006-01-02 15:04:05 -0700"

func shortDate(date string) string {
	if t, err := time.Parse(gitDateLayout, date); err == nil {
		return t.Format("2006-01-02 15:04")
	}
	return date
}

func agoFromDate(date string) string {
	if t, err := time.Parse(gitDateLayout, date); err == nil {
		return humanDuration(time.Since(t))
	}
	return ""
}
