package install

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/blamely/blamely/internal/config"
	"github.com/blamely/blamely/internal/daemon"
)

// killRunningDaemon terminates the live daemon SYNCHRONOUSLY during uninstall,
// AFTER the respawn mechanism (Scheduled Task / Startup .vbs / LaunchAgent /
// systemd unit) is removed so it can't come back. Doing it here, in-process,
// rather than leaving it to the racy detached `taskkill` is what stops the
// "files removed but the daemon kept running (and kept bin/blamely.exe locked,
// so the dir survived)" failure on Windows.
//
// Two complementary kills, both best-effort:
//  1. By exact PID from the daemon's PID file — catches a daemon running from a
//     renamed/dev binary that an image-name match would miss.
//  2. killOtherDaemonProcesses — by image name, excluding our own PID, so a
//     daemon with no PID file (older build) is still reaped. This is the
//     reliable one for a normal install (the daemon IS blamely.exe).
func killRunningDaemon() {
	if p, err := config.PidFile(); err == nil {
		if data, err := os.ReadFile(p); err == nil {
			pid, perr := strconv.Atoi(strings.TrimSpace(string(data)))
			if perr == nil && pid > 0 && pid != os.Getpid() {
				_ = killProcess(pid)
			}
		}
	}
	killOtherDaemonProcesses()
}

