// Package filelock provides an exclusive, process-scoped file lock (flock on
// Unix, LockFileEx on Windows). The OS releases it when its holder exits, so
// there is no stale lock to detect or take over.
package filelock
