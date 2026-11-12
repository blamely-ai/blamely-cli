package gitnotes

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// issueKeyHook stands in for a server-side commit-message rule (e.g. Bitbucket
// requiring a Jira key): it rejects any pushed commit — notes commits included —
// whose message lacks PROJ-<n>. It is test scaffolding only — the product
// never looks at keys. Every push touching the notes ref is counted in
// notes-pushes.
const issueKeyHook = `#!/bin/sh
zero=0000000000000000000000000000000000000000
input=$(cat)
case "$input" in *refs/notes/blamely*) echo push >> notes-pushes ;; esac
printf '%s\n' "$input" | while read old new ref; do
    [ -z "$ref" ] && continue
    [ "$new" = "$zero" ] && continue
    if [ "$old" = "$zero" ]; then range="$new"; else range="$old..$new"; fi
    for c in $(git rev-list $range); do
        if ! git log -1 --format=%B "$c" | grep -qE 'PROJ-[0-9]+'; then
            echo "commit $c on $ref has no Jira issue key" >&2
            exit 1
        fi
    done
done
`

type syncFixture struct {
	t      *testing.T
	remote string
}

func newSyncFixture(t *testing.T, withRule bool) *syncFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	for _, kv := range [][2]string{
		{"GIT_AUTHOR_NAME", "t"}, {"GIT_AUTHOR_EMAIL", "t@t"},
		{"GIT_COMMITTER_NAME", "t"}, {"GIT_COMMITTER_EMAIL", "t@t"},
	} {
		t.Setenv(kv[0], kv[1])
	}
	f := &syncFixture{t: t, remote: filepath.Join(t.TempDir(), "remote.git")}
	runIn(t, "", "init", "-q", "--bare", f.remote)
	// No automatic gc. A push of many notes commits arrives as many packs, and
	// receive-pack then starts a DETACHED `git gc --auto` that can still be
	// writing objects/pack when the test returns, so TempDir's cleanup fails
	// with "directory not empty" (seen on ubuntu CI in
	// TestSyncNotes_StuckHistoryWithFanout).
	runIn(t, f.remote, "config", "gc.auto", "0")
	runIn(t, f.remote, "config", "receive.autogc", "false")
	if withRule {
		f.enableRule()
	}
	return f
}

func (f *syncFixture) enableRule() {
	hooks := filepath.Join(f.remote, "hooks")
	if err := os.WriteFile(filepath.Join(hooks, "pre-receive"), []byte(issueKeyHook), 0o755); err != nil {
		f.t.Fatal(err)
	}
	// A repo-local core.hooksPath beats a global one (e.g. Blamely's own).
	runIn(f.t, f.remote, "config", "core.hooksPath", filepath.ToSlash(hooks))
}

// clone makes a working clone of the remote with the remote's notes fetched.
func (f *syncFixture) clone() string {
	f.t.Helper()
	dir := filepath.Join(f.t.TempDir(), "clone")
	runIn(f.t, "", "clone", "-q", f.remote, dir)
	runIn(f.t, dir, "config", "core.hooksPath", "")
	runIn(f.t, dir, "config", "gc.auto", "0") // see newSyncFixture
	runIn(f.t, dir, "fetch", "-q", "origin", "+refs/notes/*:refs/notes/*")
	return dir
}

// notesPushes is how many pushes touching the notes ref the rule has seen.
func (f *syncFixture) notesPushes() int {
	data, _ := os.ReadFile(filepath.Join(f.remote, "notes-pushes"))
	return strings.Count(string(data), "push")
}

// commitWithNote makes a commit with msg and attaches note to it the way
// writeNote does, returning the commit's sha.
func commitWithNote(t *testing.T, repo, file, msg, note string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, file), []byte(msg+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runIn(t, repo, "add", file)
	runIn(t, repo, "commit", "-q", "-m", msg)
	sha := runIn(t, repo, "rev-parse", "HEAD")
	if err := writeNote(repo, sha, []byte(note)); err != nil {
		t.Fatal(err)
	}
	return sha
}

func runIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	if dir != "" {
		args = append([]string{"-C", dir}, args...)
	}
	out, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func syncOK(t *testing.T, repo string) {
	t.Helper()
	if err := SyncNotes(repo, "origin", "", nil); err != nil {
		t.Fatalf("SyncNotes: %v", err)
	}
}

func pushCode(t *testing.T, repo string) {
	t.Helper()
	runIn(t, repo, "push", "-q", "origin", "HEAD")
}

func noteIn(t *testing.T, dir, sha string) (string, bool) {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "notes", "--ref="+NotesRef, "show", sha).Output()
	return strings.TrimSpace(string(out)), err == nil
}

func remoteNote(t *testing.T, f *syncFixture, sha string) string {
	t.Helper()
	n, ok := noteIn(t, f.remote, sha)
	if !ok {
		t.Fatalf("remote has no note for %s", sha[:7])
	}
	return n
}

type notesCommit struct {
	sha, msg string
	parents  int
	changed  []string // annotated commits whose note this commit adds, changes or deletes
}

// remoteNotesHistory lists the remote's notes commits, newest first, with the
// annotated commits each one changes.
func remoteNotesHistory(t *testing.T, f *syncFixture) []notesCommit {
	t.Helper()
	var out []notesCommit
	for _, sha := range strings.Fields(runIn(t, f.remote, "rev-list", NotesRef)) {
		parents := strings.Fields(runIn(t, f.remote, "rev-list", "--no-walk", "--parents", sha))[1:]
		c := notesCommit{sha: sha, msg: runIn(t, f.remote, "log", "-1", "--format=%B", sha), parents: len(parents)}
		cur, err := notesMap(f.remote, sha)
		if err != nil {
			t.Fatal(err)
		}
		prev := map[string]string{}
		if len(parents) > 0 {
			if prev, err = notesMap(f.remote, parents[0]); err != nil {
				t.Fatal(err)
			}
		}
		for k, v := range cur {
			if prev[k] != v {
				c.changed = append(c.changed, k)
			}
		}
		for k := range prev {
			if _, ok := cur[k]; !ok {
				c.changed = append(c.changed, k)
			}
		}
		sort.Strings(c.changed)
		out = append(out, c)
	}
	return out
}

// assertOnePerCommit checks the remote history's newest len(want) commits: each
// changes exactly one note, for the expected commit, and its message is that
// commit's message followed by the marker. want is oldest first.
func assertOnePerCommit(t *testing.T, f *syncFixture, repo string, want []string) {
	t.Helper()
	hist := remoteNotesHistory(t, f)
	if len(hist) < len(want) {
		t.Fatalf("remote notes history has %d commits, want at least %d", len(hist), len(want))
	}
	for i, target := range want {
		c := hist[len(want)-1-i]
		if len(c.changed) != 1 || c.changed[0] != target {
			t.Errorf("notes commit %s should change only %s's note, changes %v", c.sha[:7], target[:7], c.changed)
		}
		if c.parents > 1 {
			t.Errorf("notes commit %s is a merge", c.sha[:7])
		}
		if msg := runIn(t, repo, "log", "-1", "--format=%B", target); c.msg != msg+"\n\n"+notesSyncMarker {
			t.Errorf("notes commit %s message:\n%q\nwant the commit's own message plus the marker:\n%q", c.sha[:7], c.msg, msg+"\n\n"+notesSyncMarker)
		}
	}
}

