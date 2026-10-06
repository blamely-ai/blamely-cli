//go:build !windows

package procattr

import (
	"os/exec"
	"syscall"
)

// Hide is a no-op off Windows: only Windows gives a console-less parent's child
// a console window of its own. See procattr_windows.go for what this exists for.
func Hide(cmd *exec.Cmd) *exec.Cmd { return cmd }

// Detach starts cmd in a session of its own, so it outlives the caller and is
// not tied to the caller's terminal (no SIGHUP when it closes, no tty to prompt
// on). The caller should leave Stdin/Stdout/Stderr nil.
func Detach(cmd *exec.Cmd) *exec.Cmd {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
	return cmd
}
