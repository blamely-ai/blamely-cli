package authorship

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/blamely/blamely/internal/gitutil"
)

// Working-log + baseline storage. The working log is the source of truth for
// uncommitted authorship; it is plain files under the repo's .git so the
// checkpoint→diff→commit path needs neither a daemon nor a database (G4).
//
// Layout (docs/attribution-v2-design.md §3.2):
//
//	.git/blamely/working_logs/<branch>/<base_sha>/<path>.json        ← attributions
//	.git/blamely/working_logs/<branch>/<base_sha>/.baselines/<path>  ← content the
//	                                                                    attributions describe
//
// Everything here is path/filepath-based and uses temp+rename, so it behaves
// identically on Windows, Linux, and macOS.

// workingLogDir is the per-(repo,branch,base_sha) directory. Rotating base_sha on
// commit gives a fresh tree for free; sanitizing the branch keeps slashes and
// Windows-illegal characters out of path components.
func workingLogDir(repoRoot, branch, baseSHA string) string {
	return filepath.Join(gitutil.GitDir(repoRoot), "blamely", "working_logs",
		sanitizeComponent(branch), sanitizeComponent(baseSHA))
}

// WorkingLogPath is where relPath's attributions live (mirrors the repo tree).
func WorkingLogPath(repoRoot, branch, baseSHA, relPath string) string {
	return filepath.Join(workingLogDir(repoRoot, branch, baseSHA),
		filepath.FromSlash(cleanRel(relPath))+".json")
}

// BaselinePath is where relPath's last-known content lives (raw, no extension).
func BaselinePath(repoRoot, branch, baseSHA, relPath string) string {
	return filepath.Join(workingLogDir(repoRoot, branch, baseSHA), ".baselines",
		filepath.FromSlash(cleanRel(relPath)))
}

