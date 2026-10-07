# Changelog

Notable changes to the **Blamely CLI** follow [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). This project uses [semantic versioning](https://semver.org/).

## [1.8.5] - 2026-10-07

### Added

- **Blamely now works with OpenCode.** Code written by OpenCode is now tracked and credited to it, with both OpenCode V1 and V2.

### Fixed

- **GitHub Copilot's work in VS Code is recorded again.** Files Copilot created or changed in VS Code's agent mode were being counted as your own work. They are now credited to Copilot.
- **Copilot reading a file no longer turns your lines into Copilot's.** When Copilot only looked at a file, the lines you had written yourself in it could be credited to Copilot. Now only what Copilot actually writes counts.
- **Copilot's work is no longer counted twice.** The same Copilot change could be recorded more than once, inflating its share in your reports. Each change now counts once.
- **AI edits are recognised in files with Windows-style line endings.** In some Windows projects, lines written by an AI assistant could end up credited to you. They are now recognised the same way on every operating system.
- **Git worktrees are fully supported.** If you work on several branches side by side in separate worktrees, each one now keeps its own correct record of who wrote what.
- **Editing very large files no longer slows your computer down.** A small change to a very large file could make Blamely use a huge amount of memory and freeze the machine. It now stays light however large the file is, and very large changes are attributed more accurately too.
- **`git push` no longer waits for Blamely.** Sharing attribution with your team now happens in the background, so a push finishes as quickly as it would without Blamely. If sharing fails, your next push tells you.
- **Pushing from several places at once is safe.** Two pushes running at the same time, for example from two worktrees, could get in each other's way. They now take turns, and both get through.
- **A failed push no longer loses your in-progress attribution.** Your work is kept until it actually reaches the server, so you can simply push again.
- **Commits are fast again in repositories used with Blamely for a long time.** Tidying up old records could add tens of seconds to every commit, especially on Windows. It now takes a moment.
- **Copilot waits less on Blamely.** Copilot pauses for Blamely before and after every step it takes, and those pauses are now much shorter.
- **No more command windows flashing on Windows.** A few remaining background tasks could still briefly open a black window. They now all run out of sight.

## [1.8.4] - 2026-09-30

### Fixed

- **Attribution now reaches servers that require an issue key in every commit message.** Some teams configure their git server (for example a Bitbucket hook requiring a Jira key) to reject any commit whose message lacks one — and that rule also applies to the commits git makes when it records notes, whose messages are fixed ("Notes added by 'git notes add'"). Every push of Blamely's notes was refused, so attribution silently stayed on the developer's machine. Blamely now publishes each commit's note in a notes commit of its own whose message is that commit's own message — the one that already passed the rule — followed by the usual "Notes added by" line. A note goes out together with its commit: notes on commits you have not pushed yet wait until you push them. A machine whose notes were stuck gets all of them through on its next push. If the server still refuses, the push output now shows the server's reason instead of a generic warning — and if you push `refs/notes/blamely` yourself, it tells you to leave that to Blamely, since git's own notes commits are exactly what the server rejects. Syncing notes also works with a non-English git, with git older than 2.29, with a read mirror as fetch URL, and when pushing from a bare repository.

## [1.8.3] - 2026-09-22

### Added

- **Blamely now works with Devin.** Devin can write code in three different places, and Blamely follows all three: its command-line tool, its editor, and work it does for you in the cloud and hands back as a finished branch. That last one never touches your machine while it is being written, so Blamely reads the credit Devin leaves on the commit itself. On a machine that has both Devin and Claude installed, Devin's work used to be filed under Claude; it is now filed under Devin.
- **Devin's work shows which model wrote it, and how much it used.** Devin doesn't say which model is running, so everything it wrote was recorded with no model name at all. Blamely now finds that out for itself (for example `swe-2-high`), along with how much each step cost, and Devin appears with its own logo in the report.
- **Every update now leaves a record.** `~/.blamely/update.log` gets one line per update — whether it worked, which version it came from and went to, and the reason when it didn't. Updates happen in the background with nobody watching, so a machine that quietly stopped updating gave you nothing to go on. `blamely doctor` shows the latest line, and points it out when the last update failed.

### Fixed

- **Blamely starts tracking again immediately after an update (Windows).** An update shut the background service down and then failed to bring it back: the step that stops the old service was also stopping the very thing meant to start the new one. Nothing was recorded until the 15-minute safety check happened to run, so any AI work in that gap was credited to nobody. The restart is now handled by Windows itself, and the new version is up within seconds.
- **No more black command window flashing on your screen (Windows).** Blamely checks every 15 minutes that its background service is still alive, and Windows insisted on giving that check a command window of its own — one that popped open and shut four times an hour, plus once every time you signed in. The service now starts completely out of sight. The 15-minute check itself is unchanged: it is what brings Blamely back after a crash or an update.
- **Install and `blamely doctor` now tell you when the background service is still starting the old way (Windows).** If Blamely was ever installed as an administrator, a later ordinary install cannot change how the service starts — so fixes we shipped never arrived on that machine, while install still showed a green tick and doctor said nothing. Both now notice the difference, say what it is, and print the one command that repairs it. It is deliberately not repaired for you: that needs administrator rights, which is exactly why it has to be reported rather than swallowed.
- **Blamely's program files now show the right version (Windows).** The version written inside the program itself had been left behind at 1.6.7 while Blamely shipped 1.8.2, and the small helper that starts the background service carried no name, publisher or icon at all. A nameless program that launches another one is precisely what antivirus software treats as suspicious. Both files now carry full details and are signed by the same publisher.

## [1.8.2] - 2026-09-04

### Fixed

- **Updating Blamely now updates your editor plugins too.** `blamely update` moved only the command-line tool and left the VS Code and JetBrains plugins on whatever version they were, so a machine that only ever auto-updated slowly drifted — a current CLI beside a months-old editor plugin, even though all three are released together at the same version. The plugins now come along with the update. `--skip-plugins` opts out (the old `--with-plugins` is no longer needed and has been removed).
- **`blamely doctor` now tells you whether your editor plugin is actually installed.** Doctor checked the CLI, the hooks, the service and the database — but nothing about the plugin that draws the attribution UI in your editor. So a machine where the plugin had never been installed reported "All checks passed" while the user saw nothing in VS Code and had no way to tell why. Doctor now lists every editor and JetBrains IDE it finds and whether the Blamely plugin is there. A missing plugin is flagged but not treated as a fault, since a CLI-only install is a legitimate setup.
- **Your own uncommitted work is no longer credited to the AI.** Two separate faults could mark lines you had typed yourself as AI-written. The before-the-edit hook was registered without the flag that tells it to take a snapshot, so instead of snapshotting it ran the full recording step against the file's previous contents — crediting the agent with whatever was already there. And when a commit later established a file's starting point, it threw that snapshot away, so the next edit was compared against the last commit and swept up every uncommitted line with it. Both are fixed: a line nobody was observed writing stays human.
- **Codex, Copilot and Gemini now get credit for what they write through the terminal.** Only Claude's shell writes were being attributed. For the other three, a file written by a script, a heredoc or a formatter run through the agent's own terminal tool was recorded as human-written. All four now share one code path, so the next agent we add cannot quietly miss it.
- **AI-written code is no longer credited to you when the AI works through the terminal.** When an AI agent writes a file by running a shell command — a script, a heredoc, `sed` — instead of using its file-editing tool, the change was recorded but never reached the log that commit notes are built from, so those lines were committed as human-written. They are now attributed to the AI that wrote them. Sessions where the agent works mostly through the terminal were affected most.
- **Files written by long-running commands are no longer missed.** A command that writes files and then keeps working — running a formatter, a build, or the test suite — finishes long after the write, and anything written more than 15 seconds before it exited was dropped from attribution entirely. The window now covers the command's whole run, so a `… && black . && pytest` chain still credits the files it wrote.
- **Uninstall no longer flashes a burst of black command windows (Windows).** Removing Blamely used to blink a dozen or more console windows across the screen, one after another, looking like something was pinging in a loop. The cleanup that runs after uninstall now does its work in a single hidden window.
- **Uninstall now actually deletes Blamely's files (Windows).** The final cleanup step — which removes `~/.blamely`, the program itself, the database and the logs once Blamely has exited — was silently failing, so an uninstall that reported success could leave those files on disk and the background service registered. They are now removed as intended.

## [1.6.9] - 2026-07-27

### Fixed

- **Blamely's background service now starts reliably on macOS.** Some users — especially on company-managed Macs or after a reinstall — ended up with everything installed but the service never running, so nothing was tracked. Install now starts the service in a way macOS respects in all of these cases, including when it had been switched off by an earlier uninstall.
- **Company-wide installs no longer look broken.** When IT pushes Blamely to a Mac while the user isn't logged in, install used to end with a scary "daemon health check failed" warning even though nothing was wrong. It now simply says the service will start automatically at the user's next login. A step-by-step guide for IT teams deploying via Jamf/Kandji/Intune is included (`docs/macos-bulk-deployment.md`).
- **Updating Blamely on Windows now switches to the new version immediately.** Previously the old version could quietly keep running after an update — and stop receiving edits from your AI tools — until you signed out or restarted.
- **Fewer false alarms on slower machines.** On corporate Windows machines where antivirus scans new programs, the service can take a little longer to start the first time. Install now waits long enough for that instead of reporting a failure while the service was actually coming up fine.
- **Editor extension installs are reported correctly.** In some editors (seen with Antigravity on Windows), the Blamely extension installed successfully but was reported as failed. Install now reports what actually happened.
- **Better guidance when something really is wrong.** If the service genuinely fails to start on macOS, install now tells you the specific cause and the exact command to fix it, instead of a generic list of things to try.

## [1.6.8] - 2026-07-13

### Added

- **Attribution now survives cherry-pick, squash, and stash.** When you cherry-pick a commit, squash commits together, or stash and re-apply your work, Blamely keeps the AI-vs-human credit on each line instead of losing it. This joins the existing support for rebase and amend, so reorganizing your history no longer wipes out who wrote what.

### Fixed

- **Blamely keeps running on a laptop that's on battery (Windows).** Previously, if you started your laptop while unplugged, Windows quietly refused to launch Blamely's background service until you plugged in — so nothing was tracked in the meantime. It now starts and stays running whether or not you're on battery.
- **RustRover now gets the Blamely plugin.** If you use RustRover, the installer was skipping it and leaving it without the plugin. It's now detected like every other JetBrains IDE, so the plugin installs automatically.
- **More reliable attribution on Windows across editors.** Fixed cases where Windows file paths (with drive letters like `C:\`) weren't matched correctly in Cursor, Antigravity, and Claude, which could leave AI edits credited to the wrong person.
- **More accurate per-file AI vs. human line counts.** The breakdown of how many lines were written by AI versus a human on each file is now calculated more reliably, so totals no longer drop lines in edge cases.

## [1.6.7] - 2026-07-06

### Added

- **Files deleted by the Cursor agent are now credited to AI.** When you ask Cursor to delete a file, the deletion is attributed to Cursor instead of showing up as a human change.
- **Better Windows support for AI deletions.** File deletions made through the terminal on Windows are now recognized and attributed correctly.
- **Support for custom tool locations.** If your team runs Codex or Claude from a non-standard location, you can now point Blamely at it so those edits are still tracked. Set `CODEX_HOME` and/or `CLAUDE_CONFIG_DIR` before running `blamely install` (Blamely picks them up automatically), or add them anytime with `blamely config add tools.codex_home <path>` / `blamely config add tools.claude_config_dir <path>`. These are additive — the standard `~/.codex` and `~/.claude` locations keep working as before.
- **One-click installers.** Blamely now ships proper installers for Windows, macOS, and Linux, so you no longer need the command line to get set up.
- **Branded Windows installer.** The Windows installer and the installed app now show the Blamely icon instead of a blank one.

### Fixed

- **More accurate attribution when AI rewrites a whole file.** Both the new and replaced lines are now credited correctly.
- **Windows path matching.** Deletions on Windows are matched reliably instead of occasionally falling back to a human change.
- **File deletions from Copilot and Cursor agents** are now recognized and credited to the right tool.
- **Cleaner install log.** The setup log no longer shows garbled characters.
- **Reports stay fully private.** HTML reports now open completely offline — they no longer reach out to Google for fonts, so nothing leaves your machine when you open one.

##DASDA [1.6.0] - 2026-06-15

### Added

- **Claude Desktop attribution** — a new watcher records file edits and deletions made by Claude Desktop's "cowork" sandbox, which (unlike Claude Code) exposes no PostToolUse hook. It tails the app's undocumented logs, maps the sandbox VM's mounted repo path back to the host repo, and records deletions straight from the `rm` command (hashed from HEAD content) so attribution doesn't depend on when the VM syncs its changes back to the host filesystem.

### Changed

- **VS Code extension installed from Open VSX, not the Microsoft Marketplace** — `blamely install` now downloads the extension `.vsix` from [Open VSX](https://open-vsx.org/extension/blamely/blamely) and installs it by path (`--install-extension <path>`). Installing by path is registry-independent, so it works on VS Code proper — whose Marketplace listing was delisted — as well as the Open-VSX-based forks (Cursor, Antigravity). The `.vsix` is downloaded once per run and reused for every detected editor.

### Fixed

- **`blamely report --html` crashed on obfuscated release builds** — `garble -literals -tiny` renamed the HTML report's view-model fields, so the template's reflection-based `{{.ShortHash}}` lookups failed at run time (`can't evaluate field ShortHash`) only in published binaries. The report's root view-model type is now marked as reflection-used (`reflect.TypeOf`), so garble preserves its (and the nested structs') field names. Verified by rebuilding with the pinned garble/Go release toolchain.
- **Copilot chat attribution** — `scanTextEdits` now scans Copilot chat sessions incrementally (much better performance on long-running sessions), detects new request appends mid-session, and handles responses more robustly — fixing cases where Copilot chat edits failed to load and were left unattributed.
- **`blamely install` no longer fails on VS Code** — the previous `--install-extension Blamely.blamely` resolved against the Microsoft Marketplace, which no longer lists the extension, so the install silently did nothing on VS Code proper. When the Open VSX download is unavailable (offline / registry down), it falls back to the registry id, which still resolves via the editor's own gallery on Cursor/Antigravity.

### Notes

- **Gemini (Antigravity)** — the bundled Gemini *agent* is attributed via its transcript logs (shipped earlier). The Gemini *inline-completion* fix this cycle is editor-plugin side — see the VS Code extension's 1.6.0 changelog (Antigravity inline completions now attributed as AI).
