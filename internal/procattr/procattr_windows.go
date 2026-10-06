//go:build windows

// Package procattr keeps subprocesses from flashing a console window on
// Windows.
//
// A process launched with CREATE_NO_WINDOW — which is how the daemon is started
// (see install.startDaemonNow) — has no console of its own. When such a process
// runs a CONSOLE program, Windows allocates a brand new console for the child
// and shows it: a cmd window that pops up and vanishes. The daemon shells out to
// git constantly (repo id, toplevel, branch, rev-parse on every hook event and
// watcher signal), so the user sees a window flash over and over; uninstall adds
// its own from taskkill, schtasks, tasklist and powershell.
//
// Passing CREATE_NO_WINDOW down to the child suppresses that console. It does
// not affect output: stdout/stderr still go to whatever pipes the caller set up,
// which is how every one of these call sites reads its result.
package procattr

import (
	"os/exec"
	"syscall"
)

// createNoWindow is CREATE_NO_WINDOW. Spelled out rather than imported so this
// file needs nothing beyond syscall.
const createNoWindow = 0x08000000

// Hide marks cmd so the child gets no console window, and returns cmd so it can
// wrap an exec.Command call inline. Any CreationFlags the caller already set are
// preserved.
func Hide(cmd *exec.Cmd) *exec.Cmd {
	if cmd == nil {
		return cmd
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= createNoWindow
	return cmd
}

// createNewConsole and createNewProcessGroup are CREATE_NEW_CONSOLE and
// CREATE_NEW_PROCESS_GROUP.
const (
	createNewConsole      = 0x00000010
	createNewProcessGroup = 0x00000200
)

// Detach starts cmd detached from the caller's console, so it outlives the
// caller and a Ctrl-C or a closed terminal there does not reach it.
//
// It gets a console of its own, created hidden, rather than none
// (CREATE_NO_WINDOW / DETACHED_PROCESS): a console-less process that runs a
// console program — git, and the helpers git starts for a push — gets that child
// a brand new, VISIBLE console. With a hidden console of its own, every
// descendant inherits it instead (see install.removeInstalledBinary).
func Detach(cmd *exec.Cmd) *exec.Cmd {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags &^= createNoWindow
	cmd.SysProcAttr.CreationFlags |= createNewConsole | createNewProcessGroup
	return cmd
}
