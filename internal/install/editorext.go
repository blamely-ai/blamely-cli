package install

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// blamelyExtensionID is the publisher.name identifier for the Blamely extension,
// used to detect (--list-extensions) and uninstall it. The Microsoft VS Code
// Marketplace has DELISTED the extension, so `--install-extension <id>` no longer
// resolves on VS Code proper. We therefore install from a .vsix downloaded from
// Open VSX (downloadOpenVSXVSIX), which is registry-independent and works on every
// VS Code-family editor; the id stays the source-of-record for detect/uninstall.
//
//	https://open-vsx.org/extension/blamely/blamely  (Open VSX — Antigravity/Cursor gallery; our install source)
//
// The publisher and name are both lowercase in the manifest (and in the Open VSX
// URL above), so the canonical id is "blamely.blamely". Detection lowercases
// before comparing, but `--uninstall-extension` is matched case-sensitively by
// several Code-OSS forks, so the case here must match what the editor records or
// uninstall silently no-ops and the extension is left behind.
const blamelyExtensionID = "blamely.blamely"

// openVSXLatestAPI returns metadata (including the .vsix download URL) for the
// latest published Blamely release on Open VSX.
const openVSXLatestAPI = "https://open-vsx.org/api/blamely/blamely/latest"

// signatureVerificationError is VS Code's own message when its bundled
// vsce-sign helper fails to run during `--install-extension`. It's transient
// and environment-dependent — e.g. it reliably fires when `code` is spawned
// non-interactively (as blamely install does) but a retry of the exact same
// command in the same environment succeeds.
const signatureVerificationError = "Signature verification was not executed"

// editorExtensionTarget describes how to locate one VS Code-family editor's
// bundled CLI so we can drive its `--install-extension` flow — the exact
// mechanism each editor's own marketplace search / "Install from VSIX" action
// uses under the hood, just invoked headlessly. Every Code-OSS fork (VS Code,
// Cursor, Antigravity IDE, …) ships this CLI and accepts the same flags
// (--list-extensions / --install-extension / --uninstall-extension); only the
// binary name and where it lives differ.
type editorExtensionTarget struct {
	Label string
	// PathNames are CLI binary names to look up on $PATH, tried in order.
	// This is the portable path — every OS's installer can wire these up.
	PathNames []string
	// AppBundles are macOS .app bundle names whose
	// Contents/Resources/app/bin/<one of PathNames> we fall back to when the
	// CLI isn't on PATH. Cursor and Antigravity IDE both ship a `cursor-alike`
	// CLI inside the bundle even when no shell command was installed.
	AppBundles []string
}

var editorExtensionTargets = []editorExtensionTarget{
	{
		Label:      "VS Code",
		PathNames:  []string{"code"},
		AppBundles: []string{"Visual Studio Code"},
	},
	{
		Label:      "Cursor",
		PathNames:  []string{"cursor"},
		AppBundles: []string{"Cursor"},
	},
	{
		Label:      "Antigravity IDE",
		PathNames:  []string{"antigravity-ide", "antigravity"},
		AppBundles: []string{"Antigravity IDE"},
	},
	{
		// Devin IDE is a Windsurf/Code-OSS fork. Its CLI is named after
		// product.json's applicationName ("devin-desktop"), not after the
		// product, and its extension host reads ~/.devin/extensions in the
		// standard VS Code layout.
		//
		// It ships its OWN gallery (marketplace.windsurf.com) rather than Open
		// VSX, which does not matter here: InstallEditorExtensions installs a
		// downloaded .vsix by path, and that is registry-independent. Only the
		// id-based fallback would depend on the editor's gallery.
		Label:      "Devin IDE",
		PathNames:  []string{"devin-desktop"},
		AppBundles: []string{"Devin"},
	},
}

// EditorExtensionResult is one row of the install log's "Editors" group: the
// outcome of trying to get the Blamely extension into a single editor.
type EditorExtensionResult struct {
	Label     string
	CLIPath   string // "" => editor not found on this machine
	Installed bool   // true only when THIS run did the initial install (drives uninstall tracking)
	Updated   bool   // true when the extension was already present and we force-reinstalled it to pull the latest marketplace version
	Err       error
}

// AlreadyPresent reports whether the extension was found already installed
// (as opposed to freshly installed by us, absent, or failed).
func (r EditorExtensionResult) AlreadyPresent() bool {
	return r.CLIPath != "" && !r.Installed && r.Err == nil
}

