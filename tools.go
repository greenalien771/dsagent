package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

const maxOut = 8000

// toolSchemas returns the tools the model is allowed to see in this mode.
// The model is never offered a tool it would only be refused later, which wastes
// a step and confuses the transcript.
func toolSchemas(mode string) []map[string]any {
	fn := func(name, desc string, props map[string]any, required []string) map[string]any {
		// A tool with no required arguments must still send an array: the API
		// validates against JSON Schema, where "required" null is not an array.
		if required == nil {
			required = []string{}
		}
		return map[string]any{"type": "function", "function": map[string]any{
			"name": name, "description": desc,
			"parameters": map[string]any{"type": "object", "properties": props, "required": required},
		}}
	}
	// Read-only tools: available everywhere.
	tools := []map[string]any{
		fn("list_dir", "List files under a path relative to the working dir.", map[string]any{
			"path": map[string]any{"type": "string", "description": `e.g. "." or "src"`}}, []string{"path"}),

		fn("read_file", "Read a text file. Returns up to 8KB; pass offset to page.", map[string]any{
			"path":   map[string]any{"type": "string"},
			"offset": map[string]any{"type": "integer", "description": "byte offset, default 0"}}, []string{"path"}),
		// read_image is the file counterpart of screenshot: the picture rides on
		// the same SHOT path, so a PNG on disk reaches the model the way a live
		// capture does. It is offered in every mode, because reading is free.
		fn("read_image", "Look at an image file that already exists on disk: PNG, JPEG, GIF or WebP, inside the working dir. The picture is attached to the conversation and you will see it on the next step, exactly like a screenshot. Use read_file for text; this is how you look at a mockup, a diagram, a saved capture or a photo.", map[string]any{
			"path": map[string]any{"type": "string", "description": `e.g. "assets/logo.png" or "testdata/vision/sample.png"`}}, []string{"path"}),

		fn("web_fetch", "Fetch a URL and return its plain text (truncated to 8KB).", map[string]any{
			"url": map[string]any{"type": "string"}}, []string{"url"}),

		fn("list_windows", `Enumerate the visible windows on the desktop: title, process, state (minimized/maximized/normal), focus and rect, each with an hwnd. This is how you find a window to capture or control without the user opening or focusing anything.`, map[string]any{
			"title": map[string]any{"type": "string", "description": "optional substring filter on the title"}}, nil),

		fn("wait", `Pause the run, on purpose. Either 'seconds' (a number, fractions allowed: 0.5 is half a second) or 'window' plus 'until' to wait until a window appears or disappears - that is what makes "open, let it load, capture, close" reliable instead of a guess.`, map[string]any{
			"seconds":    map[string]any{"type": "number", "description": "how long to sleep, e.g. 3 or 0.5"},
			"window":     map[string]any{"type": "string", "description": "title substring or hwnd to wait for"},
			"until":      map[string]any{"type": "string", "enum": []string{"appear", "gone"}, "description": "condition to wait for, default appear"},
			"timeout_ms": map[string]any{"type": "integer", "description": "give up after this long, default 10000"}}, nil),

		fn("screenshot", `Capture an image you can then look at: the whole screen, one region, or one window. Pass window (a title substring, or an hwnd from list_windows) to capture just that window, even when it is minimized or behind others - the user does not have to open or focus it; x/y are then offsets inside it. Pass x/y/width/height to grab only one region: that is how you zoom in on a dialog, an error box or a chart instead of guessing from a full-screen picture. Pass scale (0.1-1) to shrink a big region so it costs fewer tokens. The image appears on the step after this call.`, map[string]any{
			"x":      map[string]any{"type": "integer", "description": "left edge in screen pixels, default 0"},
			"y":      map[string]any{"type": "integer", "description": "top edge in screen pixels, default 0"},
			"width":  map[string]any{"type": "integer", "description": "region width in pixels, default the whole desktop"},
			"height": map[string]any{"type": "integer", "description": "region height in pixels, default the whole desktop"},
			"scale":  map[string]any{"type": "number", "description": "zoom factor 0.1-1.0, default 1 (native pixels)"},
			"window": map[string]any{"type": "string", "description": "title substring or hwnd from list_windows: capture just that window, even if it is minimized or behind others, without the user opening or focusing it. x/y then offset inside the window"}}, nil),
	}

	// done is how a run ends on purpose. Without it the only ending is the model
	// answering with no tool calls, which is the same shape as "I stopped here":
	// neither the UI nor the user can tell a finished task from a run that gave
	// up. It is offered in every mode because a plan or an answer is finished
	// too, and it writes nothing.
	tools = append(tools,
		fn("done", `End the task because it is finished. Call this once the work is complete and verified: it stops the run, and the UI shows "Task finished" with your summary. Do not call it while anything is left to do, and do not use it to report a failure - a task that is blocked is described in your normal answer. In plan or ask mode it marks the plan or answer as complete; nothing on disk has been changed.`, map[string]any{
			"summary": map[string]any{"type": "string", "description": "one or two sentences: what changed (which files), and how it was verified. This is what the user reads under \"Task finished\"."}}, []string{"summary"}))

	// Mutating tools: agent always, plan never, ask never.
	if mode != ModePlan && mode != ModeAsk {
		tools = append(tools,
			fn("write_file", "Create or overwrite a text file.", map[string]any{
				"path":    map[string]any{"type": "string"},
				"content": map[string]any{"type": "string"}}, []string{"path", "content"}))
	}

	// Window control is a side effect on something outside the workspace, so it
	// is offered only where side effects are allowed at all: the same rule
	// write_file follows, and for the same reason.
	if mode != ModePlan && mode != ModeAsk {
		tools = append(tools,
			fn("window_control", `Minimize, maximize, restore, focus or close another window by title or hwnd. Closing is a request (WM_CLOSE), not a kill: a window with unsaved work can refuse and will still be open - check with list_windows and say so rather than claiming it closed. The process hosting this agent is refused by pid; the browser tab showing it is not something this tool can recognise, so leave that one alone.`, map[string]any{
				"action": map[string]any{"type": "string", "enum": []string{"focus", "minimize", "maximize", "restore", "close"}},
				"window": map[string]any{"type": "string", "description": "title substring, or an hwnd from list_windows"}}, []string{"action", "window"}))
	}

	// mcp_control is the agent's own handle on the MCP servers, not a tool one of
	// them advertises. It answers the two things the transcript cannot: whether a
	// server is still running, and how to stop one so it stays stopped instead of
	// being started again by the next call.
	if mode != ModePlan && mode != ModeAsk {
		tools = append(tools,
			fn("mcp_control", "Inspect, stop, start or restart the MCP servers this agent was configured with. status is free. stop is a real stop: the server is asked to leave through its stdin, killed only if it ignores that, and it stays down - the automatic restart a crashed server gets does not undo it - until start. Use status when a tool from a server failed and you need to know whether the server is still there, and restart to pick up a rebuilt binary, because a running executable cannot be replaced.", map[string]any{
				"action": map[string]any{"type": "string", "enum": []string{"status", "stop", "start", "restart"}, "description": "what to do; the default is status"},
				"name":   map[string]any{"type": "string", "description": "server name from the configuration; empty or all means every server"},
			}, nil))
	}

	// run_shell is how plan mode investigates read-only, so it stays in plan
	// (approval is forced there) and disappears entirely in ask mode.
	if mode != ModeAsk {
		tools = append(tools,
			fn("run_shell", "Run a shell command in the working dir. Returns stdout+stderr (truncated).", map[string]any{
				"command": map[string]any{"type": "string"}}, []string{"command"}))
	}

	return dropDesktopTools(tools, runtime.GOOS)
}

