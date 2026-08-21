package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/spf13/cobra"

	"time"

	"github.com/blamely/blamely/internal/authorship"
	"github.com/blamely/blamely/internal/config"
	"github.com/blamely/blamely/internal/daemon"
	"github.com/blamely/blamely/internal/gitnotes"
	"github.com/blamely/blamely/internal/gitutil"
	"github.com/blamely/blamely/internal/install"
	"github.com/blamely/blamely/internal/report"
	"github.com/blamely/blamely/internal/store"
	"github.com/blamely/blamely/internal/tools"
	"github.com/blamely/blamely/internal/updatehint"
)

// Wire Attribution note-seeding into the engine here, at the top level: main can
// import both authorship and gitnotes without a cycle (gitnotes already imports
// tools, so the hook can't live there). Before a file's first recorded edit, this
// seeds its working log from committed authorship so unchanged committed lines keep
// their real author across a commit. Flag-gated inside the callee.
func init() {
	authorship.SeedHook = func(repoRoot, branch, baseSHA, relPath string) {
		_ = gitnotes.SeedCommittedWorkingLog(repoRoot, branch, baseSHA, relPath)
	}
}

// version is the baseline product version, kept in sync with the VS Code and
// JetBrains plugins. The release workflow can still override it at link time via
// `-ldflags "-X main.version=<tag>"`; otherwise this hardcoded value is what
// `blamely --version` reports.
var version = "1.8.1"

// resolveVersion returns the effective CLI version. Precedence:
//  1. an explicit -ldflags override (release builds);
//  2. the module version Go records for `go install <module>@vX.Y.Z`;
//  3. a `dev+<short-commit>[-dirty]` stamp from the embedded VCS info that
//     `go build` adds for builds inside a git checkout;
//  4. the bare "dev" sentinel when no build metadata is available.
func resolveVersion() string {
	if version != "dev" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return version
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	var rev string
	var dirty bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return version
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	out := "dev+" + rev
	if dirty {
		out += "-dirty"
	}
	return out
}

func main() {
	// On Windows, switch the console to UTF-8 and enable ANSI color processing
	// so the install log's glyphs and colors render instead of mojibake. No-op
	// on Linux/macOS.
	initConsole()

	// Propagate the resolved version so notes stamp their writer (generated_by)
	// and reports show the running version, instead of a hardcoded placeholder.
	ver := resolveVersion()
	gitnotes.Version = ver
	report.Version = ver
	// The updater compares against this, and the daemon-side check needs it too
	// (resolveVersion lives here, out of reach of internal/daemon).
	install.Version = ver

	root := &cobra.Command{
		Use:   "blamely",
		Short: "Trace code changes and attribute them to AI tools or humans",
		Long: "Blamely watches your filesystem and AI-tool logs, then writes a per-line\n" +
			"AI-vs-human attribution report as a git note on every commit.",
		Version: resolveVersion(),
	}
	// Suppress Cobra's auto-generated `completion` command from the menu;
	// it's framework boilerplate and not a blamely feature.
	root.CompletionOptions.DisableDefaultCmd = true

	root.AddCommand(cmdDaemon())
	root.AddCommand(cmdInstall())
	root.AddCommand(cmdUpdate())
	root.AddCommand(cmdInstallJetbrainsZip())
	root.AddCommand(cmdUninstall())
	root.AddCommand(cmdRepair())
	root.AddCommand(cmdDetect())
	root.AddCommand(cmdRecord())
	root.AddCommand(cmdAttribute())
	root.AddCommand(cmdPostRewrite())
	root.AddCommand(cmdAuthorship())
	root.AddCommand(cmdRecordDeletion())
	root.AddCommand(cmdReport())
	root.AddCommand(cmdBlame())
	root.AddCommand(cmdStats())
	root.AddCommand(cmdHistory())
	root.AddCommand(cmdStatus())
	root.AddCommand(cmdDoctor())
	root.AddCommand(cmdLog())
	root.AddCommand(cmdConfig())

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// logWriter forwards a child process's output into the standard logger, one
// log line per output line, so the daemon's auto-update leaves the installer's
// own messages in daemon.log instead of dropping them.
//
// Child output arrives in arbitrary chunks, not whole lines, so Write buffers
// until a newline and logs complete lines only; CR is stripped so CRLF output
// from a Windows installer doesn't leave a trailing \r in daemon.log. A final
// unterminated line stays buffered — installer output ends with a newline, and
// a truncated last line is not worth a Flush hook on an io.Writer.
type logWriter struct {
	buf []byte
}

func (w *logWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			return len(p), nil
		}
		line := strings.TrimRight(string(w.buf[:i]), "\r")
		w.buf = w.buf[i+1:]
		if strings.TrimSpace(line) != "" {
			log.Printf("update: %s", line)
		}
	}
}

