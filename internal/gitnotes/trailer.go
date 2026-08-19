package gitnotes

import (
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// Attribution from commit trailers.
//
// Every other path in blamely attributes code by OBSERVING the edit as it
// happens — a tool hook fires, or a watcher tails a session log. That requires
// the edit to occur on this machine. Devin Cloud breaks the assumption: the
// agent edits files inside a remote sandbox and the work reaches you as a
// finished branch, so there is no local edit to observe and every line would
// otherwise attribute to whoever merged it.
//
// What such a commit does carry is the agent's own claim of authorship, in a
// Co-Authored-By trailer:
//
//	Co-Authored-By: Devin <158243242+devin-ai-integration[bot]@users.noreply.github.com>
//
// That is weaker evidence than an observed edit — it is commit-scoped, so it
// cannot tell which LINES the agent wrote — but it is the only evidence that
// exists for remote work, and "all of it" is the right reading for a commit an
// agent authored end to end.

// devinCoAuthorRe matches Devin's Co-Authored-By trailer. It keys on the bot's
// account name rather than the display name or the numeric id prefix: the
// display name is user-configurable and the id varies by installation, while
// `devin-ai-integration` is the GitHub App's fixed slug.
var devinCoAuthorRe = regexp.MustCompile(`(?im)^\s*co-authored-by:.*devin-ai-integration`)

// toolFromCoAuthorTrailer returns the blamely tool id claimed by a commit
// message's Co-Authored-By trailers, or "" when none is recognised.
//
// Only Devin is matched today, deliberately. Claude Code and several other
// tools also write Co-Authored-By trailers, but their edits are already
// observed locally by a hook — reading their trailers too would re-attribute
// lines that the precise, line-level path has already handled, and would change
// long-standing behaviour for those tools. Add a tool here only when its work
// genuinely cannot be observed on this machine.
func toolFromCoAuthorTrailer(message string) string {
	if message == "" {
		return ""
	}
	if devinCoAuthorRe.MatchString(message) {
		return "devin"
	}
	return ""
}

// backfillFromCommitTrailer credits a commit's lines to the agent named in its
// Co-Authored-By trailer, for agents whose edits happen off this machine.
//
// It runs ONLY when the note has no AI lines at all. Any recorded edit — from a
// tool hook or a watcher — is line-level evidence and strictly better than a
// commit-scoped trailer, so the presence of even one means the observed path
// handled this commit and the trailer must not overwrite it. In practice the
// two never overlap: a commit is either produced locally (records exist) or
// pulled from a remote agent session (no records, trailer present).
//
// Deleted lines are credited too: a commit that removes code the agent removed
// is the agent's deletion, and leaving it as Human would misreport the split.
func backfillFromCommitTrailer(note *Note, message string) {
	if note == nil {
		return
	}
	// Some AI line already attributed — the observed path won, leave it alone.
	if note.Totals.AILines > 0 || note.Totals.AIDeletedLines > 0 {
		return
	}
	tool := toolFromCoAuthorTrailer(message)
	if tool == "" {
		return
	}
	// A remote agent session is a chat/agent surface, not a local CLI run.
	gen := "chat"
	for i := range note.Files {
		f := &note.Files[i]
		flipFileToAI(note, f, "add", tool, &gen)
		flipFileToAI(note, f, "delete", tool, &gen)
	}
}

// RevList returns the commit SHAs in revRange, newest first, capped at limit.
//
// Merge commits are excluded (--no-merges): a merge's own diff is the conflict
// resolution, which the person merging authored, not the agent whose branch is
// being merged in. The agent's actual work is in the commits the merge brings
// with it, and those are listed here in their own right.
func RevList(repoPath, revRange string, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, nil
	}
	out, err := exec.Command("git", "-C", repoPath, "rev-list", "--no-merges",
		"-n", strconv.Itoa(limit), revRange).Output()
	if err != nil {
		return nil, fmt.Errorf("rev-list %s: %w", revRange, err)
	}
	var shas []string
	for _, line := range strings.Split(string(out), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			shas = append(shas, s)
		}
	}
	return shas, nil
}

// CommitsWithAITrailer filters shas down to those whose commit message carries
// a recognised agent Co-Authored-By trailer. Used by the post-merge path so a
// `git pull` that brings in hundreds of ordinary commits only pays the cost of
// attributing the handful an agent actually authored.
func CommitsWithAITrailer(repoPath string, shas []string) []string {
	out := make([]string, 0, len(shas))
	for _, sha := range shas {
		if sha = strings.TrimSpace(sha); sha == "" {
			continue
		}
		if toolFromCoAuthorTrailer(CommitMessage(repoPath, sha)) != "" {
			out = append(out, sha)
		}
	}
	return out
}