// Run is the orchestrator behind `blamely install`. It:
//  1. Detects which AI tools are present on the machine.
//  2. Resolves the absolute path of the running blamely binary.
//  3. Merges a `blamely record <tool>` PostToolUse hook into each detected
//     tool's settings file (Claude, Cursor, Codex, Copilot). Existing hooks
//     from the user or other tools are preserved.
//  4. Sets `git config --global core.hooksPath` to the Blamely hooks dir and
//     writes a post-commit script that calls `blamely attribute`.
//  5. Registers the daemon under launchd / systemd --user / Scheduled Tasks.
//  6. Persists a state.json so `uninstall` can fully reverse the install.
//
// installPlugins gates step 3.5: downloading/installing the VS Code-family and
// JetBrains IDE plugins from their marketplaces. Local dev installs default to
// skipping this (rebuilding from source already exercises the plugin code, and
// re-downloading the marketplace build on every `install.sh` run is wasteful
// and can clobber a sideloaded dev build); the distributed installers and
// release pipeline always pass true so end users get the full experience.
func Run(installPlugins bool) error {
	srcBinPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate blamely binary: %w", err)
	}

	// Copy the binary to ~/.blamely/bin/ so the post-commit hook and the OS
	// agent reference a stable path that doesn't break if the user moves or
	// re-installs the dev binary.
	binPath, err := CopyBinary(srcBinPath)
	if err != nil {
		return fmt.Errorf("install binary: %w", err)
	}

	// Tee the full step-by-step report to ~/.blamely/last-install.log so the
	// native installers can display exactly what was set up WITHOUT any shell
	// redirection (`blamely install > log`) — that pattern is what EDR/SmartScreen
	// flag. The installer just runs the signed blamely.exe directly and reads this
	// file. Best-effort: if the dir/file can't be created, output is unchanged.
	if dir, derr := config.EnsureBlamelyDir(); derr == nil {
		defer uiTeeToFile(filepath.Join(dir, "last-install.log"))()
	}

	detected, err := Detect()
	if err != nil {
		return err
	}

	printDetected(detected)
	info("Binary", binPath)

	s, err := LoadState()
	if err != nil {
		return err
	}
	if s == nil {
		s = &State{}
	}
	s.InstalledAt = time.Now()
	s.BinaryPath = binPath

	// Capture any custom Codex/Claude home from the env NOW, while we run in the
	// user's shell. The daemon runs under launchd/systemd/schtasks and does NOT
	// inherit these, so persisting them into config is the only way it learns to
	// watch a corporate CODEX_HOME / CLAUDE_CONFIG_DIR. Additive — the defaults are
	// always watched too. Best-effort: a failure here must not abort install.
	if err := config.CaptureToolDirsFromEnv(); err != nil {
		info("Tool dirs", "could not persist custom Codex/Claude dirs — "+err.Error())
	}

	// Hooks: a `blamely record <tool>` hook merged into each detected AI
	// tool's own settings/config file, plus the global git post-commit hook
	// that turns recorded edits into per-commit attribution notes. Grouped
	// together because they're the same mechanism (config-file merge) and the
	// same thing a user would look for when checking "is blamely wired up?".
	section("Hooks")

	if detected.Claude.Present {
		added, settingsPath, err := InstallClaudeHook(binPath)
		if err != nil {
			return fmt.Errorf("claude hook: %w", err)
		}
		if added {
			ok("Claude Code", settingsPath)
		} else {
			info("Claude Code", "hook already present · "+settingsPath)
		}
		s.ClaudeHookAdded = true
	} else {
		info("Claude Code", "not detected — skipped")
	}

	if detected.Cursor.Present {
		added, hooksPath, err := InstallCursorHook(binPath)
		if err != nil {
			return fmt.Errorf("cursor hook: %w", err)
		}
		if added {
			ok("Cursor", hooksPath)
		} else {
			info("Cursor", "hook already present · "+hooksPath)
		}
		s.CursorHookAdded = true
	} else {
		info("Cursor", "not detected — skipped")
	}

	if detected.Codex.Present {
		added, configPath, err := InstallCodexHook(binPath)
		if err != nil {
			return fmt.Errorf("codex hook: %w", err)
		}
		if added {
			ok("Codex CLI", configPath)
		} else {
			info("Codex CLI", "hook already present · "+configPath)
		}
		s.CodexHookAdded = true
	} else {
		info("Codex CLI", "not detected — skipped")
	}

	if detected.Copilot.Present {
		added, hookPath, err := InstallCopilotHook(binPath)
		if err != nil {
			return fmt.Errorf("copilot hook: %w", err)
		}
		if added {
			ok("GitHub Copilot", hookPath)
		} else {
			info("GitHub Copilot", "hook already present · "+hookPath)
		}
		s.CopilotHookAdded = true
	} else {
		info("GitHub Copilot", "not detected — skipped")
	}

	if detected.Gemini.Present {
		added, settingsPath, err := InstallGeminiHook(binPath)
		if err != nil {
			return fmt.Errorf("gemini hook: %w", err)
		}
		if added {
			ok("Gemini CLI", settingsPath)
		} else {
			info("Gemini CLI", "hook already present · "+settingsPath)
		}
		s.GeminiHookAdded = true
	} else {
		info("Gemini CLI", "not detected — skipped")
	}

	if detected.Devin.Present {
		added, configPath, err := InstallDevinHook(binPath)
		if err != nil {
			return fmt.Errorf("devin hook: %w", err)
		}
		if added {
			ok("Devin CLI", configPath)
		} else {
			info("Devin CLI", "hook already present · "+configPath)
		}
		s.DevinHookAdded = true
	} else {
		info("Devin CLI", "not detected — skipped")
	}

	prior, hadPrior, err := InstallGitHook(binPath)
	if err != nil {
		return fmt.Errorf("git hook: %w", err)
	}
	s.PriorCoreHooksPath = prior
	s.HadCoreHooksPath = hadPrior
	s.GitHookInstalled = true
	if hadPrior {
		ok("Git post-commit", fmt.Sprintf("global hook installed · previous core.hooksPath %q stashed", prior))
	} else {
		ok("Git post-commit", "global hook installed")
	}

	// Editors: marketplace-distributed extensions that give a VS Code-family
	// editor its own attribution surface (chat-panel detection, inline UI, …),
	// auto-installed via each editor's bundled CLI when the editor is present.
	// Separate from Hooks because these come from an external marketplace
	// (VS Code Marketplace / Open VSX) rather than a config-file merge.
	//
	// Skippable via installPlugins=false (local dev installs default to this —
	// see the doc comment on Run): downloading marketplace builds is slow and
	// can overwrite a sideloaded dev build of the plugin under test.
	section("Editors")
	if !installPlugins {
		info("Editors", "skipped (--skip-plugins)")
		info("JetBrains IDEs", "skipped (--skip-plugins)")
	} else {
		var editorLabelsInstalled []string
		for _, r := range InstallEditorExtensions() {
			switch {
			case r.Err != nil:
				fail(r.Label, r.Err.Error())
			case r.CLIPath == "":
				info(r.Label, "not detected — skipped")
			case r.Installed:
				ok(r.Label, "extension installed from marketplace · "+blamelyExtensionID)
				editorLabelsInstalled = append(editorLabelsInstalled, r.Label)
			case r.Updated:
				ok(r.Label, "extension reinstalled to latest · "+blamelyExtensionID)
				// Track it too: uninstall must be able to remove an extension we
				// force-reinstalled, not just one we first-installed.
				editorLabelsInstalled = append(editorLabelsInstalled, r.Label)
			default:
				info(r.Label, "extension already installed · "+blamelyExtensionID)
			}
		}
		s.EditorExtensionsInstalled = mergeLabels(s.EditorExtensionsInstalled, editorLabelsInstalled)

		// JetBrains IDEs (IntelliJ IDEA, WebStorm, GoLand, …) don't expose a CLI
		// extension-install flow the way Code-OSS forks do, so we go straight to
		// the JetBrains Marketplace: download a build-compatible plugin zip and
		// unzip it into the IDE's plugins directory.
		jetResults := InstallJetBrainsPlugins()
		if len(jetResults) == 0 {
			info("JetBrains IDEs", "not detected — skipped")
		} else {
			var jetbrainsRestartNeeded bool
			var jetbrainsDirsInstalled []string
			for _, r := range jetResults {
				switch {
				case r.Err != nil:
					fail(r.Label, r.Err.Error())
				case r.Installed:
					ok(r.Label, "plugin installed from marketplace · ai.blamely")
					jetbrainsDirsInstalled = append(jetbrainsDirsInstalled, r.PluginsDir)
					jetbrainsRestartNeeded = true
				case r.Updated:
					ok(r.Label, "plugin reinstalled to latest · ai.blamely")
					jetbrainsDirsInstalled = append(jetbrainsDirsInstalled, r.PluginsDir)
					jetbrainsRestartNeeded = true
				default:
					info(r.Label, "plugin already installed · ai.blamely")
				}
			}
			s.JetBrainsPluginsInstalled = mergeLabels(s.JetBrainsPluginsInstalled, jetbrainsDirsInstalled)
			if jetbrainsRestartNeeded {
				info("JetBrains IDEs", "restart to load the newly installed plugin")
			}
		}
	}

	// System: the background daemon that receives hook events, the shell PATH
	// entry, and the default config/exclude files that shape what attribution
	// looks like. The plumbing that makes the above two groups actually work.
	section("System")

	// Windows: kill any running daemon SYNCHRONOUSLY before touching its
	// discovery file. macOS/Linux restart the daemon through the service
	// manager (kickstart -k / systemctl restart), but the Windows agent paths
	// have no restart primitive — the Startup-folder install never kills the
	// old daemon at all. Without this, a reinstall over a live daemon plays out
	// as: port file deleted below → new spawn exits on the old daemon's
	// instance lock → old daemon only notices its port file is gone on a 30s
	// poll → revival waits for the periodic keepalive. The health check below
	// can't survive that, and worse, the OLD binary keeps running until
	// logoff. Killing first releases the lock so the fresh spawn (new binary)
	// takes over within seconds.
	if runtime.GOOS == "windows" {
		killRunningDaemon()
	}

	// We invalidate any stale port file BEFORE registering the agent so the
	// post-install health check can't false-positive on the previous daemon's
	// port. Then we register/restart and wait for the new daemon to come up.
	if portPath, perr := config.PortFile(); perr == nil {
		_ = os.Remove(portPath)
	}
	agentRef, err := InstallDaemonAgent(binPath)
	switch {
	case errors.Is(err, ErrNoGUISession):
		// MDM/SSH bulk install (e.g. a Jamf policy): the agent is registered
		// but the user has no Aqua session to start it in right now. launchd
		// bootstraps ~/Library/LaunchAgents at the next GUI login, so this is
		// a deferred start, not a failure — and waiting on /health below would
		// only burn 8s and print misleading "daemon didn't start" diagnostics.
		s.LaunchAgentInstalled = true
		ok("Daemon agent", agentRef)
		info("Daemon", "no GUI login session right now — starts automatically at next login")
	case err != nil:
		return fmt.Errorf("daemon agent: %w", err)
	default:
		s.LaunchAgentInstalled = true
		ok("Daemon agent", agentRef)

		// Block until the daemon actually answers /health, so the user knows
		// hooks are being listened to before this command exits. 25s, not a
		// few: on corporate Windows the first exec of a freshly downloaded
		// binary gets a full antivirus scan (fleets often set
		// BLAMELY_SKIP_DEFENDER_EXCLUSION), which alone can eat 10s+ before
		// blamelyd's first instruction runs. A healthy daemon answers in ~1s
		// and returns immediately — the long ceiling only costs time on the
		// genuinely-broken path.
		if sock, derr := daemon.WaitForReady(25 * time.Second); derr != nil {
			diagnoseDaemon(derr, agentRef)
		} else {
			ok("Daemon", fmt.Sprintf("listening on %s · ready to receive hooks", sock))
		}
	}

	// Best-effort — if shell detection fails or the rc isn't writable, we
	// print a manual hint instead of failing the install (the binary is still
	// on disk and the daemon is still wired up).
	if rcPath, added, perr := InstallPathEntry(); perr != nil {
		fail("PATH", fmt.Sprintf("could not auto-add ~/.blamely/bin: %v", perr))
	} else {
		s.PathRcFile = rcPath
		if added {
			s.PathEntryAdded = true
			if runtime.GOOS == "windows" {
				ok("PATH", fmt.Sprintf("added %s to user PATH · open a new terminal", rcPath))
			} else {
				ok("PATH", fmt.Sprintf("added to %s · reload your shell or run `source %s`", rcPath, rcPath))
			}
		} else {
			if runtime.GOOS == "windows" {
				info("PATH", "entry already present in user PATH ("+rcPath+")")
			} else {
				info("PATH", "entry already present in "+rcPath)
			}
		}
	}

	// Default exclude/config files. We never overwrite an existing one —
	// users edit these to customise what's skipped from attribution and what
	// each commit's git note includes, and a fresh install must keep their
	// edits intact.
	if excludePath, created, eerr := config.EnsureDefaultExcludeFile(); eerr != nil {
		fail("Exclude list", eerr.Error())
	} else if created {
		ok("Exclude list", excludePath)
	} else {
		info("Exclude list", "already present · "+excludePath)
	}

	if configPath, created, cerr := config.EnsureDefaultConfigFile(); cerr != nil {
		fail("Config", cerr.Error())
	} else if created {
		ok("Config", configPath)
	} else {
		info("Config", "already present · "+configPath)
	}

	if err := SaveState(s); err != nil {
		return fmt.Errorf("save state: %w", err)
	}

	fmt.Fprintln(uiOut)
	printNextSteps(detected)
	return nil
}