// desktopTool reports the tools that need a real Windows desktop: the capture
// path (capture.ps1, window.ps1), the window enumeration and the window control.
// wait stays available everywhere, because its plain-sleep half needs nothing
// from the platform.
func desktopTool(name string) bool {
	switch name {
	case "list_windows", "window_control", "screenshot":
		return true
	}
	return false
}

// dropDesktopTools removes the tools that cannot run on goos. It takes goos as an
// argument instead of reading runtime.GOOS so the non-Windows path can be tested
// from the one platform where the mistake it prevents is invisible.
//
// The desktop tools are Win32 through PowerShell. Anywhere else every one of them
// refuses before it reaches a window, so offering them would cost the model a step
// to learn what this binary already knows - and a tool that can only be refused is
// exactly the wasted step toolSchemas exists to avoid.
func dropDesktopTools(tools []map[string]any, goos string) []map[string]any {
	if goos == "windows" {
		return tools
	}
	kept := tools[:0]
	for _, t := range tools {
		name, _ := t["function"].(map[string]any)["name"].(string)
		if !desktopTool(name) {
			kept = append(kept, t)
		}
	}
	return kept
}

// needsApproval lists the tools a human must see before they run. screenshot is
// on it for a privacy reason rather than a destructive one: it ships the
// contents of the user's screen to the remote API.
func needsApproval(name string) bool {
	// An MCP tool belongs to a third-party server the user configured. This agent
	// cannot classify what it does with its arguments, so every call is shown
	// before it runs - the same rule run_shell follows, for the same reason.
	return name == "run_shell" || name == "write_file" || name == "screenshot" || name == "window_control" ||
		isMCPToolName(name)
}