func cmdDaemon() *cobra.Command {
	var background bool
	c := &cobra.Command{
		Use:   "daemon",
		Short: "Run the long-lived attribution daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Autostart entry point (Windows Scheduled Task / Startup shortcut):
			// drop the console window the launcher gave us. Only ever set by the
			// autostart registration, so running `blamely daemon` by hand keeps
			// its terminal. No-op off Windows.
			if background {
				hideConsole()
			}
			// Watchers use direct, observable signals (hooks, log parsers,
			// editor plugin events). The velocity/heuristic watcher has been
			// removed: inline completions are now attributed at high confidence
			// by the VS Code and IntelliJ plugins via the
			// editor.action.inlineSuggest.commit command and AnActionListener
			// APIs respectively, both of which POST directly to /edit.
			daemon.Watchers = []daemon.Watcher{
				// Cursor: file-history presence signal (emit is a deliberate no-op —
				// see CursorWatcher.emit — so no replay/duplicate on restart) + logs.
				&tools.CursorWatcher{},
				&tools.CursorLogWatcher{},
				// Copilot: storage-touch signal (primes on first scan, no replay) + logs.
				&tools.CopilotWatcher{},
				&tools.CopilotLogWatcher{},
				// Claude Desktop's "cowork" sandbox has no PostToolUse hook; this
				// tails its undocumented logs to attribute its file edits/deletes.
				&tools.ClaudeDesktopWatcher{},
			}
			// DB-backed watchers persist their resume state (watcher_watermarks) so a
			// daemon restart doesn't re-parse/re-emit history. CopilotChatWatcher
			// watches Code/workspaceStorage; CursorChatWatcher watches
			// Cursor/workspaceStorage — they never overlap. CodexWatcher tails
			// ~/.codex session JSONL (per-file byte offset).
			daemon.DBWatcherFactories = []func(*store.DB) daemon.Watcher{
				func(db *store.DB) daemon.Watcher { return &tools.CodexWatcher{DB: db} },
				func(db *store.DB) daemon.Watcher { return &tools.CursorChatWatcher{DB: db} },
				// Copilot Chat is recorded in REAL TIME from the extension's append-only
				// transcript stream (GitHub.copilot-chat/transcripts), enriched with
				// model + tokens read from the sibling chatSessions file. This replaces
				// the old CopilotChatWatcher (which read the lazily-flushed chatSessions
				// directly and lagged minutes behind, attributing edits to Human); it's
				// retired here so the same edit isn't recorded twice (double-counted tokens).
				func(db *store.DB) daemon.Watcher { return &tools.CopilotTranscriptWatcher{DB: db} },
				func(db *store.DB) daemon.Watcher { return &tools.AntigravityGeminiWatcher{DB: db} },
				// Session-level (not per-edit) metric: the Copilot CLI's cumulative
				// per-model token totals from each session's terminal shutdown event.
				func(db *store.DB) daemon.Watcher { return &tools.CopilotCliUsageWatcher{DB: db} },
			}
			// Self-heal the autostart registration every time the daemon comes up,
			// however it was started (logon task, keepalive, editor-plugin respawn,
			// or by hand). Windows Scheduled Tasks have no launchd/systemd-style
			// KeepAlive and older installs miss the on-battery settings, so a
			// broken machine converges to a working autostart on first daemon
			// start. No-op on macOS/Linux. Async + best-effort: schtasks/
			// powershell must never delay or fail daemon startup.
			go func() {
				if exe, err := os.Executable(); err == nil {
					if err := install.EnsureDaemonAgent(exe); err != nil {
						log.Printf("daemon agent self-heal: %v", err)
					}
				}
			}()
			// Hand the daemon its update check/apply as closures: internal/daemon
			// cannot import internal/install (install imports daemon), so this is
			// the same indirection the watcher lists above use.
			daemon.CurrentVersion = install.Version
			daemon.CheckUpdate = func(ctx context.Context) (updatehint.Hint, bool, error) {
				rel, newer, err := install.CheckForUpdate(ctx, install.Version)
				if err != nil {
					return updatehint.Hint{}, false, err
				}
				_, url, _ := rel.ArchiveURL()
				return updatehint.Hint{Version: rel.Version, Tag: rel.Tag, URL: url}, newer, nil
			}
			daemon.ApplyUpdate = func(ctx context.Context) error {
				// Route the updater's own output — including the staged
				// installer's stderr — into daemon.log. It used to be discarded,
				// which left a failed auto-update reported as an exit status with
				// no diagnosis anywhere on the machine.
				_, err := install.Update(ctx, install.UpdateOptions{Current: install.Version, Out: &logWriter{}})
				return err
			}
			return daemon.Run(cmd.Context())
		},
	}
	c.Flags().BoolVar(&background, "background", false,
		"started by the autostart registration: detach from the launcher's console (Windows; no-op elsewhere)")
	_ = c.Flags().MarkHidden("background")
	return c
}

func cmdRepair() *cobra.Command {
	var dryRun bool
	c := &cobra.Command{
		Use:   "repair",
		Short: "Configure hooks for newly-detected AI tools, remove stale hooks",
		Long: "Re-checks every AI tool blamely supports (Claude, Cursor, Codex,\n" +
			"Copilot, Gemini): if one is now present but its hook was never\n" +
			"configured — e.g. you installed it after running `blamely install` —\n" +
			"repair configures it.\n\n" +
			"Also scans your home directory for .git/hooks/post-commit files written\n" +
			"by an old blamely or blamely-cli installation and removes them. The\n" +
			"global core.hooksPath hook installed by `blamely install` takes over.\n\n" +
			"--dry-run only previews stale-hook removal; it does not write hooks.",
		RunE: func(cmd *cobra.Command, args []string) error {
			result, err := install.Repair(dryRun)
			if err != nil {
				return err
			}
			if dryRun {
				if len(result.Found) == 0 {
					fmt.Println("No stale blamely hooks found.")
					return nil
				}
				fmt.Printf("Would remove %d stale hook(s):\n", len(result.Found))
				for _, f := range result.Found {
					fmt.Printf("  - %s\n", f)
				}
				fmt.Println("\nRun without --dry-run to remove them.")
				return nil
			}
			for _, p := range result.Removed {
				fmt.Printf("  ✓ removed stale hook %s\n", p)
			}
			for _, h := range result.HooksAdded {
				fmt.Printf("  ✓ configured hook for %s\n", h)
			}
			for _, e := range result.Errors {
				fmt.Printf("  ✗ %s\n", e)
			}
			if len(result.Removed) == 0 && len(result.HooksAdded) == 0 && len(result.Errors) == 0 {
				fmt.Println("Nothing to repair — hooks are configured and no stale entries were found.")
				return nil
			}
			if len(result.Removed) > 0 {
				fmt.Printf("\nRemoved %d stale hook(s). Your commits will now use the global hook at\n", len(result.Removed))
				if hooksDir, err := install.GitHooksDirPath(); err == nil {
					fmt.Printf("  %s/post-commit\n", hooksDir)
				}
			}
			return nil
		},
	}
	c.Flags().BoolVar(&dryRun, "dry-run", false, "show what would be removed without actually removing")
	return c
}

