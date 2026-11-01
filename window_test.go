package main

import (
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A window reference is either a title substring or an hwnd. Getting this wrong
// sends a capture at the wrong window, so the two spellings are pinned here.
func TestParseWindowRef(t *testing.T) {
	cases := []struct {
		in    any
		title string
		id    int64
	}{
		{"Notepad", "Notepad", 0},
		{"Untitled - Notepad", "Untitled - Notepad", 0},
		{"  0x1a2b  ", "", 0x1a2b},
		{"0X1A2B", "", 0x1a2b},
		{"6690", "", 6690},
		{"", "", 0},
		{nil, "", 0},
	}
	for _, c := range cases {
		ref, err := parseWindowRef(c.in)
		if err != nil {
			t.Fatalf("%v: %v", c.in, err)
		}
		if ref.Title != c.title || ref.ID != c.id {
			t.Errorf("%v: got title=%q id=%d, want %q/%d", c.in, ref.Title, ref.ID, c.title, c.id)
		}
	}

	// "0x" that is not hex is a typo, not a title: guessing would capture
	// whatever window happened to match those characters.
	if _, err := parseWindowRef("0xzz"); err == nil {
		t.Error("a malformed hex handle was accepted")
	}
	// A number must never be sent through the title path: the CLI generator
	// would pass it as a string anyway, but a float from JSON would otherwise
	// be silently dropped.
	if _, err := parseWindowRef(12.5); err == nil {
		t.Error("a number was accepted as a window reference")
	}
}

// An empty reference means "the whole desktop": the pre-existing behaviour of
// the screenshot tool must not change just because windows exist now.
func TestShotArgsWindowIsOptional(t *testing.T) {
	a, err := parseShotArgs(`{}`)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if a.Window != "" || a.WindowID != 0 {
		t.Fatalf("no window was asked for, got %+v", a)
	}

	a, err = parseShotArgs(`{"window":"Calculator","width":300,"height":200}`)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if a.Window != "Calculator" || a.W != 300 || a.H != 200 {
		t.Fatalf("got %+v", a)
	}

	a, err = parseShotArgs(`{"window":"0x1234"}`)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if a.WindowID != 0x1234 || a.Window != "" {
		t.Fatalf("hwnd was not parsed: %+v", a)
	}
}

// The three new tools have to be offered where they can work and withheld where
// every side effect is withheld - the schema is the first gate, and a tool the
// model can see but never use wastes a step.
func TestWindowToolsAvailability(t *testing.T) {
	names := func(mode string) map[string]bool {
		out := map[string]bool{}
		for _, tl := range toolSchemas(mode) {
			out[tl["function"].(map[string]any)["name"].(string)] = true
		}
		return out
	}

	// Agent can do everything.
	agent := names(ModeAgent)
	for _, want := range []string{"list_windows", "window_control", "wait", "screenshot"} {
		if !agent[want] {
			t.Errorf("agent: %s is missing", want)
		}
	}

	// Plan is read-only: looking and waiting are free, moving a window is not.
	plan := names(ModePlan)
	for _, want := range []string{"list_windows", "wait", "screenshot"} {
		if !plan[want] {
			t.Errorf("plan: %s is missing", want)
		}
	}
	if plan["window_control"] {
		t.Error("plan mode offers window_control, which changes the desktop")
	}

	ask := names(ModeAsk)
	for _, unwanted := range []string{"window_control", "write_file", "run_shell"} {
		if ask[unwanted] {
			t.Errorf("ask mode offers %s, which changes something", unwanted)
		}
	}
	// Looking and waiting change nothing, so they stay available in ask mode.
	for _, want := range []string{"list_windows", "wait", "screenshot"} {
		if !ask[want] {
			t.Errorf("ask mode is missing %s", want)
		}
	}

	// The runtime gate has to agree with the schema, or a call gets through.
	if _, denied := modeDenies(ModeAsk, "window_control"); !denied {
		t.Error("ask mode would have executed window_control")
	}
	if _, denied := modeDenies(ModePlan, "window_control"); !denied {
		t.Error("plan mode would have executed window_control")
	}
	if why, denied := modeDenies(ModeAgent, "window_control"); denied {
		t.Errorf("agent mode refused window_control: %s", why)
	}
}

// Moving or closing a window is shown to the user before it happens, and the
// card has to say which window and why.
func TestWindowControlNeedsApproval(t *testing.T) {
	if !needsApproval("window_control") {
		t.Fatal("window_control must be shown to the user first")
	}
	risk := classifyTool("window_control", `{"action":"close","window":"Untitled - Notepad"}`)
	if risk.Level != riskElevated {
		t.Fatalf("close risk = %q, want elevated", risk.Level)
	}
	joined := strings.Join(risk.Reasons, " ")
	if !strings.Contains(joined, "Notepad") || !strings.Contains(strings.ToLower(joined), "unsaved") {
		t.Fatalf("the card does not name the window or the risk: %v", risk.Reasons)
	}
	// Looking and waiting are free.
	if got := classifyTool("list_windows", `{}`).Level; got != riskSafe {
		t.Fatalf("list_windows risk = %q, want safe", got)
	}
	if got := classifyTool("wait", `{"seconds":3}`).Level; got != riskSafe {
		t.Fatalf("wait risk = %q, want safe", got)
	}
}

// wait must sleep for the fraction asked for and must not overshoot, because
// both directions break timing: too short captures a loading window, too long
// wastes the run.
func TestWaitSeconds(t *testing.T) {
	start := time.Now()
	out := waitTool(context.Background(), `{"seconds":0.3}`, nil)
	elapsed := time.Since(start)
	if elapsed < 300*time.Millisecond || elapsed > 1500*time.Millisecond {
		t.Fatalf("waited %v for 0.3s: %s", elapsed, out)
	}
	if !strings.Contains(out, "Waited") {
		t.Fatalf("the result does not say what happened: %q", out)
	}

	// No argument at all is a mistake, not "wait zero": say how to use it.
	if out := waitTool(context.Background(), `{}`, nil); !strings.HasPrefix(out, "BAD ARGS") {
		t.Fatalf("an empty wait was accepted: %q", out)
	}
	if out := waitTool(context.Background(), `{"seconds":-1}`, nil); !strings.HasPrefix(out, "BAD ARGS") {
		t.Fatalf("a negative wait was accepted: %q", out)
	}
	if out := waitTool(context.Background(), `{"window":"x","until":"sideways"}`, nil); !strings.HasPrefix(out, "BAD ARGS") {
		t.Fatalf("an invented condition was accepted: %q", out)
	}
}

// A cancelled run must not sit in a wait: Stop has to feel immediate.
func TestWaitHonoursContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	start := time.Now()
	out := waitTool(ctx, `{"seconds":30}`, nil)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("a cancelled wait ran for %v", elapsed)
	}
	if !strings.Contains(out, "CANCELLED") {
		t.Fatalf("a cancelled wait reported %q", out)
	}
}

