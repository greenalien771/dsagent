package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// The MCP client is tested against a real child process, not a stub: these
// tests re-execute this test binary as a fake server (the standard Go
// helper-process pattern). That is the only way to verify the parts that matter
// - a JSON-RPC handshake over a pipe, paginated tools/list, tools/call with an
// answer that is not JSON, and a server that dies in the middle of a call -
// because every one of those is a property of the pipe rather than of a
// function.

// helperServerConfig is how the tests point the client at that fake server.
func helperServerConfig(t *testing.T) MCPServerConfig {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("cannot find the test binary: %v", err)
	}
	return MCPServerConfig{
		Name:    "fake",
		Command: exe,
		Args:    []string{"-test.run=TestMCPHelperServer"},
		Env:     []string{"DSAGENT_MCP_HELPER=1"},
	}
}

// TestMCPHelperServer is not a test. When the client starts this binary as a
// server, this function is what runs in it.
func TestMCPHelperServer(t *testing.T) {
	if os.Getenv("DSAGENT_MCP_HELPER") != "1" {
		t.Skip("helper process for the MCP client tests")
	}
	runHelperServer(os.Stdin, os.Stdout)
	os.Exit(0)
}

// runHelperServer is a minimal MCP server: initialize, a two-page tools/list,
// and tools/call for the tools it advertises.
func runHelperServer(in io.Reader, out io.Writer) {
	enc := json.NewEncoder(out)
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64<<10), 1<<20)

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
				Cursor    string          `json:"cursor"`
			} `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}
		// Notifications (initialized) and answers meant for us need no reply.
		if msg.Method == "" || len(msg.ID) == 0 {
			continue
		}

		reply := func(result any) {
			enc.Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
		}
		fail := func(code int, text string) {
			enc.Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "error": map[string]any{"code": code, "message": text}})
		}

		switch msg.Method {
		case "initialize":
			reply(map[string]any{
				"protocolVersion": mcpProtocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "fake", "version": "1.0"},
			})
		case "tools/list":
			if msg.Params.Cursor == "" {
				reply(map[string]any{
					"tools": []any{
						map[string]any{
							"name":        "echo",
							"description": "Echo the text back.",
							"inputSchema": map[string]any{
								"type":       "object",
								"properties": map[string]any{"text": map[string]any{"type": "string"}},
								"required":   []string{"text"},
							},
						},
						map[string]any{"name": "fail", "description": "Always fails.", "inputSchema": map[string]any{"type": "object"}},
						map[string]any{"name": "die", "description": "Takes the server down."},
					},
					"nextCursor": "page2",
				})
			} else {
				// A second page with a tool name the API would reject as-is, so the
				// wire-name sanitiser is exercised against a real catalogue.
				reply(map[string]any{"tools": []any{map[string]any{"name": "read file/by-path"}}})
			}
		case "tools/call":
			switch msg.Params.Name {
			case "echo":
				var a struct {
					Text string `json:"text"`
				}
				json.Unmarshal(msg.Params.Arguments, &a)
				reply(map[string]any{"content": []any{
					map[string]any{"type": "text", "text": "echo: " + a.Text},
					map[string]any{"type": "image", "mimeType": "image/png", "data": "iVBORw0KGgo="},
				}})
			case "fail":
				reply(map[string]any{
					"content": []any{map[string]any{"type": "text", "text": "it broke"}},
					"isError": true,
				})
			case "die":
				// No answer at all: the caller has to notice the process is gone
				// instead of waiting for its own timeout.
				os.Exit(3)
			default:
				fail(-32602, "unknown tool "+msg.Params.Name)
			}
		default:
			fail(-32601, "method not implemented: "+msg.Method)
		}
	}
}

func startHelperClient(t *testing.T) *mcpClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := startMCPClient(ctx, helperServerConfig(t))
	if err != nil {
		t.Fatalf("the client could not start its server: %v", err)
	}
	t.Cleanup(c.stop)
	return c
}

// The handshake has to walk both pages of tools/list: a server that paginates is
// the normal case for a large catalogue, and a client that reads only the first
// page silently loses tools.
func TestMCPClientHandshakeAndToolCatalogue(t *testing.T) {
	c := startHelperClient(t)

	var names []string
	for _, tool := range c.tools {
		names = append(names, tool.Name)
	}
	got := strings.Join(names, ",")
	want := "echo,fail,die,read file/by-path"
	if got != want {
		t.Fatalf("tool catalogue = %q, want %q", got, want)
	}
	for _, tool := range c.tools {
		if tool.server != "fake" {
			t.Errorf("tool %q is not attributed to its server: %q", tool.Name, tool.server)
		}
	}
}

// A call has to reach the server and the answer has to come back as text the
// model can use - including the block it cannot carry, which must be named
// rather than silently dropped.
func TestMCPClientToolCallAndContent(t *testing.T) {
	c := startHelperClient(t)
	ctx := context.Background()

	out, err := c.callTool(ctx, "echo", `{"text":"hello"}`, nil)
	if err != nil {
		t.Fatalf("echo failed: %v", err)
	}
	if !strings.Contains(out, "echo: hello") {
		t.Errorf("the tool's text did not come back: %q", out)
	}
	if !strings.Contains(out, "image content") {
		t.Errorf("the dropped image block is not named in the result: %q", out)
	}

	// A tool that reports a failure is not a broken transport: the sentence has
	// to reach the model as the result, with the server's own words.
	out, err = c.callTool(ctx, "fail", `{}`, nil)
	if err != nil {
		t.Fatalf("a tool that reports a failure must not return a transport error: %v", err)
	}
	if !strings.HasPrefix(out, "ERROR:") || !strings.Contains(out, "it broke") {
		t.Errorf("the failure was not reported as a failed tool call: %q", out)
	}

	// The server refuses an unknown tool: that is an error, and it says who
	// refused what.
	if _, err := c.callTool(ctx, "nope", `{}`, nil); err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Errorf("a refused call did not report the server's reason: %v", err)
	}

	// Arguments that are not JSON are refused before anything is sent, so the
	// server cannot blame its own schema for a malformed call.
	if _, err := c.callTool(ctx, "echo", `{"text":`, nil); err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Errorf("malformed arguments were sent anyway: %v", err)
	}
}

// A server that disappears mid-call must be noticed when its stdout ends, not
// when the caller's ten-minute tool timeout expires.
func TestMCPClientNoticesAServerThatDies(t *testing.T) {
	c := startHelperClient(t)

	done := make(chan error, 1)
	go func() {
		_, err := c.callTool(context.Background(), "die", `{}`, nil)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a call to a server that exited reported success")
		}
		if !strings.Contains(err.Error(), "fake") {
			t.Errorf("the error does not name the server: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the call hung after the server exited instead of reporting it")
	}
}

// The catalogue the model is offered has to be valid JSON Schema with a
// function name the API accepts, whatever the server called its tools.
func TestMCPSchemasAreValidAndRoutable(t *testing.T) {
	m := newMCPManager()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	m.refresh(ctx, []MCPServerConfig{helperServerConfig(t)}, false)
	t.Cleanup(m.close)

	schemas := m.schemas()
	if len(schemas) != 4 {
		t.Fatalf("the manager published %d tools, want 4", len(schemas))
	}

	raw, err := json.Marshal(schemas)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), `"required":null`) {
		t.Fatalf("a schema sends required as null: %s", raw)
	}

	seen := map[string]bool{}
	for _, s := range schemas {
		fn := s["function"].(map[string]any)
		name := fn["name"].(string)
		if !isMCPToolName(name) {
			t.Errorf("%q is not recognisable as an MCP tool", name)
		}
		if len(name) > mcpMaxNameLen {
			t.Errorf("%q is %d characters; the API allows %d", name, len(name), mcpMaxNameLen)
		}
		for _, r := range name {
			ok := r == '_' || r == '-' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
			if !ok {
				t.Errorf("%q contains %q, which is not a legal function-name character", name, r)
			}
		}
		if seen[name] {
			t.Errorf("%q is published twice", name)
		}
		seen[name] = true

		if !strings.Contains(fn["description"].(string), "[MCP fake]") {
			t.Errorf("%q does not say which server it belongs to: %v", name, fn["description"])
		}
		params := fn["parameters"].(map[string]any)
		if params["type"] != "object" {
			t.Errorf("%q: parameters.type = %v, want object", name, params["type"])
		}
		if _, ok := params["properties"].(map[string]any); !ok {
			t.Errorf("%q: properties is not an object", name)
		}
		if _, ok := params["required"].([]string); !ok {
			t.Errorf("%q: required is not a string array (%T)", name, params["required"])
		}
	}

	// echo's schema is passed through, so the server's own argument still is
	// described and still is required.
	var echoWire string
	for _, s := range schemas {
		fn := s["function"].(map[string]any)
		if strings.Contains(fn["description"].(string), "Echo the text back") {
			echoWire = fn["name"].(string)
			props := fn["parameters"].(map[string]any)["properties"].(map[string]any)
			if _, ok := props["text"]; !ok {
				t.Errorf("echo lost its arguments: %v", props)
			}
			req := fn["parameters"].(map[string]any)["required"].([]string)
			if len(req) != 1 || req[0] != "text" {
				t.Errorf("echo lost its required list: %v", req)
			}
		}
	}
	if echoWire == "" {
		t.Fatal("echo is not in the catalogue")
	}

	// And the name the model was given is the name that routes back.
	out, err := m.callTool(context.Background(), echoWire, `{"text":"routed"}`, nil)
	if err != nil {
		t.Fatalf("routing %q back to its server failed: %v", echoWire, err)
	}
	if !strings.Contains(out, "echo: routed") {
		t.Fatalf("the call did not reach the right tool: %q", out)
	}

	// A name that is no longer served is refused with a sentence, not a panic.
	if _, err := m.callTool(context.Background(), "mcp__gone__tool", `{}`, nil); err == nil {
		t.Fatal("a call to an unknown MCP tool succeeded")
	}
}

// An entry that cannot start is one status line, not a failure of the agent: the
// other servers and every built-in tool still work.
func TestMCPServerThatCannotStartIsReported(t *testing.T) {
	m := newMCPManager()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	m.refresh(ctx, []MCPServerConfig{
		{Name: "ghost", Command: "dsagent-no-such-server-binary"},
		{Name: "off", Command: "whatever", Disabled: true},
		helperServerConfig(t),
	}, false)
	t.Cleanup(m.close)

	status := map[string]mcpStatus{}
	for _, s := range m.status() {
		status[s.Name] = s
	}
	if got := status["ghost"]; got.Connected || got.Error == "" {
		t.Errorf("a server that cannot start is not reported: %+v", got)
	}
	if got := status["off"]; !got.Disabled || got.Connected {
		t.Errorf("a disabled entry is not reported as disabled: %+v", got)
	}
	if got := status["fake"]; !got.Connected || got.Tools != 4 {
		t.Errorf("the working server was not started: %+v", got)
	}
	if m.toolCount() != 4 {
		t.Errorf("the catalogue has %d tools, want the 4 from the server that worked", m.toolCount())
	}
}

// The names the model sees are built, not passed through: they have to stay
// inside the API's 64 characters and stay unique even when two tools differ only
// past the cut.
func TestMCPWireNames(t *testing.T) {
	taken := map[string]mcpTarget{}
	long := strings.Repeat("a", 60)
	a := wireName("srv", long+"one", taken)
	taken[a] = mcpTarget{}
	b := wireName("srv", long+"two", taken)
	taken[b] = mcpTarget{}

	if a == b {
		t.Fatalf("two long tool names collapsed into %q", a)
	}
	for _, n := range []string{a, b} {
		if len(n) > mcpMaxNameLen {
			t.Errorf("%q is %d characters, over the %d the API allows", n, len(n), mcpMaxNameLen)
		}
	}

	// A name that is legal but already taken gets a suffix rather than colliding.
	first := wireName("fs", "read", taken)
	taken[first] = mcpTarget{}
	second := wireName("fs", "read", taken)
	if first == second {
		t.Fatalf("%q was handed out twice", first)
	}

	// Server and tool names the API would reject are sanitised, and the result
	// still routes: the mapping is by wire name, not by re-parsing it.
	odd := wireName("my server", "read file/by-path", map[string]mcpTarget{})
	for _, r := range odd {
		if !(r == '_' || r == '-' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			t.Fatalf("%q keeps an illegal character %q", odd, r)
		}
	}
	if !isMCPToolName(odd) {
		t.Errorf("%q is not recognisable as an MCP tool", odd)
	}
}

// MCP tools have to be gated like the side effects they are: always offered in
// Agent mode, offered nowhere else, always approved, and classified as elevated
// so the card says where the call is going.
func TestMCPToolsAreGatedLikeSideEffects(t *testing.T) {
	ag := NewAgent(&Config{})
	ag.mcp.toolset = []map[string]any{{"type": "function", "function": map[string]any{"name": "mcp__fake__echo"}}}

	hasMCP := func(mode string) bool {
		for _, tl := range ag.toolList(mode) {
			if isMCPToolName(tl["function"].(map[string]any)["name"].(string)) {
				return true
			}
		}
		return false
	}
	if !hasMCP(ModeAgent) {
		t.Error("Agent mode is not offered the MCP tools")
	}
	if hasMCP(ModePlan) || hasMCP(ModeAsk) {
		t.Error("a read-only mode is offered a third-party tool that may write")
	}
	for _, mode := range []string{ModePlan, ModeAsk} {
		if _, denied := modeDenies(mode, "mcp__fake__echo"); !denied {
			t.Errorf("%s mode does not refuse an MCP tool at runtime", mode)
		}
	}
	if _, denied := modeDenies(ModeAgent, "mcp__fake__echo"); denied {
		t.Error("Agent mode refuses an MCP tool")
	}

	if !needsApproval("mcp__fake__echo") {
		t.Fatal("an MCP call must be shown to the user before it runs")
	}
	risk := classifyTool("mcp__fake__echo", `{"text":"hi"}`)
	if risk.Level != riskElevated || len(risk.Reasons) == 0 {
		t.Fatalf("an MCP call is classified %q (%v), want elevated with a reason", risk.Level, risk.Reasons)
	}
	if !strings.Contains(strings.Join(risk.Reasons, " "), "MCP") {
		t.Errorf("the approval card does not say where the call goes: %v", risk.Reasons)
	}
}

// The whole path one task takes has to end at the server: execTool is where the
// approval gate, the mode refusal and the dispatch meet, and an MCP tool that
// only works when called directly is a tool the model can never use.
func TestMCPToolCallThroughExecTool(t *testing.T) {
	cfg := &Config{AutoApprove: true}
	ag := NewAgent(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ag.mcp.refresh(ctx, []MCPServerConfig{helperServerConfig(t)}, false)
	t.Cleanup(ag.mcp.close)

	wire := ""
	for _, s := range ag.mcp.schemas() {
		fn := s["function"].(map[string]any)
		if strings.Contains(fn["description"].(string), "Echo the text back") {
			wire = fn["name"].(string)
		}
	}
	if wire == "" {
		t.Fatal("the server's echo tool is not in the catalogue")
	}

	emit := func(kind, text string) {}
	tc := ToolCall{ID: "call_1", Type: "function"}
	tc.Function.Name = wire
	tc.Function.Arguments = `{"text":"through execTool"}`

	out := ag.execTool(ctx, t.TempDir(), tc, ModeAgent, emit)
	if !strings.Contains(out, "echo: through execTool") {
		t.Fatalf("the call did not come back with the server's answer: %q", out)
	}

	// In a read-only mode the same call is refused before anything is started,
	// and the refusal says which posture refused it.
	tc.Function.Arguments = `{"text":"second"}`
	out = ag.execTool(ctx, t.TempDir(), tc, ModePlan, emit)
	if !strings.Contains(out, "DENIED by plan mode") {
		t.Fatalf("plan mode did not refuse the call: %q", out)
	}

	// A call to a tool nobody serves reports it as an error result rather than
	// panicking or running something else.
	tc.Function.Name = "mcp__nobody__nothing"
	out = ag.execTool(ctx, t.TempDir(), tc, ModeAgent, emit)
	if !strings.HasPrefix(out, "ERROR:") {
		t.Fatalf("a call to a missing MCP tool did not report an error: %q", out)
	}
}

// A hand-edited config.json is validated the same way a form post is.
func TestNormalizeMCPServers(t *testing.T) {
	list, problems := normalizeMCPServers([]MCPServerConfig{
		{Name: "fs", Command: "npx", Args: []string{"-y", "server-filesystem"}, Env: []string{"TOKEN=abc"}},
		{Name: " ", Command: "x"},
		{Name: "no-command"},
		{Name: "bad name", Command: "x"},
		{Name: "FS", Command: "dupe"},
		{Name: "off", Command: "x", Disabled: true},
		{Name: "env", Command: "x", Env: []string{"NOEQUALS"}},
	})
	if len(list) != 2 {
		t.Fatalf("kept %d entries, want fs and off: %+v", len(list), list)
	}
	if list[0].Name != "fs" || len(list[0].Args) != 2 {
		t.Errorf("the valid entry was altered: %+v", list[0])
	}
	if !list[1].Disabled {
		t.Errorf("a disabled entry was enabled: %+v", list[1])
	}
	// Six problems: no name, no command, illegal name, duplicate, bad env - and
	// the count is asserted so a rule that stops firing is caught.
	if len(problems) != 5 {
		t.Errorf("reported %d problems, want 5: %v", len(problems), problems)
	}
}

// Shutting the agent down has to take its servers with it. Nothing on screen can
// show a server that outlived the process, so a close that only forgets the
// catalogue would leave a leaked child behind with no way to notice it.
func TestMCPManagerCloseStopsEveryServer(t *testing.T) {
	m := newMCPManager()
	t.Cleanup(m.close)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg := helperServerConfig(t)
	m.refresh(ctx, []MCPServerConfig{cfg}, false)

	m.mu.Lock()
	client := m.clients[cfg.Name]
	m.mu.Unlock()
	if client == nil || len(m.schemas()) == 0 {
		t.Fatal("the server did not come up, so there is nothing to close")
	}

	m.close()

	if n := m.toolCount(); n != 0 {
		t.Errorf("%d tools are still offered after the agent closed", n)
	}
	if n := len(m.schemas()); n != 0 {
		t.Errorf("%d schemas survived the close", n)
	}

	client.mu.Lock()
	closed := client.closed
	client.mu.Unlock()
	if closed == nil {
		t.Error("the server was never marked closed, so a waiting call would hang")
	}

	// The wire name is gone with the catalogue: a call that was composed before
	// the close is refused instead of being written to a dead pipe.
	if _, err := m.callTool(context.Background(), "mcp__fake__echo", `{"text":"x"}`, nil); err == nil {
		t.Error("a call after close was accepted")
	}
}

// waitGone waits until the client has noticed that its process is gone, so a
// test that kills a server does not race the reader that reports it.
func waitGone(t *testing.T, c *mcpClient) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if c.dead() != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the client never noticed that its server was gone")
}

// A server that is gone is started again by the next call that needs it. This is
// the case that made a killed SparrowMCP look like a broken handshake: the
// handshake had worked, but nothing ever started the process again, so every
// later call repeated the same exit status until the agent itself restarted.
func TestMCPDeadServerIsStartedAgainByTheNextCall(t *testing.T) {
	m := newMCPManager()
	t.Cleanup(m.close)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cfg := helperServerConfig(t)
	m.refresh(ctx, []MCPServerConfig{cfg}, false)

	m.mu.Lock()
	client := m.clients[cfg.Name]
	m.mu.Unlock()
	if client == nil {
		t.Fatal("the helper server did not start")
	}
	before := client.pid()

	if err := client.cmd.Process.Kill(); err != nil {
		t.Fatalf("could not stop the helper server: %v", err)
	}
	waitGone(t, client)

	out, err := m.callTool(ctx, "mcp__fake__echo", `{"text":"after a restart"}`, nil)
	if err != nil {
		t.Fatalf("the call after the server died was not recovered: %v", err)
	}
	if !strings.Contains(out, "echo: after a restart") {
		t.Fatalf("the revived server did not answer the call: %q", out)
	}

	m.mu.Lock()
	replacement := m.clients[cfg.Name]
	m.mu.Unlock()
	if replacement == nil {
		t.Fatal("no server is registered after the revive")
	}
	if replacement.pid() == before {
		t.Error("the old process id is still in the table, so nothing was started again")
	}
	if m.toolCount() == 0 {
		t.Error("the revived server's tools are not offered again")
	}
}

// Stop means stop. The server goes, its tools leave the tool list, the status
// says it was stopped rather than crashed, a call that needs it is refused
// instead of restarting it, and only an explicit start brings it back. A stop
// that the next call silently undoes is worse than no stop: the user would
// believe a process was gone while it was still running.
func TestMCPStopIsRealAndStaysStopped(t *testing.T) {
	m := newMCPManager()
	t.Cleanup(m.close)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cfg := helperServerConfig(t)
	m.refresh(ctx, []MCPServerConfig{cfg}, false)

	m.mu.Lock()
	client := m.clients[cfg.Name]
	m.mu.Unlock()
	if client == nil {
		t.Fatal("the helper server did not start")
	}
	pid := client.pid()

	if err := m.stopServer(cfg.Name); err != nil {
		t.Fatalf("stop failed: %v", err)
	}
	waitGone(t, client)
	if n := m.toolCount(); n != 0 {
		t.Errorf("%d tools are still offered after the stop", n)
	}
	var status mcpStatus
	for _, s := range m.status() {
		if s.Name == cfg.Name {
			status = s
		}
	}
	if status.Connected || status.PID != 0 {
		t.Errorf("the status still reports it as running: %+v", status)
	}
	if !strings.Contains(status.Error, "stopped") {
		t.Errorf("the status does not say it was stopped: %+v", status)
	}

	if _, err := m.callTool(ctx, "mcp__fake__echo", `{"text":"x"}`, nil); err == nil {
		t.Fatal("a call to a stopped server was accepted")
	} else if !strings.Contains(err.Error(), "stopped") {
		t.Errorf("the refusal does not explain that the server was stopped: %v", err)
	}

	if err := m.startServer(ctx, cfg.Name); err != nil {
		t.Fatalf("start failed: %v", err)
	}
	m.mu.Lock()
	revived := m.clients[cfg.Name]
	m.mu.Unlock()
	if revived == nil {
		t.Fatal("start did not register a server")
	}
	if revived.pid() == pid {
		t.Error("start reused the stopped process instead of running a new one")
	}
	out, err := m.callTool(ctx, "mcp__fake__echo", `{"text":"back"}`, nil)
	if err != nil || !strings.Contains(out, "echo: back") {
		t.Fatalf("the restarted server did not answer: %q %v", out, err)
	}
}

// The tool the agent uses for all of this has to reach the same code the
// settings endpoint reaches: status, stop, start, and the tool list that follows
// from them.
func TestMCPControlToolStopsAndStartsAServer(t *testing.T) {
	cfg := &Config{AutoApprove: true}
	ag := NewAgent(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ag.mcp.refresh(ctx, []MCPServerConfig{helperServerConfig(t)}, false)
	t.Cleanup(ag.mcp.close)

	call := func(args string) string {
		tc := ToolCall{ID: "mcp_control", Type: "function"}
		tc.Function.Name = "mcp_control"
		tc.Function.Arguments = args
		return ag.execTool(ctx, t.TempDir(), tc, ModeAgent, func(string, string) {})
	}

	if out := call(`{"action":"status"}`); !strings.Contains(out, "running (pid ") {
		t.Fatalf("status does not report the running server: %q", out)
	}
	if out := call(`{"action":"stop","name":"fake"}`); strings.Contains(out, "running (pid ") {
		t.Fatalf("the server is still reported as running after a stop: %q", out)
	} else if !strings.Contains(out, "stopped") {
		t.Fatalf("the stop is not visible in the report: %q", out)
	}
	if out := call(`{"action":"start","name":"fake"}`); !strings.Contains(out, "running (pid ") {
		t.Fatalf("the server did not come back: %q", out)
	}
	if out := call(`{"action":"wobble"}`); !strings.HasPrefix(out, "ERROR:") {
		t.Fatalf("an unknown action was not refused: %q", out)
	}
}

// The report has to answer for the process that is running now, not for the one
// that was running when the list was built. A server killed from outside used to
// keep reading as "connected, N tools" until something asked it something, which
// is the state the settings pane was showing while every call failed.
func TestMCPStatusNoticesAServerKilledFromOutside(t *testing.T) {
	m := newMCPManager()
	t.Cleanup(m.close)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cfg := helperServerConfig(t)
	m.refresh(ctx, []MCPServerConfig{cfg}, false)

	m.mu.Lock()
	client := m.clients[cfg.Name]
	m.mu.Unlock()
	if client == nil {
		t.Fatal("the helper server did not start")
	}

	status := func() mcpStatus {
		t.Helper()
		for _, s := range m.status() {
			if s.Name == cfg.Name {
				return s
			}
		}
		t.Fatalf("the server %q is missing from the report", cfg.Name)
		return mcpStatus{}
	}

	if s := status(); !s.Connected || s.PID == 0 {
		t.Fatalf("a running server is not reported as running: %+v", s)
	}

	if err := client.cmd.Process.Kill(); err != nil {
		t.Fatalf("could not stop the helper server: %v", err)
	}
	waitGone(t, client)

	s := status()
	if s.Connected || s.Tools != 0 || s.PID != 0 {
		t.Fatalf("a killed server is still reported as running: %+v", s)
	}
	if s.Error == "" {
		t.Error("the report does not say why the server is gone")
	}
}