// InstallEditorExtensions drives the marketplace install for every known
// VS Code-family editor present on the machine, returning one result per
// target regardless of outcome so the caller can render a single consistent
// "Editors" group (found-and-installed / found-and-already-there / absent /
// failed).
func InstallEditorExtensions() []EditorExtensionResult {
	results := make([]EditorExtensionResult, 0, len(editorExtensionTargets))
	// Resolve the install source ONCE. Prefer a .vsix downloaded from Open VSX —
	// installing by path is registry-independent, so it works on VS Code proper
	// (whose Marketplace listing was delisted) as well as the Open-VSX forks. If
	// the download is unavailable (offline / Open VSX down), fall back to the
	// registry id, which still resolves via the editor's own gallery on
	// Cursor/Antigravity. Downloaded once and reused for every editor.
	source := blamelyExtensionID
	if vsix, err := downloadOpenVSXVSIX(); err == nil {
		source = vsix
		defer os.Remove(vsix)
	}
	for _, t := range editorExtensionTargets {
		cliPath, _ := findEditorCLI(t)
		r := EditorExtensionResult{Label: t.Label, CLIPath: cliPath}
		if cliPath == "" {
			results = append(results, r)
			continue
		}
		// Force-reinstall unconditionally: a fresh pull both installs it the
		// first time and updates it to latest on every subsequent run, so users
		// never get stuck on a stale version.
		wasPresent := extensionInstalled(cliPath, blamelyExtensionID)
		// Uninstall the existing extension before reinstalling. `--install-extension
		// --force` is supposed to overwrite, but several Code-OSS forks treat it as a
		// no-op when the VSIX version isn't strictly newer (e.g. a republished same
		// version, or a downgrade), so the user stays on the stale build. Removing it
		// first — mirroring the JetBrains path, which wipes the plugin dir before
		// extracting — guarantees the install below always lands the downloaded build.
		if wasPresent {
			if exactID := installedExtensionID(cliPath, blamelyExtensionID); exactID != "" {
				_ = exec.Command(cliPath, "--uninstall-extension", exactID).Run()
			}
		}
		out, err := installExtensionWithRetry(cliPath, source)
		if err != nil {
			r.Err = fmt.Errorf("%s --install-extension %s --force: %w: %s",
				filepath.Base(cliPath), source, err, strings.TrimSpace(string(out)))
		} else if wasPresent {
			r.Updated = true
		} else {
			r.Installed = true
		}
		results = append(results, r)
	}
	return results
}

