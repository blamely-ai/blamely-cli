# Linked Git worktrees

Blamely keeps a shared repository identity (`git rev-parse --git-common-dir`) for database records and commit matching, but uses the active checkout for reading files and resolving branch/HEAD. CLI hooks and transcript watchers carry that checkout as `worktree_path` alongside `repo_path`.

Attribution working logs live in the checkout's **actual Git directory** (`git rev-parse --absolute-git-dir`), not a hard-coded `<checkout>/.git` directory. For a linked worktree this is normally `<main>/.git/worktrees/<name>/blamely/working_logs/`. Baselines, deletion logs, branch adoption, migration, and garbage collection use the same location. This also isolates detached sibling worktrees at the same commit.

The daemon resolves branch-based sessions and their base commit from `worktree_path`. File snapshots are keyed by checkout, preventing two branches editing the same relative file from sharing a before-image. Existing clients that omit `worktree_path` retain the previous `repo_path` behavior; editor integrations should send the new field to support linked checkouts safely.

No repository migration is needed: normal checkouts still store logs under `.git/blamely/working_logs/`. Run attribution/report commands from the checkout being committed. Git notes remain shared between worktrees, as Git intends.

Regression tests cover checkout-local storage and GC, detached-worktree isolation, HTTP/watcher branch and base-SHA sessions, isolated snapshots, and a real Codex pre-hook → post-hook → commit → note sequence with pre-existing human edits.
