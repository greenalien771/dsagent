package main

import (
	"context"
	"encoding/xml"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// This file is the Windows notification: a real toast, shown by dsagent in the
// user's Action Center, not an in-page message. It exists because the useful
// moment to learn "the agent is about to run this command" is while you are
// doing something else - the tab is behind another window, or minimised.
//
// Two decisions are baked in here:
//
//   - The toast is shown under an AppUserModelID Windows already knows. A toast
//     for an id with no Start Menu entry is silently dropped, and installing a
//     shortcut on the user's Start Menu is not something a coding agent should
//     do to light up a message. Windows PowerShell's own id is registered on
//     every Windows 10 machine, so the notification arrives with no setup; the
//     title says "dsagent", which is what the reader needs to know.
//   - Nothing from the command line is ever put on a command line. The toast
//     text goes into a file and PowerShell is handed the path, so a command
//     containing quotes, >, % or a newline cannot turn into PowerShell code.

// toastAppID is the registered AppUserModelID the notification is shown under.
const toastAppID = `{1AC14E77-02E7-4E5D-B744-2EB1AE5198B7}\WindowsPowerShell\v1.0\powershell.exe`

// toastScript is written next to the XML it is handed: arguments are paths this
// process generated, never text the user or the model wrote.
const toastScript = `param([Parameter(Mandatory=$true)][string]$Xml,[Parameter(Mandatory=$true)][string]$AppId)
$ErrorActionPreference = 'Stop'
[Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime] | Out-Null
[Windows.Data.Xml.Dom.XmlDocument, Windows.Data.Xml.Dom.XmlDocument, ContentType = WindowsRuntime] | Out-Null
$doc = New-Object Windows.Data.Xml.Dom.XmlDocument
$doc.LoadXml([System.IO.File]::ReadAllText($Xml))
$toast = New-Object Windows.UI.Notifications.ToastNotification $doc
try { $toast.Tag = 'dsagent' } catch { }
[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier($AppId).Show($toast)
`

// desktopToast shows one Windows notification. It is best effort by design: a
// machine with notifications switched off, a locked session or a missing
// PowerShell must cost nothing but the notification itself, so every error is
// dropped. Callers run it in a goroutine - it waits for PowerShell to start,
// and no tool call should wait for a message.
func desktopToast(title, body string) {
	dir := filepath.Join(os.TempDir(), "dsagent-toast")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return
	}
	script := filepath.Join(dir, "toast.ps1")
	if err := os.WriteFile(script, []byte(toastScript), 0600); err != nil {
		return
	}
	xmlPath := filepath.Join(dir, "toast.xml")
	if err := os.WriteFile(xmlPath, []byte(toastXML(title, body)), 0600); err != nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, "powershell",
		"-NoProfile", "-ExecutionPolicy", "Bypass", "-WindowStyle", "Hidden",
		"-File", script, "-Xml", xmlPath, "-AppId", toastAppID)
	c.Env = scrubbedEnv()
	// A hidden PowerShell that leaves something behind must not keep this
	// goroutine alive forever.
	c.WaitDelay = 3 * time.Second
	_ = c.Run()
}

// toastXML is the ToastText02 template: a bold title and one wrapped body line.
// Text is XML-escaped, so a command with < or & in it is text and not markup.
func toastXML(title, body string) string {
	return `<toast><visual><binding template="ToastText02">` +
		`<text id="1">` + xmlEscape(clipText(title, 120)) + `</text>` +
		`<text id="2">` + xmlEscape(clipText(body, 300)) + `</text>` +
		`</binding></visual></toast>`
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