func cmdDetect() *cobra.Command {
	return &cobra.Command{
		Use:   "detect",
		Short: "Print which AI tools Blamely found on this machine (read-only)",
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := install.Detect()
			if err != nil {
				return err
			}
			for _, row := range []struct {
				name string
				p    install.ToolPresence
			}{
				{"Claude Code", d.Claude},
				{"Cursor", d.Cursor},
				{"Codex CLI", d.Codex},
				{"GitHub Copilot", d.Copilot},
				{"Gemini CLI", d.Gemini},
			} {
				mark := "absent"
				if row.p.Present {
					mark = "found"
				}
				fmt.Printf("%-16s %s\n", row.name, mark)
				for _, h := range row.p.Hints {
					fmt.Printf("  - %s\n", h)
				}
			}
			return nil
		},
	}
}

func cmdInstall() *cobra.Command {
	var skipPlugins bool
	c := &cobra.Command{
		Use:   "install",
		Short: "Install Blamely (Claude hook, global git hook, daemon agent)",
		RunE: func(cmd *cobra.Command, args []string) error {
			// `sudo blamely install` would set up the per-user daemon under root;
			// drop back to the invoking user so it installs in their session.
			if err := install.DropToInvokingUserIfRoot(); err != nil {
				return err
			}
			if err := install.Run(!skipPlugins); err != nil {
				return err
			}
			// Show the channel's release notes (best-effort; never fails install).
			install.PrintWhatsNew()
			return nil
		},
	}
	c.Flags().BoolVar(&skipPlugins, "skip-plugins", false,
		"skip installing the VS Code-family/JetBrains IDE plugins (CLI + hooks only — handy for local dev builds)")
	return c
}

func cmdUpdate() *cobra.Command {
	var (
		check       bool
		force       bool
		dryRun      bool
		skipPlugins bool
		channel     string
		from        string
		sha256Sum   string
	)
	c := &cobra.Command{
		Use:   "update",
		Short: "Update blamely to the latest release",
		Long: "Download and install the newest blamely, then re-register the tool hooks\n" +
			"and restart the daemon.\n\n" +
			"The download is checked against the release's SHA256SUMS and the new binary\n" +
			"is run once before anything is replaced, so a bad download can never leave\n" +
			"you without a working install.\n\n" +
			"Air-gapped or behind a proxy that blocks api.github.com:\n" +
			"  blamely update --from ./blamely_v1.8.0_linux_amd64.tar.gz --sha256 <sum>\n" +
			"  BLAMELY_UPDATE_API=https://mirror.corp/repos/x/y blamely update\n\n" +
			"The daemon already does this for you once a day. To be notified without\n" +
			"installing: blamely config set update.auto off — or stop checking entirely\n" +
			"with blamely config set update.check off.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			if check {
				// Report only — never downloads, never touches the installed copy.
				ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
				defer cancel()
				rel, newer, err := install.CheckForUpdate(ctx, install.Version)
				if err != nil {
					return err
				}
				if !newer {
					fmt.Fprintf(out, "blamely %s is up to date\n", install.Version)
					return nil
				}
				fmt.Fprintf(out, "update available: %s -> %s  (run `blamely update`)\n", install.Version, rel.Version)
				return nil
			}
			// `sudo blamely update` would reinstall the per-user daemon under
			// root; drop back to the invoking user, exactly like install does.
			if err := install.DropToInvokingUserIfRoot(); err != nil {
				return err
			}
			res, err := install.Update(cmd.Context(), install.UpdateOptions{
				Current:     install.Version,
				Channel:     channel,
				Force:       force,
				DryRun:      dryRun,
				SkipPlugins: skipPlugins,
				FromArchive: from,
				ExpectSHA:   sha256Sum,
				Out:         out,
			})
			if err != nil {
				return err
			}
			if !res.Updated {
				return nil
			}
			fmt.Fprintf(out, "updated %s -> %s\n", res.From, res.To)
			// The hint (if any) described the version we just installed.
			_ = updatehint.Clear()
			install.PrintWhatsNew()
			return nil
		},
	}
	c.Flags().BoolVar(&check, "check", false, "only report whether a newer version exists")
	c.Flags().BoolVar(&force, "force", false, "install even at the same version, and from a non-installed copy")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "resolve and compare versions, then stop before downloading")
	c.Flags().BoolVar(&skipPlugins, "skip-plugins", false,
		"leave the IDE plugins alone (they are updated with the CLI by default, so all three stay on one version)")
	c.Flags().StringVar(&channel, "channel", "", "release channel to update from (default: latest)")
	c.Flags().StringVar(&from, "from", "", "install this local release archive instead of downloading (air-gapped)")
	c.Flags().StringVar(&sha256Sum, "sha256", "", "expected sha256 of --from's archive (required with --from)")
	return c
}