// sanitizeComponent makes an arbitrary string safe as a SINGLE path component on
// all three OSes: branch names contain '/', and Windows forbids \ : * ? " < > |.
func sanitizeComponent(s string) string {
	if s == "" {
		return "_"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' ||
			r == '"' || r == '<' || r == '>' || r == '|' || r < 0x20:
			b.WriteByte('-')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// cleanRel normalizes a repo-relative path to forward slashes and strips any
// leading "./" or drive/leading separators, so it maps to a stable subtree.
func cleanRel(rel string) string {
	rel = filepath.ToSlash(rel)
	rel = strings.TrimPrefix(rel, "./")
	rel = strings.TrimLeft(rel, "/")
	return rel
}

// LoadWorkingLog reads relPath's working log, or (nil, nil) if none exists yet.
func LoadWorkingLog(repoRoot, branch, baseSHA, relPath string) (*WorkingLog, error) {
	return loadWorkingLogFile(WorkingLogPath(repoRoot, branch, baseSHA, relPath))
}

func loadWorkingLogFile(path string) (*WorkingLog, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var wl WorkingLog
	if err := json.Unmarshal(data, &wl); err != nil {
		return nil, fmt.Errorf("authorship: parse working log %s: %w", path, err)
	}
	return &wl, nil
}

func loadBaseline(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return string(data), true
}

// PutBaseline records relPath's current content as the pre-edit baseline (the
// `record --pre` fallback in Decision B). Subsequent Update calls diff against it
// when no working-log state exists yet.
func PutBaseline(repoRoot, branch, baseSHA, relPath, content string) error {
	return atomicWrite(BaselinePath(repoRoot, branch, baseSHA, relPath), []byte(content))
}

// SeedWorkingLog writes a STARTING working log + baseline for relPath at (branch,
// baseSHA) from a known per-line attribution — used to carry COMMITTED authorship
// across a commit so re-editing an unchanged committed line keeps its real author
// instead of defaulting to Human (I5). It is a no-op if a working log already
// exists, so it never clobbers uncommitted state. nowMS=0 leaves the stamp unset
// (the next Update restamps). Held under the same per-file lock as Update.
//
// A pre-edit baseline with NO working log next to it is the third case: the
// pre-hook (`record --pre`) captured the file's content before an agent ran, and
// that content is NEWER than the commit because the file already had uncommitted
// changes nobody observed. That baseline is kept, and the divergence from the
// commit is folded into the seeded log as Human — the same rule an unobserved line
// gets everywhere else. Overwriting it with the committed content instead is what
// made the very next edit diff against the commit and claim the user's own
// uncommitted lines for the agent.
func SeedWorkingLog(repoRoot, branch, baseSHA, relPath, content string, lines []LineAttribution, nowMS int64) error {
	rel := cleanRel(relPath)
	wlPath := WorkingLogPath(repoRoot, branch, baseSHA, relPath)
	basePath := BaselinePath(repoRoot, branch, baseSHA, relPath)
	return withFileLock(wlPath, func() error {
		existing, err := loadWorkingLogFile(wlPath)
		if err != nil {
			return err
		}
		if existing != nil {
			return nil // already tracking this file — don't overwrite
		}
		wl := &WorkingLog{
			Schema: WorkingLogSchema, File: rel, BaseSHA: baseSHA,
			BlobSHA: sha256Hex(content), UpdatedMS: nowMS, Lines: lines,
		}
		stored, hadBaseline := loadBaseline(basePath)
		if hadBaseline && stored != content {
			// Re-key the committed attributions onto the captured content: unchanged
			// committed lines keep their author, the uncommitted divergence is Human.
			wl = Attribute(wl, content, stored, HumanAuthor(), nowMS)
			wl.File, wl.BaseSHA, wl.UpdatedMS = rel, baseSHA, nowMS
		}
		data, err := json.MarshalIndent(wl, "", "  ")
		if err != nil {
			return err
		}
		if err := atomicWrite(wlPath, data); err != nil {
			return err
		}
		if hadBaseline {
			return nil // keep the captured pre-edit baseline
		}
		return atomicWrite(basePath, []byte(content))
	})
}

// Update applies one observed edit to relPath's working log under a per-file lock:
// it diffs the stored baseline (the content the current attributions describe)
// against newContent, attributes the changed lines to author, and persists both
// the updated log and newContent as the next baseline.
//
// fallbackBaseline is used as the diff's old side ONLY on the first observed edit
// (no stored baseline): pass the pre-edit capture or HEAD content per Decision B,
// or "" for a brand-new file. nowMS=0 stamps with the wall clock.
func Update(repoRoot, branch, baseSHA, relPath, newContent, fallbackBaseline string, author Author, nowMS int64) (*WorkingLog, error) {
	return update(repoRoot, branch, baseSHA, relPath, newContent, func() string { return fallbackBaseline }, author, nowMS)
}

// update is Update with the fallback baseline computed only when it is needed
// (no stored baseline), under the same lock that reads the stored one — so a
// caller whose fallback costs a git process pays it on a file's first edit only.
func update(repoRoot, branch, baseSHA, relPath, newContent string, fallbackBaseline func() string, author Author, nowMS int64) (*WorkingLog, error) {
	rel := cleanRel(relPath)
	wlPath := WorkingLogPath(repoRoot, branch, baseSHA, relPath)
	basePath := BaselinePath(repoRoot, branch, baseSHA, relPath)

	var result *WorkingLog
	err := withFileLock(wlPath, func() error {
		prior, err := loadWorkingLogFile(wlPath)
		if err != nil {
			return err
		}
		baseline, ok := loadBaseline(basePath)
		if !ok {
			baseline = fallbackBaseline()
		}

		wl := Attribute(prior, baseline, newContent, author, nowMS)
		wl.File, wl.BaseSHA = rel, baseSHA

		data, err := json.MarshalIndent(wl, "", "  ")
		if err != nil {
			return err
		}
		if err := atomicWrite(wlPath, data); err != nil {
			return err
		}
		if err := atomicWrite(basePath, []byte(newContent)); err != nil {
			return err
		}
		// Record lines this edit removed (attributed to `author`) in the separate
		// deletions log, so the commit note can attribute deletions too.
		_ = AppendDeletions(repoRoot, branch, baseSHA, rel, DeletedBaselineLines(baseline, newContent), author)
		result = wl
		return nil
	})
	return result, err
}

// RenameWorkingLog carries the source log and its baseline to a new path after
// an observed file move. The caller first reconciles the source's captured
// pre-edit content, then applies any edits at the destination. Lock both paths
// in a stable order so opposing moves cannot deadlock.
func RenameWorkingLog(repoRoot, branch, baseSHA, oldRel, newRel string) error {
	oldRel, newRel = cleanRel(oldRel), cleanRel(newRel)
	if oldRel == newRel {
		return nil
	}
	oldPath := WorkingLogPath(repoRoot, branch, baseSHA, oldRel)
	newPath := WorkingLogPath(repoRoot, branch, baseSHA, newRel)
	first, second := oldPath, newPath
	if first > second {
		first, second = second, first
	}
	return withFileLock(first, func() error {
		return withFileLock(second, func() error {
			wl, err := loadWorkingLogFile(oldPath)
			if err != nil {
				return err
			}
			if wl == nil {
				return fmt.Errorf("authorship: missing rename source log %s", oldRel)
			}
			baseline, err := os.ReadFile(BaselinePath(repoRoot, branch, baseSHA, oldRel))
			if err != nil {
				return err
			}
			wl.File = newRel
			data, err := json.MarshalIndent(wl, "", "  ")
			if err != nil {
				return err
			}
			if err := atomicWrite(newPath, data); err != nil {
				return err
			}
			if err := atomicWrite(BaselinePath(repoRoot, branch, baseSHA, newRel), baseline); err != nil {
				return err
			}
			if err := os.Remove(oldPath); err != nil {
				return err
			}
			return os.Remove(BaselinePath(repoRoot, branch, baseSHA, oldRel))
		})
	})
}

// atomicWrite writes data to path via temp-file + rename (atomic, replace-existing
// on all three OSes — Go's os.Rename uses MOVEFILE_REPLACE_EXISTING on Windows).
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".wl-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // harmless no-op once the rename succeeds
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

const (
	lockTimeout = 5 * time.Second
	lockStale   = 10 * time.Second
	lockPoll    = 15 * time.Millisecond
)

// withFileLock serializes the read-modify-write of one working log across the two
// writers (editor plugin and CLI). It uses an O_CREATE|O_EXCL lock file rather
// than flock, since Go has no portable flock; a lock older than lockStale is
// treated as orphaned (writer crashed) and stolen.
func withFileLock(target string, fn func() error) error {
	lockPath := target + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return err
	}
	deadline := time.Now().Add(lockTimeout)
	for {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			f.Close()
			defer os.Remove(lockPath)
			return fn()
		}
		if !os.IsExist(err) {
			return err
		}
		if fi, e := os.Stat(lockPath); e == nil && time.Since(fi.ModTime()) > lockStale {
			os.Remove(lockPath) // orphaned lock from a crashed writer — steal it
			continue
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("authorship: timed out acquiring lock %s", lockPath)
		}
		time.Sleep(lockPoll)
	}
}

