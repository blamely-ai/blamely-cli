package tools

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/blamely/blamely/internal/daemon"
	"github.com/blamely/blamely/internal/gitutil"
)

// ClaudeDesktopWatcher records file changes made by the Claude Desktop app's
// "cowork" sandbox, which — unlike Claude Code — exposes no PostToolUse hook.
// Claude Desktop runs file operations inside a VM with the user's repo mounted
// at /sessions/<session>/mnt/<repo>/, and leaves only undocumented logs under
// ~/Library/Logs/Claude:
//
//   - coworkd.log carries the VM's process spawns, e.g.
//     `[process:oneshot-<id>] spawn: ... cmd=bash args=[-c rm /sessions/<s>/mnt/intellij-test/foo.html]`
//   - main.log maps each VM mount name back to a host repo path
//     (`LocalSessions.checkTrust: cwd=/Users/.../intellij-test`).
//
// Deletions are recorded straight from the `rm` command's file argument (mapped
// to the host repo, hashed from HEAD content) so attribution doesn't depend on
// when the VM's change syncs back to the host filesystem — the timing race that
// made a git-status reconcile miss them.
//
// Writes are trickier: cowork often writes files through its MCP filesystem
// tool, which leaves NO bash `spawn:` line — so there's no per-write event to
// react to. We instead reconcile the working tree on a ticker for the duration
// of a live cowork session. coworkd.log brackets every session with explicit
// `mounting subpath … <host> -> /sessions/<s>/mnt/<base>` and `unmounting …`
// lines, so we know exactly which host repos are mounted-and-active and only
// reconcile those (a status-signature debounce skips unchanged trees). Those
// same mount lines give a reliable mount-basename -> host map, independent of
// main.log. A file-mutating bash spawn still triggers an immediate reconcile
// too — the two paths are complementary and the debounce prevents duplicates.
//
// Trade-off: while a cowork session is live, any working-tree change in the
// mounted repo is credited to cowork — including a hand-edit the user makes
// during the session. That's accepted because during an active cowork session
// the AI, not the user, is the one editing the mounted repo.
//
// Best-effort and inherently brittle: the log format is private to Claude
// Desktop and can change. It only does work where those logs exist (macOS
// today); elsewhere it idles.
type ClaudeDesktopWatcher struct {
	// LogDir overrides ~/Library/Logs/Claude (tests).
	LogDir string
	// mountsOverride injects the mount basename -> host repo map instead of
	// reading the logs (tests). nil in production.
	mountsOverride map[string]string

	mu           sync.Mutex
	lastWriteSig map[string]string    // host -> last git-status signature (write debounce)
	mounts       map[string]string    // mount basename -> host repo (learned from coworkd.log)
	active       map[string]time.Time // host repo -> time its cowork session was mounted
}

func (c *ClaudeDesktopWatcher) Name() string { return "claude-desktop" }

func defaultClaudeDesktopLogDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "Logs", "Claude")
}

// reconcileDelay is how long after a cowork command we re-scan the working tree
// for writes, giving the VM time to finish the op and sync it to the host.
const reconcileDelay = 3 * time.Second

// coworkReconcileInterval is how often we re-scan a live cowork session's repo
// for MCP-filesystem writes (which leave no bash spawn). It must be well under
// bashWriteWindow (15s) so every write falls inside the recently-changed window.
const coworkReconcileInterval = 4 * time.Second

// coworkMaxSessionAge caps how long a mount stays "active" without an explicit
// unmount — a safety net so a missed `unmounting` line can't make us reconcile
// (and credit human edits to cowork) indefinitely.
const coworkMaxSessionAge = 2 * time.Hour

func (c *ClaudeDesktopWatcher) Run(ctx context.Context, sink daemon.Sink) error {
	c.lastWriteSig = map[string]string{}
	c.mounts = map[string]string{}
	c.active = map[string]time.Time{}
	go c.reconcileActiveLoop(ctx, sink)
	dir := c.LogDir
	if dir == "" {
		dir = defaultClaudeDesktopLogDir()
	}
	if dir == "" {
		return nil
	}
	coworkd := filepath.Join(dir, "coworkd.log")

	for {
		if _, err := os.Stat(coworkd); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(5 * time.Second):
		}
	}

	f, err := os.Open(coworkd)
	if err != nil {
		return err
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() > 1<<16 {
		_, _ = f.Seek(-(1 << 16), io.SeekEnd)
	}
	reader := bufio.NewReaderSize(f, 1<<16)

	seen := map[string]bool{} // spawn id -> already processed
	for {
		line, err := readLineGrowing(ctx, reader, f)
		if err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return nil
			}
			return err
		}
		c.handleLine(string(line), sink, seen)
	}
}