// The customer case: a server rejecting commits without an issue key used to
// refuse the stock "Notes added by 'git notes add'" commits. Each note must now
// go out in a notes commit of its own, carrying its commit's message.
func TestSyncNotes_OneNotesCommitPerCommit(t *testing.T) {
	f := newSyncFixture(t, true)
	repo := f.clone()
	a := commitWithNote(t, repo, "a.txt", "PROJ-12 login fix", `{"n":"a"}`)
	b := commitWithNote(t, repo, "b.txt", "PROJ-13 validation\n\nbody line", `{"n":"b"}`)
	c := commitWithNote(t, repo, "c.txt", "PROJ-14 tests", `{"n":"c"}`)
	pushCode(t, repo)

	// Sanity: the plain notes push is what the rule rejects.
	if out, err := exec.Command("git", "-C", repo, "push", "-q", "origin", NotesRef).CombinedOutput(); err == nil {
		t.Fatalf("setup: stock notes commits should be rejected, pushed fine:\n%s", out)
	}

	syncOK(t, repo)
	for sha, want := range map[string]string{a: `{"n":"a"}`, b: `{"n":"b"}`, c: `{"n":"c"}`} {
		if got := remoteNote(t, f, sha); got != want {
			t.Errorf("note for %s = %q, want %q", sha[:7], got, want)
		}
	}
	if hist := remoteNotesHistory(t, f); len(hist) != 3 {
		t.Fatalf("want exactly 3 notes commits, got %d", len(hist))
	}
	assertOnePerCommit(t, f, repo, []string{a, b, c})
	if revParse(repo, NotesRef) != runIn(t, f.remote, "rev-parse", NotesRef) {
		t.Error("with nothing pending, the local notes ref should equal the remote's")
	}

	// Nothing new → nothing to do.
	before := runIn(t, f.remote, "rev-parse", NotesRef)
	syncOK(t, repo)
	if after := runIn(t, f.remote, "rev-parse", NotesRef); after != before {
		t.Error("a no-op sync moved the remote notes ref")
	}
}

// A note on a commit the server does not have stays local — until the commit
// itself is pushed, when the note follows in its own notes commit.
func TestSyncNotes_NoteWaitsForItsCommit(t *testing.T) {
	f := newSyncFixture(t, true)
	repo := f.clone()
	pushed := commitWithNote(t, repo, "a.txt", "PROJ-1 shipped", `{"n":"a"}`)
	pushCode(t, repo)
	runIn(t, repo, "checkout", "-q", "-b", "wip")
	wip := commitWithNote(t, repo, "w.txt", "PROJ-9 in progress", `{"n":"w"}`)

	syncOK(t, repo)
	remoteNote(t, f, pushed)
	if _, ok := noteIn(t, f.remote, wip); ok {
		t.Error("the note of an unpushed commit must not be published")
	}
	if n, ok := noteIn(t, repo, wip); !ok || n != `{"n":"w"}` {
		t.Errorf("the pending note must stay in the local notes ref, got %q (%v)", n, ok)
	}

	runIn(t, repo, "push", "-q", "origin", "wip")
	syncOK(t, repo)
	if got := remoteNote(t, f, wip); got != `{"n":"w"}` {
		t.Errorf("once its commit is pushed the note should follow, got %q", got)
	}
	assertOnePerCommit(t, f, repo, []string{pushed, wip})
	if revParse(repo, NotesRef) != runIn(t, f.remote, "rev-parse", NotesRef) {
		t.Error("with nothing pending any more, the local notes ref should equal the remote's")
	}
}

// Before the hook's own push lands, only its stdin tells which commits it sends.
func TestSyncNotes_PublishesNotesOfCommitsInThisPush(t *testing.T) {
	f := newSyncFixture(t, true)
	repo := f.clone()
	sha := commitWithNote(t, repo, "a.txt", "PROJ-3 in this push", `{"n":"a"}`)
	if err := SyncNotes(repo, "origin", "", []string{sha}); err != nil {
		t.Fatalf("SyncNotes: %v", err)
	}
	if got := remoteNote(t, f, sha); got != `{"n":"a"}` {
		t.Errorf("note = %q", got)
	}
}

// git copies a note onto an amended commit and leaves the original's behind:
// the amended commit's note is published, the orphan's never is.
func TestSyncNotes_AmendCopy(t *testing.T) {
	f := newSyncFixture(t, true)
	repo := f.clone()
	runIn(t, repo, "config", "notes.rewriteRef", NotesRef)
	orig := commitWithNote(t, repo, "a.txt", "PROJ-2 second", `{"n":"b"}`)
	runIn(t, repo, "commit", "-q", "--amend", "-m", "PROJ-2 second, amended")
	amended := runIn(t, repo, "rev-parse", "HEAD")
	pushCode(t, repo)

	syncOK(t, repo)
	if got := remoteNote(t, f, amended); got != `{"n":"b"}` {
		t.Errorf("amended commit's note = %q", got)
	}
	if _, ok := noteIn(t, f.remote, orig); ok {
		t.Error("the pre-amend original was never pushed; its note must not be")
	}
	assertOnePerCommit(t, f, repo, []string{amended})
}

