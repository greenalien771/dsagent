package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The toast text is XML, and a command is arbitrary text: an unescaped < would
// make the notification unloadable and a silent no-show.
//
// This test lives in a _windows file because the payload it checks is built by
// notify_windows.go, which only exists on Windows. Beside the other notification
// tests it failed to compile on the unix runners - `undefined: toastXML` - which
// is the cost of testing a platform's toast somewhere the platform is not.
func TestToastXMLEscapesTheCommand(t *testing.T) {
	xmlText := toastXML(`dsagent: command`, `echo "<b>&" > out.txt`)
	for _, bad := range []string{"<b>", `&"`} {
		if strings.Contains(xmlText, bad) {
			t.Fatalf("the command was not escaped: %s", xmlText)
		}
	}
	if !strings.Contains(xmlText, "&lt;b&gt;") || !strings.Contains(xmlText, "&amp;") {
		t.Fatalf("expected escaped text, got %s", xmlText)
	}
	if !strings.Contains(xmlText, `template="ToastText02"`) || !strings.Contains(xmlText, "dsagent: command") {
		t.Fatalf("not a ToastText02 payload: %s", xmlText)
	}
}

// The toast is a console program launched by the same process that owns the
// user's shell window. Started with no creation flags it inherits that console,
// and "-WindowStyle Hidden" then hides the window the user is typing in while the
// shell keeps running: the terminal vanishes, the process does not. Creating the
// child without a console of its own is what stops that, so it is asserted here
// rather than left to a comment.
func TestToastCommandDoesNotShareTheConsole(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c := toastCommand(ctx, `C:\temp\toast.ps1`, `C:\temp\toast.xml`)

	if c.SysProcAttr == nil {
		t.Fatal("no SysProcAttr: the toast PowerShell would inherit this console and hide the user's shell window")
	}
	const createNoWindow = 0x08000000
	if c.SysProcAttr.CreationFlags&createNoWindow == 0 {
		t.Fatalf("CREATE_NO_WINDOW is not set (flags 0x%x): the child would share the console and hiding it would hide the user's shell", c.SysProcAttr.CreationFlags)
	}
	if !c.SysProcAttr.HideWindow {
		t.Fatal("HideWindow is not set: a child that asks for a visible window would show one")
	}
	if got := strings.Join(c.Args, " "); !strings.Contains(got, "-WindowStyle Hidden") {
		t.Fatalf("the toast still has to run hidden: %s", got)
	}
}