// cmdInstallJetbrainsZip lets the offline/beta installers sideload a LOCAL plugin
// zip using the CLI's robust IDE discovery (findJetBrainsIDEs), instead of a shell
// glob over %APPDATA%\JetBrains that misses Toolbox installs and not-yet-launched
// IDEs. Hidden: it's an installer helper, not an end-user command.
func cmdInstallJetbrainsZip() *cobra.Command {
	return &cobra.Command{
		Use:    "install-jetbrains-zip <plugin.zip>",
		Short:  "Install a local JetBrains plugin zip into every detected IDE",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			results := install.InstallJetBrainsPluginFromZip(args[0])
			n := 0
			for _, r := range results {
				if r.Err != nil {
					fmt.Fprintf(os.Stderr, "  %s: %v\n", r.Label, r.Err)
					continue
				}
				if r.Installed || r.Updated {
					n++
					fmt.Printf("  %s: installed (%s)\n", r.Label, r.PluginsDir)
				}
			}
			if n == 0 {
				fmt.Println("no JetBrains IDEs detected")
				return nil
			}
			fmt.Printf("JetBrains plugin installed into %d IDE(s) — restart them to load it\n", n)
			return nil
		},
	}
}

func cmdUninstall() *cobra.Command {
	var keepDB bool
	c := &cobra.Command{
		Use:   "uninstall",
		Short: "Reverse `blamely install` (removes the entire ~/.blamely directory)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return install.Uninstall(keepDB)
		},
	}
	c.Flags().BoolVar(&keepDB, "keep-db", false,
		"keep the attribution database (~/.blamely/db.sqlite); everything else is still removed")
	return c
}

func cmdRecord() *cobra.Command {
	c := &cobra.Command{
		Use: "record <tool>",
		// Hidden — this is the entry point each AI tool's PostToolUse hook
		// calls (see internal/install/{claude,cursor,...}hook.go). Not for
		// direct user invocation.
		Hidden: true,
		Short:  "Internal: ingest an AI-tool edit event from stdin",
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Always exit 0. This command IS a hook, and blamely deliberately
			// runs first in each tool's hook chain (see the install hooks). A
			// non-zero exit here could abort the rest of that chain — i.e. break
			// the very tools blamely sits in front of. So we record best-effort
			// and swallow errors (surfaced on stderr for debugging only); a
			// failed recording must never block the host tool.

			// --pre: PreToolUse mode. Snapshot the target file's pre-edit content
			// as the Attribution baseline (flag-gated, best-effort) instead of
			// recording an edit. The matching PostToolUse `record` then diffs the
			// agent's write against this exact baseline.
			if pre, _ := cmd.Flags().GetBool("pre"); pre {
				if err := tools.CaptureBaselineFromStdin(os.Stdin); err != nil {
					fmt.Fprintf(os.Stderr, "blamely record %s --pre: %v\n", args[0], err)
				}
				return nil
			}

			var recErr error
			switch args[0] {
			case "claude", "cursor":
				// Claude Code and Cursor share the PostToolUse payload shape;
				// the handler distinguishes them via `cursor_version`.
				recErr = tools.RecordClaudeFromStdin(os.Stdin)
			case "codex":
				recErr = tools.RecordCodexFromStdin(os.Stdin)
			case "copilot":
				recErr = tools.RecordCopilotFromStdin(os.Stdin)
			case "gemini":
				recErr = tools.RecordGeminiFromStdin(os.Stdin)
			default:
				recErr = fmt.Errorf("unknown tool %q (supported: claude, cursor, codex, copilot, gemini)", args[0])
			}
			if recErr != nil {
				fmt.Fprintf(os.Stderr, "blamely record %s: %v\n", args[0], recErr)
			}
			return nil
		},
	}
	c.Flags().Bool("pre", false, "PreToolUse mode: snapshot the target file's pre-edit content as the Attribution baseline (no edit recorded)")
	return c
}

// cmdAuthorship is the Attribution gutter source (invariant I4: note and gutter
// derive from the same working log). For a file it seeds the working log from
// committed authorship when untracked, then prints the working log as JSON — so the
// editor renders committed + uncommitted authorship from one place. Hidden; the
// plugins call it for the gutter.
// restrictLinesToChanged keeps only the per-line authorship for lines in `changed`
// (the uncommitted-diff set), re-collapsing runs that share an author. An empty
// `changed` yields no lines — the gutter shows nothing once a file has no changes.
// allWorkingLogLines returns the set of every line number covered by the working
// log's authorship ranges. Used as the "changed" set for untracked files, which
// don't appear in `git diff HEAD` — so the gutter shows their full attribution on
// creation instead of staying blank until `git add`.
func allWorkingLogLines(wl *authorship.WorkingLog) map[int]bool {
	set := map[int]bool{}
	for _, r := range wl.Lines {
		for ln := r.Start; ln <= r.End; ln++ {
			set[ln] = true
		}
	}
	return set
}

func restrictLinesToChanged(lines []authorship.LineAttribution, changed map[int]bool, overrides map[int]authorship.Author) []authorship.LineAttribution {
	if len(changed) == 0 {
		return nil
	}
	var out []authorship.LineAttribution
	for _, r := range lines {
		for ln := r.Start; ln <= r.End; ln++ {
			if !changed[ln] {
				continue
			}
			author, overrode := r.Author, r.Overrode
			// A content_sha reconciliation (gutter twin of the note path) may upgrade
			// a Human line the working-log fold couldn't resolve to AI; that clears
			// any human-override marker the line carried.
			if ov, ok := overrides[ln]; ok {
				author, overrode = ov, nil
			}
			if n := len(out); n > 0 && out[n-1].End == ln-1 && out[n-1].Author == author && out[n-1].Overrode == overrode {
				out[n-1].End = ln
				continue
			}
			out = append(out, authorship.LineAttribution{Start: ln, End: ln, Author: author, Overrode: overrode})
		}
	}
	return out
}

