//go:build !windows

package main

import "syscall"

// noConsoleSysProcAttr has nothing to arrange off Windows: there is no console to
// hide, and the child inherits the terminal the agent was started from, which is
// where its stderr belongs anyway.
func noConsoleSysProcAttr() *syscall.SysProcAttr { return nil }
