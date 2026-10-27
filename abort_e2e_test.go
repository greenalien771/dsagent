package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// wireMsg is the shape the API sees, used to validate what we actually send.
type wireMsg struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCallID string     `json:"tool_call_id"`
	ToolCalls  []ToolCall `json:"tool_calls"`
}

func asMsgs(w []wireMsg) []Msg {
	out := make([]Msg, 0, len(w))
	for _, m := range w {
		out = append(out, Msg{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID, ToolCalls: m.ToolCalls})
	}
	return out
}

// fakeAPI answers like DeepSeek: grammar violations get a 400. It records the
// first violation so a test can report it.
type fakeAPI struct {
	mu       sync.Mutex
	requests int
	violated string
	last     []wireMsg
}

func (f *fakeAPI) handler(firstReply string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []wireMsg `json:"messages"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}

		f.mu.Lock()
		f.requests++
		n := f.requests
		f.last = req.Messages
		if err := checkGrammar(asMsgs(req.Messages)); err != nil && f.violated == "" {
			f.violated = err.Error()
		}
		violated := f.violated
		f.mu.Unlock()

		if violated != "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"message":"`+violated+`"}}`)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		send := func(s string) {
			io.WriteString(w, "data: "+s+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
		if n == 1 {
			send(firstReply)
		} else {
			send(`{"choices":[{"delta":{"content":"recovered"}}]}`)
		}
		send(`[DONE]`)
	}
}

// twoCalls is a first assistant turn that asks for two tools at once.
const twoCalls = `{"choices":[{"delta":{"tool_calls":[` +
	`{"index":0,"id":"call_a","type":"function","function":{"name":"list_dir","arguments":"{\"path\":\".\"}"}},` +
	`{"index":1,"id":"call_b","type":"function","function":{"name":"list_dir","arguments":"{\"path\":\"..\"}"}}` +
	`]}}]}`

// twoWrites is the same shape with two side effects, so a test can watch the
// disk instead of the transcript.
const twoWrites = `{"choices":[{"delta":{"tool_calls":[` +
	`{"index":0,"id":"call_w1","type":"function","function":{"name":"write_file","arguments":"{\"path\":\"one.txt\",\"content\":\"1\"}"}},` +
	`{"index":1,"id":"call_w2","type":"function","function":{"name":"write_file","arguments":"{\"path\":\"two.txt\",\"content\":\"2\"}"}}` +
	`]}}]}`

func withFakeAPI(t *testing.T, firstReply string) *fakeAPI {
	t.Helper()
	f := &fakeAPI{}
	srv := httptest.NewServer(f.handler(firstReply))
	t.Cleanup(srv.Close)
	old := baseURL
	baseURL = srv.URL
	t.Cleanup(func() { baseURL = old })
	return f
}

// The bug: force-abort between the assistant turn and its tool results left the
// transcript with an unanswered tool_call, and every later request was rejected
// with HTTP 400. Run one task, abort it mid-batch, then run a second task.
func TestAbortMidBatchKeepsSessionAlive(t *testing.T) {
	f := withFakeAPI(t, twoCalls)

	a := newTestAgent(t)
	a.cfg.APIKey = "sk-[REDACTED]"
	a.cfg.Model = "test-model"
	a.calls = map[string]int{}

	// Abort exactly when the first tool result lands: the assistant turn has two
	// calls, only one of them is answered at that moment.
	ctx1, cancel1 := context.WithCancel(context.Background())
	var events []string
	emit1 := func(kind, text string) {
		events = append(events, kind)
		if kind == "tool_result" {
			cancel1()
		}
	}
	a.run(ctx1, "task one", ModeAgent, nil, emit1)
	cancel1()

	f.mu.Lock()
	if f.violated != "" {
		f.mu.Unlock()
		t.Fatalf("first request broke the grammar: %s", f.violated)
	}
	f.mu.Unlock()

	// The aborted run must have closed out the call it never answered.
	a.mu.Lock()
	history := append([]Msg(nil), a.messages...)
	a.mu.Unlock()
	if err := checkGrammar(history); err != nil {
		t.Fatalf("transcript after abort is unusable: %v", err)
	}

	// Second task in the same session: this is the request that used to 400.
	events = nil
	a.run(context.Background(), "task two", ModeAgent, nil, func(kind, text string) {
		events = append(events, kind)
	})

	f.mu.Lock()
	violated := f.violated
	requests := f.requests
	f.mu.Unlock()
	if violated != "" {
		t.Fatalf("request %d broke the grammar: %s", requests, violated)
	}
	if requests != 2 {
		t.Fatalf("%d requests, want 2", requests)
	}
	if !contains(events, "done") {
		t.Fatalf("second run did not finish: %v", events)
	}
	if contains(events, "error") {
		t.Fatalf("second run reported an error: %v", events)
	}
}

