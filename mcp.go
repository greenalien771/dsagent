package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Model Context Protocol support.
//
// dsagent is the MCP *client*: a server is a child process the user configured
// in mcp_servers, speaking JSON-RPC 2.0 over stdin/stdout with one message per
// line - the stdio transport the specification defines. Nothing here talks to
// the network on its own; a server is started only because it is listed.
//
// The file has three jobs:
//
//  1. keep one server process alive per entry, handshake once, and cache the
//     tool catalogue it advertises with tools/list;
//  2. turn each advertised tool into an ordinary OpenAI-style function schema,
//     renamed so it can neither collide with a built-in tool nor with another
//     server's tool of the same name; and
//  3. route a call back to the server that advertised it (tools/call) and
//     flatten the content blocks it answers with into the text a tool result
//     has to be.
//
// What it deliberately does not do: there is no sampling, no roots and no
// prompts support. Those would mean letting a third-party server drive this
// agent or ask the user questions through it, and none of that is worth the
// attack surface for the tool-calling this exists for. A server that asks for
// one gets a method-not-found answer instead of silence, so it does not hang.

const (
	// mcpProtocolVersion is the revision this client speaks. Both 2024-11-05 and
	// the later revisions shape initialize, tools/list and tools/call the same
	// way, and a server is free to answer with the revision it chose: only the
	// tool calls below depend on it, and they do not differ.
	mcpProtocolVersion = "2024-11-05"

	// mcpToolPrefix marks a function name as belonging to an MCP server. The API
	// caps a function name at 64 characters and allows [A-Za-z0-9_-] only, which
	// is why the name is built and sanitised rather than passed through.
	mcpToolPrefix = "mcp__"
	mcpMaxNameLen = 64

	mcpInitTimeout  = 20 * time.Second
	mcpListTimeout  = 20 * time.Second
	mcpToolTimeout  = 10 * time.Minute
	mcpPokeInterval = 5 * time.Second
	mcpStopGrace    = 2 * time.Second
)

// MCPServerConfig is one entry of mcp_servers in config.json: a command to run,
// not an address to connect to.
type MCPServerConfig struct {
	// Name is how the server is named to the model (its tools become
	// mcp__<name>__<tool>) and in the UI. It must be unique among the entries.
	Name string `json:"name"`
	// Command is the executable, resolved through PATH like any other child
	// process. Args are passed verbatim, one argument each: no shell, so a
	// quoted string here is literal and '&&' is not an operator.
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
	// Env adds "NAME=value" entries on top of the user's environment.
	Env []string `json:"env,omitempty"`
	// Dir is the server's working directory. Empty means inheriting the agent's.
	Dir string `json:"dir,omitempty"`
	// Disabled keeps an entry configured without starting it.
	Disabled bool `json:"disabled,omitempty"`
}

// commandLine is the entry as one line, for the banner, the status endpoint and
// error text.
func (c MCPServerConfig) commandLine() string {
	parts := append([]string{c.Command}, c.Args...)
	return strings.Join(parts, " ")
}

// mcpServerNames lists the enabled entries, for the line the transcript shows
// while the servers start. A disabled entry is not named there: it is not being
// started, and saying it is would be a lie the user cannot see through.
func mcpServerNames(list []MCPServerConfig) string {
	var names []string
	for _, s := range list {
		if !s.Disabled {
			names = append(names, s.Name)
		}
	}
	if len(names) == 0 {
		return "nothing enabled"
	}
	return strings.Join(names, ", ")
}

// normalizeMCPServers validates and cleans a server list. It returns the cleaned
// list plus what it found wrong, so a typo in config.json is dropped at load
// with a note on stderr and a typo from the settings form is refused with a
// sentence instead of a tool list that silently lost a server.
func normalizeMCPServers(in []MCPServerConfig) ([]MCPServerConfig, []string) {
	out := make([]MCPServerConfig, 0, len(in))
	var errs []string
	seen := map[string]bool{}

	for i, s := range in {
		name := strings.TrimSpace(s.Name)
		command := strings.TrimSpace(s.Command)
		if name == "" || command == "" {
			errs = append(errs, fmt.Sprintf("mcp server #%d needs both a name and a command", i+1))
			continue
		}
		if !validMCPName(name) {
			errs = append(errs, fmt.Sprintf("mcp server %q: the name may use letters, digits, '-' and '_' only", name))
			continue
		}
		key := strings.ToLower(name)
		if seen[key] {
			errs = append(errs, fmt.Sprintf("mcp server %q is listed twice; names have to be unique", name))
			continue
		}
		// An env entry that is not NAME=value would be handed to the child as
		// "-" nonsense: the server is refused here instead of starting broken.
		if bad := envProblems(name, s.Env); len(bad) > 0 {
			errs = append(errs, bad...)
			continue
		}
		seen[key] = true
		out = append(out, MCPServerConfig{
			Name:     name,
			Command:  command,
			Args:     s.Args,
			Env:      s.Env,
			Dir:      strings.TrimSpace(s.Dir),
			Disabled: s.Disabled,
		})
	}
	return out, errs
}

