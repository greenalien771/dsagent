package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// windowScript is the whole Windows windowing backend: enumeration, capture,
// state changes and condition waits, with no dependency beyond the user's own
// PowerShell.
//
// It lives in a .ps1 file rather than a Go string because it is 300 lines of
// Win32 interop and bitmap work that is edited as PowerShell, and a 12 KB raw
// string in the middle of a Go file makes both halves harder to read.
//
//go:embed window.ps1
var windowScript string

// windowArgTimeout bounds one PowerShell call. Add-Type dominates the cost
// (roughly 300-400 ms), so this is generous for a listing and still finite: a
// hung window message must not hang the run.
const windowArgTimeout = 45 * time.Second

// waitMaxSeconds and waitMaxMs bound the wait tool. The idle watchdog closes a
// run that produces nothing for a couple of minutes, so a sleep long enough to
// trip it is not useful anyway - and the tool reports progress while it sleeps.
const (
	waitMaxSeconds = 300
	waitMaxMs      = 60000
	waitMinMs      = 200
)

// windowArgs is one call into window.ps1.
type windowArgs struct {
	Action   string
	Title    string
	ID       int64
	Act      string
	Out      string
	Scale    float64
	OffsetX  int
	OffsetY  int
	CropW    int
	CropH    int
	Until    string
	TimeoutM int

	// Budget is how long this particular call may take, when the action itself
	// waits (wait). Zero means the default windowArgTimeout.
	Budget time.Duration
}

// runWindowScript writes the script out and runs one action through it.
// Every caller routes through here so there is exactly one place that builds
// the argument list - and therefore one place where a title with a space, a
// quote or a percent sign can be mis-quoted.
func runWindowScript(ctx context.Context, wa windowArgs) (string, error) {
	if runtime.GOOS != "windows" {
		return "", fmt.Errorf("window control is only implemented on Windows")
	}
	dir := filepath.Join(os.TempDir(), "dsagent-shots")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	script := filepath.Join(dir, "window.ps1")
	// Rewritten on every call: the running binary owns this copy, so a stale
	// one from an older version can never be executed by mistake.
	if err := os.WriteFile(script, []byte(windowScript), 0600); err != nil {
		return "", err
	}

	budget := wa.Budget
	if budget < windowArgTimeout {
		// Add-Type alone costs a few hundred milliseconds on every call, so the
		// floor stays above that even when the wait itself is short.
		budget = windowArgTimeout
	}
	cmdCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	args := []string{"-NoProfile", "-ExecutionPolicy", "Bypass", "-File", script,
		"-Action", wa.Action,
		"-Title", wa.Title,
		"-Id", strconv.FormatInt(wa.ID, 10),
		"-Act", wa.Act,
		"-Out", wa.Out,
		"-Scale", strconv.FormatFloat(wa.Scale, 'f', 3, 64),
		"-OffsetX", strconv.Itoa(wa.OffsetX), "-OffsetY", strconv.Itoa(wa.OffsetY),
		"-CropW", strconv.Itoa(wa.CropW), "-CropH", strconv.Itoa(wa.CropH),
		"-Until", wa.Until,
		"-TimeoutMs", strconv.Itoa(wa.TimeoutM),
		// The script refuses to act on this process's own window. Closing the
		// agent's window is closing the run, mid-edit - the same class of
		// mistake as killing the process, and just as final.
		"-SelfPid", strconv.Itoa(os.Getpid())}

	c := exec.CommandContext(cmdCtx, "powershell", args...)
	c.Env = scrubbedEnv()
	c.WaitDelay = 3 * time.Second
	outBytes, err := c.CombinedOutput()
	text := strings.TrimSpace(string(outBytes))
	if err != nil {
		if ctx.Err() != nil {
			return text, fmt.Errorf("cancelled")
		}
		if errors.Is(cmdCtx.Err(), context.DeadlineExceeded) {
			return text, fmt.Errorf("the window call did not finish within %s", budget)
		}
		// The script reports its refusals as exceptions; a bare "exit status 1"
		// would hide the reason the user needs to read.
		if text == "" {
			text = err.Error()
		}
		return text, fmt.Errorf("%s", firstLine(text))
	}
	return text, nil
}