func (c *ClaudeDesktopWatcher) handleLine(line string, sink daemon.Sink, seen map[string]bool) {
	// Session lifecycle: learn the mount→host map and which repos are live, so
	// the ticker can reconcile MCP-filesystem writes (which leave no spawn).
	if base, host, ok := parseCoworkMountLine(line); ok {
		c.mu.Lock()
		c.mounts[base] = host
		c.active[host] = time.Now()
		c.mu.Unlock()
		return
	}
	if base, ok := parseCoworkUnmountBase(line); ok {
		c.mu.Lock()
		if host := c.mounts[base]; host != "" {
			delete(c.active, host)
		}
		c.mu.Unlock()
		return
	}

	if !strings.Contains(line, "spawn:") || !looksFileMutating(line) {
		return
	}
	if id := spawnID(line); id != "" {
		if seen[id] {
			return
		}
		seen[id] = true
	}
	mounts := c.currentMounts()

	// Deletions: recorded straight from the rm targets, hashed from HEAD —
	// timing-independent, so it works even if the user commits immediately.
	for _, vm := range coworkRmTargets(line) {
		base, rel, ok := splitVMPath(vm)
		if !ok {
			continue
		}
		host := mounts[base]
		if host == "" {
			continue
		}
		for _, f := range expandRelAtHead(host, rel) {
			c.recordDeletion(host, f, sink)
		}
	}

	// Writes: reconcile the working tree shortly after, once the VM has synced.
	for _, base := range coworkMutationBasenames(line) {
		if host := mounts[base]; host != "" {
			h := host
			time.AfterFunc(reconcileDelay, func() { c.reconcileWrites(h, sink) })
		}
	}
}

func (c *ClaudeDesktopWatcher) recordDeletion(host, rel string, sink daemon.Sink) {
	removed := headFileRemovedHashes(host, rel)
	if len(removed) == 0 {
		return
	}
	repoID, _ := gitutil.RepoID(resolveSymlinks(host))
	if repoID == "" {
		repoID = host
	}
	if err := sink.Record(daemon.Event{
		When: time.Now(), Tool: "claude", Confidence: "medium", GenType: "chat",
		RepoPath: repoID, FilePath: rel,
		WorktreePath:   host,
		SuggestedLines: int64(len(removed)),
		RemovedLines:   toDaemonRemovedLines(removed),
		RawMeta:        `{"tool":"claude","source":"claude_desktop_cowork"}`,
	}); err != nil {
		log.Printf("claude-desktop: record delete %s: %v", rel, err)
	}
}

func (c *ClaudeDesktopWatcher) reconcileWrites(host string, sink daemon.Sink) {
	if _, ok := gitToplevel(host); !ok {
		return
	}
	sig := repoStatusSignature(host)
	if sig == "" {
		return
	}
	c.mu.Lock()
	if c.lastWriteSig[host] == sig {
		c.mu.Unlock()
		return
	}
	c.lastWriteSig[host] = sig
	c.mu.Unlock()

	repoID, _ := gitutil.RepoID(resolveSymlinks(host))
	if repoID == "" {
		repoID = host
	}
	for _, rel := range recentlyChangedFiles(host, bashWriteWindow) {
		ranges := perLineShaRanges(filepath.Join(host, rel))
		if len(ranges) == 0 {
			continue
		}
		_ = sink.Record(daemon.Event{
			When: time.Now(), Tool: "claude", Confidence: "medium", GenType: "chat",
			RepoPath: repoID, FilePath: rel,
			WorktreePath:   host,
			SuggestedLines: int64(len(ranges)),
			Lines:          toDaemonLineRanges(ranges),
			RawMeta:        `{"tool":"claude","source":"claude_desktop_cowork"}`,
		})
	}
}

