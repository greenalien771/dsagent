package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// A server entry carries more than a command line, and each setting has to
// reach the thing that uses it: the catalogue, the schema the model reads, the
// approval gate, or the call's own deadline.

func TestMCPConfigSettingsValidate(t *testing.T) {
	ok := []MCPServerConfig{
		{Name: "fs", Command: "npx", Approval: "allow"},
		{Name: "web", Command: "node", Approval: "block", Tools: []string{"open", " close "}, TimeoutSec: 45},
		{Name: "plain", Command: "run"},
	}
	cleaned, errs := normalizeMCPServers(ok)
	if len(errs) != 0 {
		t.Fatalf("valid settings were refused: %v", errs)
	}
	if len(cleaned) != 3 {
		t.Fatalf("%d entries survived, want 3", len(cleaned))
	}
	if cleaned[1].TimeoutSec != 45 || len(cleaned[1].Tools) != 2 || cleaned[1].Tools[1] != "close" {
		t.Fatalf("the settings were not kept: %+v", cleaned[1])
	}
	// An entry that names no policy asks. That is the safe reading of a config
	// written before the field existed, and it must not become "allow" by
	// accident.
	if cleaned[2].approval() != mcpApprovalAsk {
		t.Fatalf("a server without a policy is %q, want ask", cleaned[2].approval())
	}

	bad := []MCPServerConfig{
		{Name: "typo", Command: "run", Approval: "alow"},
		{Name: "long", Command: "run", TimeoutSec: 99999},
	}
	_, errs = normalizeMCPServers(bad)
	if len(errs) != 2 {
		t.Fatalf("%d problems reported, want 2: %v", len(errs), errs)
	}
	// The message has to name the field and the allowed values, because the
	// symptom of a misspelled policy is "nothing happened".
	if !strings.Contains(errs[0], "approval") || !strings.Contains(errs[0], "ask") {
		t.Fatalf("the refusal does not say what approval accepts: %q", errs[0])
	}
}

func TestMCPToolFilter(t *testing.T) {
	cfg := MCPServerConfig{Name: "srv", Command: "x", Tools: []string{"Read", "close"}}
	if !cfg.wantsTool("read") || !cfg.wantsTool("CLOSE") {
		t.Fatal("the allow-list is case-sensitive; a tool name from a server should not be")
	}
	if cfg.wantsTool("write") {
		t.Fatal("a tool outside the allow-list was accepted")
	}
	// Empty means everything: a filter nobody set must not hide every tool.
	if !(&MCPServerConfig{Name: "s"}).wantsTool("anything") {
		t.Fatal("a server with no allow-list hid a tool")
	}
}

func TestMCPToolTimeout(t *testing.T) {
	if got := (&MCPServerConfig{}).toolTimeout(); got != mcpToolTimeout {
		t.Fatalf("default timeout %s, want %s", got, mcpToolTimeout)
	}
	if got := (&MCPServerConfig{TimeoutSec: 45}).toolTimeout().Seconds(); got != 45 {
		t.Fatalf("configured timeout %v, want 45", got)
	}
	// Out of range is clamped rather than allowed to disable the limit.
	if got := (&MCPServerConfig{TimeoutSec: 100000}).toolTimeout().Seconds(); got != 3600 {
		t.Fatalf("timeout was not clamped: %v", got)
	}
}

// The policy is part of the description: a model that knows a call will not
// stop for a card plans around it instead of spending a step finding out.
func TestMCPPolicyReachesTheSchema(t *testing.T) {
	tool := mcpTool{Name: "snapshot", Description: "the page as text"}
	tool.wire = "mcp__srv__snapshot"

	ask := mcpToolSchema(MCPServerConfig{Name: "srv"}, tool)
	desc := ask["function"].(map[string]any)["description"].(string)
	if !strings.Contains(desc, "[MCP srv]") || strings.Contains(desc, "trusted") {
		t.Fatalf("the default description says the wrong thing: %q", desc)
	}
	allow := mcpToolSchema(MCPServerConfig{Name: "srv", Approval: "allow"}, tool)
	if d := allow["function"].(map[string]any)["description"].(string); !strings.Contains(d, "trusted") {
		t.Fatalf("the trusted policy is not in the description: %q", d)
	}
	block := mcpToolSchema(MCPServerConfig{Name: "srv", Approval: "block"}, tool)
	if d := block["function"].(map[string]any)["description"].(string); !strings.Contains(d, "blocked") {
		t.Fatalf("the blocked policy is not in the description: %q", d)
	}
}

