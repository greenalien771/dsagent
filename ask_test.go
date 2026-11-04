package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ask_user is what keeps a short interactive task inside one run: the step
// parks, the answer comes back as the tool result, and the model carries on
// instead of the user having to type the whole task again.
//
// Before it existed the only way to get an answer was to end the run, which cost
// a fresh request, a fresh plan and the thread of the conversation - expensive
// for "which file did you mean", absurd for a game move.
const askThenAnswer = `{"choices":[{"delta":{"tool_calls":[` +
	`{"index":0,"id":"call_ask","type":"function","function":{"name":"ask_user","arguments":"{\"question\":\"Which file did you mean: one.txt or two.txt?\",\"options\":[\"one.txt\",\"two.txt\"]}"}}` +
	`]}}]}`

// answerThroughTheAPI posts an answer the way the browser does, so the test
// covers the handler the UI actually calls and not just the channel.
func answerThroughTheAPI(t *testing.T, a *Agent, id, answer string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"id": id, "answer": answer})
	w := httptest.NewRecorder()
	a.handleAnswer(w, httptest.NewRequest("POST", "/api/answer", strings.NewReader(string(body))))
	return w
}

// The run stops on the question, the answer reaches the model as the tool
// result, and the task finishes in the same run.
func TestAskUserParksTheRunUntilAnswered(t *testing.T) {
	f := withFakeAPI(t, askThenAnswer)

	a := newTestAgent(t)
	a.cfg.APIKey = "sk-test"
	a.cfg.Model = "test-model"

	var (
		mu         sync.Mutex
		kinds      []string
		asked      questionRequest
		questionAt time.Time
		answerAt   time.Time
	)

	emit := func(kind, text string) {
		mu.Lock()
		kinds = append(kinds, kind)
		mu.Unlock()
		if kind != "question" {
			return
		}
		var q questionRequest
		if err := json.Unmarshal([]byte(text), &q); err != nil {
			t.Errorf("the question payload is not JSON: %v (%s)", err, text)
			return
		}
		mu.Lock()
		asked = q
		questionAt = time.Now()
		mu.Unlock()

		// The browser posts here while the tool call is parked. Doing it from
		// the emit is the same interleaving: the run is already waiting.
		w := answerThroughTheAPI(t, a, q.ID, "two.txt")
		if w.Code != http.StatusOK {
			t.Errorf("the answer was refused: %d %s", w.Code, w.Body.String())
		}
		mu.Lock()
		answerAt = time.Now()
		mu.Unlock()
	}

	a.run(context.Background(), "fix the parser", ModeAgent, nil, emit)

	mu.Lock()
	gotKinds, q, qAt, aAt := kinds, asked, questionAt, answerAt
	mu.Unlock()

	if q.Question == "" || !strings.Contains(q.Question, "one.txt") {
		t.Fatalf("the question did not reach the UI: %+v", q)
	}
	if len(q.Options) != 2 || q.Options[1] != "two.txt" {
		t.Fatalf("the suggested answers were lost: %+v", q.Options)
	}
	if q.ID != "call_ask" {
		t.Fatalf("the question carries id %q, so the answer could not be matched to the call", q.ID)
	}
	if qAt.IsZero() || aAt.Before(qAt) {
		t.Fatal("the answer was delivered before the question was shown")
	}
	if !contains(gotKinds, "done") {
		t.Fatalf("the run did not finish after the answer: %v", gotKinds)
	}
	if contains(gotKinds, "error") || contains(gotKinds, "abort") {
		t.Fatalf("the run failed instead of continuing: %v", gotKinds)
	}

	// Two requests: the one that asked, and the one that finished. If the run had
	// ended on the question there would be one.
	f.mu.Lock()
	requests, violated := f.requests, f.violated
	f.mu.Unlock()
	if requests != 2 {
		t.Fatalf("%d model requests, want 2: the answer has to continue the same run", requests)
	}
	if violated != "" {
		t.Fatalf("the transcript was left invalid for the API: %s", violated)
	}

	// The answer must be in the transcript as the ask_user result - that is how
	// the model learns it, and it must not be an assistant/user turn that would
	// change the shape of the conversation.
	a.mu.Lock()
	history := append([]Msg(nil), a.messages...)
	a.mu.Unlock()
	var result string
	for _, m := range history {
		if m.Role == "tool" && m.ToolCallID == "call_ask" {
			result = m.Content
		}
	}
	if !strings.Contains(result, "two.txt") {
		t.Fatalf("the tool result does not carry the answer: %q", result)
	}
	if err := checkGrammar(history); err != nil {
		t.Fatalf("transcript after a question is unusable: %v", err)
	}
}

// An id that is not waiting is refused, not queued: the run it belonged to is
// over, and a late answer would land in a transcript that already stopped.
func TestAnswerForAnUnknownQuestionIsRefused(t *testing.T) {
	a := newTestAgent(t)
	w := answerThroughTheAPI(t, a, "call_nobody", "hello")
	if w.Code != http.StatusNotFound {
		t.Fatalf("answer for an unknown id: got %d want 404 (%s)", w.Code, w.Body.String())
	}
}

// A question only gets one answer: the second click must not queue a second
// result for the same tool call.
func TestQuestionIsAnsweredOnce(t *testing.T) {
	a := newTestAgent(t)
	ch := make(chan string, 1)
	a.asks["call_once"] = ch

	if w := answerThroughTheAPI(t, a, "call_once", "yes"); w.Code != http.StatusOK {
		t.Fatalf("first answer: got %d want 200", w.Code)
	}
	if w := answerThroughTheAPI(t, a, "call_once", "no"); w.Code != http.StatusNotFound {
		t.Fatalf("second answer: got %d want 404", w.Code)
	}
	select {
	case got := <-ch:
		if got != "yes" {
			t.Fatalf("the delivered answer is %q, want the first one", got)
		}
	case <-time.After(time.Second):
		t.Fatal("no answer was delivered")
	}
}