// safeJoin resolves rel against root and refuses anything that lands outside it.
//
// Checking the joined name against the root prefix is not enough on its own: a
// symlink or NTFS junction inside the workspace points somewhere else while the
// name still looks contained, so read_file and write_file would walk straight
// out of the sandbox. The path is therefore resolved and checked a second time.
func safeJoin(root, rel string) (string, error) {
	root = filepath.Clean(root)
	p := filepath.Join(root, filepath.Clean("/"+rel))
	if p != root && !strings.HasPrefix(p, root+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes working dir")
	}
	realRoot := resolveExisting(root)
	realPath := resolveExisting(p)
	if realRoot == "" || realPath == "" {
		return "", fmt.Errorf("cannot resolve %s", rel)
	}
	// filepath.Rel compares Windows paths case-insensitively, which string
	// prefix matching does not: "c:\Users" and "C:\Users" are one folder.
	sub, err := filepath.Rel(realRoot, realPath)
	if err != nil || sub == ".." || strings.HasPrefix(sub, ".."+string(filepath.Separator)) || filepath.IsAbs(sub) {
		return "", fmt.Errorf("path escapes working dir (link to %s)", realPath)
	}
	return p, nil
}

// resolveExisting returns the fully resolved path of the deepest ancestor of p
// that exists, with the not-yet-created tail put back on. write_file targets
// usually do not exist yet, and EvalSymlinks reports an error for those.
func resolveExisting(p string) string {
	cur := filepath.Clean(p)
	tail := ""
	for {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			if tail == "" {
				return real
			}
			return filepath.Join(real, tail)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return ""
		}
		tail = filepath.Join(filepath.Base(cur), tail)
		cur = parent
	}
}

