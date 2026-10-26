package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
	a.cfg.APIKey = "sk-test-000000000000000000000000"
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
	a.run(ctx1, "task one", ModeAgent, emit1)
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
	a.run(context.Background(), "task two", ModeAgent, func(kind, text string) {
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
	a.cfg.APIKey = "sk-test-000000000000000000000000"
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

	a.run(context.Background(), "task two", ModeAgent, func(string, string) {})

	f.mu.Lock()
	violated := f.violated
	f.mu.Unlock()
	if violated != "" {
		t.Fatalf("corrupted transcript reached the API unpaired: %s", violated)
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
