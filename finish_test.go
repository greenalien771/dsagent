package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The run ends because the model said so, not because it ran out of steps.
//
// Before done existed the only ending was "an assistant turn with no tool
// calls", which is the same shape as a run that gave up - so the UI could not
// mark a finished task, and a model that had two calls left in a batch had no
// way to say "stop after this one". This test pins both halves: the run stops
// after the done call (one request, not two), and the batch it abandoned is
// closed out so the transcript still parses.
const doneThenList = `{"choices":[{"delta":{"tool_calls":[` +
	`{"index":0,"id":"call_done","type":"function","function":{"name":"done","arguments":"{\"summary\":\"Patched the parser and ran go test.\"}"}},` +
	`{"index":1,"id":"call_after","type":"function","function":{"name":"list_dir","arguments":"{\"path\":\".\"}"}}` +
	`]}}]}`

func TestDoneEndsTheRun(t *testing.T) {
	f := withFakeAPI(t, doneThenList)

	a := newTestAgent(t)
	a.cfg.APIKey = "sk-[REDACTED]"
	a.cfg.Model = "test-model"

	var kinds []string
	var finished string
	emit := func(kind, text string) {
		kinds = append(kinds, kind)
		if kind == "done" {
			finished = text
		}
	}
	a.run(context.Background(), "fix the parser", ModeAgent, nil, emit)

	f.mu.Lock()
	requests := f.requests
	violated := f.violated
	f.mu.Unlock()
	if requests != 1 {
		t.Fatalf("%d model requests, want 1: the run should end on the done call, not ask for another turn", requests)
	}
	if violated != "" {
		t.Fatalf("the transcript was left invalid for the API: %s", violated)
	}
	if !contains(kinds, "done") {
		t.Fatalf("no done event was emitted: %v", kinds)
	}
	if !strings.Contains(finished, "go test") {
		t.Fatalf("the done event lost the model's summary: %q", finished)
	}

	a.mu.Lock()
	history := append([]Msg(nil), a.messages...)
	a.mu.Unlock()
	if err := checkGrammar(history); err != nil {
		t.Fatalf("transcript after a done call is unusable: %v", err)
	}
	// The call that never ran must say so rather than vanish.
	var after string
	for _, m := range history {
		if m.Role == "tool" && m.ToolCallID == "call_after" {
			after = m.Content
		}
	}
	if !strings.Contains(after, "not executed") {
		t.Fatalf("the abandoned call was not marked as not executed: %q", after)
	}
}

// The sentence under "Task finished". An empty argument still ends the task, and
// a long summary cannot turn the card into a page of text.
func TestFinishedNote(t *testing.T) {
	if got := finishedNote(`{"summary":"  wrote a.txt, go vet is clean  "}`); got != "wrote a.txt, go vet is clean" {
		t.Fatalf("summary not used verbatim: %q", got)
	}
	if got := finishedNote(`{}`); got == "" || !strings.Contains(got, "finished") {
		t.Fatalf("no fallback sentence for an empty done call: %q", got)
	}
	if got := finishedNote(`not json`); got == "" {
		t.Fatal("unparsable arguments produced no sentence")
	}
	long := finishedNote(`{"summary":"` + strings.Repeat("x", 2000) + `"}`)
	if len([]rune(long)) > 603 {
		t.Fatalf("a long summary was not clipped: %d runes", len([]rune(long)))
	}
}

// done is offered in every posture and never asks for approval: it changes
// nothing, it only ends the run.
func TestDoneToolIsAvailableEverywhere(t *testing.T) {
	for _, mode := range []string{ModeAgent, ModePlan, ModeAsk} {
		found := false
		for _, tl := range toolSchemas(mode) {
			if tl["function"].(map[string]any)["name"] == "done" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: the done tool is missing, so the model cannot end a task", mode)
		}
		if _, denied := modeDenies(mode, "done"); denied {
			t.Errorf("%s: done is refused by modeDenies", mode)
		}
	}
	if needsApproval("done") {
		t.Fatal("done asks for approval; it has no side effect to approve")
	}
	if risk := classifyTool("done", `{"summary":"ok"}`); risk.Level != riskSafe {
		t.Fatalf("done is classified %q, want safe", risk.Level)
	}
}