// cmdRecordDeletion records the baseline lines an AI edit REMOVED, so committed
// deletions attribute to the AI tool instead of falling back to Human. The IDE
// trackers call it when an AI edit shrinks a file (the CLI `record` path already does
// this for tool hooks; the editor previously did not, so editor-originated AI
// deletions were lost). The CURRENT file content is read from stdin (the editor buffer
// may be unsaved), the committed content is the baseline, and the difference — via the
// same alignment/move detection as Attribute — is appended to .deletions.jsonl.
func cmdRecordDeletion() *cobra.Command {
	var tool, genType, model string
	c := &cobra.Command{
		Use:    "record-deletion <file>",
		Hidden: true,
		Short:  "Internal: record AI-deleted baseline lines (Attribution). Current content on stdin.",
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !authorship.Enabled() {
				return nil
			}
			abs, err := filepath.Abs(args[0])
			if err != nil {
				return err
			}
			ctx, ok := authorship.ResolveContext(abs)
			if !ok {
				return nil // not inside a work tree
			}
			current, err := io.ReadAll(os.Stdin)
			if err != nil {
				return err
			}
			baseline, ok := gitnotes.ShowFileAt(ctx.RepoRoot, ctx.BaseSHA, ctx.RelPath)
			if !ok {
				return nil // not present at base (new file) → no baseline deletions
			}
			deleted := authorship.DeletedBaselineLines(baseline, string(current))
			if len(deleted) == 0 {
				return nil
			}
			author := authorship.Author{Type: authorship.AI, Tool: tool, GenType: genType, Model: model}
			return authorship.AppendDeletions(ctx.RepoRoot, ctx.Branch, ctx.BaseSHA, ctx.RelPath, deleted, author)
		},
	}
	c.Flags().StringVar(&tool, "tool", "", "AI tool that performed the deletion (copilot|cursor|claude|gemini|...)")
	c.Flags().StringVar(&genType, "gen-type", "completion", "gen_type of the deleting edit (chat|completion|cli)")
	c.Flags().StringVar(&model, "model", "", "model id, if known")
	return c
}

func cmdAuthorship() *cobra.Command {
	var all bool
	c := &cobra.Command{
		Use:    "authorship <file>",
		Hidden: true,
		Short:  "Internal: per-line authorship for a file (Attribution gutter source)",
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			abs, err := filepath.Abs(args[0])
			if err != nil {
				return err
			}
			ctx, ok := authorship.ResolveContext(abs)
			if !ok {
				return nil // not inside a work tree → nothing to report
			}
			// The gutter marks only the UNCOMMITTED CHANGES, not the whole file's
			// history: intersect each file's working-log authorship with `git diff
			// HEAD`. After a commit those lines are no longer changed → no icon.
			//
			// One DB handle for the whole request (the --all path restricts many
			// files); nil is fine — ReconcileGutterOverrides no-ops on a nil db.
			db, _ := store.Open()
			if db != nil {
				defer db.Close()
			}
			restrict := func(wl *authorship.WorkingLog) {
				if wl == nil {
					return
				}
				changed := gitnotes.UncommittedAddedLines(ctx.RepoRoot, wl.File)
				// A brand-new, untracked file (e.g. one Copilot just wrote) isn't in
				// `git diff HEAD`, so `changed` is empty and the gutter would stay blank
				// until `git add`. Treat every working-log line as changed for
				// untracked-but-not-ignored files so AI attribution shows on creation
				// — mirroring the uncommitted-working-tree scoping the plugin's V1 path
				// applied for untracked files.
				if len(changed) == 0 && gitnotes.IsUntracked(ctx.RepoRoot, wl.File) {
					changed = allWorkingLogLines(wl)
				}
				// Live twin of the commit-note reconciliation: upgrade Human lines the
				// working-log fold couldn't resolve to AI when their content matches a
				// recorded AI edit, so the gutter and the eventual note agree (I4).
				overrides := gitnotes.ReconcileGutterOverrides(db, ctx.RepoRoot, ctx.BaseSHA, wl, changed)
				wl.Lines = restrictLinesToChanged(wl.Lines, changed, overrides)
			}

			// --all: the repo-wide gutter/sidebar source. The gutter only ever marks
			// UNCOMMITTED changes, so the ONLY files that can contribute are those that
			// currently differ from HEAD (modified) or are untracked (new). We resolve
			// that change-set once (one `git diff HEAD` + one `git ls-files --others`) and
			// load ONLY those files' working logs — we do NOT enumerate every working log
			// on disk (that set grew once committed logs became retained, and walking it
			// plus a git spawn per file was the Windows slowness). Committed, unchanged
			// files would restrict to an empty gutter anyway, so they're simply omitted;
			// the plugin replaces its whole map each refresh, so an omitted file == a
			// cleared gutter, identical in effect.
			if all {
				changedByFile := gitnotes.UncommittedAddedLinesAll(ctx.RepoRoot)
				untracked := gitnotes.UntrackedFiles(ctx.RepoRoot)
				repoID, _ := gitutil.RepoID(ctx.RepoRoot)
				if repoID == "" {
					repoID = ctx.RepoRoot
				}
				sinceNanos, _ := gitnotes.CommitTimestampNanos(ctx.RepoRoot, ctx.BaseSHA)

				relevant := make(map[string]bool, len(changedByFile)+len(untracked))
				for f := range changedByFile {
					relevant[f] = true
				}
				for f := range untracked {
					relevant[f] = true
				}
				out := make([]*authorship.WorkingLog, 0, len(relevant))
				for file := range relevant {
					wl, lerr := authorship.LoadWorkingLog(ctx.RepoRoot, ctx.Branch, ctx.BaseSHA, file)
					if lerr != nil || wl == nil {
						continue // no working log for this changed file → no AI gutter
					}
					changed := changedByFile[file]
					if len(changed) == 0 { // untracked (new) file: every line is uncommitted
						changed = allWorkingLogLines(wl)
					}
					overrides := gitnotes.ReconcileGutterOverridesAt(db, ctx.RepoRoot, repoID, sinceNanos, wl, changed)
					wl.Lines = restrictLinesToChanged(wl.Lines, changed, overrides)
					out = append(out, wl)
				}
				return json.NewEncoder(os.Stdout).Encode(map[string]any{"files": out})
			}
			// Single file: seed from committed authorship if untracked (so changed
			// lines that were authored earlier still resolve), then restrict to the
			// uncommitted changes.
			_ = gitnotes.SeedCommittedWorkingLog(ctx.RepoRoot, ctx.Branch, ctx.BaseSHA, ctx.RelPath)
			wl, err := authorship.LoadWorkingLog(ctx.RepoRoot, ctx.Branch, ctx.BaseSHA, ctx.RelPath)
			if err != nil {
				return err
			}
			if wl == nil {
				wl = &authorship.WorkingLog{Schema: authorship.WorkingLogSchema, File: ctx.RelPath, BaseSHA: ctx.BaseSHA}
			}
			restrict(wl)
			return json.NewEncoder(os.Stdout).Encode(wl)
		},
	}
	c.Flags().BoolVar(&all, "all", false, "output working logs for ALL tracked files in the repo (gutter/sidebar repo-wide source)")
	return c
}