// Two clones noting different commits: the second sync builds on the first's
// notes. The remote history stays linear — no "Merged notes from" commit — and
// no scratch ref is left anywhere.
func TestSyncNotes_DivergedTeammate(t *testing.T) {
	f := newSyncFixture(t, true)
	seed := f.clone()
	s := commitWithNote(t, seed, "seed.txt", "PROJ-1 seed", `{"n":"seed"}`)
	pushCode(t, seed)
	syncOK(t, seed)

	alice, bob := f.clone(), f.clone()
	a := commitWithNote(t, alice, "alice.txt", "PROJ-10 alice", `{"n":"alice"}`)
	pushCode(t, alice)
	runIn(t, bob, "pull", "-q", "--rebase", "origin", "HEAD")
	b := commitWithNote(t, bob, "bob.txt", "PROJ-20 bob", `{"n":"bob"}`)
	pushCode(t, bob)
	syncOK(t, alice)
	syncOK(t, bob)

	for sha, want := range map[string]string{s: `{"n":"seed"}`, a: `{"n":"alice"}`, b: `{"n":"bob"}`} {
		if got := remoteNote(t, f, sha); got != want {
			t.Errorf("note for %s = %q, want %q", sha[:7], got, want)
		}
	}
	if hist := remoteNotesHistory(t, f); len(hist) != 3 {
		t.Errorf("want 3 notes commits (seed, alice, bob), got %d", len(hist))
	}
	assertOnePerCommit(t, f, bob, []string{s, a, b})
	if n, ok := noteIn(t, bob, a); !ok || n != `{"n":"alice"}` {
		t.Error("bob's local notes should now include alice's")
	}
	if refs := runIn(t, bob, "for-each-ref", "--format=%(refname)", syncScratchBase); refs != "" {
		t.Errorf("scratch refs left behind:\n%s", refs)
	}
	if refs := runIn(t, f.remote, "for-each-ref", "--format=%(refname)", "refs/notes/"); refs != NotesRef {
		t.Errorf("remote should hold only %s, got:\n%s", NotesRef, refs)
	}

	// Alice is now behind: her sync just fast-forwards locally, pushing nothing.
	remoteTip := runIn(t, f.remote, "rev-parse", NotesRef)
	syncOK(t, alice)
	if revParse(alice, NotesRef) != remoteTip || runIn(t, f.remote, "rev-parse", NotesRef) != remoteTip {
		t.Error("a sync behind the remote should adopt the remote tip and push nothing")
	}
}