// The gate: a trusted server does not stop for a card, a blocked one is refused
// before anything is sent, and the ordinary one still asks.
func TestMCPApprovalPolicyIsEnforced(t *testing.T) {
	a := newTestAgent(t)
	a.mcp.list = []MCPServerConfig{
		{Name: "trusted", Command: "x", Approval: "allow"},
		{Name: "blocked", Command: "x", Approval: "block"},
		{Name: "normal", Command: "x"},
	}
	a.mcp.wire = map[string]mcpTarget{
		"mcp__trusted__go": {server: "trusted", tool: "go"},
		"mcp__blocked__go": {server: "blocked", tool: "go"},
		"mcp__normal__go":  {server: "normal", tool: "go"},
	}

	if a.approvalNeeded("mcp__trusted__go") {
		t.Error("a trusted server still asks for approval")
	}
	if !a.approvalNeeded("mcp__normal__go") {
		t.Error("an ordinary MCP call no longer asks")
	}
	// The built-in gate is untouched by any of this.
	if !a.approvalNeeded("run_shell") {
		t.Error("run_shell stopped asking")
	}

	// A blocked server refuses the call before the ledger, the card and the
	// server: nothing may be sent, and the sentence has to say how to undo it.
	tc := ToolCall{ID: "b1", Type: "function"}
	tc.Function.Name = "mcp__blocked__go"
	tc.Function.Arguments = `{}`
	out := a.execTool(context.Background(), a.cfg.WorkDir, tc, ModeAgent, func(string, string) {})
	if !strings.HasPrefix(out, "REFUSED") || !strings.Contains(out, "block") {
		t.Fatalf("a blocked server did not refuse the call: %q", out)
	}
}

// A trusted MCP call must really run without a card: with the run context
// alive and no approval channel, a prompt would hang until the watchdog. The
// server here is not running, so the call fails fast - the point is that it got
// as far as trying to reach the server at all.
func TestTrustedMCPCallDoesNotPrompt(t *testing.T) {
	a := newTestAgent(t)
	a.cfg.AutoApprove = false
	a.mcp.list = []MCPServerConfig{{Name: "trusted", Command: "x", Approval: "allow"}}
	a.mcp.wire = map[string]mcpTarget{"mcp__trusted__go": {server: "trusted", tool: "go"}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan string, 1)
	go func() {
		tc := ToolCall{ID: "t1", Type: "function"}
		tc.Function.Name = "mcp__trusted__go"
		tc.Function.Arguments = `{}`
		done <- a.execTool(ctx, a.cfg.WorkDir, tc, ModeAgent, func(string, string) {})
	}()

	select {
	case out := <-done:
		if strings.Contains(out, "DENIED") {
			t.Fatalf("the call was denied instead of run: %q", out)
		}
	case <-timeAfterSeconds(10):
		t.Fatal("a trusted MCP call waited for an approval card that the policy says should not appear")
	}
}

// ---- task effort --------------------------------------------------------

// The three levels are prompt text, and each one has to say the thing it is for.
// They are the answer to "a plain instruction costs five steps", so the direct
// text has to actually forbid the survey.
func TestEffortBlocks(t *testing.T) {
	direct := effortBlock(effortDirect)
	for _, want := range []string{"first step", "do not survey", "genuinely complex", "not a cap"} {
		if !strings.Contains(direct, want) {
			t.Errorf("the direct block does not say %q", want)
		}
	}
	thorough := effortBlock(effortThorough)
	if !strings.Contains(thorough, "THOROUGH") || !strings.Contains(thorough, "verify") {
		t.Errorf("the thorough block lost its point: %q", thorough)
	}
	balanced := effortBlock(effortBalanced)
	if !strings.Contains(balanced, "BALANCED") || !strings.Contains(balanced, "ask_user") {
		t.Errorf("the balanced block does not mention ask_user: %q", balanced)
	}
	// An unknown level is balanced, not the shortest one: a typo must not make
	// every task skip its investigation.
	if effortBlock(normalizeEffort("fast")) != balanced {
		t.Error("an unknown effort did not fall back to balanced")
	}
}

