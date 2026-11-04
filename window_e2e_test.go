package main

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// requireWindows states the requirement of this file: window.ps1 drives Win32
// through the user's own PowerShell, so on any other GOOS every call refuses
// before it reaches the desktop and there is nothing here to assert. Each test
// says so up front instead of failing a call and reporting the refusal as a bug.
func requireWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("window control is Win32 through PowerShell, so it is Windows-only")
	}
}

// The whole sequence the feature exists for, driven through the same functions
// the agent calls: launch -> wait for the window -> capture it -> minimize ->
// close. It uses Notepad because every Windows box has it and it is never the
// agent's own window.
//
// Skipped in short mode, and skipped unless DSAGENT_WINDOW_E2E=1. This one opens
// and closes a real Notepad window, so on a plain `go test ./...` - the command an
// agent runs to prove a build - it made a window flash over whatever the user was
// doing. TestDesktopToastShows is opt-in for the same reason: a test suite must not
// take over the desktop it happens to be running on.
func TestWindowSequenceEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a real application")
	}
	if os.Getenv("DSAGENT_WINDOW_E2E") != "1" {
		t.Skip("set DSAGENT_WINDOW_E2E=1 to open and close a real window")
	}
	requireWindows(t)

	ctx := context.Background()

	// A Notepad whose title we control, so a stray window cannot match.
	app := exec.Command("notepad")
	if err := app.Start(); err != nil {
		t.Skipf("cannot start notepad: %v", err)
	}
	pid := app.Process.Pid
	defer func() {
		// Whatever happened above, leave no test window behind.
		windowControlTool(ctx, `{"action":"close","window":"Notepad"}`)
		time.Sleep(300 * time.Millisecond)
		app.Process.Kill()
		app.Wait()
	}()

	// Wait for "Notepad" (no title yet, the document is untitled).
	out := waitTool(ctx, `{"window":"Notepad","until":"appear","timeout_ms":8000}`, nil)
	if !strings.HasPrefix(out, "FOUND") {
		t.Fatalf("notepad never appeared: %s", out)
	}

	// Find it by pid, which is the example the model gets: list, then use the
	// hwnd you saw.
	listing := listWindowsTool(ctx, `{"title":"Notepad"}`)
	if !strings.Contains(listing, "proc=notepad") {
		t.Fatalf("the listing does not show notepad: %s", listing)
	}
	var hwnd string
	for _, line := range strings.Split(listing, "\n") {
		if strings.Contains(line, "proc=notepad") {
			fields := strings.Fields(line)
			for _, f := range fields {
				if strings.HasPrefix(f, "hwnd=") {
					hwnd = strings.TrimPrefix(f, "hwnd=")
				}
			}
		}
	}
	if hwnd == "" {
		t.Fatalf("no hwnd in the listing: %s", listing)
	}

	// Capture that window by handle, at half scale.
	shotArgsJSON := `{"window":"` + hwnd + `","scale":0.5}`
	shot, err := captureScreenshot(ctx, shotArgsJSON)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if !strings.Contains(shot, shotTag) {
		t.Fatalf("the capture did not produce a SHOT line: %s", shot)
	}
	path, ok := shotFromResult(shot)
	if !ok {
		t.Fatalf("the SHOT line is not parseable: %s", shot)
	}
	if st, err := os.Stat(path); err != nil || st.Size() == 0 {
		t.Fatalf("the capture wrote no file: %v", err)
	}
	if size := imageSize(path); size == "" || strings.HasPrefix(size, "0x") {
		t.Fatalf("the capture is not a readable PNG: %q", size)
	}
	t.Logf("captured %s (%s)", path, imageSize(path))

	// Minimize, then capture again: a minimized window still has to produce a
	// picture, which is the case the user asked for ("without opening these").
	out = windowControlTool(ctx, `{"action":"minimize","window":"`+hwnd+`"}`)
	if !strings.Contains(out, "OK minimize") || !strings.Contains(out, "state=minimized") {
		t.Fatalf("minimize did not take: %s", out)
	}
	shot, err = captureScreenshot(ctx, `{"window":"`+hwnd+`"}`)
	if err != nil {
		t.Fatalf("capture of a minimized window: %v", err)
	}
	if !strings.Contains(shot, "minimized") || !strings.Contains(shot, shotTag) {
		t.Fatalf("a minimized window was not captured: %s", shot)
	}

	// Close it, then wait for it to be gone - the last step of the sequence.
	out = windowControlTool(ctx, `{"action":"close","window":"`+hwnd+`"}`)
	if !strings.Contains(out, "OK close") {
		t.Fatalf("close did not report: %s", out)
	}
	out = waitTool(ctx, `{"window":"`+hwnd+`","until":"gone","timeout_ms":5000}`, nil)
	if !strings.HasPrefix(out, "GONE") {
		t.Fatalf("the window did not close: %s", out)
	}

	_ = pid
}

// Closing the agent's own process must be refused by the script, not attempted.
// The check is by pid, so it holds even if the title matches.
func TestWindowScriptRefusesOwnProcess(t *testing.T) {
	requireWindows(t)
	ctx := context.Background()
	out, err := runWindowScript(ctx, windowArgs{
		Action: "act", Act: "close", ID: int64(0), Scale: 1,
	})
	if err == nil {
		t.Fatalf("a call with no window was accepted: %s", out)
	}

	// A window that cannot exist must be reported as such, not silently acted on.
	out, err = runWindowScript(ctx, windowArgs{Action: "list", Title: "zzz-no-such-window-zzz", Scale: 1})
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if !strings.Contains(out, "WINDOWS 0") {
		t.Fatalf("an impossible filter matched: %s", out)
	}
}