// A clone whose notes piled up under the old hook — one stock notes commit per
// note, enough of them that the notes tree fans out — publishes every note on
// its first push, one notes commit each, with every note intact.
func TestSyncNotes_StuckHistoryWithFanout(t *testing.T) {
	f := newSyncFixture(t, true)
	repo := f.clone()
	const n = 300
	var stream strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&stream, "commit refs/heads/main\nmark :%d\ncommitter t <t@t> %d +0000\ndata <<EOM\nPROJ-%d change %d\nEOM\n", i, 1767225600+i, i, i)
		if i > 1 {
			fmt.Fprintf(&stream, "from :%d\n", i-1)
		}
		fmt.Fprintf(&stream, "M 644 inline f\ndata <<EOM\n%d\nEOM\n\n", i)
	}
	for i := 1; i <= n; i++ { // the old hook: `git notes add` per commit
		fmt.Fprintf(&stream, "commit %s\ncommitter t <t@t> %d +0000\ndata <<EOM\n%s\nEOM\nN inline :%d\ndata <<EOM\n{\"n\":%d}\nEOM\n\n", NotesRef, 1767300000+i, notesSyncMarker, i, i)
	}
	cmd := exec.Command("git", "-C", repo, "fast-import", "--quiet")
	cmd.Stdin = strings.NewReader(stream.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fast-import: %v\n%s", err, out)
	}
	runIn(t, repo, "checkout", "-q", "main")
	pushCode(t, repo)
	if !strings.Contains(runIn(t, repo, "ls-tree", NotesRef), "\t") || len(strings.Fields(runIn(t, repo, "ls-tree", "--name-only", NotesRef))) >= n {
		t.Fatal("setup: the notes tree should fan out")
	}

	syncOK(t, repo)
	local, _ := notesMap(repo, revParse(repo, NotesRef))
	remote, _ := notesMap(f.remote, runIn(t, f.remote, "rev-parse", NotesRef))
	if len(remote) != n || fmt.Sprint(local) != fmt.Sprint(remote) {
		t.Fatalf("remote has %d notes, want the same %d as local", len(remote), len(local))
	}
	// One notes commit per note, each with its commit's message — checked in
	// bulk, since walking 300 commits one git call at a time is slow.
	shas := strings.Fields(runIn(t, f.remote, "rev-list", NotesRef))
	if len(shas) != n {
		t.Fatalf("want %d notes commits, got %d", n, len(shas))
	}
	msgs := strings.Split(strings.TrimSuffix(runIn(t, f.remote, "log", "--format=%B%x00", NotesRef), "\x00"), "\x00")
	for i, m := range msgs {
		want := fmt.Sprintf("PROJ-%d change %d\n\n%s", n-i, n-i, notesSyncMarker) // newest first
		if strings.TrimSpace(m) != want {
			t.Fatalf("notes commit %d message %q, want %q", i, strings.TrimSpace(m), want)
		}
	}
	for _, i := range []int{0, 1, n / 2, n - 2} { // and each changes exactly one note
		cur, _ := notesMap(f.remote, shas[i])
		prev, _ := notesMap(f.remote, shas[i+1])
		if len(cur) != len(prev)+1 {
			t.Errorf("notes commit %s should add exactly one note (%d -> %d)", shas[i][:7], len(prev), len(cur))
		}
	}
}

// With `+refs/notes/*:refs/notes/*` in remote.origin.fetch, a plain fetch also
// force-updates the local notes ref. The sync's own fetch must not: that
// silently dropped every note not pushed yet.
func TestSyncNotes_ConfiguredNotesFetchRefspecKeepsLocalNotes(t *testing.T) {
	f := newSyncFixture(t, true)
	alice, bob := f.clone(), f.clone()
	runIn(t, bob, "config", "--add", "remote.origin.fetch", "+refs/notes/*:refs/notes/*")
	a := commitWithNote(t, alice, "alice.txt", "PROJ-10 alice", `{"n":"alice"}`)
	pushCode(t, alice)
	runIn(t, bob, "pull", "-q", "--rebase", "origin", "HEAD")
	b1 := commitWithNote(t, bob, "b1.txt", "PROJ-21 bob one", `{"n":"b1"}`)
	b2 := commitWithNote(t, bob, "b2.txt", "PROJ-22 bob two", `{"n":"b2"}`)
	pushCode(t, bob)
	syncOK(t, alice)
	// By remote name, where git applies the remote's configured refspecs (by
	// URL, as the hook calls it, they never apply).
	if err := SyncNotes(bob, "origin", "origin", nil); err != nil {
		t.Fatalf("SyncNotes: %v", err)
	}

	for sha, want := range map[string]string{a: `{"n":"alice"}`, b1: `{"n":"b1"}`, b2: `{"n":"b2"}`} {
		if got := remoteNote(t, f, sha); got != want {
			t.Errorf("remote note for %s = %q, want %q", sha[:7], got, want)
		}
	}
}

