package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// checkGrammar mirrors the rule the remote API enforces: every tool_call in an
// assistant message must be answered by a tool message before anything else.
func checkGrammar(msgs []Msg) error {
	var open []string
	for i, m := range msgs {
		switch m.Role {
		case "tool":
			if len(open) == 0 {
				return fmt.Errorf("msg %d: tool message without a preceding tool_calls turn", i)
			}
			if m.ToolCallID != open[0] {
				return fmt.Errorf("msg %d: tool_call_id %q, expected %q", i, m.ToolCallID, open[0])
			}
			if m.Content == "" {
				return fmt.Errorf("msg %d: empty tool content", i)
			}
			open = open[1:]
		default:
			if len(open) > 0 {
				return fmt.Errorf("msg %d: %d unanswered tool_calls before role %q", i, len(open), m.Role)
			}
			if m.Role == "assistant" {
				for _, tc := range m.ToolCalls {
					if tc.ID == "" {
						return fmt.Errorf("msg %d: tool call without id", i)
					}
					open = append(open, tc.ID)
				}
			}
		}
	}
	if len(open) > 0 {
		return fmt.Errorf("history ends with %d unanswered tool_calls", len(open))
	}
	return nil
}

func call(id, name string) ToolCall {
	tc := ToolCall{ID: id, Type: "function"}
	tc.Function.Name = name
	tc.Function.Arguments = "{}"
	return tc
}

func user(s string) Msg        { return Msg{Role: "user", Content: s} }
func assistant(s string) Msg   { return Msg{Role: "assistant", Content: s} }
func toolMsg(id, s string) Msg { return Msg{Role: "tool", ToolCallID: id, Content: s} }

func TestNormalizeHistoryValid(t *testing.T) {
	in := []Msg{
		{Role: "system", Content: "sys"},
		user("do it"),
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{call("a", "list_dir"), call("b", "read_file")}},
		toolMsg("a", "out a"),
		toolMsg("b", "out b"),
		assistant("done"),
	}
	out := normalizeHistory(in)
	if err := checkGrammar(out); err != nil {
		t.Fatalf("valid history was rejected: %v", err)
	}
	if len(out) != len(in) {
		t.Fatalf("len = %d, want %d", len(out), len(in))
	}
	if out[3].Content != "out a" || out[4].Content != "out b" {
		t.Fatalf("results reordered: %+v", out[3:5])
	}
}

// The force-abort case: the assistant asked for two tools, only the first ever
// produced a result. Before the fix this history was sent as-is and the API
// answered 400.
func TestNormalizeHistoryFillsAbortedCall(t *testing.T) {
	in := []Msg{
		{Role: "system", Content: "sys"},
		user("do it"),
		{Role: "assistant", ToolCalls: []ToolCall{call("a", "list_dir"), call("b", "run_shell")}},
		toolMsg("a", "out a"),
	}
	out := normalizeHistory(in)
	if err := checkGrammar(out); err != nil {
		t.Fatalf("repaired history still invalid: %v", err)
	}
	if len(out) != 5 {
		t.Fatalf("len = %d, want 5: %+v", len(out), out)
	}
	if out[4].ToolCallID != "b" || out[4].Content != abortedToolResult {
		t.Fatalf("missing call not closed out: %+v", out[4])
	}
}

// Aborted before the first result of the batch: nothing answered at all.
func TestNormalizeHistoryFillsWholeBatch(t *testing.T) {
	in := []Msg{
		{Role: "system", Content: "sys"},
		user("do it"),
		{Role: "assistant", ToolCalls: []ToolCall{call("a", "run_shell"), call("b", "run_shell")}},
	}
	out := normalizeHistory(in)
	if err := checkGrammar(out); err != nil {
		t.Fatalf("repaired history still invalid: %v", err)
	}
	if len(out) != 5 {
		t.Fatalf("len = %d, want 5", len(out))
	}
}

func TestNormalizeHistoryDropsOrphanTool(t *testing.T) {
	in := []Msg{
		{Role: "system", Content: "sys"},
		toolMsg("ghost", "stale"),
		user("hi"),
		toolMsg("ghost2", "stale"),
		assistant("hello"),
	}
	out := normalizeHistory(in)
	if err := checkGrammar(out); err != nil {
		t.Fatalf("orphans not dropped: %v\n%+v", err, out)
	}
	if len(out) != 3 {
		t.Fatalf("len = %d, want 3: %+v", len(out), out)
	}
}

// A tool result carrying the wrong id must not silently answer the wrong call.
func TestNormalizeHistoryOrderAndMismatch(t *testing.T) {
	in := []Msg{
		{Role: "system", Content: "sys"},
		{Role: "assistant", ToolCalls: []ToolCall{call("a", "x"), call("b", "y")}},
		toolMsg("b", "out b"), // API order is broken: b answered before a
		toolMsg("a", "out a"),
	}
	out := normalizeHistory(in)
	if err := checkGrammar(out); err != nil {
		t.Fatalf("repaired history still invalid: %v", err)
	}
	if out[2].ToolCallID != "a" || out[2].Content != "out a" {
		t.Fatalf("out of order result not reordered: %+v", out[2])
	}
	if out[3].ToolCallID != "b" || out[3].Content != "out b" {
		t.Fatalf("out of order result not reordered: %+v", out[3])
	}
}