// runtimeArtifacts lists the files Uninstall removes from ~/.blamely. config.json
// (user config) and exclude (the exclusion list) are deliberately NOT included —
// they're kept across reinstalls. The attribution DB is included unless keepDB.
func runtimeArtifacts(keepDB bool) []string {
	var files []string
	add := func(p string, err error) {
		if err == nil && p != "" {
			files = append(files, p)
		}
	}
	add(config.LogFile())    // daemon.log
	add(config.SocketFile()) // daemon.sock (unix)
	add(config.PortFile())   // daemon.port (windows)
	add(config.PidFile())    // daemon.pid
	add(config.StateFile())  // state.json
	if dir, err := config.BlamelyDir(); err == nil {
		files = append(files, filepath.Join(dir, "blamely.db")) // legacy, pre-db.sqlite
	}
	if !keepDB {
		if db, err := config.DBPath(); err == nil {
			// db.sqlite plus its WAL sidecars.
			for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
				files = append(files, db+suffix)
			}
		}
	}
	return files
}

// Uninstall reverses `blamely install`. It removes hooks, editor/IDE plugins, the
// daemon agent, the PATH entry, the binary, and runtime files (logs, sockets,
// state) — and the attribution DB unless keepDB. config.json and the exclude list
// are always preserved.
func Uninstall(keepDB bool) error {
	s, err := LoadState()
	if err != nil {
		return err
	}

	// On Windows an open editor (holding the plugin / bundled sqlite3.exe) or a
	// live daemon keeps files locked, so removal silently leaves them behind.
	// Surface that up front and offer to close them. No-op off Windows.
	promptCloseBlockers()

	var firstErr error
	report := func(label string, err error) {
		if err == nil {
			fmt.Printf("  ✓ %s\n", label)
			return
		}
		fmt.Printf("  ✗ %s: %v\n", label, err)
		if firstErr == nil {
			firstErr = err
		}
	}

	if s.GitHookInstalled || s.HadCoreHooksPath {
		report("removed global git post-commit hook", UninstallGitHook(s.PriorCoreHooksPath, s.HadCoreHooksPath))
	}
	if s.ClaudeHookAdded {
		_, err := UninstallClaudeHook()
		report("removed Claude record hook from ~/.claude/settings.json", err)
	}
	if s.CursorHookAdded {
		_, err := UninstallCursorHook()
		report("removed Cursor record hook from ~/.cursor/hooks.json", err)
	}
	if s.CodexHookAdded {
		_, err := UninstallCodexHook()
		report("removed Codex record hook from ~/.codex/config.toml", err)
	}
	if s.CopilotHookAdded {
		_, err := UninstallCopilotHook()
		report("removed Copilot record hook from ~/.copilot/hooks/blamely.json", err)
	}
	if s.GeminiHookAdded {
		_, err := UninstallGeminiHook()
		report("removed Gemini record hook from ~/.gemini/settings.json", err)
	}
	if s.DevinHookAdded {
		_, err := UninstallDevinHook()
		report("removed Devin record hook from ~/.config/devin/config.json", err)
	}
	// Don't rely solely on state: the extension may have been already present
	// when install ran (tracked as "Updated", not "Installed"), installed by the
	// user from the marketplace, or predate state tracking. Discover every
	// detected editor that actually has it and remove from all of them — the
	// same self-sufficient approach the JetBrains block below uses.
	editorLabels := mergeLabels(s.EditorExtensionsInstalled, DiscoverInstalledEditorExtensions())
	if len(editorLabels) > 0 {
		report(fmt.Sprintf("removed Blamely extension from %s", strings.Join(editorLabels, ", ")),
			UninstallEditorExtensions(editorLabels))
	}
	// Don't rely solely on state: the plugin may have been sideloaded for
	// development, installed manually, or predate state tracking, so it was
	// never recorded in JetBrainsPluginsInstalled. The "Blamely-intellij*"
	// directory name is uniquely ours (the same glob install/uninstall already
	// trust to recognise it), so finding live matches in every detected
	// JetBrains IDE is a safe, self-sufficient way to discover what to remove.
	var discoveredJetBrainsDirs []string
	if ides, err := findJetBrainsIDEs(); err == nil {
		for _, ide := range ides {
			if hasJetBrainsPlugin(ide.PluginsDir) {
				discoveredJetBrainsDirs = append(discoveredJetBrainsDirs, ide.PluginsDir)
			}
		}
	}
	jetbrainsDirs := mergeLabels(s.JetBrainsPluginsInstalled, discoveredJetBrainsDirs)
	if len(jetbrainsDirs) > 0 {
		report(fmt.Sprintf("removed Blamely plugin from %d JetBrains IDE(s)", len(jetbrainsDirs)),
			UninstallJetBrainsPlugins(jetbrainsDirs))
	}
	// Always attempt removal, not just when state says we installed it: a
	// daemon agent registered outside `blamely install` (the Windows installer's
	// own Scheduled Task / Startup .vbs, a manual setup, or a pre-state install)
	// won't be in LaunchAgentInstalled, and a surviving daemon keeps the binary
	// locked so the cleanup below can't delete the bin dir. UninstallDaemonAgent
	// is idempotent and returns nil when nothing is registered — same
	// self-sufficient approach as the editor/JetBrains removal above.
	report("removed daemon agent", UninstallDaemonAgent())

	// Now that the respawn mechanism (Scheduled Task / Startup .vbs / LaunchAgent
	// / systemd unit) is gone, kill the still-running daemon by its recorded PID.
	// This is synchronous and exact, so the binary is unlocked before the cleanup
	// below runs — fixing "uninstall removed the files but the process kept
	// running" on Windows, where the by-image-name taskkill could lose the race.
	killRunningDaemon()
	if s.PathEntryAdded {
		rcPath, _, err := UninstallPathEntry(s.PathRcFile)
		report(fmt.Sprintf("removed PATH entry from %s", rcPath), err)
	}

	// Remove the stable binary copy AND the runtime/data files (logs, sockets,
	// state, and the DB unless keepDB). With --purge, the WHOLE ~/.blamely tree
	// goes — config.json and exclude included. On Windows the running blamely.exe
	// can't delete itself or the files the daemon holds open, so
	// removeInstalledBinary schedules a detached cleanup that taskkills the daemon
	// and deletes them all once this process exits. On Unix they're unlinked
	// immediately.
	artifacts := runtimeArtifacts(keepDB)
	purgeRoot := ""
	if keepDB {
		// Keep ONLY the attribution database. Everything else under ~/.blamely —
		// config.json and the exclude list included — is removed, so add them to
		// the delete list (the directory survives because db.sqlite remains).
		if p, e := config.ConfigFile(); e == nil {
			artifacts = append(artifacts, p)
		}
		if p, e := config.ExcludeFile(); e == nil {
			artifacts = append(artifacts, p)
		}
	} else {
		// Default: wipe the entire ~/.blamely tree — binary, database, config,
		// exclude, everything.
		if dir, derr := config.BlamelyDir(); derr == nil {
			purgeRoot = dir
		}
	}
	if p, err := InstalledBinaryPath(); err == nil {
		// On Windows the binary/dir removal is DEFERRED to a detached cleanup that
		// runs once this process exits (a running .exe can't delete its own image),
		// so don't claim it's already gone — that's why the old "removed binary" ✓
		// printed while the file was still on disk.
		label := "removed binary " + p
		if runtime.GOOS == "windows" {
			label = "scheduled removal of " + p
		}
		report(label, removeInstalledBinary(p, artifacts, purgeRoot))
	} else {
		// No binary path (unexpected) — still clean up directly.
		for _, f := range artifacts {
			_ = os.Remove(f)
		}
		if purgeRoot != "" {
			_ = os.RemoveAll(purgeRoot)
		}
	}

	if firstErr != nil {
		return firstErr
	}
	fmt.Println()
	// The daemon is already stopped (killed synchronously above). On Windows the
	// final binary + directory deletion runs in a detached helper that fires the
	// moment this process exits and releases its own image — about a second. Phrase
	// it as "clearing on exit" rather than claiming it's already gone.
	deferred := runtime.GOOS == "windows"
	switch {
	case keepDB && deferred:
		fmt.Println("Blamely uninstalled and the daemon stopped. Clearing ~/.blamely (keeping")
		fmt.Println("the attribution database) on exit.")
	case keepDB:
		fmt.Println("Blamely uninstalled. Removed everything under ~/.blamely except the")
		fmt.Println("attribution database (db.sqlite).")
	case deferred:
		fmt.Println("Blamely uninstalled and the daemon stopped. Clearing ~/.blamely on exit.")
	default:
		fmt.Println("Blamely uninstalled. Removed the entire ~/.blamely directory.")
	}
	return nil
}