// The effort must actually reach the request the model gets, and it must not
// touch the tools or the posture.
func TestEffortReachesTheSystemPrompt(t *testing.T) {
	f := withFakeAPI(t, `{"choices":[{"delta":{"content":"ok"}}]}`)

	a := newTestAgent(t)
	a.cfg.APIKey = "sk-test"
	a.cfg.Model = "test-model"
	a.cfg.Effort = effortDirect

	a.run(context.Background(), "shoot the enemy", ModeAgent, nil, func(string, string) {})

	f.mu.Lock()
	last := f.last
	f.mu.Unlock()
	if len(last) == 0 || last[0].Role != "system" {
		t.Fatalf("no system message was sent: %+v", last)
	}
	if !strings.Contains(last[0].Content, "# Effort: DIRECT") {
		t.Fatalf("the effort block is missing from the system prompt:\n%s", clipText(last[0].Content, 400))
	}
	// The hard rules do not move with the effort: shortening the deliberation
	// must never shorten the safety net.
	if !strings.Contains(last[0].Content, "Ways you are likely to break something") {
		t.Error("the direct effort dropped the safety rules")
	}
}

// The setting survives a save and a reload, and the view the browser reads
// carries it back.
func TestEffortIsSavedAndClamped(t *testing.T) {
	withTempConfig(t)
	c := defaultConfig()
	if c.Effort != effortBalanced {
		t.Fatalf("the default effort is %q, want balanced", c.Effort)
	}
	p := configPatch{Effort: strPtr("thorough")}
	if err := applyPatch(c, p); err != nil {
		t.Fatalf("saving the effort failed: %v", err)
	}
	if c.Effort != effortThorough {
		t.Fatalf("effort is %q after the save", c.Effort)
	}
	if v := viewOf(c); v.Effort != effortThorough {
		t.Fatalf("the view reports %q", v.Effort)
	}
	// Nonsense is repaired rather than stored.
	p = configPatch{Effort: strPtr("turbo")}
	if err := applyPatch(c, p); err != nil {
		t.Fatalf("an unknown effort was refused instead of normalised: %v", err)
	}
	if c.Effort != effortBalanced {
		t.Fatalf("an unknown effort was stored as %q", c.Effort)
	}
}

func strPtr(s string) *string { return &s }

// The UI half: the three buttons exist, changing one posts the setting, and the
// MCP row offers the per-server settings.
func TestUISettingsForEffortAndServers(t *testing.T) {
	html := loadUI(t)
	src := scriptBody(html)

	for _, want := range []string{
		`data-effort="direct"`, `data-effort="balanced"`, `data-effort="thorough"`,
		`id="cfg-effort"`, `function setEffort(`, `function paintEffort(`,
		`function mcpSettingsBlock(`, `class="mcp-approval"`, `class="mcp-tools"`, `class="mcp-timeout"`,
	} {
		if !strings.Contains(html, want) && !strings.Contains(src, want) {
			t.Errorf("index.html is missing %s", want)
		}
	}
	// A local named `state` shadows the UI state object; that mistake made the
	// per-server settings render from an empty config and cost a debugging
	// round, so it is pinned here.
	row := src[strings.Index(src, "function mcpRow("):]
	row = row[:strings.Index(row, "\n}\n")]
	if strings.Contains(row, "const state =") {
		t.Error("mcpRow declares a local `state`, which shadows the UI state object")
	}
	// The server settings are applied with a reload, not one server's restart:
	// the catalogue is rebuilt from the config, which only a reload re-reads.
	if !strings.Contains(src, "action: 'reload'") {
		t.Error("the per-server settings are not followed by a reload")
	}
}

// timeAfterSeconds keeps the test's timeout readable at the call site.
func timeAfterSeconds(n int) <-chan time.Time {
	return time.After(time.Duration(n) * time.Second)
}

// A JSON round-trip of the settings is what config.json actually stores, so a
// tag typo would silently drop a setting on every save.
func TestMCPSettingsSurviveJSON(t *testing.T) {
	in := MCPServerConfig{
		Name: "srv", Command: "run", Approval: "allow",
		Tools: []string{"a", "b"}, TimeoutSec: 30,
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out MCPServerConfig
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.approval() != mcpApprovalAllow || len(out.Tools) != 2 || out.TimeoutSec != 30 {
		t.Fatalf("settings were lost through JSON: %s", b)
	}
}