// downloadOpenVSXVSIX fetches the latest Blamely .vsix from Open VSX into a temp
// file and returns its path; the caller removes it. Installing that file with
// `--install-extension <path>` is registry-independent, so it works on VS Code
// proper (Microsoft Marketplace delisted the extension) as well as the
// Open-VSX-based forks (Cursor, Antigravity IDE).
func downloadOpenVSXVSIX() (string, error) {
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(openVSXLatestAPI)
	if err != nil {
		return "", fmt.Errorf("query open-vsx: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("open-vsx returned %s", resp.Status)
	}
	var rel struct {
		Version string `json:"version"`
		Files   struct {
			Download string `json:"download"`
		} `json:"files"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return "", fmt.Errorf("decode open-vsx response: %w", err)
	}
	if rel.Files.Download == "" {
		return "", fmt.Errorf("open-vsx response has no download url")
	}
	// The download URL 302-redirects to the CDN; http.Client follows redirects.
	dl, err := client.Get(rel.Files.Download)
	if err != nil {
		return "", fmt.Errorf("download vsix: %w", err)
	}
	defer dl.Body.Close()
	if dl.StatusCode != http.StatusOK {
		return "", fmt.Errorf("vsix download returned %s", dl.Status)
	}
	tmp, err := os.CreateTemp("", "blamely-*.vsix")
	if err != nil {
		return "", err
	}
	defer tmp.Close()
	if _, err := io.Copy(tmp, io.LimitReader(dl.Body, 100<<20)); err != nil {
		_ = os.Remove(tmp.Name())
		return "", fmt.Errorf("save vsix: %w", err)
	}
	return tmp.Name(), nil
}

// signatureVerificationRetries is how many extra attempts we make when an
// install fails with signatureVerificationError. The failure is transient and
// environment-dependent — the bundled vsce-sign helper intermittently doesn't
// run when the editor CLI is spawned non-interactively (as blamely install
// does). A retry of the identical command usually succeeds; a few attempts with
// a short backoff makes it reliable across VS Code and Cursor.
const signatureVerificationRetries = 3

// installExtensionWithRetry runs `<cli> --install-extension <source> --force`,
// where source is either a downloaded .vsix path or the registry id, retrying
// only the signature-verification flake (never other errors). Returns the
// combined output and error of the last attempt.
func installExtensionWithRetry(cliPath, source string) ([]byte, error) {
	run := func() ([]byte, error) {
		return exec.Command(cliPath, "--install-extension", source, "--force").CombinedOutput()
	}
	out, err := run()
	for attempt := 0; attempt < signatureVerificationRetries; attempt++ {
		if err == nil || !strings.Contains(string(out), signatureVerificationError) {
			break
		}
		time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
		out, err = run()
	}
	// Crash AFTER success: some editor CLIs (seen with Antigravity on Windows —
	// a v8 FATAL ERROR during its own post-install teardown) exit non-zero even
	// though their output already reported the extension installed. The install
	// DID land, so trust the CLI's own success line over its exit code;
	// reporting ✗ here made whole fleets look broken over a cosmetic crash.
	if err != nil && strings.Contains(string(out), "successfully installed") {
		return out, nil
	}
	return out, err
}

// DiscoverInstalledEditorExtensions returns the label of every detected
// VS Code-family editor that currently has the Blamely extension installed.
//
// Uninstall uses this so it removes the extension from editors even when the
// install was never state-tracked — the extension was already present when
// `blamely install` ran (recorded as "Updated", not "Installed"), the user
// installed it from the marketplace themselves, or the install predates state
// tracking. Mirrors the JetBrains discovery path in Uninstall().
func DiscoverInstalledEditorExtensions() []string {
	var labels []string
	for _, t := range editorExtensionTargets {
		cliPath, ok := findEditorCLI(t)
		if !ok {
			continue
		}
		if extensionInstalled(cliPath, blamelyExtensionID) {
			labels = append(labels, t.Label)
		}
	}
	return labels
}

// UninstallEditorExtensions removes the Blamely extension from every editor
// labelled in `labels` — the set we recorded as "this run installed it" at
// install time. We never remove an extension the user installed themselves;
// that would be a surprising, hard-to-reverse side effect of `blamely
// uninstall`. Best-effort: an editor that vanished since install is skipped.
func UninstallEditorExtensions(labels []string) error {
	if len(labels) == 0 {
		return nil
	}
	want := make(map[string]bool, len(labels))
	for _, l := range labels {
		want[l] = true
	}
	var firstErr error
	for _, t := range editorExtensionTargets {
		if !want[t.Label] {
			continue
		}
		cliPath, ok := findEditorCLI(t)
		if !ok {
			continue
		}
		// Uninstall by the exact id the editor reports rather than our constant:
		// `--uninstall-extension` is case-sensitive on some forks, so passing the
		// listed id guarantees a match even if the manifest case ever drifts. An
		// empty result means it's already gone — skip it.
		exactID := installedExtensionID(cliPath, blamelyExtensionID)
		if exactID == "" {
			continue
		}
		if err := exec.Command(cliPath, "--uninstall-extension", exactID).Run(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// findEditorCLI locates an editor's bundled CLI: first on $PATH (the portable
// path — most installers wire up a shell command there), then — on macOS —
// inside the app bundle's Contents/Resources/app/bin/, where Code-OSS forks
// place their CLI launcher even when no shell command was set up.
func findEditorCLI(t editorExtensionTarget) (string, bool) {
	for _, name := range t.PathNames {
		if p, ok := lookPath(name); ok {
			return p, true
		}
	}
	if runtime.GOOS != "darwin" {
		return "", false
	}
	for _, app := range t.AppBundles {
		base := filepath.Join("/Applications", app+".app", "Contents", "Resources", "app", "bin")
		for _, name := range t.PathNames {
			p := filepath.Join(base, name)
			if fileExists(p) {
				return p, true
			}
		}
	}
	return "", false
}

func extensionInstalled(cliPath, id string) bool {
	return installedExtensionID(cliPath, id) != ""
}

// installedExtensionID returns the extension id exactly as the editor's CLI
// lists it (case preserved), or "" if it isn't installed. The match is
// case-insensitive, so callers can pass our canonical id and still get back the
// editor's own casing — which uninstall needs because `--uninstall-extension`
// is case-sensitive on some Code-OSS forks.
func installedExtensionID(cliPath, id string) string {
	out, err := exec.Command(cliPath, "--list-extensions").Output()
	if err != nil {
		return ""
	}
	target := strings.ToLower(id)
	for _, line := range strings.Split(string(out), "\n") {
		got := strings.TrimSpace(line)
		if strings.ToLower(got) == target {
			return got
		}
	}
	return ""
}