// Even a transcript corrupted outside the runner (the old force-abort shape)
// must be repaired on the way out, otherwise one bad session is permanent.
func TestCorruptedTranscriptIsRepairedOnTheWire(t *testing.T) {
	f := withFakeAPI(t, twoCalls)

	a := newTestAgent(t)
	a.cfg.APIKey = "sk-[REDACTED]"
	a.cfg.Model = "test-model"

	// The exact shape a force-abort used to leave behind.
	a.mu.Lock()
	a.messages = []Msg{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "task one"},
		{Role: "assistant", ToolCalls: []ToolCall{call("call_a", "list_dir"), call("call_b", "list_dir")}},
		toolMsg("call_a", "entries"),
	}
	a.mu.Unlock()

	a.run(context.Background(), "task two", ModeAgent, nil, func(string, string) {})

	f.mu.Lock()
	violated := f.violated
	f.mu.Unlock()
	if violated != "" {
		t.Fatalf("corrupted transcript reached the API unpaired: %s", violated)
	}
}

// ---- cancelled runs must not execute tools in the background -------------

// The bug: a tool call the run had already given up on still ran. execTool never
// looked at the context before doing the side effect, so a call could land after
// the run was cancelled - and with Always Allow on there is no approval card in
// between to swallow the cancel, so the write simply happened.
func TestCancelledContextNeverRunsTheTool(t *testing.T) {
	a := newTestAgent(t)
	a.cfg.AutoApprove = true // no approval card: nothing else can catch the cancel
	root := a.cfg.WorkDir

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tc := ToolCall{ID: "w1", Type: "function"}
	tc.Function.Name = "write_file"
	tc.Function.Arguments = `{"path":"one.txt","content":"1"}`

	out := a.execTool(ctx, root, tc, ModeAgent, func(string, string) {})
	if _, err := os.Stat(filepath.Join(root, "one.txt")); err == nil {
		t.Fatalf("a cancelled tool call still wrote to disk: %s", out)
	}
	if !strings.Contains(strings.ToLower(out), "cancel") {
		t.Fatalf("the result does not say the call was abandoned: %q", out)
	}
	// A call that never ran must not count against the repeat ledger either:
	// after a restart the model is allowed to make it again.
	a.mu.Lock()
	ledger := len(a.calls)
	a.mu.Unlock()
	if ledger != 0 {
		t.Fatalf("%d cancelled call(s) were counted as executed", ledger)
	}
}

// The window that is easy to miss: the cancel arrives after the loop's own
// context check and inside this call - the user pressed Stop, the tab closed, or
// the watchdog fired while the tool was being set up. The tool must not run.
func TestCancelArrivingInsideTheCallStopsTheTool(t *testing.T) {
	a := newTestAgent(t)
	a.cfg.AutoApprove = true
	root := a.cfg.WorkDir

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tc := ToolCall{ID: "w2", Type: "function"}
	tc.Function.Name = "write_file"
	tc.Function.Arguments = `{"path":"two.txt","content":"2"}`

	// "tool" is emitted as the call starts, which is exactly where a Stop lands.
	emit := func(kind, text string) {
		if kind == "tool" {
			cancel()
		}
	}
	a.execTool(ctx, root, tc, ModeAgent, emit)

	if _, err := os.Stat(filepath.Join(root, "two.txt")); err == nil {
		t.Fatal("the tool ran although the run was cancelled as the call started")
	}
}

// End to end through run(): the user presses Stop at the first approval card, so
// neither write in that turn reaches the disk and the transcript stays sendable.
func TestStoppedBatchWritesNothing(t *testing.T) {
	withFakeAPI(t, twoWrites)

	a := newTestAgent(t)
	a.cfg.APIKey = "sk-" + strings.Repeat("a", 32)
	a.cfg.Model = "test-model"
	a.calls = map[string]int{}
	root := a.cfg.WorkDir

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	emit := func(kind, text string) {
		if kind == "approval" {
			cancel() // Stop instead of answering the card
		}
	}
	a.run(ctx, "write two files", ModeAgent, nil, emit)

	for _, name := range []string{"one.txt", "two.txt"} {
		if _, err := os.Stat(filepath.Join(root, name)); err == nil {
			t.Fatalf("%s was written after the run was stopped", name)
		}
	}

	a.mu.Lock()
	history := append([]Msg(nil), a.messages...)
	a.mu.Unlock()
	if err := checkGrammar(history); err != nil {
		t.Fatalf("transcript unusable after the stop: %v", err)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