// git before 2.29 does not know --no-write-fetch-head. The sync must then
// fetch without it and put the user's FETCH_HEAD back — or remove the one it
// wrote if the user had none — instead of failing every push.
func TestSyncNotes_GitWithoutNoWriteFetchHead(t *testing.T) {
	check := func(t *testing.T, flag string) {
		saved := noWriteFetchHeadFlag
		noWriteFetchHeadFlag = flag
		t.Cleanup(func() { noWriteFetchHeadFlag = saved })

		f := newSyncFixture(t, true)
		alice, bob := f.clone(), f.clone()
		fetchHead := filepath.Join(bob, ".git", "FETCH_HEAD")
		const users = "0123456789012345678901234567890123456789\t\tbranch 'topic' of elsewhere\n"
		if err := os.WriteFile(fetchHead, []byte(users), 0o644); err != nil {
			t.Fatal(err)
		}

		// Remote has no notes yet: the fetch finds no ref, the push creates it.
		b1 := commitWithNote(t, bob, "b1.txt", "PROJ-21 bob one", `{"n":"b1"}`)
		pushCode(t, bob)
		syncOK(t, bob)
		// Remote now ahead of alice and bob diverged: fetch, rebuild, push.
		runIn(t, alice, "pull", "-q", "--rebase", "origin", "HEAD")
		a := commitWithNote(t, alice, "alice.txt", "PROJ-10 alice", `{"n":"alice"}`)
		pushCode(t, alice)
		syncOK(t, alice)
		if got, _ := os.ReadFile(fetchHead); string(got) != users {
			t.Fatalf("setup: bob's FETCH_HEAD changed before his sync:\n%s", got)
		}
		// bob pulls with an explicit refspec that does not touch FETCH_HEAD's
		// meaning for this test; rewrite it afterwards to the user's content.
		runIn(t, bob, "pull", "-q", "--rebase", "origin", "HEAD")
		if err := os.WriteFile(fetchHead, []byte(users), 0o644); err != nil {
			t.Fatal(err)
		}
		b2 := commitWithNote(t, bob, "b2.txt", "PROJ-22 bob two", `{"n":"b2"}`)
		pushCode(t, bob)
		syncOK(t, bob)

		for sha, want := range map[string]string{a: `{"n":"alice"}`, b1: `{"n":"b1"}`, b2: `{"n":"b2"}`} {
			if got := remoteNote(t, f, sha); got != want {
				t.Errorf("remote note for %s = %q, want %q", sha[:7], got, want)
			}
		}
		if got, _ := os.ReadFile(fetchHead); string(got) != users {
			t.Errorf("the user's FETCH_HEAD was changed:\n%s", got)
		}

		// A user without a FETCH_HEAD must not be left with one.
		os.Remove(fetchHead)
		b3 := commitWithNote(t, bob, "b3.txt", "PROJ-23 bob three", `{"n":"b3"}`)
		pushCode(t, bob)
		syncOK(t, bob)
		remoteNote(t, f, b3)
		if _, err := os.Stat(fetchHead); !os.IsNotExist(err) {
			t.Errorf("sync left a FETCH_HEAD behind (err=%v)", err)
		}
	}
	t.Run("current git", func(t *testing.T) { check(t, noWriteFetchHeadFlag) })
	// A flag no git knows, standing in for an old git's "unknown option".
	t.Run("old git", func(t *testing.T) { check(t, "--blamely-test-unknown-flag") })
}

// Removing a published commit's note publishes the removal, in a notes commit
// carrying that commit's message.
func TestSyncNotes_DeletedNote(t *testing.T) {
	f := newSyncFixture(t, true)
	repo := f.clone()
	a := commitWithNote(t, repo, "a.txt", "PROJ-1 first", `{"n":"a"}`)
	b := commitWithNote(t, repo, "b.txt", "PROJ-2 second", `{"n":"b"}`)
	pushCode(t, repo)
	syncOK(t, repo)

	runIn(t, repo, "notes", "--ref="+NotesRef, "remove", a)
	syncOK(t, repo)
	if _, ok := noteIn(t, f.remote, a); ok {
		t.Error("the deleted note should be gone from the remote")
	}
	remoteNote(t, f, b)
	assertOnePerCommit(t, f, repo, []string{a})
}

// A rule rejection is final: retrying only doubles the round trips (and any
// credential prompts) inside the user's push. The error carries the reason.
func TestSyncNotes_ServerRejectionIsReportedNotRetried(t *testing.T) {
	f := newSyncFixture(t, false)
	repo := f.clone()
	commitWithNote(t, repo, "a.txt", "no key here", `{"n":"a"}`)
	pushCode(t, repo) // before the rule existed
	f.enableRule()

	err := SyncNotes(repo, "origin", "", nil)
	if err == nil {
		t.Fatal("expected the rule to reject a note for a commit without a key")
	}
	if !strings.Contains(err.Error(), "no Jira issue key") {
		t.Errorf("error should carry the server's reason, got: %v", err)
	}
	if n := f.notesPushes(); n != 1 {
		t.Errorf("notes pushes = %d, want 1 (a rejection must not be retried)", n)
	}
}