// A cancelled run must not hang on the question, and the transcript has to say
// the call was never answered instead of inventing one.
func TestAskUserIsReleasedByACancelledRun(t *testing.T) {
	a := newTestAgent(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tc := ToolCall{ID: "call_ask", Type: "function"}
	tc.Function.Name = "ask_user"
	tc.Function.Arguments = `{"question":"continue?"}`

	emit := func(kind, _ string) {
		if kind == "question" {
			cancel()
		}
	}
	out := a.execTool(ctx, a.cfg.WorkDir, tc, ModeAgent, emit)
	if !strings.Contains(strings.ToLower(out), "cancelled") {
		t.Fatalf("a cancelled question did not report itself as unanswered: %q", out)
	}
	a.mu.Lock()
	waiting := len(a.asks)
	a.mu.Unlock()
	if waiting != 0 {
		t.Fatalf("%d question(s) left waiting after the run was cancelled", waiting)
	}
}

// The force-abort path drops a waiting question with the approvals: the run that
// asked it is over.
func TestAbortDropsWaitingQuestions(t *testing.T) {
	a := newTestAgent(t)
	a.asks["call_ask"] = make(chan string, 1)

	w := httptest.NewRecorder()
	a.handleAbort(w, httptest.NewRequest("POST", "/api/abort", strings.NewReader("{}")))

	a.mu.Lock()
	waiting := len(a.asks)
	a.mu.Unlock()
	if waiting != 0 {
		t.Fatalf("%d question(s) survived a force-abort", waiting)
	}
}

// ask_user writes nothing, so it is offered in every posture, never asks for
// approval, and can never be mistaken for a side effect.
func TestAskUserIsFreeEverywhere(t *testing.T) {
	for _, mode := range []string{ModeAgent, ModePlan, ModeAsk} {
		found := false
		for _, tl := range toolSchemas(mode) {
			fn := tl["function"].(map[string]any)
			if fn["name"] == "ask_user" {
				found = true
				if !strings.Contains(fn["description"].(string), "wait") {
					t.Errorf("%s: the description does not say the run waits", mode)
				}
			}
		}
		if !found {
			t.Errorf("%s: ask_user is missing, so a question costs a whole new task", mode)
		}
		if _, denied := modeDenies(mode, "ask_user"); denied {
			t.Errorf("%s: ask_user is refused by modeDenies", mode)
		}
	}
	if needsApproval("ask_user") {
		t.Fatal("ask_user asks for approval; there is nothing to approve")
	}
	if risk := classifyTool("ask_user", `{"question":"which one?"}`); risk.Level != riskSafe {
		t.Fatalf("ask_user is classified %q, want safe", risk.Level)
	}
	if promptsOnRisk("ask_user") {
		t.Fatal("ask_user prompts on risk; asking a question is not a side effect")
	}
}

// Bad arguments are a sentence, not a panic or an empty question card.
func TestAskUserRejectsAnEmptyQuestion(t *testing.T) {
	a := newTestAgent(t)
	tc := ToolCall{ID: "call_ask", Type: "function"}
	tc.Function.Name = "ask_user"
	tc.Function.Arguments = `{"question":"   "}`
	out := a.execTool(context.Background(), a.cfg.WorkDir, tc, ModeAgent, func(string, string) {})
	if !strings.Contains(out, "BAD ARGS") {
		t.Fatalf("an empty question was accepted: %q", out)
	}
}

// The UI half: the question event must reach a card that posts to /api/answer,
// and it must not be routed to the composer - that would start a second task
// while the first one is still waiting.
func TestUIAsksTheQuestionInTheThread(t *testing.T) {
	html := loadUI(t)
	src := scriptBody(html)

	for _, want := range []string{
		`function askQuestion(`,
		`'/api/answer'`,
		`ev.type === 'question'`,
		"card.className = 'question'",
	} {
		if !strings.Contains(src, want) && !strings.Contains(html, want) {
			t.Errorf("index.html is missing %s", want)
		}
	}
	// The card is drawn in the thread, not in the composer: the composer path
	// runs postTask(), which is refused with 429 while a run is in flight.
	body := src[strings.Index(src, "function askQuestion("):]
	body = body[:strings.Index(body, "\n}\n")]
	if strings.Contains(body, "el.input.value") || strings.Contains(body, "postTask(") {
		t.Fatal("askQuestion puts the answer in the composer instead of posting it to /api/answer")
	}
}

// The MCP pane: its own nav section, the list of running servers, and a status
// line - so the servers are visible without opening the agent's JSON box.
func TestUIMCPPaneIsItsOwnSection(t *testing.T) {
	html := loadUI(t)
	src := scriptBody(html)

	for _, want := range []string{
		`data-sec="mcp"`,
		`id="sec-mcp"`,
		`id="mcpServers"`,
		`id="cfg-mcp"`,
		`id="mcpState"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html is missing %s", want)
		}
	}
	// The JSON box moved: it belongs to the MCP pane, not to Agent.
	agent := html[strings.Index(html, `id="sec-agent"`):]
	agent = agent[:strings.Index(agent, `id="sec-mcp"`)]
	if strings.Contains(agent, `id="cfg-mcp"`) {
		t.Error("the MCP configuration box is still in the Agent pane")
	}
	for _, fn := range []string{"paintMCPState", "mcpRow", "mcpServerAction", "refreshMCPState", "reloadMCP", "stopMCP"} {
		if !strings.Contains(src, "function "+fn+"(") {
			t.Errorf("the script never defines %s()", fn)
		}
	}
}