func TestNormalizeHistoryAssignsMissingIDs(t *testing.T) {
	tc := ToolCall{Type: "function"}
	tc.Function.Name = "list_dir"
	dup := call("same", "read_file")
	in := []Msg{
		{Role: "system", Content: "sys"},
		{Role: "assistant", ToolCalls: []ToolCall{tc, dup, dup}},
	}
	out := normalizeHistory(in)
	if err := checkGrammar(out); err != nil {
		t.Fatalf("history with unnamed ids still invalid: %v", err)
	}
	seen := map[string]bool{}
	for _, m := range out[1].ToolCalls {
		if m.ID == "" || seen[m.ID] {
			t.Fatalf("duplicate or empty id survived: %+v", out[1].ToolCalls)
		}
		seen[m.ID] = true
	}
}

func TestNormalizeHistoryEmptyToolContent(t *testing.T) {
	in := []Msg{
		{Role: "system", Content: "sys"},
		{Role: "assistant", ToolCalls: []ToolCall{call("a", "run_shell")}},
		toolMsg("a", ""),
	}
	out := normalizeHistory(in)
	if err := checkGrammar(out); err != nil {
		t.Fatalf("empty tool content not repaired: %v", err)
	}
}

// addToolResult is what the runner uses; it must never append an empty body.
func TestAddToolResultPlaceholder(t *testing.T) {
	a := newTestAgent(t)
	a.addToolResult("a", "   ")
	if len(a.messages) != 1 || a.messages[0].Content != "(no output)" {
		t.Fatalf("got %+v", a.messages)
	}
}

// closeRemaining must answer every call left in an aborted batch.
func TestCloseRemainingAnswersTheBatch(t *testing.T) {
	a := newTestAgent(t)
	calls := []ToolCall{call("a", "run_shell"), call("b", "run_shell"), call("c", "write_file")}
	a.closeRemaining(calls, func(string, string) {})

	a.mu.Lock()
	got := append([]Msg(nil), a.messages...)
	a.mu.Unlock()

	if len(got) != 3 {
		t.Fatalf("%d tool messages, want 3", len(got))
	}
	for i, m := range got {
		if m.Role != "tool" || m.ToolCallID != calls[i].ID || m.Content != abortedToolResult {
			t.Fatalf("call %d not closed out: %+v", i, m)
		}
	}
}

// The exact production failure: a force-abort landed between the assistant turn
// and its tool results, then the user carried on in the same session.
func TestForceAbortLeavesUsableHistory(t *testing.T) {
	a := newTestAgent(t)
	a.mu.Lock()
	a.messages = []Msg{
		{Role: "system", Content: "sys"},
		user("fix the bug"),
		{Role: "assistant", ToolCalls: []ToolCall{call("a", "read_file"), call("b", "run_shell")}},
		toolMsg("a", "file contents"),
	}
	a.mu.Unlock()

	w := httptest.NewRecorder()
	a.handleAbort(w, httptest.NewRequest("POST", "/api/abort", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("abort returned %d", w.Code)
	}

	a.mu.Lock()
	repaired := append([]Msg(nil), a.messages...)
	a.mu.Unlock()

	if err := checkGrammar(repaired); err != nil {
		t.Fatalf("history after force abort is unusable: %v\n%+v", err, repaired)
	}
}

// run() repairs whatever it was handed before it touches the network: the next
// task after an aborted one must not inherit a dangling tool_calls turn.
func TestRunRepairsHistoryBeforeTheNextTask(t *testing.T) {
	a := newTestAgent(t)
	a.mu.Lock()
	a.messages = []Msg{
		{Role: "system", Content: "old prompt"},
		user("old task"),
		{Role: "assistant", ToolCalls: []ToolCall{call("a", "run_shell")}},
	}
	a.mu.Unlock()

	// An already-cancelled context: run() repairs, appends the task, and stops at
	// the first loop check without ever calling the API.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a.run(ctx, "new task", ModeAgent, func(string, string) {})

	a.mu.Lock()
	got := append([]Msg(nil), a.messages...)
	a.mu.Unlock()

	if err := checkGrammar(got); err != nil {
		t.Fatalf("run left an unusable transcript: %v\n%+v", err, got)
	}
	if got[len(got)-1].Role != "user" || got[len(got)-1].Content != "new task" {
		t.Fatalf("task not appended last: %+v", got[len(got)-1])
	}
	if got[0].Role != "system" || got[0].Content == "old prompt" {
		t.Fatalf("system prompt not refreshed: %+v", got[0])
	}
	if v := a.mode; v != ModeAgent {
		t.Fatalf("mode = %q", v)
	}
}

// What actually goes over the wire must parse back to a valid transcript.
func TestWireMessagesAreValidJSON(t *testing.T) {
	a := newTestAgent(t)
	a.mu.Lock()
	a.messages = []Msg{
		{Role: "system", Content: "sys"},
		user("go"),
		{Role: "assistant", ToolCalls: []ToolCall{call("a", "list_dir"), call("b", "read_file")}},
		// only one result made it in
		toolMsg("a", "entries"),
	}
	a.mu.Unlock()

	raw, err := json.Marshal(a.wireMessages())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire []struct {
		Role       string     `json:"role"`
		Content    string     `json:"content"`
		ToolCallID string     `json:"tool_call_id"`
		ToolCalls  []ToolCall `json:"tool_calls"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var asMsgs []Msg
	for _, m := range wire {
		asMsgs = append(asMsgs, Msg{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID, ToolCalls: m.ToolCalls})
	}
	if err := checkGrammar(asMsgs); err != nil {
		t.Fatalf("wire history invalid: %v\n%s", err, raw)
	}
	for _, m := range wire {
		for _, tc := range m.ToolCalls {
			if tc.Index != 0 {
				t.Fatalf("index leaked onto the wire: %+v", tc)
			}
		}
	}
}
