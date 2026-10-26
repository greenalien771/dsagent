package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A Settings save that lands in the middle of a run must not extend that run's
// step budget: the limit is snapshotted under cfgMu when the task starts.
func TestRunKeepsItsStepBudgetWhenSettingsChange(t *testing.T) {
	f := withFakeAPI(t, twoCalls)

	a := newTestAgent(t)
	a.cfg.APIKey = "sk-test-000000000000000000000000"
	a.cfg.MaxSteps = 1 // one step only: step 2 would mean the snapshot was ignored
	a.calls = map[string]int{}

	var mu sync.Mutex
	var events []string
	raised := false
	emit := func(kind, text string) {
		mu.Lock()
		events = append(events, kind+": "+text)
		mu.Unlock()
		if kind != "tool_result" || raised {
			return
		}
		raised = true
		// The user saves Settings while the tools run. Before the snapshot this
		// write was both a data race and a live change to the loop condition.
		nine := 9
		if err := applyPatch(a.cfg, configPatch{MaxSteps: &nine}); err != nil {
			t.Errorf("applyPatch: %v", err)
		}
	}

	a.run(context.Background(), "task", ModeAgent, emit)

	f.mu.Lock()
	requests := f.requests
	f.mu.Unlock()
	if requests != 1 {
		t.Fatalf("%d requests: the raised step limit leaked into the running task", requests)
	}
	mu.Lock()
	joined := strings.Join(events, "\n")
	mu.Unlock()
	if !strings.Contains(joined, "Step limit reached") {
		t.Fatalf("task did not stop at its own step limit:\n%s", joined)
	}

	// The new limit must still apply to the next task.
	if _, steps := a.cfg.runSettings(); steps != 9 {
		t.Fatalf("saved step limit = %d, want 9", steps)
	}
}

// The workspace is decided when the run starts, not re-read per tool call: a
// Settings change must not redirect a command into another folder mid-task.
func TestExecToolUsesTheRunsOwnWorkspace(t *testing.T) {
	a := newTestAgent(t)
	a.cfg.AutoApprove = true // no approval prompt; this test drives the tool directly

	root := t.TempDir()
	tc := ToolCall{ID: "w1", Type: "function"}
	tc.Function.Name = "write_file"
	tc.Function.Arguments = `{"path":"note.txt","content":"hi"}`

	out := a.execTool(context.Background(), root, tc, ModeAgent, func(string, string) {})
	if !strings.HasPrefix(out, "OK") {
		t.Fatalf("write_file: %s", out)
	}
	if _, err := os.Stat(filepath.Join(root, "note.txt")); err != nil {
		t.Fatalf("file did not land in the given workspace: %v", err)
	}
	// The live config workspace must be untouched: no tool may write there by
	// reading a field that can change under it.
	if _, err := os.Stat(filepath.Join(a.cfg.WorkDir, "note.txt")); err == nil {
		t.Fatal("tool wrote into the configured workspace instead of the run's own")
	}
}

// A new session must not inherit "this call already ran three times" from the
// conversation the user just discarded.
func TestClearResetsTheRepeatLedger(t *testing.T) {
	a := newTestAgent(t)
	a.calls = map[string]int{}
	root := t.TempDir()

	tc := ToolCall{ID: "l1", Type: "function"}
	tc.Function.Name = "list_dir"
	tc.Function.Arguments = `{"path":"."}`
	emit := func(string, string) {}

	for i := 0; i < 4; i++ {
		a.execTool(context.Background(), root, tc, ModeAsk, emit)
	}
	a.mu.Lock()
	before := len(a.calls)
	a.mu.Unlock()
	if before == 0 {
		t.Fatal("ledger did not record the calls; the test proves nothing")
	}

	w := httptest.NewRecorder()
	a.handleClear(w, httptest.NewRequest("POST", "/api/clear", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("clear: got %d want 200", w.Code)
	}

	a.mu.Lock()
	messages, calls := len(a.messages), len(a.calls)
	a.mu.Unlock()
	if messages != 0 {
		t.Fatalf("%d message(s) survived the clear", messages)
	}
	if calls != 0 {
		t.Fatalf("%d ledger entry/entries survived the clear", calls)
	}
	if out := a.execTool(context.Background(), root, tc, ModeAsk, emit); strings.HasPrefix(out, "REFUSED") {
		t.Fatalf("a fresh session was refused for a call made before it: %s", out)
	}
}

// Wiping the transcript mid-run would leave the request in flight carrying only
// orphan tool results - exactly the shape the API rejects.
func TestClearRefusesWhileARunIsInFlight(t *testing.T) {
	a := newTestAgent(t)

	a.mu.Lock()
	a.runToken++
	a.isRunning = true
	a.runStart = time.Now()
	a.lastActivity = time.Now()
	a.messages = []Msg{{Role: "system", Content: "sys"}, {Role: "user", Content: "keep me"}}
	a.calls = map[string]int{"k": 2}
	a.mu.Unlock()

	w := httptest.NewRecorder()
	a.handleClear(w, httptest.NewRequest("POST", "/api/clear", nil))
	if w.Code != http.StatusConflict {
		t.Fatalf("clear during a run: got %d want 409", w.Code)
	}

	a.mu.Lock()
	messages, calls := len(a.messages), len(a.calls)
	a.mu.Unlock()
	if messages != 2 {
		t.Fatalf("transcript was wiped by a refused clear: %d message(s) left", messages)
	}
	if calls != 1 {
		t.Fatalf("ledger was wiped by a refused clear: %d entry/entries left", calls)
	}

	// Once the run is over the clear goes through normally.
	a.mu.Lock()
	a.isRunning = false
	a.mu.Unlock()

	w = httptest.NewRecorder()
	a.handleClear(w, httptest.NewRequest("POST", "/api/clear", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("clear after the run: got %d want 200", w.Code)
	}
}

// Reading the config from a run and saving it from /api/config must not be able
// to deadlock: no path may hold cfgMu and Agent.mu at the same time.
func TestConfigSavesDoNotDeadlockARun(t *testing.T) {
	f := withFakeAPI(t, twoCalls)

	a := newTestAgent(t)
	a.cfg.APIKey = "sk-test-000000000000000000000000"
	a.cfg.MaxSteps = 3
	a.calls = map[string]int{}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			steps, approve := 7, true
			applyPatch(a.cfg, configPatch{MaxSteps: &steps, AutoApprove: &approve})
			autoApprove(a.cfg)
			credentials(a.cfg)
			viewOf(a.cfg)
		}
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		a.run(context.Background(), "task", ModeAgent, func(string, string) {})
	}()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		close(stop)
		t.Fatal("a settings save deadlocked the run")
	}
	close(stop)
	wg.Wait()

	f.mu.Lock()
	violated := f.violated
	f.mu.Unlock()
	if violated != "" {
		t.Fatalf("grammar broke during concurrent saves: %s", violated)
	}
}