// reconcileActiveLoop periodically reconciles the working tree of every live
// cowork session, catching MCP-filesystem writes that produce no bash spawn.
// Only mounts seen mounted (and not yet unmounted, and younger than the safety
// cap) are scanned; the per-host status-signature debounce means an idle repo
// records nothing.
func (c *ClaudeDesktopWatcher) reconcileActiveLoop(ctx context.Context, sink daemon.Sink) {
	t := time.NewTicker(coworkReconcileInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.mu.Lock()
			hosts := make([]string, 0, len(c.active))
			for host, mountedAt := range c.active {
				if time.Since(mountedAt) > coworkMaxSessionAge {
					delete(c.active, host) // stale: assume a missed unmount
					continue
				}
				hosts = append(hosts, host)
			}
			c.mu.Unlock()
			for _, host := range hosts {
				c.reconcileWrites(host, sink)
			}
		}
	}
}

// currentMounts returns the mount-basename -> host map, merging what we've
// learned from coworkd.log's `mounting subpath` lines (authoritative) with the
// legacy main.log heuristics, or the test override when set.
func (c *ClaudeDesktopWatcher) currentMounts() map[string]string {
	if c.mountsOverride != nil {
		return c.mountsOverride
	}
	m := map[string]string{}
	dir := c.LogDir
	if dir == "" {
		dir = defaultClaudeDesktopLogDir()
	}
	if data, err := os.ReadFile(filepath.Join(dir, "main.log")); err == nil {
		for k, v := range parseClaudeDesktopMounts(string(data)) {
			m[k] = v
		}
	}
	c.mu.Lock()
	for k, v := range c.mounts {
		m[k] = v // coworkd-learned mappings win
	}
	c.mu.Unlock()
	return m
}

var (
	coworkMntRe     = regexp.MustCompile(`/mnt/([^/\s'"]+)/`)
	spawnIDRe       = regexp.MustCompile(`process:(oneshot-[a-f0-9-]+)`)
	rmVerbRe        = regexp.MustCompile(`(^|\s)(?:git\s+)?rm\s`)
	shellBreakRe    = regexp.MustCompile(`\s(?:&&|\|\||;|\|)\s|;`)
	coworkMountRe   = regexp.MustCompile(`mounting subpath for user \S+ \([^)]*\):\s*(\S+)\s*->\s*(\S+)`)
	coworkUnmountRe = regexp.MustCompile(`unmount(?:ing|ed)\s+(/sessions/\S+)`)
)

// parseCoworkMountLine parses a `mounting subpath … <hostVMPath> -> <vmPath>`
// line into the VM mount basename and the real host repo path. The host path is
// the VM's bind-mount source with its `…/shared` virtiofs prefix stripped
// (e.g. /mnt/.virtiofs-root/shared/Users/x/repo -> /Users/x/repo); the basename
// is the last segment of the /sessions/<s>/mnt/<base> target.
func parseCoworkMountLine(line string) (base, host string, ok bool) {
	m := coworkMountRe.FindStringSubmatch(line)
	if m == nil {
		return "", "", false
	}
	hostVM, vmPath := m[1], m[2]
	i := strings.Index(hostVM, "/shared")
	if i < 0 {
		return "", "", false
	}
	host = strings.TrimRight(hostVM[i+len("/shared"):], "/")
	base = filepath.Base(strings.TrimRight(vmPath, "/"))
	if !strings.HasPrefix(host, "/") || base == "" || base == "." {
		return "", "", false
	}
	return base, host, true
}

// parseCoworkUnmountBase returns the VM mount basename from an `unmounting` /
// `unmounted /sessions/<s>/mnt/<base>` line.
func parseCoworkUnmountBase(line string) (base string, ok bool) {
	m := coworkUnmountRe.FindStringSubmatch(line)
	if m == nil {
		return "", false
	}
	base = filepath.Base(strings.TrimRight(m[1], "/"))
	if base == "" || base == "." || base == "/" {
		return "", false
	}
	return base, true
}

// spawnID extracts the unique oneshot id from a cowork log line so each spawn
// is processed once (the startup tail-replay can re-show recent lines).
func spawnID(line string) string {
	if m := spawnIDRe.FindStringSubmatch(line); m != nil {
		return m[1]
	}
	return ""
}