// firstLine keeps an error message to the sentence that says what went wrong:
// a PowerShell stack trace buries it.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(strings.TrimRight(line, "\r"))
		if line == "" || strings.HasPrefix(line, "At line:") || strings.HasPrefix(line, "+") ||
			strings.HasPrefix(line, "CategoryInfo") || strings.HasPrefix(line, "FullyQualifiedErrorId") {
			continue
		}
		return line
	}
	return "the window call failed"
}

// windowRef is the window a call is aimed at: a title substring or an hwnd.
type windowRef struct {
	Title string
	ID    int64
}

func (w windowRef) empty() bool { return w.Title == "" && w.ID == 0 }

// parseWindowRef reads "window" from a tool call. A value that looks like a
// handle ("0x1a2b" or "6690") is an id, anything else is a title substring -
// the shape list_windows prints is what the model is expected to copy back.
func parseWindowRef(v any) (windowRef, error) {
	// Absent is the common case: every tool that takes a window also works
	// without one (whole desktop, next window found, plain sleep).
	if v == nil {
		return windowRef{}, nil
	}
	s, ok := v.(string)
	if !ok {
		return windowRef{}, fmt.Errorf("window must be a string (a title substring, or an hwnd from list_windows)")
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return windowRef{}, nil
	}
	if strings.HasPrefix(strings.ToLower(s), "0x") {
		n, err := strconv.ParseInt(s[2:], 16, 64)
		if err != nil {
			return windowRef{}, fmt.Errorf("window %q looks like a handle but is not valid hex", s)
		}
		return windowRef{ID: n}, nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
		return windowRef{ID: n}, nil
	}
	return windowRef{Title: s}, nil
}

// listWindowsTool enumerates the windows the agent can name afterwards.
func listWindowsTool(ctx context.Context, raw string) string {
	var args map[string]any
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			return "BAD ARGS: " + err.Error()
		}
	}
	ref, err := parseWindowRef(args["title"])
	if err != nil {
		return "BAD ARGS: " + err.Error()
	}
	out, err := runWindowScript(ctx, windowArgs{Action: "list", Title: ref.Title, Scale: 1})
	if err != nil {
		return "ERROR: listing windows failed: " + err.Error() + "\n" + out
	}
	return out + "\nPass any of these to screenshot (window) or window_control (window)."
}

// windowControlTool focuses, minimizes, maximizes, restores or closes a window.
func windowControlTool(ctx context.Context, raw string) string {
	var args map[string]any
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return "BAD ARGS: " + err.Error()
	}
	str := func(k string) string { s, _ := args[k].(string); return s }
	act := strings.ToLower(strings.TrimSpace(str("action")))
	switch act {
	case "focus", "minimize", "maximize", "restore", "close":
	default:
		return `BAD ARGS: action must be one of focus, minimize, maximize, restore, close`
	}
	ref, err := parseWindowRef(args["window"])
	if err != nil {
		return "BAD ARGS: " + err.Error()
	}
	if ref.empty() {
		return "BAD ARGS: give 'window' - a title substring, or an hwnd from list_windows"
	}
	out, err := runWindowScript(ctx, windowArgs{Action: "act", Title: ref.Title, ID: ref.ID, Act: act, Scale: 1})
	if err != nil {
		return "ERROR: " + act + " failed: " + err.Error() + "\n" + out
	}
	return out
}

