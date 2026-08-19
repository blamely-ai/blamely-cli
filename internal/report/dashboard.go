package report

import (
	"fmt"
	"io"
	"strings"

	"github.com/blamely/blamely/internal/gitnotes"
)

// renderDashboard prints the commit report as a terminal "stats head" dashboard
// — the same layout as the HTML panel (commit header, an Attribution / Changes
// row, a full-width Generation block, a Files / Leaderboard row, and a tokens /
// coding footer) drawn with box-drawing cards and block-gauge bars — followed by
// the per-file line-range attribution detail. Reuses buildHTMLModel so the
// terminal and HTML views are fed by exactly the same numbers.
func renderDashboard(w io.Writer, note *gitnotes.Note, meta commitMeta_, withRanges bool) {
	vm := buildHTMLModel(note, meta)

	fmt.Fprintln(w)
	emitHeader(w, vm)

	fmt.Fprintln(w)
	ab, cb := equalize(attribBody(vm), changesBody(vm))
	emitLines(w, twoUp(card("Attribution", dashCol, ab), card("Changes", dashCol, cb)))
	fmt.Fprintln(w)
	emitLines(w, card("Generation", dashFull, genBody(vm)))
	if hasAITools(note) {
		fmt.Fprintln(w)
		emitLines(w, card("Tools", dashFull, toolsBody(note)))
	}
	fmt.Fprintln(w)
	fb, lb := equalize(filesBody(vm), leaderBody(vm))
	emitLines(w, twoUp(card("Files", dashCol, fb), card("Leaderboard", dashCol, lb)))

	fmt.Fprintln(w)
	emitFooter(w, vm)

	if withRanges && len(note.Files) > 0 {
		fmt.Fprintln(w)
		emitLines(w, card("File ranges", dashFull, fileRangesBody(note)))
	}

	fmt.Fprintln(w)
	versionLine(w, noteVersion(note))
	fmt.Fprintln(w)
}

const (
	dashFull = 78
	dashCol  = 38
	dashGap  = 2
)

// RenderCommitSummary prints the compact inline summary shown by the post-commit
// hook right after `git commit`: a brand line, an AI/Human gauge, a one-glance
// changes line, and gen-type + coding. It's the small sibling of `blamely
// report` in the same visual language — full detail stays one command away
// (`blamely report HEAD`).
func RenderCommitSummary(w io.Writer, note *gitnotes.Note) {
	t := note.Totals
	ai, human := t.AILines, t.HumanLines
	added := ai + human
	deleted := t.DeletedLines

	sha := note.Commit
	if len(sha) > 8 {
		sha = sha[:8]
	}
	head := green("●") + " " + bold("Blamely") + "  " + dim(sha)
	var ps []string
	if note.Branch != "" {
		ps = append(ps, note.Branch)
	}
	if s := firstLine(note.Message); s != "" {
		ps = append(ps, "“"+truncPlain(s, 46)+"”")
	}
	if len(ps) > 0 {
		head += dim("  " + glyphDot + "  " + strings.Join(ps, dim("  "+glyphDot+"  ")))
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, gutter+head)
	fmt.Fprintln(w, gutter+hairline(ruleW))

	if added == 0 && deleted == 0 {
		fmt.Fprintln(w, gutter+dim("no attributable changes"))
		fmt.Fprintln(w)
		return
	}

	// The AI/Human gauge counts additions AND deletions, so a deletion-only
	// commit still shows a progress bar (its AI-vs-human deletion split) rather
	// than dropping it — and a commit where a human deletes code isn't headlined
	// "AI 100%" off the few added lines alone.
	aiTouched := ai + t.AIDeletedLines
	humanTouched := human + (t.DeletedLines - t.AIDeletedLines)
	aiPct := 0.0
	if tot := aiTouched + humanTouched; tot > 0 {
		aiPct = float64(aiTouched) * 100 / float64(tot)
	}
	fmt.Fprintf(w, "%s%s  %s  %s\n", gutter,
		style(ansiGreen+ansiBold, fmt.Sprintf("AI %.0f%%", aiPct)),
		stackBar(28, aiPct, ansiGreen, ansiBlue),
		style(ansiBlue+ansiBold, fmt.Sprintf("Human %.0f%%", 100-aiPct)))

	// Changes line: omit the "+0 added" half for a deletion-only commit.
	var parts []string
	if added > 0 {
		parts = append(parts, green(fmt.Sprintf("+%d", added))+" "+dim("added"))
	}
	if deleted > 0 {
		parts = append(parts, red(fmt.Sprintf("−%d", deleted))+" "+dim("deleted"))
	}
	fmt.Fprintln(w, gutter+strings.Join(parts, "   "))

	// Per-tool detail (tool · lines · model · tokens).
	emitToolDetail(w, note)

	// Generation status bars (chat / cli / completion / human).
	if g := note.ByGenType; g.Chat+g.CLI+g.Completion+g.Human > 0 {
		sectionHead(w, "Generation")
		renderGenRows(w, g)
	}
	if note.CodingTimeNanos > 0 {
		fmt.Fprintf(w, "\n%s%s\n", gutter,
			dim(fmt.Sprintf("~%d min   first edit %s commit", note.CodingTimeNanos/int64(60*1e9), glyphArr)))
	}
	fmt.Fprintln(w)
}

