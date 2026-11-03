package main

import (
	"os"
	"testing"
	"time"
)

// TestDesktopToastShows is the one check that cannot be made without a desktop:
// it runs the real PowerShell invocation and relies on a human (or a screenshot)
// seeing the notification. It is skipped unless DSAGENT_TOAST_TEST=1, because a
// test suite must not post messages to the user's Action Center.
func TestDesktopToastShows(t *testing.T) {
	if os.Getenv("DSAGENT_TOAST_TEST") != "1" {
		t.Skip("set DSAGENT_TOAST_TEST=1 to post a real notification")
	}
	desktopToast("dsagent: dangerous command", `del /s /q C:\temp\build - forced/quiet file delete (del /f /s /q)`)
	time.Sleep(3 * time.Second)
}