// mergeLabels folds freshly-installed editor labels into the persisted set,
// de-duplicating. Re-running `install` after an editor was added shouldn't
// drop the editors recorded by earlier runs.
func mergeLabels(existing, fresh []string) []string {
	seen := make(map[string]bool, len(existing)+len(fresh))
	out := make([]string, 0, len(existing)+len(fresh))
	for _, l := range append(append([]string{}, existing...), fresh...) {
		if !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	return out
}

func printDetected(d *Detected) {
	if uiColor() {
		fmt.Fprintf(uiOut, "%sDetected%s\n", uiBold, uiReset)
	} else {
		fmt.Fprintln(uiOut, "Detected")
	}
	for _, row := range []struct {
		name string
		p    ToolPresence
	}{
		{"Claude Code", d.Claude},
		{"Cursor", d.Cursor},
		{"Codex CLI", d.Codex},
		{"GitHub Copilot", d.Copilot},
		{"Gemini CLI", d.Gemini},
		{"Devin CLI", d.Devin},
	} {
		hint := ""
		if h := row.p.FirstHint(); h != "" {
			hint = h
			if more := len(row.p.Hints) - 1; more > 0 {
				hint += fmt.Sprintf("  (+%d more)", more)
			}
		}
		if row.p.Present {
			ok(row.name, hint)
		} else {
			info(row.name, "not detected")
		}
	}
}

func printNextSteps(d *Detected) {
	section("Next steps")
	if d.Claude.Present {
		fmt.Fprintln(uiOut, "  · Make an edit with Claude Code, then commit. Run `blamely report HEAD` to see the per-line attribution.")
	} else {
		fmt.Fprintln(uiOut, "  · Claude Code wasn't detected. Install it, run `blamely install` again, or add the hook manually.")
	}
	if !d.Cursor.Present || !d.Codex.Present || !d.Copilot.Present || !d.Gemini.Present || !d.Devin.Present {
		fmt.Fprintln(uiOut, "  · Install Cursor/Codex/Copilot/Gemini/Devin later? Run `blamely repair` to wire up its hook (`blamely doctor` checks first).")
	}
	fmt.Fprintln(uiOut, "  · `blamely status` shows the daemon health.")
	fmt.Fprintln(uiOut, "  · `blamely uninstall` reverses every change above.")
}