// Waiting for a window that never appears has to end in a TIMEOUT the model can
// read, not in a killed process: the call budget must outlast the wait.
func TestWaitForWindowTimesOutCleanly(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("window enumeration is Windows-only")
	}
	start := time.Now()
	out := waitTool(context.Background(), `{"window":"zzz-no-such-window-zzz","until":"appear","timeout_ms":1500}`, nil)
	elapsed := time.Since(start)
	if !strings.HasPrefix(out, "TIMEOUT") {
		t.Fatalf("expected a timeout, got %q", out)
	}
	if elapsed < 1200*time.Millisecond {
		t.Fatalf("gave up early after %v", elapsed)
	}
	if elapsed > 20*time.Second {
		t.Fatalf("the 1.5s wait took %v: the call budget is wrong", elapsed)
	}
}

// The poke callback is what keeps the watchdog from cancelling a deliberate
// pause; it has to actually fire while sleeping.
func TestWaitPokesWhileSleeping(t *testing.T) {
	var pokes int
	waitTool(context.Background(), `{"seconds":1.2}`, func() { pokes++ })
	if pokes < 1 {
		t.Fatalf("the activity clock was never touched during a 1.2s wait")
	}
}

// An unknown action must be refused before anything is shown to the user as a
// window change - and it must name the actions that do exist.
func TestWindowControlRejectsUnknownAction(t *testing.T) {
	out := windowControlTool(context.Background(), `{"action":"explode","window":"Notepad"}`)
	if !strings.HasPrefix(out, "BAD ARGS") {
		t.Fatalf("got %q", out)
	}
	if !strings.Contains(out, "minimize") || !strings.Contains(out, "close") {
		t.Fatalf("the refusal does not list the valid actions: %q", out)
	}
	if out := windowControlTool(context.Background(), `{"action":"close"}`); !strings.HasPrefix(out, "BAD ARGS") {
		t.Fatalf("a close with no window was accepted: %q", out)
	}
}

// windowsToolRefusal checks the "nothing matched" path end to end. It needs a
// real desktop, so it is Windows-only - the parsing tests above cover the rest.
func TestListWindowsFindsSomething(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("window enumeration is Windows-only")
	}
	out := listWindowsTool(context.Background(), `{}`)
	if strings.HasPrefix(out, "ERROR") {
		t.Fatalf("listing windows failed: %s", out)
	}
	if !strings.Contains(out, "WINDOWS ") {
		t.Fatalf("unexpected listing: %.200s", out)
	}

	// A title that cannot exist: the refusal must point at list_windows rather
	// than returning a bare PowerShell error.
	// (list_windows itself just lists; the lookup lives in the act/capture paths.)
	out = listWindowsTool(context.Background(), `{"title":"zzz-no-such-window-zzz"}`)
	if !strings.Contains(out, "WINDOWS 0") {
		t.Fatalf("an impossible filter matched something: %.200s", out)
	}
}

// The script embedded in the binary is the thing that actually runs, so it is
// checked here for the properties that would silently break every call.
func TestEmbeddedWindowScript(t *testing.T) {
	if windowScript == "" {
		t.Fatal("window.ps1 was not embedded: the build would ship a tool that cannot run")
	}
	for _, must := range []string{"param(", "$Action", "PrintWindow", "$SelfPid", "CopyFromScreen"} {
		if !strings.Contains(windowScript, must) {
			t.Errorf("window.ps1 is missing %q", must)
		}
	}
	// The self-protection check is the difference between closing a notepad and
	// closing the run, so its exact expression is pinned.
	if !strings.Contains(windowScript, "PidOf($win) -eq $SelfPid") {
		t.Error("window.ps1 lost the guard that protects the agent's own window")
	}
	// A backtick would end a Go raw string; the script is embedded, not quoted,
	// so this also catches a future paste of the wrong form.
	if strings.Contains(windowScript, "`") {
		t.Error("window.ps1 contains a backtick")
	}

	var decoded map[string]any
	if err := json.Unmarshal([]byte(`{"ok":true}`), &decoded); err != nil {
		t.Fatalf("unrelated: %v", err)
	}
}
