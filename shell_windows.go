//go:build windows

package main

import (
	"context"
	"os/exec"
	"syscall"
)

// shellCommand builds the command that runs one shell string in the workspace.
//
// Windows hands the line to cmd.exe verbatim: Go's usual syscall.EscapeArg
// rewrites the quoting the caller wrote, and cmd.exe wants its own /c string.
// This is the only place in the tree that may name SysProcAttr.CmdLine - the
// field does not exist on any other platform, and naming it there is a compile
// error, which is what broke the Linux and macOS CI jobs.
func shellCommand(ctx context.Context, command string) *exec.Cmd {
	c := exec.CommandContext(ctx, "cmd")
	c.SysProcAttr = &syscall.SysProcAttr{CmdLine: `/c ` + command}
	return c
}