// runTool executes one tool call. shellLimit is the per-command cap for
// run_shell; the caller reads it from the config so the number is a setting
// rather than a constant of the binary.
// poke is called by tools that block on purpose (wait) so the run does not look
// idle to the watchdog while it is deliberately sleeping.
func runTool(ctx context.Context, root string, tc ToolCall, shellLimit time.Duration, poke func()) string {
	var args map[string]any
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return "BAD ARGS: " + err.Error() + " - retry with valid JSON matching the schema"
	}
	str := func(k string) string { s, _ := args[k].(string); return s }

	switch tc.Function.Name {
	case "list_dir":
		rel := str("path")
		p, err := safeJoin(root, rel)
		if err != nil {
			return "ERROR: " + rel + ": " + err.Error()
		}
		var names []string
		walkErr := filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if path == p {
					// The folder itself could not be read. Reporting that as an
					// empty listing is how a permission problem used to look.
					return err
				}
				// One unreadable entry must not hide the rest of the listing, and
				// must not disappear from it either.
				r, _ := filepath.Rel(root, path)
				names = append(names, r+" (could not be read: "+err.Error()+")")
				return nil
			}
			if path != p {
				r, _ := filepath.Rel(root, path)
				if d.IsDir() {
					r += "/"
				}
				names = append(names, r)
			}
			if len(names) > 500 {
				return filepath.SkipAll
			}
			return nil
		})
		if walkErr != nil {
			return "ERROR: cannot list " + rel + ": " + walkErr.Error()
		}
		if len(names) == 0 {
			// An empty folder and a file are two different answers, and
			// "<empty or file>" was neither of them.
			if st, serr := os.Stat(p); serr == nil && !st.IsDir() {
				return fmt.Sprintf("ERROR: %s is a file, not a folder (%s); read_file is how you read it", rel, humanBytes(st.Size()))
			}
			return "<empty folder: " + rel + " has no entries>"
		}
		return clip(strings.Join(names, "\n"))

	case "read_file":
		rel := str("path")
		p, err := safeJoin(root, rel)
		if err != nil {
			return "ERROR: " + rel + ": " + err.Error()
		}
		if st, serr := os.Stat(p); serr == nil && st.IsDir() {
			// On Windows a folder reaches ReadFile as "Access is denied", which
			// reads like a permissions problem instead of the wrong tool.
			return "ERROR: " + rel + " is a folder, not a file; call list_dir to see what is inside it"
		}
		b, err := os.ReadFile(p)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return "ERROR: there is no file at " + rel + "; call list_dir to see what exists"
			}
			return "ERROR: cannot read " + rel + ": " + err.Error()
		}
		off := 0
		if f, ok := args["offset"].(float64); ok {
			off = int(f)
		}
		if off < 0 {
			return fmt.Sprintf("ERROR: offset %d is negative; an offset is a byte position, so 0 means the start of the file", off)
		}
		if len(b) == 0 {
			return "<" + rel + " is empty: 0 bytes>"
		}
		if off == len(b) {
			return fmt.Sprintf("ERROR: offset %d is exactly the end of %s (%d bytes), so there is nothing left to read from there; call read_file again with offset=0 to read it from the start", off, rel, len(b))
		}
		if off > len(b) {
			return fmt.Sprintf("ERROR: offset %d is past the end of %s, which is %d bytes long; call read_file again with offset=0", off, rel, len(b))
		}

		end := off + maxOut
		if end >= len(b) {
			return string(b[off:])
		}
		chunk := b[off:end]
		// Keep backing up until we find a valid UTF-8 boundary
		for len(chunk) > 0 && !utf8.Valid(chunk) {
			chunk = chunk[:len(chunk)-1]
		}
		return string(chunk) + fmt.Sprintf("\n... TRUNCATED at %d/%d bytes; call read_file with offset=%d", off+len(chunk), len(b), off+len(chunk))

	case "read_image":
		p, err := safeJoin(root, str("path"))
		if err != nil {
			return err.Error()
		}
		// Validate before anything is encoded and sent: the same content sniffing
		// and the same caps an upload from the composer goes through, so a folder
		// or a renamed .txt fails here with a sentence rather than at the API with
		// a 400 halfway through the task.
		size, err := checkImageFile(p)
		if err != nil {
			return "ERROR: " + str("path") + ": " + err.Error()
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			rel = str("path")
		}
		// The SHOT tag is what hands the picture to the model (see attachImage):
		// image blocks are only legal in a user message, so the file is attached
		// one step after this result, not inside it.
		return fmt.Sprintf("Loaded the image %s (%s).\nThe image is attached to the conversation - you will see it on the next step; describe what is actually visible rather than what you expect.\n%s%s",
			rel, humanBytes(size), shotTag, p)

	case "write_file":
		p, err := safeJoin(root, str("path"))
		if err != nil {
			return err.Error()
		}
		content, ok := args["content"].(string)
		if !ok {
			return "BAD ARGS: 'content' must be a string, not " + fmt.Sprintf("%T", args["content"])
		}
		os.MkdirAll(filepath.Dir(p), 0755)
		if err := os.WriteFile(p, []byte(content), 0644); err != nil {
			return "ERROR: " + err.Error()
		}
		return fmt.Sprintf("OK: wrote %d bytes to %s", len(content), str("path"))

	case "run_shell":
		// Per-command cap, from Settings > Agent. The parent ctx still bounds the
		// call as well: a cancelled run kills the child wherever it is.
		if shellLimit <= 0 {
			shellLimit = defaultShellTimeoutSec * time.Second
		}
		cmdCtx, cancel := context.WithTimeout(ctx, shellLimit)
		defer cancel()

		// The shell itself is platform-specific (cmd.exe vs sh). That split lives
		// in shell_windows.go / shell_other.go, because SysProcAttr has different
		// fields per OS and naming them here did not compile on Linux or macOS.
		c := shellCommand(cmdCtx, str("command"))
		c.Dir = root
		// Never hand this agent's own credentials to a child process: a bare
		// `env`/`set` would otherwise print the key straight into the transcript.
		c.Env = scrubbedEnv()
		// A command that leaves something running in the background (`start /b`,
		// `nohup ... &`, a detached server) hands that child our stdout/stderr
		// pipe. cmd.exe then exits, the pipe stays open, and reading it to EOF
		// never returns - the tool call, and with it the whole run, hangs with
		// no output and no error. WaitDelay bounds that: once the process itself
		// is gone the pipes are closed and whatever was printed is returned.
		c.WaitDelay = 3 * time.Second
		out, err := c.CombinedOutput()
		s := clip(string(out))
		if err != nil {
			if errors.Is(err, exec.ErrWaitDelay) {
				// The command ran; it just left a background process behind, so
				// this is not a failure and must not read like one.
				return s + "\nNOTE: the command left a background process holding its output pipe; any further output was cut off."
			}
			// Say which of the two possible endings happened, and name the knob:
			// "exit status: killed" tells nobody that the limit, not the command,
			// was the problem.
			if ctx.Err() != nil {
				return s + "\nCANCELLED: the run was stopped while this command was still running."
			}
			if errors.Is(cmdCtx.Err(), context.DeadlineExceeded) {
				return s + fmt.Sprintf("\nTIMEOUT: still running after %s, so it was killed. If it needs longer, raise \"Shell command timeout\" in Settings (shell_timeout_sec) and run it again.", shellLimit)
			}
			return s + "\n" + exitNote(err)
		}
		return s

	case "screenshot":
		out, err := captureScreenshot(ctx, tc.Function.Arguments)
		if err != nil {
			return "ERROR: " + err.Error()
		}
		return out

	case "list_windows":
		return listWindowsTool(ctx, tc.Function.Arguments)

	case "window_control":
		return windowControlTool(ctx, tc.Function.Arguments)

	case "done":
		// The run loop ends the task when it sees this name. The text below is
		// what the transcript records for the call, so the model and the
		// transcript see the same ending.
		return "OK: the task is marked finished. The run ends here, so no further tool call will execute in this task."

	case "wait":
		return waitTool(ctx, tc.Function.Arguments, poke)

	case "web_fetch":
		raw := strings.TrimSpace(str("url"))
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Sprintf("ERROR: web_fetch needs an absolute http:// or https:// URL, and %q is not one", raw)
		}
		// allowLocal mirrors the approval the user saw: it is true only when the
		// target itself is this machine or the LAN, which is the case the risk
		// layer parks for a human. Everything else must stay on the public
		// internet, including after a redirect or a DNS answer.
		req, _ := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
		req.Header.Set("User-Agent", "Mozilla/5.0")
		resp, err := webFetchClient(isLocalAddr(u.Hostname())).Do(req)
		if err != nil {
			return "ERROR: could not fetch " + u.String() + ": " + err.Error() + "\nNothing was retrieved; check the URL and the connection."
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxOut))
		text := clip(stripTags(string(b)))
		// A 404 or a 500 is not the page. Handing the server's error document
		// back as content is how a caller ends up quoting "Not Found" as the
		// answer it found on the site.
		if resp.StatusCode != http.StatusOK {
			if strings.TrimSpace(text) == "" {
				text = "(the response body was empty)"
			}
			return fmt.Sprintf("ERROR: %s answered HTTP %d %s, so the page was not fetched. What follows is the server's own response, not the page:\n%s",
				u.Host, resp.StatusCode, http.StatusText(resp.StatusCode), text)
		}
		if strings.TrimSpace(text) == "" {
			return "<" + u.String() + " returned an empty body>"
		}
		return text
	}
	return "ERROR: there is no tool called " + fmt.Sprintf("%q", tc.Function.Name) + "; the tools this mode allows are in the request's tool list - call one of those, or answer from what you already have."
}