// AuthorTypesForFile loads relPath's working log and returns its per-line author
// types (1-based line number → "ai"/"human"), plus whether a log was found. Used
// after the flip, by note generation.
func AuthorTypesForFile(repoRoot, branch, baseSHA, relPath string) (map[int]AuthorType, bool) {
	wl, err := LoadWorkingLog(repoRoot, branch, baseSHA, relPath)
	if err != nil || wl == nil {
		return nil, false
	}
	m := make(map[int]AuthorType, len(wl.Lines))
	for _, r := range wl.Lines {
		for ln := r.Start; ln <= r.End; ln++ {
			m[ln] = r.Author.Type
		}
	}
	return m, true
}

// AuthorsForFile is AuthorTypesForFile but returns the FULL author (tool, model,
// gen_type), used by the flip to rewrite a note's per-line attribution.
func AuthorsForFile(repoRoot, branch, baseSHA, relPath string) (map[int]Author, bool) {
	wl, err := LoadWorkingLog(repoRoot, branch, baseSHA, relPath)
	if err != nil || wl == nil {
		return nil, false
	}
	m := make(map[int]Author, len(wl.Lines))
	for _, r := range wl.Lines {
		for ln := r.Start; ln <= r.End; ln++ {
			m[ln] = r.Author
		}
	}
	return m, true
}

// ListWorkingLogs returns every file's working log under (repoRoot, branch, baseSHA)
// — the set of files with uncommitted attribution this work cycle. Used to paint
// the gutter/sidebar repo-wide from one source. Missing dir → empty, not an error.
func ListWorkingLogs(repoRoot, branch, baseSHA string) ([]*WorkingLog, error) {
	dir := workingLogDir(repoRoot, branch, baseSHA)
	var out []*WorkingLog
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			// Skip the .baselines subtree (raw content, not logs).
			if d.Name() == ".baselines" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".json") {
			return nil
		}
		wl, lerr := loadWorkingLogFile(path)
		if lerr != nil || wl == nil {
			return nil // skip unreadable/partial entries; never fail the whole scan
		}
		out = append(out, wl)
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return out, err
	}
	return out, nil
}