func TestSyncNotes_FromBareRepository(t *testing.T) {
	f := newSyncFixture(t, true)
	work := f.clone()
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runIn(t, work, "add", "a.txt")
	runIn(t, work, "commit", "-q", "-m", "PROJ-3 from bare")
	pushCode(t, work)
	sha := runIn(t, work, "rev-parse", "HEAD")

	bare := filepath.Join(t.TempDir(), "bare.git")
	runIn(t, "", "clone", "-q", "--bare", f.remote, bare)
	runIn(t, bare, "config", "core.hooksPath", "")
	if err := writeNote(bare, sha, []byte(`{"n":"bare"}`)); err != nil {
		t.Fatal(err)
	}
	// A bare clone has no refs/remotes/*: the commit is known through the tip.
	if err := SyncNotes(bare, "origin", "", []string{sha}); err != nil {
		t.Fatalf("SyncNotes: %v", err)
	}
	if got := remoteNote(t, f, sha); got != `{"n":"bare"}` {
		t.Errorf("note = %q", got)
	}
}

// A read mirror as fetch URL and the primary as push URL: fetching the notes
// tip from the mirror would build on a stale tip and every push would bounce.
func TestSyncNotes_PushURLDiffersFromFetchURL(t *testing.T) {
	primary := newSyncFixture(t, true)
	mirror := filepath.Join(t.TempDir(), "mirror.git")
	repo := primary.clone()
	commitWithNote(t, repo, "a.txt", "PROJ-1 first", `{"n":"a"}`)
	pushCode(t, repo)
	syncOK(t, repo)
	runIn(t, "", "clone", "-q", "--mirror", primary.remote, mirror) // the mirror lags from here on

	runIn(t, repo, "remote", "set-url", "origin", mirror)
	runIn(t, repo, "remote", "set-url", "--push", "origin", primary.remote)
	b := commitWithNote(t, repo, "b.txt", "PROJ-2 second", `{"n":"b"}`)
	pushCode(t, repo) // goes to the primary
	syncOK(t, repo)
	c := commitWithNote(t, repo, "c.txt", "PROJ-3 third", `{"n":"c"}`)
	pushCode(t, repo)
	syncOK(t, repo) // would build on the mirror's stale tip if it fetched there
	remoteNote(t, primary, b)
	remoteNote(t, primary, c)
}

// remote.<name>.tagOpt=--tags and a local tag that differs from the remote's
// made the notes fetch fail as a whole ("would clobber existing tag").
func TestSyncNotes_TagOptDoesNotBreakFetch(t *testing.T) {
	f := newSyncFixture(t, true)
	alice, bob := f.clone(), f.clone()
	commitWithNote(t, alice, "a.txt", "PROJ-1 first", `{"n":"a"}`)
	runIn(t, alice, "tag", "nightly")
	runIn(t, alice, "push", "-q", "origin", "HEAD", "nightly")
	syncOK(t, alice)

	runIn(t, bob, "pull", "-q", "--rebase", "origin", "HEAD")
	runIn(t, bob, "config", "remote.origin.tagOpt", "--tags")
	b := commitWithNote(t, bob, "b.txt", "PROJ-2 second", `{"n":"b"}`)
	runIn(t, bob, "tag", "-f", "nightly") // moved locally
	pushCode(t, bob)
	// By remote name: tagOpt is a per-remote setting, so only then does it apply.
	if err := SyncNotes(bob, "origin", "origin", nil); err != nil {
		t.Fatalf("SyncNotes: %v", err)
	}
	remoteNote(t, f, b)
}