func cmdAttribute() *cobra.Command {
	var quiet bool
	c := &cobra.Command{
		Use: "attribute <repo> <sha>",
		// Hidden from the user-facing menu — this is called by the global
		// post-commit hook (see internal/install/hookspath.go) with the
		// repo path + commit sha. End users never invoke it manually.
		Hidden: true,
		Short:  "Internal: compute attribution for a commit and write the git note",
		Args:   cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Self-heal: drop any stale per-repo Blamely hooks/runner left by
			// older installs. Hooks live globally (core.hooksPath) now, so the
			// per-repo copies are redundant and the legacy pre-push runner
			// recursed. Best-effort; never blocks the commit.
			install.RemoveLegacyRepoHooks(args[0])

			note, err := gitnotes.AttributeAndWrite(args[0], args[1])
			if err != nil {
				return err
			}
			if !quiet && note != nil {
				report.RenderCommitSummary(os.Stdout, note)
			}
			return nil
		},
	}
	c.Flags().BoolVarP(&quiet, "quiet", "q", false, "suppress the AI/Human bar after writing the note")
	return c
}

func cmdPostRewrite() *cobra.Command {
	return &cobra.Command{
		Use: "post-rewrite <repo> <kind>",
		// Hidden: called by the global post-rewrite hook (see
		// internal/install/hookspath.go) with git's old→new sha mapping on
		// stdin. Rebuilds a single valid note for each interactive-rebase
		// squash/fixup target (N→1 fold) from the folded commits' notes.
		Hidden: true,
		Short:  "Internal: merge attribution notes across a history rewrite (rebase squash/fixup)",
		Args:   cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			pairs := gitnotes.ParseRewritePairs(cmd.InOrStdin())
			gitnotes.HandlePostRewrite(args[0], args[1], pairs)
			return nil // best-effort by contract: never fail the user's rebase
		},
	}
}

func cmdReport() *cobra.Command {
	var since, out string
	var htmlMode, noOpen bool
	c := &cobra.Command{
		Use:   "report [<sha>]",
		Short: "Show line-by-line attribution for a commit in the terminal (or --html for a browser dashboard)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return report.RenderSince(since)
			}
			sha := args[0]
			if htmlMode {
				path, err := report.RenderCommitHTML(sha, out, !noOpen)
				if err != nil {
					return err
				}
				fmt.Printf("Report written to %s\n", path)
				return nil
			}
			return report.RenderCommit(sha)
		},
	}
	c.Flags().BoolVar(&htmlMode, "html", false, "render an HTML dashboard and open it in the browser")
	c.Flags().StringVarP(&out, "out", "o", "", "with --html: write the report to this path instead of a temp file")
	c.Flags().BoolVar(&noOpen, "no-open", false, "with --html: write the file but don't open the browser")
	c.Flags().StringVar(&since, "since", "7d", "time window for the aggregated table (e.g. 1d, 7d)")
	return c
}

func cmdStats() *cobra.Command {
	return &cobra.Command{
		Use:   "stats [head|<sha>]",
		Short: "Deep view: no arg → current uncommitted change; `head` → last commit; <sha> → that commit",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// No argument → the CURRENT uncommitted change (working tree vs HEAD).
			if len(args) == 0 {
				return report.RenderCurrentStats()
			}
			// `head` (any case) → the last commit; otherwise the given commit-ish.
			sha := args[0]
			if strings.EqualFold(sha, "head") {
				sha = "HEAD"
			}
			return report.RenderStats(sha)
		},
	}
}

func cmdBlame() *cobra.Command {
	var rev string
	c := &cobra.Command{
		Use:   "blame <file>",
		Short: "Per-line attribution for a file: who wrote each line — human or AI tool/model",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return report.RenderBlame(args[0], rev)
		},
	}
	c.Flags().StringVar(&rev, "rev", "HEAD", "revision to blame (commit, branch, tag)")
	return c
}

