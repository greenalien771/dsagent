//go:build windows

package main

import "syscall"

// noConsoleSysProcAttr starts a console program without giving it - or taking -
// a console window.
//
// A stdio server is a console program, and launching one from a GUI process
// gives it its own console: every task that starts the servers would pop up
// black windows over whatever the user is doing. CREATE_NO_WINDOW (0x08000000)
// is what stops that; HideWindow covers the same case for programs that ask for
// a visible window explicitly.
//
// The same flags matter for a child of a process that already has a visible
// console, which is how the toast PowerShell is started. A console child with no
// creation flags inherits its parent's console window, so asking it to run hidden
// hides the window the user is typing in rather than a new one.
//
// Naming syscall.SysProcAttr lives here, not in mcp.go, because the fields differ
// per OS and naming them elsewhere is a compile error on the platforms that do
// not have them.
func noConsoleSysProcAttr() *syscall.SysProcAttr {
	const createNoWindow = 0x08000000
	return &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}