// waitTool blocks this run on purpose, so that "open, let it load, look, close"
// is one sequence instead of a guess. It reports progress while it sleeps: the
// idle watchdog measures silence, and a deliberate pause is not a stall.
func waitTool(ctx context.Context, raw string, poke func()) string {
	var args map[string]any
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			return "BAD ARGS: " + err.Error()
		}
	}
	num := func(k string) (float64, bool) {
		f, ok := args[k].(float64)
		return f, ok
	}

	ref, err := parseWindowRef(args["window"])
	if err != nil {
		return "BAD ARGS: " + err.Error()
	}

	// Window mode: wait until a window appears or goes away. This is what makes
	// the sequence deterministic - "wait for the dialog" beats "wait 3 seconds
	// and hope", and it returns the moment the condition holds.
	if !ref.empty() {
		until := strings.ToLower(strings.TrimSpace(func() string { s, _ := args["until"].(string); return s }()))
		if until == "" {
			until = "appear"
		}
		if until != "appear" && until != "gone" {
			return `BAD ARGS: until must be "appear" or "gone"`
		}
		timeout := 10000
		if ms, ok := num("timeout_ms"); ok {
			timeout = int(ms)
		}
		if timeout < waitMinMs {
			timeout = waitMinMs
		}
		if timeout > waitMaxMs {
			timeout = waitMaxMs
		}
		// The budget must outlast the wait the caller asked for, or the script
		// gets killed just before it would have answered.
		budget := windowArgTimeout + time.Duration(timeout)*time.Millisecond
		out, err := runWindowScript(ctx, windowArgs{
			Action: "wait", Title: ref.Title, ID: ref.ID,
			Until: until, TimeoutM: timeout, Scale: 1, Budget: budget,
		})
		if err != nil {
			if err.Error() == "cancelled" {
				return "CANCELLED: the run was stopped while waiting."
			}
			return "ERROR: waiting for the window failed: " + err.Error() + "\n" + out
		}
		return out
	}

	// Plain sleep. Fractional seconds are the point ("wait 0.5" is half of the
	// poll interval of a loaded UI), so it is a float, not an int.
	secs, ok := num("seconds")
	if !ok {
		return `BAD ARGS: give 'seconds' (a number) to pause, or 'window' to wait for a window to appear or close`
	}
	if secs < 0 {
		return "BAD ARGS: seconds cannot be negative"
	}
	if secs > waitMaxSeconds {
		secs = waitMaxSeconds
	}
	d := time.Duration(secs * float64(time.Second))
	if d <= 0 {
		return "Waited 0s (nothing to do)."
	}

	start := time.Now()
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	deadline := time.NewTimer(d)
	defer deadline.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Sprintf("CANCELLED: the run was stopped after %s of the %s wait.",
				time.Since(start).Round(100*time.Millisecond), d)
		case <-tick.C:
			if poke != nil {
				poke()
			}
		case <-deadline.C:
			return fmt.Sprintf("Waited %s.", time.Since(start).Round(10*time.Millisecond))
		}
	}
}

// captureWindow grabs one window into a PNG and returns the result text with
// the SHOT tag, so the existing attachShot path hands the picture to the model
// exactly like a desktop capture does.
func captureWindow(ctx context.Context, args shotArgs) string {
	if runtime.GOOS != "windows" {
		return "ERROR: capturing a single window is only implemented on Windows. On this machine, capture the screen another way and attach the file with read_file (text) or the composer's Add Image."
	}
	ref := windowRef{Title: args.Window}
	if args.WindowID != 0 {
		ref = windowRef{ID: args.WindowID}
	}
	dir := filepath.Join(os.TempDir(), "dsagent-shots")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "ERROR: " + err.Error()
	}
	sweepShots(dir)

	out := filepath.Join(dir, fmt.Sprintf("shot-%d.png", time.Now().UnixNano()))
	text, err := runWindowScript(ctx, windowArgs{
		Action: "capture", Title: ref.Title, ID: ref.ID, Out: out,
		Scale: args.Scale, OffsetX: args.X, OffsetY: args.Y, CropW: args.W, CropH: args.H,
	})
	if err != nil {
		return "ERROR: capturing that window failed: " + err.Error() + "\n" + text
	}
	if st, statErr := os.Stat(out); statErr != nil || st.Size() == 0 {
		return "ERROR: the window capture produced no image\n" + text
	}

	info := text
	if size := imageSize(out); size != "" {
		info = strings.TrimSpace(text + "\nSaved image: " + size + " px")
	}
	return fmt.Sprintf("Captured one window - the user did not have to open or focus it.\n%s\nFile: %s (%s)\nThe image is attached to the conversation - you will see it on the next step; describe what is actually visible rather than what you expect.\n%s%s",
		info, out, humanBytes(fileSize(out)), shotTag, out)
}