func cmdHistory() *cobra.Command {
	var since string
	var all bool
	c := &cobra.Command{
		Use:   "history",
		Short: "Aggregate report across all noted commits: totals, tools, tokens, coding time",
		RunE: func(cmd *cobra.Command, args []string) error {
			var d report.HistoryOptions
			d.AllRepos = all
			if since != "" {
				dur, err := parseDuration(since)
				if err != nil {
					return fmt.Errorf("--since: %w", err)
				}
				d.Since = dur
			}
			return report.RenderHistory(d)
		},
	}
	c.Flags().StringVar(&since, "since", "30d", "time window, e.g. 7d, 30d, 90d, 1y")
	c.Flags().BoolVar(&all, "all", false, "include all tracked repos, not just the current one")
	return c
}

// parseDuration extends time.ParseDuration with 'd' (days) and 'y' (years).
func parseDuration(s string) (time.Duration, error) {
	if len(s) >= 2 {
		switch s[len(s)-1] {
		case 'd':
			var n int
			if _, err := fmt.Sscanf(s[:len(s)-1], "%d", &n); err == nil {
				return time.Duration(n) * 24 * time.Hour, nil
			}
		case 'y':
			var n int
			if _, err := fmt.Sscanf(s[:len(s)-1], "%d", &n); err == nil {
				return time.Duration(n) * 365 * 24 * time.Hour, nil
			}
		}
	}
	return time.ParseDuration(s)
}

func cmdStatus() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show daemon status, detected tools, and recent activity",
		RunE: func(cmd *cobra.Command, args []string) error {
			return daemon.PrintStatus()
		},
	}
}

func cmdLog() *cobra.Command {
	parent := &cobra.Command{
		Use:   "log",
		Short: "Live-tail tool logs for debugging attribution",
	}
	parent.AddCommand(cmdLogCursor())
	parent.AddCommand(cmdLogCopilot())
	parent.AddCommand(cmdLogCodex())
	parent.AddCommand(cmdLogClaude())
	parent.AddCommand(cmdLogGemini())
	return parent
}

func cmdLogCopilot() *cobra.Command {
	return &cobra.Command{
		Use:   "copilot",
		Short: "Trace GitHub Copilot chat + completion detection events",
		Long: "Runs blamely's Copilot watchers (chat-session JSONL + editor/JetBrains\n" +
			"logs) against a printing sink and shows every attribution event they\n" +
			"would record — tool, gen_type (chat/completion), model, and file.\n" +
			"Nothing is written to the database.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return tools.DebugCopilotLogs(cmd.Context(), os.Stdout)
		},
	}
}

func cmdLogCodex() *cobra.Command {
	return &cobra.Command{
		Use:   "codex",
		Short: "Trace Codex CLI session detection events",
		RunE: func(cmd *cobra.Command, args []string) error {
			return tools.DebugCodexLogs(cmd.Context(), os.Stdout)
		},
	}
}

func cmdLogClaude() *cobra.Command {
	return &cobra.Command{
		Use:   "claude",
		Short: "Explain how to trace Claude Code attribution (hook-driven)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return tools.DebugClaudeLogs(cmd.Context(), os.Stdout)
		},
	}
}

func cmdLogGemini() *cobra.Command {
	return &cobra.Command{
		Use:   "gemini",
		Short: "Explain how to trace Gemini CLI attribution (hook-driven)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return tools.DebugGeminiLogs(cmd.Context(), os.Stdout)
		},
	}
}

func cmdLogCursor() *cobra.Command {
	var debug bool
	c := &cobra.Command{
		Use:   "cursor",
		Short: "Tail Cursor logs and show detected AI-apply events",
		Long: "Watches Cursor's extension-host log files and prints every line that\n" +
			"blamely's CursorLogWatcher would record as a Composer/Agent apply event.\n\n" +
			"Use --debug to see every scanned line (with [MATCH] or [skip] prefix)\n" +
			"so you can trace why a specific Composer action was or was not detected.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return tools.DebugCursorLogs(cmd.Context(), debug, os.Stdout)
		},
	}
	c.Flags().BoolVar(&debug, "debug", false, "show all scanned lines, not just matches")
	return c
}

func cmdDoctor() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Self-check: daemon + per-tool hooks + git hook + PATH + binary + DB",
		Long: "Read-only self-check that prints what's wired up correctly and what's\n" +
			"not. Mirrors the output style of `brew doctor` / `flutter doctor`.\n" +
			"Does NOT modify anything — fix recommendations are printed at the end.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return install.Doctor(os.Stdout)
		},
	}
}

// cmdConfig manages ~/.blamely/config.json — the toggles that decide what each
// commit's git note includes (file detail, conversation, message, tokens, …).
// With no subcommand it prints the current settings.
func cmdConfig() *cobra.Command {
	c := &cobra.Command{
		Use:   "config",
		Short: "View or change what blamely writes into commit notes",
		Long: "Manage ~/.blamely/config.json. Every toggle defaults to on; turning one\n" +
			"off keeps it out of future commit notes (existing notes are untouched).\n\n" +
			"Keys: " + strings.Join(settableKeys(), ", ") + "\n" +
			"List keys (extra AI-tool dirs, additive to the defaults): " +
			strings.Join(config.ListKeys(), ", ") + "\n\n" +
			"Examples:\n" +
			"  blamely config                       # show current settings\n" +
			"  blamely config get note.conversation\n" +
			"  blamely config set note.file_lines off\n" +
			"  blamely config set tokens true       # 'note.' prefix optional\n" +
			"  blamely config set update.auto off   # notify about updates, don't install\n" +
			"  blamely config add tools.codex_home /path/to/.codex-corp/codex-config\n" +
			"  blamely config remove tools.claude_config_dir /path/to/.claude-corp/claude-config\n" +
			"  blamely config path",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConfigShow(cmd)
		},
	}
	c.AddCommand(cmdConfigShow(), cmdConfigGet(), cmdConfigSet(), cmdConfigAdd(), cmdConfigRemove(), cmdConfigPath())
	return c
}

