//go:build !windows

package main

// desktopToast is a no-op away from Windows: the notification is a Windows
// toast, and there is nothing honest to fake on a machine that has none. The
// in-page notification still fires, so a run is not silent either way.
func desktopToast(title, body string) {}
