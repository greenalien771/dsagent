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

		fn("web_fetch", "Fetch a URL and return its plain text (truncated to 8KB).", map[string]any{
			"url": map[string]any{"type": "string"}}, []string{"url"}),

		fn("screenshot", `Capture the screen as an image you can then look at. Pass x/y/width/height to grab only one region: that is how you zoom in on a dialog, an error box or a chart instead of guessing from a full-screen picture. Pass scale (0.1-1) to shrink a big region so it costs fewer tokens. The image appears on the step after this call.`, map[string]any{
			"x":      map[string]any{"type": "integer", "description": "left edge in screen pixels, default 0"},
			"y":      map[string]any{"type": "integer", "description": "top edge in screen pixels, default 0"},
			"width":  map[string]any{"type": "integer", "description": "region width in pixels, default the whole desktop"},
			"height": map[string]any{"type": "integer", "description": "region height in pixels, default the whole desktop"},
			"scale":  map[string]any{"type": "number", "description": "zoom factor 0.1-1.0, default 1 (native pixels)"}}, nil),
	}

	// Mutating tools: agent always, plan never, ask never.
	if mode != ModePlan && mode != ModeAsk {
		tools = append(tools,
			fn("write_file", "Create or overwrite a text file.", map[string]any{
				"path":    map[string]any{"type": "string"},
				"content": map[string]any{"type": "string"}}, []string{"path", "content"}))
	}

	// run_shell is how plan mode investigates read-only, so it stays in plan
	// (approval is forced there) and disappears entirely in ask mode.
	if mode != ModeAsk {
		tools = append(tools,
			fn("run_shell", "Run a shell command in the working dir. Returns stdout+stderr (truncated).", map[string]any{
				"command": map[string]any{"type": "string"}}, []string{"command"}))
	}

	return tools
}

// needsApproval lists the tools a human must see before they run. screenshot is
// on it for a privacy reason rather than a destructive one: it ships the
// contents of the user's screen to the remote API.
func needsApproval(name string) bool {
	return name == "run_shell" || name == "write_file" || name == "screenshot"
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
func runTool(ctx context.Context, root string, tc ToolCall, shellLimit time.Duration) string {
	var args map[string]any
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return "BAD ARGS: " + err.Error() + " â€” retry with valid JSON matching the schema"
	}
	str := func(k string) string { s, _ := args[k].(string); return s }

	switch tc.Function.Name {
	case "list_dir":
		p, err := safeJoin(root, str("path"))
		if err != nil {
			return err.Error()
		}
		var names []string
		filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
			if err == nil && path != p {
				rel, _ := filepath.Rel(root, path)
				if d.IsDir() {
					rel += "/"
				}
				names = append(names, rel)
			}
			if len(names) > 500 {
				return filepath.SkipAll
			}
			return nil
		})
		if len(names) == 0 {
			return "<empty or file>"
		}
		return clip(strings.Join(names, "\n"))

	case "read_file":
		p, err := safeJoin(root, str("path"))
		if err != nil {
			return err.Error()
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return "ERROR: " + err.Error() + " â€” call list_dir to see what exists"
		}
		off := 0
		if f, ok := args["offset"].(float64); ok {
			off = int(f)
		}
		if off < 0 {
			return "ERROR: offset cannot be negative"
		}
		if off > len(b) {
			return "offset past EOF"
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
		return string(chunk) + fmt.Sprintf("\nâ€¦ TRUNCATED at %d/%d bytes; call read_file with offset=%d", off+len(chunk), len(b), off+len(chunk))

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
			return s + "\nEXIT: " + err.Error()
		}
		return s

	case "screenshot":
		out, err := captureScreenshot(ctx, tc.Function.Arguments)
		if err != nil {
			return "ERROR: " + err.Error()
		}
		return out

	case "web_fetch":
		raw := strings.TrimSpace(str("url"))
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return "ERROR: web_fetch needs an absolute http:// or https:// URL"
		}
		// allowLocal mirrors the approval the user saw: it is true only when the
		// target itself is this machine or the LAN, which is the case the risk
		// layer parks for a human. Everything else must stay on the public
		// internet, including after a redirect or a DNS answer.
		req, _ := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
		req.Header.Set("User-Agent", "Mozilla/5.0")
		resp, err := webFetchClient(isLocalAddr(u.Hostname())).Do(req)
		if err != nil {
			return "ERROR: " + err.Error()
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxOut))
		return clip(stripTags(string(b)))
	}
	return "UNKNOWN TOOL " + tc.Function.Name
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
var secretEnvVars = map[string]bool{
	keyEnvVar:             true,
	"OPENAI_API_KEY":      true,
	"ANTHROPIC_API_KEY":   true,
	"GEMINI_API_KEY":      true,
	"GOOGLE_API_KEY":      true,
	"HUGGINGFACE_API_KEY": true,
}

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
		return strings.ToValidUTF8(s[:maxOut], "") + "\nâ€¦ TRUNCATED"
	}
	return s
}

var tagRe = regexp.MustCompile(`<[^>]*>`)

func stripTags(s string) string {
	s = tagRe.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	return strings.Join(strings.Fields(s), " ")
}