func cmdConfigShow() *cobra.Command {
	return &cobra.Command{
		Use:     "show",
		Aliases: []string{"list", "ls"},
		Short:   "Print the current settings",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConfigShow(cmd)
		},
	}
}

func runConfigShow(cmd *cobra.Command) error {
	cfg := config.LoadConfig()
	if path, err := config.ConfigFile(); err == nil {
		fmt.Fprintf(cmd.OutOrStdout(), "%s\n\n", path)
	}
	for _, key := range append(config.NoteKeys(), config.UpdateKeys()...) {
		v, _ := cfg.GetBool(key)
		fmt.Fprintf(cmd.OutOrStdout(), "  %-30s %v\n", key, v)
	}
	for _, key := range config.ListKeys() {
		v, _ := cfg.GetList(key)
		if len(v) == 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "  %-30s (default only)\n", key)
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), "  %-30s %s\n", key, strings.Join(v, ", "))
		}
	}
	return nil
}

func cmdConfigGet() *cobra.Command {
	return &cobra.Command{
		Use:   "get <key>",
		Short: "Print one setting's value",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := config.LoadConfig()
			v, ok := cfg.GetBool(args[0])
			if !ok {
				return fmt.Errorf("unknown key %q (valid: %s)", args[0], strings.Join(settableKeys(), ", "))
			}
			fmt.Fprintln(cmd.OutOrStdout(), v)
			return nil
		},
	}
}

func cmdConfigSet() *cobra.Command {
	return &cobra.Command{
		Use:   "set <key> <true|false>",
		Short: "Change a setting and save it to ~/.blamely/config.json",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			val, err := config.ParseBoolValue(args[1])
			if err != nil {
				return fmt.Errorf("invalid value %q: want true/false (also on/off, yes/no)", args[1])
			}
			cfg := config.LoadConfig()
			if !cfg.SetBool(args[0], val) {
				return fmt.Errorf("unknown key %q (valid: %s)", args[0], strings.Join(settableKeys(), ", "))
			}
			path, err := config.SaveConfig(cfg)
			if err != nil {
				return fmt.Errorf("save config: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "set %s = %v  (%s)\n", canonicalKey(args[0]), val, path)
			return nil
		},
	}
}

func cmdConfigAdd() *cobra.Command {
	return &cobra.Command{
		Use:   "add <key> <path>",
		Short: "Add an extra AI-tool config dir (additive; defaults are always kept)",
		Long: "Add an EXTRA Codex/Claude config directory to watch, for setups that run\n" +
			"those tools from a non-default home (corporate provisioning). This is\n" +
			"additive — the standard ~/.codex and ~/.claude are ALWAYS watched too.\n\n" +
			"Keys: " + strings.Join(config.ListKeys(), ", "),
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := config.LoadConfig()
			added, ok := cfg.AddToList(args[0], args[1])
			if !ok {
				return fmt.Errorf("unknown list key %q (valid: %s)", args[0], strings.Join(config.ListKeys(), ", "))
			}
			if !added {
				fmt.Fprintf(cmd.OutOrStdout(), "%s already present in %s\n", args[1], config.CanonicalListKey(args[0]))
				return nil
			}
			path, err := config.SaveConfig(cfg)
			if err != nil {
				return fmt.Errorf("save config: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "added %s to %s  (%s)\n", args[1], config.CanonicalListKey(args[0]), path)
			return nil
		},
	}
}

func cmdConfigRemove() *cobra.Command {
	return &cobra.Command{
		Use:     "remove <key> <path>",
		Aliases: []string{"rm"},
		Short:   "Remove an extra AI-tool config dir",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := config.LoadConfig()
			removed, ok := cfg.RemoveFromList(args[0], args[1])
			if !ok {
				return fmt.Errorf("unknown list key %q (valid: %s)", args[0], strings.Join(config.ListKeys(), ", "))
			}
			if !removed {
				fmt.Fprintf(cmd.OutOrStdout(), "%s was not in %s\n", args[1], config.CanonicalListKey(args[0]))
				return nil
			}
			path, err := config.SaveConfig(cfg)
			if err != nil {
				return fmt.Errorf("save config: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed %s from %s  (%s)\n", args[1], config.CanonicalListKey(args[0]), path)
			return nil
		},
	}
}

// canonicalKey maps a user-supplied key (bare or dotted, any case) to its
// canonical dotted form so `set` echoes a consistent name. Falls back to the
// input if it's not a known key (Set already rejected it by then).
// settableKeys is every boolean key `config get`/`config set` accepts: the note
// toggles plus the self-update ones.
func settableKeys() []string {
	return append(config.NoteKeys(), config.UpdateKeys()...)
}

func canonicalKey(userKey string) string {
	norm := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(userKey)), "note.")
	for _, k := range config.NoteKeys() {
		if strings.TrimPrefix(k, "note.") == norm {
			return k
		}
	}
	return userKey
}

func cmdConfigPath() *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "Print the path to the config file",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := config.ConfigFile()
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), path)
			return nil
		},
	}
}