// MigrateWorkingLogs re-keys working logs (and their baselines) from oldBase to
// newBase under the same branch, updating each log's BaseSHA. Called post-commit to
// move uncommitted attribution onto the new HEAD so it survives the commit (a partial
// commit of one file must not strand another file's working log) and stays bounded —
// logs follow HEAD instead of accumulating at ancestor bases.
//
// Files in `committed` (the files this commit included) are DELETED from oldBase:
// their per-line attribution now lives durably in the git note (pushed to the
// remote) and in SQLite (from which amend/rebase re-attribution reconciles), so
// keeping a per-commit working log would only accumulate on disk without bound.
// A file whose newBase log already exists (a fresher editor flush) wins; the old
// one is dropped. No-op when oldBase == newBase or there is nothing to move.
func MigrateWorkingLogs(repoRoot, branch, oldBase, newBase string, committed map[string]bool) error {
	if oldBase == "" || newBase == "" || oldBase == newBase {
		return nil
	}
	logs, err := ListWorkingLogs(repoRoot, branch, oldBase)
	if err != nil || len(logs) == 0 {
		return err
	}
	for _, wl := range logs {
		rel := wl.File
		if committed[rel] {
			// Committed in this commit: KEEP the working log at the parent base. It is
			// the durable, deterministic source a later re-attribution re-flips from —
			// an amend/rebase of THIS commit, or a divergent sibling commit on the same
			// parent that re-adds the same content. Deleting it here (the behavior added
			// in 300fdf81) forced re-attribution to fall back to SQLite content-sha
			// recovery, which SILENTLY loses AI attribution when the source edit is
			// outside the commit's time window (divergent history / watermark) — the
			// note then ships all-Human with no way back. Disk growth from retaining
			// these is bounded by GCWorkingLogs pruning bases deep behind HEAD.
			continue
		}
		oldWL := WorkingLogPath(repoRoot, branch, oldBase, rel)
		oldBaseline := BaselinePath(repoRoot, branch, oldBase, rel)
		newWL := WorkingLogPath(repoRoot, branch, newBase, rel)
		if _, statErr := os.Stat(newWL); statErr == nil {
			// A fresher log already exists at the new base — drop the stale one.
			_ = os.Remove(oldWL)
			_ = os.Remove(oldBaseline)
			continue
		}
		wl.BaseSHA = newBase
		data, merr := json.MarshalIndent(wl, "", "  ")
		if merr != nil {
			continue
		}
		if atomicWrite(newWL, data) != nil {
			continue
		}
		if content, ok := loadBaseline(oldBaseline); ok {
			_ = atomicWrite(BaselinePath(repoRoot, branch, newBase, rel), []byte(content))
		}
		_ = os.Remove(oldWL)
		_ = os.Remove(oldBaseline)
	}
	// Drop the old base dir only if it's now empty (every log migrated forward and
	// no committed-file logs retained). A dir holding retained committed-file logs is
	// kept for re-attribution; GCWorkingLogs prunes it once it's deep behind HEAD.
	if remaining, rerr := ListWorkingLogs(repoRoot, branch, oldBase); rerr == nil && len(remaining) == 0 {
		_ = os.RemoveAll(workingLogDir(repoRoot, branch, oldBase))
	}
	return nil
}