// checkFetchTarget applies the fetch policy to one URL. Safe to call for the
// original request and for every redirect hop.
func checkFetchTarget(u *url.URL, allowLocal bool) error {
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("refused %q: only http and https can be fetched", scheme)
	}
	if !allowLocal && isLocalAddr(u.Hostname()) {
		return fmt.Errorf("refused %s: it is on this machine or the local network", u.Hostname())
	}
	return nil
}

// webFetchClient is the only HTTP client web_fetch uses.
//
// Two holes it closes: following a redirect into loopback (an approved public
// URL is not permission to probe this machine) and a public hostname that
// resolves to a private address. The dialer therefore checks the address it is
// about to connect to, not just the name it was given. No proxy is used: a
// proxy would make the connection to a host this check never sees.
func webFetchClient(allowLocal bool) *http.Client {
	d := &net.Dialer{Timeout: 15 * time.Second}
	if !allowLocal {
		d.Control = func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if isLocalAddr(host) {
				return errors.New("blocked: that address is on this machine or the local network")
			}
			return nil
		}
	}
	return &http.Client{
		Timeout:   20 * time.Second,
		Transport: &http.Transport{DialContext: d.DialContext, TLSHandshakeTimeout: 10 * time.Second},
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("stopped after 5 redirects")
			}
			return checkFetchTarget(r.URL, allowLocal)
		},
	}
}

