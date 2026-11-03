package main

import (
	"strings"
	"testing"
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
