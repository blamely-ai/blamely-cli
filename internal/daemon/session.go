package daemon

import (
	"database/sql"
	"sync"
	"time"

	"github.com/blamely/blamely/internal/gitutil"
	"github.com/blamely/blamely/internal/store"
)

// sessionResolver maps an edit to its branch-based work session. Git lookups
// (branch, default branch, merge-base) are cached briefly per repo so an edit
// burst from a chat watcher doesn't shell out to git on every event.
type sessionResolver struct {
	mu    sync.Mutex
	cache map[string]gitInfo // keyed by repo_path
}

type gitInfo struct {
	branch  string
	baseSha string
	at      time.Time
}

const sessionCacheTTL = 2 * time.Second

var sessions = &sessionResolver{cache: map[string]gitInfo{}}

// resolve fills e.Branch and e.SessionID in place. branchHint (from the editor
// payload, possibly "") wins over git resolution for the branch LABEL — the
// editor knows its own checked-out branch — while base_sha is the current HEAD
// commit (the tip uncommitted work builds on). After a commit, HEAD advances and
// the next edit opens a new session for that branch automatically.
func (sr *sessionResolver) resolve(db *store.DB, e *store.Edit, branchHint string) {
	sr.resolveIn(db, e, branchHint, "")
}

func (sr *sessionResolver) resolveIn(db *store.DB, e *store.Edit, branchHint, checkout string) {
	if e.RepoPath == "" {
		return
	}
	if checkout == "" {
		checkout = e.RepoPath
	}
	gi := sr.gitInfo(checkout)
	branch := branchHint
	if branch == "" {
		branch = gi.branch
	}
	e.Branch = branch
	if branch == "" {
		return // detached HEAD / not a repo → no session, edit stays unscoped
	}
	if id, err := db.ResolveSession(e.RepoPath, branch, gi.baseSha); err == nil && id != "" {
		e.SessionID = sql.NullString{Valid: true, String: id}
	}
}

// gitInfo returns the cached (or freshly computed) branch + base_sha for a repo.
// base_sha is the current HEAD SHA — one open work session per branch until commit.
//
// The value is recomputed synchronously once the cache is older than the short
// TTL. It must NOT be served stale: base_sha keys the edit's work session, and a
// stale base_sha (e.g. captured before a commit moved HEAD) keys the edit to the
// wrong session — so at commit time the session-scoped match misses and genuine
// AI additions fall back to Human. The TTL only coalesces a burst of edits within
// a couple seconds; after a commit, the next edit (>TTL later) recomputes fresh.
func (sr *sessionResolver) gitInfo(repo string) gitInfo {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	if gi, ok := sr.cache[repo]; ok && time.Since(gi.at) < sessionCacheTTL {
		return gi
	}
	branch := gitutil.BranchName(repo)
	baseSha := gitutil.HeadSHA(repo)
	gi := gitInfo{branch: branch, baseSha: baseSha, at: time.Now()}
	sr.cache[repo] = gi
	return gi
}