// coworkRmTargets returns the VM file paths an `rm` (or `git rm`) in the cowork
// command targets — the tokens after the rm verb, up to the next shell
// separator, that live under a /mnt/ mount.
func coworkRmTargets(line string) []string {
	loc := rmVerbRe.FindStringIndex(line)
	if loc == nil {
		return nil
	}
	rest := line[loc[1]:]
	if b := shellBreakRe.FindStringIndex(rest); b != nil {
		rest = rest[:b[0]]
	}
	if i := strings.Index(rest, "]"); i >= 0 { // close of args=[...]
		rest = rest[:i]
	}
	var out []string
	for _, tok := range strings.Fields(rest) {
		tok = strings.Trim(tok, `'"`)
		if strings.HasPrefix(tok, "-") || !strings.Contains(tok, "/mnt/") {
			continue
		}
		out = append(out, tok)
	}
	return out
}

// splitVMPath splits /sessions/<s>/mnt/<basename>/<rel> into the mount basename
// and the repo-relative path.
func splitVMPath(vm string) (basename, rel string, ok bool) {
	i := strings.Index(vm, "/mnt/")
	if i < 0 {
		return "", "", false
	}
	after := vm[i+len("/mnt/"):]
	j := strings.Index(after, "/")
	if j < 0 {
		return "", "", false
	}
	return after[:j], after[j+1:], true
}

// expandRelAtHead resolves a (possibly globbed) repo-relative path to the set of
// matching tracked files at HEAD, so `rm test*.html` records each deleted file.
func expandRelAtHead(host, rel string) []string {
	if !strings.ContainsAny(rel, "*?[") {
		return []string{rel}
	}
	out, err := gitutil.Output(host, "ls-tree", "-r", "--name-only", "HEAD")
	if err != nil {
		return nil
	}
	var matched []string
	for _, f := range strings.Split(string(out), "\n") {
		if f = strings.TrimSpace(f); f == "" {
			continue
		}
		if ok, _ := filepath.Match(rel, f); ok {
			matched = append(matched, f)
		}
	}
	return matched
}

// coworkMutationBasenames returns the VM mount basenames referenced by a
// file-mutating cowork spawn line. Used to trigger the write reconcile.
func coworkMutationBasenames(line string) []string {
	if !strings.Contains(line, "spawn:") || !looksFileMutating(line) {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range coworkMntRe.FindAllStringSubmatch(line, -1) {
		if b := m[1]; !seen[b] {
			seen[b] = true
			out = append(out, b)
		}
	}
	return out
}

// looksFileMutating reports whether a cowork command writes or deletes files.
func looksFileMutating(cmd string) bool {
	for _, kw := range []string{"rm ", "rm-", "git rm", " mv ", " cp ", "sed -i", " tee ", "truncate", " install ", ">", "touch ", " dd "} {
		if strings.Contains(cmd, kw) {
			return true
		}
	}
	return false
}

// parseClaudeDesktopMounts builds a mount-basename -> host-repo-path map from
// main.log, which logs the host path of each opened local session/space.
func parseClaudeDesktopMounts(mainLog string) map[string]string {
	m := map[string]string{}
	add := func(p string) {
		p = strings.TrimSpace(strings.Trim(strings.TrimSpace(p), `'",`))
		if strings.HasPrefix(p, "/") {
			m[filepath.Base(p)] = p
		}
	}
	for _, line := range strings.Split(mainLog, "\n") {
		switch {
		case strings.Contains(line, "cwd="):
			add(line[strings.Index(line, "cwd=")+4:])
		case strings.Contains(line, "FileWatching"), strings.Contains(line, "FileSystemWatcher"):
			if i := strings.LastIndex(line, ": "); i >= 0 {
				add(line[i+2:])
			}
		}
	}
	return m
}

// repoStatusSignature is a content hash of `git status --porcelain`, used to
// skip re-recording a working tree that hasn't changed since the last reconcile.
func repoStatusSignature(root string) string {
	out, err := gitutil.Output(root, "status", "--porcelain")
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(out)
	return hex.EncodeToString(sum[:])
}