// notifyNotes runs one tool call and returns the notify payloads it emitted,
// decoded the way the browser decodes them. onEvent, when set, sees every event
// in order, so a test can cancel the run at the exact point it cares about.
func notifyNotes(t *testing.T, ctx context.Context, a *Agent, tc ToolCall, onEvent func(kind, text string)) []commandNote {
	t.Helper()
	var notes []commandNote
	emit := func(kind, text string) {
		if kind == "notify" {
			var n commandNote
			if err := json.Unmarshal([]byte(text), &n); err != nil {
				t.Fatalf("the notify payload is not JSON: %v (%s)", err, text)
			}
			notes = append(notes, n)
		}
		if onEvent != nil {
			onEvent(kind, text)
		}
	}
	a.execTool(ctx, a.cfg.WorkDir, tc, ModeAgent, emit)
	return notes
}

// Every command is reported to the UI with the risk the classifier gave it; the
// browser decides whether to show it (Settings > Safety).
func TestEveryCommandIsReported(t *testing.T) {
	a := newTestAgent(t)
	a.cfg.AutoApprove = true // no approval card in the way of the ordinary path

	tc := ToolCall{ID: "s1", Type: "function"}
	tc.Function.Name = "run_shell"
	tc.Function.Arguments = `{"command":"echo hello"}`

	notes := notifyNotes(t, context.Background(), a, tc, nil)
	if len(notes) != 1 {
		t.Fatalf("%d notifications, want 1", len(notes))
	}
	if notes[0].Level != riskSafe {
		t.Fatalf("level %q, want %q", notes[0].Level, riskSafe)
	}
	if notes[0].Command != "echo hello" || notes[0].Tool != "run_shell" {
		t.Fatalf("notification does not carry the command: %+v", notes[0])
	}
}

// A dangerous command is reported before the approval card, with the reason it
// was flagged - that is the notification the user actually needs. The run is
// cancelled at the approval prompt so nothing on this machine is touched.
func TestDangerousCommandIsReportedWithReasons(t *testing.T) {
	a := newTestAgent(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tc := ToolCall{ID: "s2", Type: "function"}
	tc.Function.Name = "run_shell"
	tc.Function.Arguments = `{"command":"del /s /q C:\\temp\\build"}`

	notes := notifyNotes(t, ctx, a, tc, func(kind, _ string) {
		if kind == "approval" {
			cancel()
		}
	})
	if len(notes) != 1 {
		t.Fatalf("%d notifications, want 1", len(notes))
	}
	if notes[0].Level != riskDangerous {
		t.Fatalf("level %q, want %q", notes[0].Level, riskDangerous)
	}
	if len(notes[0].Reasons) == 0 {
		t.Fatal("the notification does not say why the command is dangerous")
	}
	if !strings.Contains(notes[0].Command, "del /s /q") {
		t.Fatalf("the notification lost the command line: %q", notes[0].Command)
	}
}

// Tools that are not commands stay out of the notification channel: a run_shell
// call is what touches the machine, and a notification for every read_file would
// be noise.
func TestNonCommandToolsAreNotReported(t *testing.T) {
	a := newTestAgent(t)
	tc := ToolCall{ID: "r1", Type: "function"}
	tc.Function.Name = "list_dir"
	tc.Function.Arguments = `{"path":"."}`

	if notes := notifyNotes(t, context.Background(), a, tc, nil); len(notes) != 0 {
		t.Fatalf("%d notifications for list_dir, want 0: %+v", len(notes), notes)
	}
}

// The UI half: both switches exist in the Safety pane and are bound to the
// server config, so the setting survives a closed tab; the finish marker is
// wired to the done event.
func TestUICommandNotificationSwitches(t *testing.T) {
	html := loadUI(t)
	src := scriptBody(html)

	for _, want := range []string{
		`id="cfg-notify-any" onchange="setNotifySwitch('any', this.checked)"`,
		`id="cfg-notify-danger" onchange="setNotifySwitch('danger', this.checked)"`,
		"Any command",
		"Dangerous command only",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html is missing %s", want)
		}
	}
	for _, fn := range []string{"setNotifySwitch", "notifyAllowed", "showCommandNote", "addFinishCard", "paintNotify"} {
		if !strings.Contains(src, "function "+fn+"(") {
			t.Errorf("the script never defines %s()", fn)
		}
	}
	// The switches are config, not browser preferences: a preference could not
	// raise a Windows notification from the server with the tab closed.
	for _, gone := range []string{"setNotifyPref", "notifyAny:", "notifyDanger:"} {
		if strings.Contains(src, gone) {
			t.Errorf("the UI still carries %q - the switches belong to the config", gone)
		}
	}
	if !strings.Contains(src, "k: 'fin'") || !strings.Contains(html, "fin: 1") {
		t.Error("the finish marker is not a persisted transcript record")
	}
	if !strings.Contains(src, "ev.type === 'notify'") {
		t.Error("the stream handler ignores notify events")
	}
}