// AdoptWorkingLogsAtBase moves working logs left behind under a DIFFERENT branch
// name, at the same base commit, into (branch, baseSHA).
//
// The working log is keyed by (branch, base_sha), but branching does not touch
// the working tree:
//
//	git checkout -b feature      # HEAD unmoved, uncommitted edits unmoved
//
// leaves every log the agents and the editor wrote this session under
// working_logs/master/<sha>/, while everything from that moment on — including
// the commit-time flip — looks under working_logs/feature/<sha>/. The flip finds
// nothing, keeps each file's prior attribution, and the AI work the user just
// branched off ships in the note as Human. This is the single most common way a
// developer actually works (build on the default branch, branch when it's time to
// commit), so it was also the most common way attribution was lost.
//
// base_sha is what makes the adoption sound: it names the exact commit the logs
// were diffed against. A log under another branch at the SAME base describes the
// same working tree — the one that is about to be committed here. Logs at any
// other base are left untouched.
//
// A file already tracked under (branch, baseSHA) always wins: it was written after
// the branch switch and is fresher. Everything else — per-file logs, their
// baselines, and the shared .deletions.jsonl — is moved, not copied, so the next
// branch switch can't adopt the same records a second time.
//
// Best-effort: a file that fails to move is skipped, never fatal. Returns how many
// files were adopted.
func AdoptWorkingLogsAtBase(repoRoot, branch, baseSHA string) (adopted int) {
	if repoRoot == "" || branch == "" || baseSHA == "" {
		return 0
	}
	root := filepath.Join(gitutil.GitDir(repoRoot), "blamely", "working_logs")
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0
	}
	want := sanitizeComponent(branch)
	for _, e := range entries {
		if !e.IsDir() || e.Name() == want {
			continue
		}
		donor := filepath.Join(root, e.Name(), sanitizeComponent(baseSHA))
		if st, serr := os.Stat(donor); serr != nil || !st.IsDir() {
			continue
		}
		adopted += adoptFrom(repoRoot, branch, baseSHA, e.Name())
	}
	return adopted
}

// adoptFrom moves one donor branch dir's logs at baseSHA into (branch, baseSHA).
func adoptFrom(repoRoot, branch, baseSHA, donorBranch string) (adopted int) {
	logs, err := ListWorkingLogs(repoRoot, donorBranch, baseSHA)
	if err != nil {
		return 0
	}
	for _, wl := range logs {
		rel := wl.File
		if rel == "" {
			continue
		}
		dstLog := WorkingLogPath(repoRoot, branch, baseSHA, rel)
		if _, serr := os.Stat(dstLog); serr == nil {
			// Written after the branch switch — the fresher record wins. Drop the
			// donor copy so a later adoption doesn't reconsider it.
			_ = os.Remove(WorkingLogPath(repoRoot, donorBranch, baseSHA, rel))
			_ = os.Remove(BaselinePath(repoRoot, donorBranch, baseSHA, rel))
			continue
		}
		if !moveFile(WorkingLogPath(repoRoot, donorBranch, baseSHA, rel), dstLog) {
			continue
		}
		// The baseline is the content the log's line numbers describe; a log without
		// it re-seeds from scratch, so move it too (absence is not fatal).
		_ = moveFile(BaselinePath(repoRoot, donorBranch, baseSHA, rel),
			BaselinePath(repoRoot, branch, baseSHA, rel))
		adopted++
	}
	if adopted > 0 {
		adoptDeletions(repoRoot, branch, baseSHA, donorBranch)
	}
	// Leave a donor dir that still holds records (a file we skipped); drop an empty one.
	if rem, rerr := ListWorkingLogs(repoRoot, donorBranch, baseSHA); rerr == nil && len(rem) == 0 {
		_ = os.RemoveAll(workingLogDir(repoRoot, donorBranch, baseSHA))
	}
	return adopted
}

// adoptDeletions appends the donor's deletion records to this branch's log. It is a
// shared append-only JSONL, so it is concatenated rather than renamed — the target
// may already hold records written since the branch switch.
func adoptDeletions(repoRoot, branch, baseSHA, donorBranch string) {
	src := deletionsLogPath(repoRoot, donorBranch, baseSHA)
	data, err := os.ReadFile(src)
	if err != nil || len(data) == 0 {
		return
	}
	dst := deletionsLogPath(repoRoot, branch, baseSHA)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(dst, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	if data[len(data)-1] != '\n' {
		data = append(data, '\n')
	}
	_, werr := f.Write(data)
	f.Close()
	if werr == nil {
		_ = os.Remove(src)
	}
}

// moveFile renames src to dst, falling back to copy+remove when the rename crosses
// a device boundary. Reports whether dst now holds the content.
func moveFile(src, dst string) bool {
	if _, err := os.Stat(src); err != nil {
		return false
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return false
	}
	if err := os.Rename(src, dst); err == nil {
		return true
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return false
	}
	if err := atomicWrite(dst, data); err != nil {
		return false
	}
	_ = os.Remove(src)
	return true
}