// validMCPName keeps the name usable as part of a function name and as a key.
func validMCPName(name string) bool {
	if name == "" || len(name) > 40 {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// envProblems checks the "NAME=value" shape of a server's environment list.
// A value is not inspected: it is the user's token, not this agent's.
func envProblems(server string, env []string) []string {
	var errs []string
	for _, kv := range env {
		name := kv
		if i := strings.IndexByte(kv, '='); i > 0 {
			name = kv[:i]
		}
		if i := strings.IndexByte(kv, '='); i <= 0 || name == "" {
			errs = append(errs, fmt.Sprintf("mcp server %q: env entry %q is not NAME=value", server, kv))
		}
	}
	return errs
}

// ---- JSON-RPC wire types -------------------------------------------------

type mcpError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type mcpMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *mcpError       `json:"error,omitempty"`
}

// numericID reads the id of an answer. This client only ever sends numeric ids,
// so anything else (a server that answers a notification, or invents an id) is
// ignored rather than delivered to the wrong caller.
func numericID(raw json.RawMessage) (int64, bool) {
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, false
	}
	return n, true
}

// mcpTool is one tool as tools/list advertises it.
type mcpTool struct {
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`

	// wire is the function name the model sees, and server is the entry that
	// answers it. Filled in by the manager when the catalogue is collected.
	wire   string
	server string
}

// ---- one server process --------------------------------------------------

// mcpClient is one running server. Requests are serialised per server by the
// pending map: several callers may be in flight, and each answer is delivered to
// the channel of the id it names.
type mcpClient struct {
	cfg    MCPServerConfig
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	tools  []mcpTool
	stderr tailBuffer

	writeMu sync.Mutex
	mu      sync.Mutex
	pending map[int64]chan mcpMessage
	nextID  int64
	// closed is the reason the server is gone, once it is. Read under mu.
	closed error
}

// tailBuffer keeps the end of a child's stderr: a server that dies explains
// itself there, and a server that chatters must not be allowed to grow the
// buffer without bound.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > 2048 {
		t.buf = t.buf[len(t.buf)-2048:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// startMCPClient spawns one server and runs the initialize handshake plus the
// first tools/list. A server that cannot start, or that does not answer, is
// reported as an error for that entry alone.
func startMCPClient(ctx context.Context, cfg MCPServerConfig) (*mcpClient, error) {
	cmd := exec.Command(cfg.Command, cfg.Args...)
	if cfg.Dir != "" {
		cmd.Dir = cfg.Dir
	}
	// The child gets the user's environment minus this agent's own credentials:
	// a server needs PATH (and whatever the entry names below), not the model
	// API key. The entry's own Env is appended after, so it can set or override.
	cmd.Env = append(scrubbedEnv(), cfg.Env...)
	cmd.SysProcAttr = mcpSysProcAttr()

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("%s: %v", cfg.Name, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("%s: %v", cfg.Name, err)
	}

	c := &mcpClient{cfg: cfg, cmd: cmd, stdin: stdin, pending: map[int64]chan mcpMessage{}}
	cmd.Stderr = &c.stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("could not start %q: %v", cfg.commandLine(), err)
	}
	go c.readLoop(stdout)

	initCtx, cancel := context.WithTimeout(ctx, mcpInitTimeout)
	defer cancel()

	var hello struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
		Capabilities json.RawMessage `json:"capabilities"`
	}
	err = c.request(initCtx, "initialize", map[string]any{
		"protocolVersion": mcpProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "dsagent", "version": "1.0"},
	}, &hello, nil)
	if err != nil {
		c.stop()
		return nil, fmt.Errorf("%s did not complete the MCP handshake: %v", cfg.Name, err)
	}
	// The spec requires this notification after the initialize result; a server
	// is entitled to refuse tools/list until it has been sent.
	c.notify("notifications/initialized", nil)

	listCtx, cancelList := context.WithTimeout(ctx, mcpListTimeout)
	defer cancelList()
	tools, err := c.listTools(listCtx)
	if err != nil {
		c.stop()
		return nil, fmt.Errorf("%s did not list its tools: %v", cfg.Name, err)
	}
	for i := range tools {
		tools[i].server = cfg.Name
	}
	c.tools = tools
	return c, nil
}

// listTools walks the catalogue, following nextCursor pages.
func (c *mcpClient) listTools(ctx context.Context) ([]mcpTool, error) {
	var out []mcpTool
	cursor := ""
	for page := 0; page < 50; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var res struct {
			Tools      []mcpTool `json:"tools"`
			NextCursor string    `json:"nextCursor"`
		}
		if err := c.request(ctx, "tools/list", params, &res, nil); err != nil {
			return out, err
		}
		out = append(out, res.Tools...)
		if res.NextCursor == "" {
			return out, nil
		}
		cursor = res.NextCursor
	}
	return out, nil
}

// request sends one JSON-RPC request and waits for the answer with that id.
// poke is called while waiting so a call that takes minutes does not look like
// a wedged run to the agent's own watchdog.
func (c *mcpClient) request(ctx context.Context, method string, params any, out any, poke func()) error {
	c.mu.Lock()
	if c.closed != nil {
		err := c.closed
		c.mu.Unlock()
		return err
	}
	c.nextID++
	id := c.nextID
	ch := make(chan mcpMessage, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	msg := mcpMessage{JSONRPC: "2.0", ID: json.RawMessage(strconv.FormatInt(id, 10)), Method: method}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			c.forget(id)
			return err
		}
		msg.Params = raw
	}
	if err := c.send(msg); err != nil {
		c.forget(id)
		return err
	}

	t := time.NewTicker(mcpPokeInterval)
	defer t.Stop()
	for {
		select {
		case got := <-ch:
			if got.Error != nil {
				return fmt.Errorf("the %s server refused %s: %s", c.cfg.Name, method, oneLine(got.Error.Message))
			}
			if out != nil {
				if err := json.Unmarshal(got.Result, out); err != nil {
					return fmt.Errorf("the %s server answered %s in a shape this client cannot read: %v", c.cfg.Name, method, err)
				}
			}
			return nil
		case <-ctx.Done():
			c.forget(id)
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("%s did not answer %s in time", c.cfg.Name, method)
			}
			return fmt.Errorf("%s: %s was cancelled before the server answered", c.cfg.Name, method)
		case <-t.C:
			if poke != nil {
				poke()
			}
		}
	}
}

// notify sends a JSON-RPC notification: no id, no answer.
func (c *mcpClient) notify(method string, params any) {
	msg := mcpMessage{JSONRPC: "2.0", Method: method}
	if params != nil {
		if raw, err := json.Marshal(params); err == nil {
			msg.Params = raw
		}
	}
	c.send(msg)
}

func (c *mcpClient) send(msg mcpMessage) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.stdin == nil {
		return errors.New("the connection to the " + c.cfg.Name + " server is closed")
	}
	if _, err := c.stdin.Write(b); err != nil {
		return fmt.Errorf("could not write to the %s server: %v", c.cfg.Name, err)
	}
	return nil
}

func (c *mcpClient) forget(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// readLoop is the one reader of the server's stdout. Splitting the dispatch from
// the waiting is what lets a notification, a server-to-client request and an
// answer arrive in any order without any of them blocking the others.
func (c *mcpClient) readLoop(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var msg mcpMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			continue // not a JSON-RPC message; the specification allows logging noise
		}
		switch {
		case msg.Method != "" && len(msg.ID) > 0:
			// A request from the server: sampling and roots are not implemented,
			// so say so instead of leaving the server waiting forever.
			c.send(mcpMessage{JSONRPC: "2.0", ID: msg.ID, Error: &mcpError{
				Code:    -32601,
				Message: "dsagent does not implement " + msg.Method,
			}})
		case msg.Method != "":
			// A notification (logging, progress, list_changed): nothing to do.
		default:
			id, ok := numericID(msg.ID)
			if !ok {
				continue
			}
			c.mu.Lock()
			ch, waiting := c.pending[id]
			delete(c.pending, id)
			c.mu.Unlock()
			if waiting {
				ch <- msg
			}
		}
	}

	// The output is at EOF, so the pipes are drained and it is safe to reap the
	// process. Wait() is what releases the handle; calling it earlier is how a
	// half-read answer gets truncated.
	waitErr := c.cmd.Wait()
	err := sc.Err()
	if err == nil {
		err = waitErr
	}
	if err == nil {
		err = errors.New("it closed its output")
	}
	c.fail(fmt.Errorf("the %s MCP server stopped: %v", c.cfg.Name, err))
}

// fail records why the server is unusable and releases every caller waiting on
// it - otherwise a request in flight would wait for its full timeout.
func (c *mcpClient) fail(err error) {
	c.mu.Lock()
	if c.closed == nil {
		if tail := strings.TrimSpace(c.stderr.String()); tail != "" {
			err = fmt.Errorf("%v (it printed: %s)", err, clipText(tail, 400))
		}
		c.closed = err
	}
	pending := c.pending
	c.pending = map[int64]chan mcpMessage{}
	c.mu.Unlock()

	for _, ch := range pending {
		select {
		case ch <- mcpMessage{Error: &mcpError{Code: -32000, Message: err.Error()}}:
		default:
		}
	}
}

// stop closes a server down: stdin first, which is how a well-behaved MCP server
// is supposed to notice it is done, then a kill if it ignores that.
func (c *mcpClient) stop() {
	if c.stdin != nil {
		c.stdin.Close()
	}
	deadline := time.Now().Add(mcpStopGrace)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		gone := c.closed != nil
		c.mu.Unlock()
		if gone {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if c.cmd != nil && c.cmd.Process != nil {
		// Either it has already exited (the kill fails harmlessly) or it is
		// ignoring stdin and has to go.
		c.cmd.Process.Kill()
	}
	c.fail(errors.New("this MCP server was shut down"))
}

// callTool runs one tools/call and returns the flattened text of the answer.
func (c *mcpClient) callTool(ctx context.Context, tool, rawArgs string, poke func()) (string, error) {
	args := json.RawMessage("{}")
	if s := strings.TrimSpace(rawArgs); s != "" {
		// The model's arguments are passed through as the server's arguments.
		// Malformed JSON is refused here rather than sent: a server that has to
		// reject it produces an error about its own schema, which reads like the
		// tool is broken instead of like the call was.
		if !json.Valid([]byte(s)) {
			return "", fmt.Errorf("the arguments for %s are not valid JSON, so nothing was sent to the %s server", tool, c.cfg.Name)
		}
		args = json.RawMessage(s)
	}

	ctx, cancel := context.WithTimeout(ctx, mcpToolTimeout)
	defer cancel()

	var res struct {
		Content []mcpContent `json:"content"`
		IsError bool         `json:"isError"`
	}
	if err := c.request(ctx, "tools/call", map[string]any{"name": tool, "arguments": args}, &res, poke); err != nil {
		return "", err
	}

	text := mcpContentText(res.Content)
	if res.IsError {
		return "ERROR: the " + c.cfg.Name + " server reported that " + tool + " failed:\n" + text, nil
	}
	return text, nil
}

// mcpContent is one block of a tools/call result.
type mcpContent struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Data     string `json:"data"`
	MimeType string `json:"mimeType"`
	URI      string `json:"uri"`
	Resource struct {
		URI      string `json:"uri"`
		MimeType string `json:"mimeType"`
		Text     string `json:"text"`
	} `json:"resource"`
}

// mcpContentText turns the content blocks of a tools/call result into the one
// string a tool result can be. Text is what the model can use; an image or a
// binary resource is named rather than dropped silently, because "the tool
// returned nothing" and "the tool returned a picture this transcript cannot
// carry" are different facts.
func mcpContentText(blocks []mcpContent) string {
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			parts = append(parts, b.Text)
		case "image", "audio":
			parts = append(parts, fmt.Sprintf("[%s content (%s, %d bytes) is not carried in a tool result]", b.Type, b.MimeType, len(b.Data)))
		case "resource":
			if b.Resource.Text != "" {
				parts = append(parts, b.Resource.Text)
			} else {
				parts = append(parts, "[resource "+b.Resource.URI+"]")
			}
		default:
			if b.Text != "" {
				parts = append(parts, b.Text)
			} else if b.URI != "" {
				parts = append(parts, "["+b.Type+" "+b.URI+"]")
			}
		}
	}
	if len(parts) == 0 {
		return "(the tool returned no content)"
	}
	return clip(strings.Join(parts, "\n"))
}

// ---- the catalogue the model sees ---------------------------------------

// mcpTarget is where one wire name goes: which server, which tool.
type mcpTarget struct {
	server string
	tool   string
}

// isMCPToolName reports whether a function name belongs to an MCP server. It is
// the only test the rest of the agent needs: the approval gate, the risk layer
// and the mode refusal all key off it.
func isMCPToolName(name string) bool {
	return strings.HasPrefix(name, mcpToolPrefix)
}

// sanitizeName rewrites a name into the character set the tool API accepts.
func sanitizeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// wireName builds the function name for one server tool, keeping it inside the
// API's 64 characters and unique against everything already taken.
//
// Long names are cut and given a fingerprint of the full name instead of just a
// prefix: two tools that differ only past the cut ("..._read_file_by_path_a" and
// "..._read_file_by_path_b") would otherwise collapse into one name and one of
// them would become unreachable.
func wireName(server, tool string, taken map[string]mcpTarget) string {
	base := sanitizeName(mcpToolPrefix + server + "__" + tool)
	if len(base) > mcpMaxNameLen {
		sum := sha256.Sum256([]byte(base))
		base = base[:mcpMaxNameLen-9] + "_" + hex.EncodeToString(sum[:4])
	}
	name := base
	for i := 2; ; i++ {
		if _, dup := taken[name]; !dup {
			return name
		}
		suffix := "_" + strconv.Itoa(i)
		cut := mcpMaxNameLen - len(suffix)
		if len(base) > cut {
			name = base[:cut] + suffix
		} else {
			name = base + suffix
		}
	}
}

// mcpToolSchema turns one advertised tool into an OpenAI-style function schema.
//
// The server's inputSchema is passed through, because it is the server that
// validates the arguments. What is repaired here is only the shape the tool API
// insists on: parameters an object, required an array - never null, which the
// API rejects with "null is not of type array" before it looks at the messages.
func mcpToolSchema(server string, tool mcpTool) map[string]any {
	params := map[string]any{
		"type":       "object",
		"properties": map[string]any{},
		"required":   []string{},
	}
	var schema map[string]any
	if len(tool.InputSchema) > 0 && json.Unmarshal(tool.InputSchema, &schema) == nil {
		if props, ok := schema["properties"].(map[string]any); ok {
			params["properties"] = props
		}
		if req, ok := schema["required"].([]any); ok {
			names := make([]string, 0, len(req))
			for _, r := range req {
				if s, ok := r.(string); ok && s != "" {
					names = append(names, s)
				}
			}
			params["required"] = names
		}
	}

	desc := strings.TrimSpace(tool.Description)
	if desc == "" {
		desc = "no description was given by the server"
	}
	// The name of the server is part of the contract: the model has to be able to
	// tell which process it is asking, and the approval card says the same name.
	desc = "[MCP " + server + "] " + desc

	return map[string]any{"type": "function", "function": map[string]any{
		"name": tool.wire, "description": desc, "parameters": params,
	}}
}

// ---- manager -------------------------------------------------------------

// mcpStatus is what /api/mcp and the settings pane need to know about one entry.
type mcpStatus struct {
	Name      string `json:"name"`
	Command   string `json:"command"`
	Connected bool   `json:"connected"`
	Tools     int    `json:"tools"`
	Disabled  bool   `json:"disabled,omitempty"`
	Error     string `json:"error,omitempty"`
}

// mcpManager owns the server processes for one agent.
type mcpManager struct {
	// refreshMu serialises start-up: the launch warm-up and the first task can
	// otherwise both decide the catalogue is missing and start every server
	// twice.
	refreshMu sync.Mutex

	mu       sync.Mutex
	sig      string
	ready    bool
	clients  map[string]*mcpClient
	wire     map[string]mcpTarget
	toolset  []map[string]any
	statuses []mcpStatus
}

func newMCPManager() *mcpManager {
	return &mcpManager{clients: map[string]*mcpClient{}, wire: map[string]mcpTarget{}}
}

// signature is a cheap fingerprint of the configured list. Servers are started
// again only when it changes, so a Save that touches an unrelated setting does
// not restart them mid-session.
func mcpSignature(list []MCPServerConfig) string {
	b, _ := json.Marshal(list)
	return string(b)
}

// stale reports whether the running catalogue belongs to a different server list
// - which is what the run loop uses to decide whether to say "connecting" first.
func (m *mcpManager) stale(list []MCPServerConfig) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.ready || m.sig != mcpSignature(list)
}

// ensure starts the configured servers if the current catalogue is stale. It is
// a no-op in the common case, where the launch warm-up got there first.
func (m *mcpManager) ensure(ctx context.Context, list []MCPServerConfig) {
	if !m.stale(list) {
		return
	}
	m.refresh(ctx, list, false)
}

// refresh replaces the whole catalogue. A server that fails to start is reported
// in its own status entry and skipped: one broken entry must not cost the run the
// other servers, let alone the built-in tools.
func (m *mcpManager) refresh(ctx context.Context, list []MCPServerConfig, force bool) {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()

	sig := mcpSignature(list)
	m.mu.Lock()
	if !force && m.ready && m.sig == sig {
		m.mu.Unlock()
		return
	}
	old := m.clients
	m.clients = map[string]*mcpClient{}
	m.wire = map[string]mcpTarget{}
	m.toolset = nil
	m.statuses = nil
	m.sig = sig
	m.ready = true
	m.mu.Unlock()

	// Shutting the previous generation down happens beside the new one: a server
	// that ignores stdin would otherwise delay every reconnect by its grace
	// period, one after another.
	var stopping sync.WaitGroup
	for _, c := range old {
		stopping.Add(1)
		go func(c *mcpClient) {
			defer stopping.Done()
			c.stop()
		}(c)
	}

	clients := map[string]*mcpClient{}
	wire := map[string]mcpTarget{}
	var schemas []map[string]any
	var statuses []mcpStatus

	for _, cfg := range list {
		if cfg.Disabled {
			statuses = append(statuses, mcpStatus{Name: cfg.Name, Command: cfg.commandLine(), Disabled: true})
			continue
		}
		if err := ctx.Err(); err != nil {
			statuses = append(statuses, mcpStatus{Name: cfg.Name, Command: cfg.commandLine(), Error: "not started: the run was cancelled"})
			continue
		}

		status := mcpStatus{Name: cfg.Name, Command: cfg.commandLine()}
		client, err := startMCPClient(ctx, cfg)
		if err != nil {
			status.Error = redact(err.Error())
			statuses = append(statuses, status)
			continue
		}
		clients[cfg.Name] = client
		status.Connected = true
		status.Tools = len(client.tools)
		statuses = append(statuses, status)

		for i := range client.tools {
			t := &client.tools[i]
			t.wire = wireName(cfg.Name, t.Name, wire)
			wire[t.wire] = mcpTarget{server: cfg.Name, tool: t.Name}
			schemas = append(schemas, mcpToolSchema(cfg.Name, *t))
		}
	}

	m.mu.Lock()
	m.clients = clients
	m.wire = wire
	m.toolset = schemas
	m.statuses = statuses
	m.mu.Unlock()
}

// schemas is the tool list to append to the built-in one.
func (m *mcpManager) schemas() []map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]map[string]any(nil), m.toolset...)
}

func (m *mcpManager) toolCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.toolset)
}

func (m *mcpManager) status() []mcpStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]mcpStatus(nil), m.statuses...)
}

// callTool routes one call to the server that advertised it.
func (m *mcpManager) callTool(ctx context.Context, name, rawArgs string, poke func()) (string, error) {
	m.mu.Lock()
	target, known := m.wire[name]
	client := m.clients[target.server]
	m.mu.Unlock()

	if !known || client == nil {
		return "", fmt.Errorf("no MCP server is serving %q any more - the server list changed while this task was running, so nothing was sent. The built-in tools still work; tell the user to reload the MCP servers to get this one back.", name)
	}
	return client.callTool(ctx, target.tool, rawArgs, poke)
}

// close shuts every server down. Called when the agent goes away; the process
// exits with its children on Windows only because they are killed here first.
func (m *mcpManager) close() {
	m.mu.Lock()
	clients := m.clients
	m.clients = map[string]*mcpClient{}
	m.wire = map[string]mcpTarget{}
	m.toolset = nil
	m.mu.Unlock()

	for _, c := range clients {
		c.stop()
	}
}