// ---- the notification switches, server side -------------------------------

// "One choice": any command, or only the dangerous ones. Both on is not a third
// state, and turning one on through the API silences the other.
func TestNotifySwitchesAreOneChoice(t *testing.T) {
	c := newTestAgent(t).cfg
	c.NotifyAny, c.NotifyDanger = false, false

	on := func(p configPatch) (any, danger bool) {
		t.Helper()
		if err := applyPatch(c, p); err != nil {
			t.Fatalf("applyPatch: %v", err)
		}
		cfgMu.Lock()
		defer cfgMu.Unlock()
		return c.NotifyAny, c.NotifyDanger
	}

	if any, danger := on(configPatch{NotifyDanger: boolPtr(true)}); any || !danger {
		t.Fatalf("danger only: any=%v danger=%v", any, danger)
	}
	// The switch the UI sends: turn "any" on, turn "danger" off with it.
	if any, danger := on(configPatch{NotifyAny: boolPtr(true), NotifyDanger: boolPtr(false)}); !any || danger {
		t.Fatalf("any command: any=%v danger=%v", any, danger)
	}
	// The same request without the partner field must still not leave both on.
	if any, danger := on(configPatch{NotifyDanger: boolPtr(true)}); !any || danger {
		t.Fatalf("danger on while any is on: any=%v danger=%v", any, danger)
	}
	if any, danger := on(configPatch{NotifyAny: boolPtr(false), NotifyDanger: boolPtr(false)}); any || danger {
		t.Fatalf("both off: any=%v danger=%v", any, danger)
	}
}

// Which commands reach the desktop: the gate the agent asks before every toast.
func TestDesktopNotifyGating(t *testing.T) {
	c := newTestAgent(t).cfg

	set := func(any, danger bool) {
		c.NotifyAny, c.NotifyDanger = any, danger
	}
	set(false, false)
	for _, level := range []string{riskSafe, riskElevated, riskDangerous} {
		if c.desktopNotifyFor(level) {
			t.Fatalf("%s: a notification with both switches off", level)
		}
	}
	set(false, true)
	if c.desktopNotifyFor(riskSafe) || c.desktopNotifyFor(riskElevated) || !c.desktopNotifyFor(riskDangerous) {
		t.Fatal("dangerous-only does not select exactly the dangerous level")
	}
	set(true, false)
	for _, level := range []string{riskSafe, riskElevated, riskDangerous} {
		if !c.desktopNotifyFor(level) {
			t.Fatalf("%s: any-command did not select every level", level)
		}
	}
}

// The toast text is XML, and a command is arbitrary text: an unescaped < would
// make the notification unloadable and a silent no-show.
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