// emitToolDetail prints a "Tools" section with one row per contributing AI
// tool: name · line count · model · tokens — the same per-tool breakdown the
// dashboard's Attribution card carries, compacted for the post-commit summary.
func emitToolDetail(w io.Writer, note *gitnotes.Note) {
	var names []string
	for _, name := range []string{"claude", "cursor", "codex", "copilot", "gemini", "devin"} {
		if tl, ok := note.ByTool[name]; ok && tl.Lines > 0 {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return
	}
	sectionHead(w, "Tools")
	for _, name := range names {
		tl := note.ByTool[name]
		s := green(fmt.Sprintf("%-9s", name)) + dim(fmt.Sprintf("%d lines", tl.Lines))
		if tl.Model != nil && *tl.Model != "" {
			s += dim("   " + *tl.Model)
		}
		if tl.Tokens != nil {
			s += dim(fmt.Sprintf("   in %s · out %s · cache %s",
				formatK(tl.Tokens.Input), formatK(tl.Tokens.Output), formatK(tl.Tokens.CacheRead)))
		}
		fmt.Fprintln(w, gutter+"  "+s)
	}
}

// toolsBody renders the "Tools" card: one block per contributing AI tool with
// its authored line count, model, acceptance (how much of the model's proposal
// was kept), and token usage. Surfaces per-tool detail that's otherwise only
// hinted at by the single model chip in the Attribution card.
func toolsBody(note *gitnotes.Note) []string {
	out := []string{""}
	any := false
	for _, name := range []string{"claude", "cursor", "codex", "copilot", "gemini", "devin"} {
		tl, ok := note.ByTool[name]
		if !ok || (tl.Lines == 0 && tl.DeletedLines == 0 && tl.SuggestedLines == 0) {
			continue
		}
		any = true
		head := green(padRight(name, 9)) + " " + bold(fmt.Sprintf("%d", tl.Lines)) + dim(" lines")
		if tl.DeletedLines > 0 {
			head += "  " + red(fmt.Sprintf("−%d", tl.DeletedLines)) + dim(" deleted")
		}
		if tl.Model != nil && *tl.Model != "" {
			head += dim("  ·  " + *tl.Model)
		}
		// Acceptance: the model proposed SuggestedLines; the user kept AcceptedLines.
		// Clamp at 100% — kept lines from edits that didn't measure a suggestion can
		// push accepted past the measured suggested.
		if tl.SuggestedLines > 0 {
			pct := min(int64(tl.AcceptedLines)*100/tl.SuggestedLines, 100)
			head += dim(fmt.Sprintf("  ·  kept %d/%d (%d%%)", tl.AcceptedLines, tl.SuggestedLines, pct))
		}
		out = append(out, head)
		if tl.Tokens != nil && (tl.Tokens.Input|tl.Tokens.Output|tl.Tokens.CacheRead|tl.Tokens.CacheWrite) != 0 {
			out = append(out, dim(fmt.Sprintf("    tokens  in %s · out %s · cache_r %s · cache_w %s",
				formatK(tl.Tokens.Input), formatK(tl.Tokens.Output),
				formatK(tl.Tokens.CacheRead), formatK(tl.Tokens.CacheWrite))))
		}
	}
	if !any {
		return []string{"", dim("no AI tools")}
	}
	return out
}

// hasAITools reports whether any AI tool contributed (gates the Tools card).
func hasAITools(note *gitnotes.Note) bool {
	for _, name := range []string{"claude", "cursor", "codex", "copilot", "gemini", "devin"} {
		if tl, ok := note.ByTool[name]; ok && (tl.Lines > 0 || tl.DeletedLines > 0 || tl.SuggestedLines > 0) {
			return true
		}
	}
	return false
}

func aiBig(s string) string { return style(ansiGreen+ansiBold, s) }

// ---- header / footer ----------------------------------------------------

func emitHeader(w io.Writer, vm htmlVM) {
	id := green("●") + " " + bold("Blamely") + dim(" report")
	if vm.Subject != "" {
		id += "   " + bold("“"+truncPlain(vm.Subject, 40)+"”")
	}
	id += "   " + dim(vm.ShortHash)
	fmt.Fprintln(w, gutter+id)
	if m := sep(vm.Branch, vm.Author, vm.Ago); m != "" {
		fmt.Fprintln(w, gutter+dim(m))
	}
	fmt.Fprintln(w, gutter+hairline(dashFull))
}

func emitFooter(w io.Writer, vm htmlVM) {
	fmt.Fprintf(w, "%s%s  %s\n", gutter, bold(fmt.Sprintf("%-7s", "Tokens")),
		dim(fmt.Sprintf("in %s · out %s · cache_read %s · cache_write %s",
			vm.TokIn, vm.TokOut, vm.TokCacheR, vm.TokCacheW)))
	fmt.Fprintf(w, "%s%s  %s\n", gutter, bold(fmt.Sprintf("%-7s", "Coding")),
		dim(vm.Coding+"   first edit "+glyphArr+" commit"))
}

// ---- card bodies --------------------------------------------------------

func attribBody(vm htmlVM) []string {
	return []string{
		"",
		aiBig(fmt.Sprintf("%.1f%%", vm.AIPct)) + "  " + dim("AI authored"),
		stackBar(32, vm.AIPct, ansiGreen, ansiBlue),
		"",
		green("●") + " " + padRight("AI", 7) + dim(plural(vm.AILines, "line")),
		blue("●") + " " + padRight("Human", 7) + dim(plural(vm.HumanLines, "line")),
		modelChip(vm.TopModel),
	}
}

func changesBody(vm htmlVM) []string {
	head := green(fmt.Sprintf("+%d", vm.Added)) + " " + dim("added") + "   " +
		red(fmt.Sprintf("−%d", vm.Deleted)) + " " + dim("deleted") + "   " +
		bold(fmt.Sprintf("%d", vm.Net)) + " " + dim("net")
	row := func(dot, label string, n int, color func(string) string) string {
		return dot + " " + padRight(label, 14) + color(fmt.Sprintf("%d", n))
	}
	return []string{
		"",
		head,
		stackBar(32, vm.AddedPct, ansiGreen, ansiRed),
		"",
		row(green("●"), "AI added", vm.AIAdded, green),
		row(blue("●"), "Human added", vm.HumanAdded, blue),
		row(red("●"), "AI deleted", vm.AIDeleted, red),
		row(blue("●"), "Human deleted", vm.HumanDeleted, blue),
	}
}

func genBody(vm htmlVM) []string {
	out := make([]string, 0, len(vm.Gen)+1)
	for _, g := range vm.Gen {
		label := padRight(g.Label, 11)
		val := fmt.Sprintf("%4d", g.Value)
		pct := ""
		if !g.Zero {
			pct = fmt.Sprintf("   %5s%%", g.Pct)
		}
		if g.Zero {
			label, val = dim(label), dim(val)
		}
		bar := gaugeFill(42, g.WidthPct, genColor(g.Label), g.Zero)
		out = append(out, label+" "+val+"  "+bar+dim(pct))
	}
	if vm.Accepted != nil {
		out = append(out, "")
		out = append(out, dim("accepted ")+green(fmt.Sprintf("%d%%", vm.Accepted.Pct))+
			dim(fmt.Sprintf("   %d suggested %s %d kept", vm.Accepted.Suggested, glyphArr, vm.Accepted.Kept)))
	}
	return out
}

func filesBody(vm htmlVM) []string {
	if len(vm.Files) == 0 {
		return []string{"", dim("no files")}
	}
	out := make([]string, 0, len(vm.Files))
	for _, f := range vm.Files {
		nm := padRight(truncPlain(f.Name, 15), 15)
		add := green(fmt.Sprintf("+%-3d", f.Added))
		del := dim(fmt.Sprintf("−%-3d", f.Deleted))
		if f.Deleted > 0 {
			del = red(fmt.Sprintf("−%-3d", f.Deleted))
		}
		bar := stackBar(7, f.AddedW, ansiGreen, ansiRed)
		out = append(out, nm+" "+add+" "+del+" "+bar)
	}
	return out
}

func leaderBody(vm htmlVM) []string {
	if len(vm.Leaders) == 0 {
		return []string{"", dim("no contributors")}
	}
	out := make([]string, 0, len(vm.Leaders)*2)
	for _, l := range vm.Leaders {
		clr := ansiGreen
		if !l.IsModel {
			clr = ansiBlue
		}
		left := dim(fmt.Sprintf("%d", l.Rank)) + " " + truncPlain(l.Name, 20)
		right := bold(fmt.Sprintf("%d", l.Lines)) + " " + dim(l.Pct+"%")
		pad := 34 - visibleLen(left) - visibleLen(right)
		if pad < 1 {
			pad = 1
		}
		out = append(out, left+strings.Repeat(" ", pad)+right)
		out = append(out, gaugeFill(34, l.WidthPct, clr, false))
	}
	return out
}

func modelChip(model string) string {
	if model == "" {
		return ""
	}
	return green("✦ " + truncPlain(model, 30))
}

// ---- per-file line-range detail ----------------------------------------

// fileRangesBody builds the card body for the per-file line-range detail: a
// right-aligned +/- file header followed by one colored, column-aligned row per
// attributed range (loc · kind · author).
func fileRangesBody(note *gitnotes.Note) []string {
	bodyW := dashFull - 4
	var out []string
	for i, f := range note.Files {
		if i > 0 {
			out = append(out, "")
		}
		left := bold(truncPlain(f.Path, 44))
		if f.Type != "" {
			left += dim(" [" + f.Type + "]")
		}
		counts := green(fmt.Sprintf("+%d", f.Added)) + "  " + delColor(f.Deleted, fmt.Sprintf("−%d", f.Deleted))
		pad := bodyW - visibleLen(left) - visibleLen(counts)
		if pad < 1 {
			pad = 1
		}
		out = append(out, left+strings.Repeat(" ", pad)+counts)
		for _, l := range f.Lines {
			loc := fmt.Sprintf("L%d", l.Start)
			if l.End > l.Start {
				loc = fmt.Sprintf("L%d-%d", l.Start, l.End)
			}
			out = append(out, dim(glyphBar)+" "+dim(padRight(loc, 12))+dim(padRight(l.Type, 8))+termRangeAttr(l))
		}
	}
	return out
}

// termRangeAttr colors one range's authorship to match the dashboard palette:
// AI tool in green with a dimmed model · gen_type tail, "human" in amber, and a
// faint dash for unattributed deletions.
func termRangeAttr(l gitnotes.RangeEntry) string {
	if l.Tool == "copypaste" {
		// copypaste is human-authored — human colour with a copy-paste note.
		return blue("human") + dim(" · copy-paste")
	}
	if l.Tool != "" && l.Tool != "human" {
		s := green(toolLabel(l.Tool))
		var extra []string
		if l.Model != nil && *l.Model != "" {
			extra = append(extra, *l.Model)
		}
		if l.GenType != nil && *l.GenType != "" {
			extra = append(extra, *l.GenType)
		}
		if len(extra) > 0 {
			s += dim(" · " + strings.Join(extra, " · "))
		}
		return s
	}
	if l.AuthorType != "" {
		return blue("human")
	}
	return dim("—")
}

func delColor(n int, s string) string {
	if n > 0 {
		return red(s)
	}
	return dim(s)
}

// ---- card + layout primitives ------------------------------------------

// card wraps body lines in a rounded box with a titled header. Returns lines
// that are each exactly cw visible cells wide so columns stay aligned.
func card(title string, cw int, body []string) []string {
	bodyW := cw - 4
	dashes := cw - 7 - len([]rune(title))
	if dashes < 0 {
		dashes = 0
	}
	lines := make([]string, 0, len(body)+2)
	lines = append(lines, dim("╭─ ")+green("●")+" "+bold(title)+" "+dim(strings.Repeat("─", dashes)+"╮"))
	for _, b := range body {
		lines = append(lines, dim("│")+" "+padRight(b, bodyW)+" "+dim("│"))
	}
	lines = append(lines, dim("╰"+strings.Repeat("─", cw-2)+"╯"))
	return lines
}

// twoUp places two equal-width cards side by side, padding the shorter one with
// blank lines so both columns end together.
func twoUp(a, b []string) []string {
	n := len(a)
	if len(b) > n {
		n = len(b)
	}
	blank := strings.Repeat(" ", dashCol)
	gap := strings.Repeat(" ", dashGap)
	out := make([]string, n)
	for i := 0; i < n; i++ {
		la, lb := blank, blank
		if i < len(a) {
			la = a[i]
		}
		if i < len(b) {
			lb = b[i]
		}
		out[i] = la + gap + lb
	}
	return out
}

func emitLines(w io.Writer, lines []string) {
	for _, l := range lines {
		fmt.Fprintln(w, gutter+l)
	}
}

// equalize pads the shorter of two card bodies with blank lines so both cards
// in a row are the same height and their bottom borders line up.
func equalize(a, b []string) ([]string, []string) {
	for len(a) < len(b) {
		a = append(a, "")
	}
	for len(b) < len(a) {
		b = append(b, "")
	}
	return a, b
}

// ---- gauges + small helpers --------------------------------------------

// stackBar draws a two-segment proportional bar: pct in c1, the remainder in c2.
func stackBar(width int, pct float64, c1, c2 string) string {
	a := int(float64(width)*pct/100 + 0.5)
	if a > width {
		a = width
	}
	if a < 0 {
		a = 0
	}
	b := width - a
	if !colorEnabled() {
		return strings.Repeat("█", a) + strings.Repeat("░", b)
	}
	return c1 + strings.Repeat("█", a) + ansiReset + c2 + strings.Repeat("█", b) + ansiReset
}

// gaugeFill draws a single-color filled bar over a dim track (or an all-dim
// track when zero).
func gaugeFill(width int, pct float64, clr string, zero bool) string {
	if zero {
		if !colorEnabled() {
			return strings.Repeat("░", width)
		}
		return dim(strings.Repeat("░", width))
	}
	f := int(float64(width)*pct/100 + 0.5)
	if f > width {
		f = width
	}
	if f < 0 {
		f = 0
	}
	if !colorEnabled() {
		return strings.Repeat("█", f) + strings.Repeat("░", width-f)
	}
	return clr + strings.Repeat("█", f) + ansiReset + dim(strings.Repeat("░", width-f))
}

func genColor(label string) string {
	switch label {
	case "chat":
		return ansiGreen
	case "cli":
		return ansiCyan
	case "completion":
		return ansiMagenta
	default:
		return ansiBlue
	}
}

func plural(n int, unit string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, unit)
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// padRight pads s with spaces to w visible cells (ANSI escapes don't count).
// Returns s unchanged when it's already at least w wide.
func padRight(s string, w int) string {
	n := visibleLen(s)
	if n >= w {
		return s
	}
	return s + strings.Repeat(" ", w-n)
}

// visibleLen counts printable cells in s, skipping ANSI SGR escape sequences.
func visibleLen(s string) int {
	n, inEsc := 0, false
	for _, r := range s {
		if inEsc {
			if r == 'm' {
				inEsc = false
			}
			continue
		}
		if r == '\x1b' {
			inEsc = true
			continue
		}
		n++
	}
	return n
}

// truncPlain shortens a plain (un-colored) string to w cells, ending in an
// ellipsis when it had to cut.
func truncPlain(s string, w int) string {
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	if w <= 1 {
		return string(r[:w])
	}
	return string(r[:w-1]) + "…"
}