func TestSyncNotes_NoNotesIsNoop(t *testing.T) {
	f := newSyncFixture(t, false)
	repo := f.clone()
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runIn(t, repo, "add", "a.txt")
	runIn(t, repo, "commit", "-q", "-m", "plain")
	syncOK(t, repo)
	if revParse(f.remote, NotesRef) != "" {
		t.Error("remote should have no notes ref")
	}
}

// The sync matches a few of git's messages, so it must run git in English
// whatever the user's locale — a translated git defeated every such check.
func TestRunGitPinsEnglishMessages(t *testing.T) {
	newSyncFixture(t, false)
	repo := t.TempDir()
	runIn(t, "", "init", "-q", repo)
	t.Setenv("LC_ALL", "tr_TR.UTF-8")
	t.Setenv("LANGUAGE", "tr")
	out, err := runGit(networkTimeout, repo, nil, nil, "-c", `alias.env=!printf '%s|%s' "$LC_ALL" "$LANGUAGE"`, "env")
	if err != nil {
		t.Fatal(err)
	}
	if out != "C|" {
		t.Errorf("git ran with LC_ALL|LANGUAGE = %q, want \"C|\"", out)
	}
}

func TestReadCommits(t *testing.T) {
	f := newSyncFixture(t, false)
	repo := f.clone()
	t.Setenv("GIT_COMMITTER_DATE", "2026-01-02T03:04:05Z")
	sha := commitWithNote(t, repo, "a.txt", "PROJ-4 subject\n\nbody", "{}")
	tree := runIn(t, repo, "rev-parse", "HEAD^{tree}")
	got := readCommits(repo, []string{sha, tree, strings.Repeat("0", 40)})
	if len(got) != 1 || got[sha].msg != "PROJ-4 subject\n\nbody" || got[sha].time != 1767323045 {
		t.Errorf("got %+v", got)
	}
}

// log.showSignature=true prints signature checks into `git log` output; the
// messages must be read in a way it cannot touch.
func TestReadCommits_SignedWithShowSignature(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not available")
	}
	f := newSyncFixture(t, false)
	repo := f.clone()
	key := filepath.Join(t.TempDir(), "key")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
		t.Skipf("ssh-keygen: %v\n%s", err, out)
	}
	pub, _ := os.ReadFile(key + ".pub")
	allowed := filepath.Join(t.TempDir(), "allowed")
	os.WriteFile(allowed, []byte("t@t "+string(pub)), 0o644)
	for k, v := range map[string]string{"gpg.format": "ssh", "user.signingkey": key, "commit.gpgSign": "true",
		"gpg.ssh.allowedSignersFile": allowed, "log.showSignature": "true"} {
		runIn(t, repo, "config", k, v)
	}
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o644)
	runIn(t, repo, "add", "a.txt")
	if out, err := exec.Command("git", "-C", repo, "commit", "-q", "-m", "PROJ-8 signed").CombinedOutput(); err != nil {
		t.Skipf("setup: this git cannot sign with ssh (needs 2.34+): %v\n%s", err, out)
	}
	sha := runIn(t, repo, "rev-parse", "HEAD")
	if !strings.Contains(runIn(t, repo, "log", "-1", "--format=%s", sha), "signature") {
		t.Skip("setup: git did not print a signature check (old git?)")
	}
	if got := readCommits(repo, []string{sha}); got[sha].msg != "PROJ-8 signed" {
		t.Errorf("msg = %q", got[sha].msg)
	}
}

func TestParsePrePushStdin(t *testing.T) {
	a, b, z := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("0", 40)
	in := strings.Join([]string{
		"refs/heads/main " + a + " refs/heads/main " + b,
		"(delete) " + z + " refs/heads/gone " + b, // deletion: no tip
		"garbage line",
		"refs/heads/x " + b + " refs/heads/x " + z,
	}, "\n")
	info := ParsePrePushStdin(strings.NewReader(in))
	if strings.Join(info.Tips, ",") != a+","+b || info.PushesNotesRef {
		t.Errorf("got %+v", info)
	}
	info = ParsePrePushStdin(strings.NewReader(NotesRef + " " + a + " " + NotesRef + " " + z + "\n"))
	if !info.PushesNotesRef {
		t.Error("pushing refs/notes/blamely by hand should be detected")
	}
}
