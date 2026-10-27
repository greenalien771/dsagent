//go:build !windows

package main

import (
	"context"
	"os/exec"
)

// shellCommand builds the command that runs one shell string in the workspace.
//
// POSIX sh, one argument: no shell dialect guessing, and no SysProcAttr (that
// struct has different fields on every platform, so the Windows-only CmdLine
// form lives in shell_windows.go and this file never names it).
func shellCommand(ctx context.Context, command string) *exec.Cmd {
	return exec.CommandContext(ctx, "sh", "-c", command)
}