// secretEnvVars are the credential variables a tool-launched process must not
// inherit. Only this agent's own key plus other LLM vendor keys are stripped;
// everything else in the user's environment is passed through untouched.
var secretEnvVars = func() map[string]bool {
	m := map[string]bool{
		"OPENAI_API_KEY":      true,
		"ANTHROPIC_API_KEY":   true,
		"GEMINI_API_KEY":      true,
		"GOOGLE_API_KEY":      true,
		"HUGGINGFACE_API_KEY": true,
	}
	// Every provider's variable, so a tool-launched process cannot inherit
	// whichever key the agent happens to be running on.
	for _, name := range providerKeyEnvVars() {
		m[name] = true
	}
	return m
}()

// scrubbedEnv returns os.Environ() without the credential variables above.
func scrubbedEnv() []string {
	env := os.Environ()
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			name = kv[:i]
		}
		if secretEnvVars[strings.ToUpper(name)] {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func clip(s string) string {
	if len(s) > maxOut {
		return strings.ToValidUTF8(s[:maxOut], "") + "\n... TRUNCATED"
	}
	return s
}

var tagRe = regexp.MustCompile(`<[^>]*>`)

func stripTags(s string) string {
	s = tagRe.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	return strings.Join(strings.Fields(s), " ")
}

// exitNote says what a finished command actually did. "exit status 1" is Go's
// wording for "a process exited"; it names neither the command nor the code, so
// the note states the code and points at the output that came with it.
func exitNote(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if code := ee.ExitCode(); code >= 0 {
			return fmt.Sprintf("EXIT: the command exited with code %d; whatever it printed is above.", code)
		}
		return "EXIT: the command was killed by a signal, so it has no exit code of its own."
	}
	// No exit code at all: the shell itself could not be started.
	return "EXIT: the command could not be run at all: " + err.Error()
}
