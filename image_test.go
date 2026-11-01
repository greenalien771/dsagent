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

// imageCall builds a read_image tool call the way the model would send it.
func imageCall(id, path string) ToolCall {
	b, _ := json.Marshal(map[string]string{"path": path})
	tc := ToolCall{ID: id, Type: "function"}
	tc.Function.Name = "read_image"
	tc.Function.Arguments = string(b)
	return tc
}

// copyFixture puts a real PNG where the tool can reach it and returns its path.
func copyFixture(t *testing.T, dir, name string) string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("testdata", "vision", "sample.png"))
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, src, 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

// read_image is the file counterpart of screenshot: its result has to carry the
// SHOT tag, or the picture never reaches the model.
func TestReadImageLoadsAFileFromDisk(t *testing.T) {
	root := t.TempDir()
	copyFixture(t, root, "logo.png")

	out := runTool(context.Background(), root, imageCall("i1", "logo.png"), 0, nil)
	if strings.HasPrefix(out, "ERROR") {
		t.Fatalf("read_image refused a real PNG: %s", out)
	}
	path, ok := shotFromResult(out)
	if !ok {
		t.Fatalf("the result carries no attachment: %s", out)
	}
	if filepath.Base(path) != "logo.png" {
		t.Fatalf("attached %q, want logo.png", path)
	}
	// The result says what it loaded, so the next step has a file name to talk
	// about instead of just "the image".
	if !strings.Contains(out, "logo.png") || !strings.Contains(out, "next step") {
		t.Fatalf("unhelpful result: %s", out)
	}
}

// Everything read_image cannot send has to be refused with a sentence the model
// can act on, before any bytes are encoded and shipped.
func TestReadImageRefusesWhatItCannotSend(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("just some text, not a picture"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "folder"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "empty.png"), nil, 0600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		arg  string
		want string
	}{
		{"text is not an image", "notes.txt", "read_file is for text"},
		{"a folder is not a file", "folder", "call list_dir"},
		{"a missing file", "nope.png", "no such file"},
		{"an empty file", "empty.png", "empty"},
		// safeJoin neutralises a climb out of the workspace instead of following
		// it, so the file simply is not there - and the tool says which.
		{"a path that climbs out", "../escape.png", "no such file"},
	}
	for _, tc := range cases {
		out := runTool(context.Background(), root, imageCall("i2", tc.arg), 0, nil)
		if !strings.HasPrefix(out, "ERROR") {
			t.Errorf("%s: accepted, got %q", tc.name, clip(out))
			continue
		}
		if !strings.Contains(out, tc.want) {
			t.Errorf("%s: %q does not mention %q", tc.name, clip(out), tc.want)
		}
	}
}

// The tool is read-only, so it is offered in every mode and never parked behind
// an approval card: looking at a file the workspace already holds changes
// nothing on the machine.
func TestReadImageIsAvailableEverywhereAndUnapproved(t *testing.T) {
	for _, mode := range []string{ModeAgent, ModePlan, ModeAsk} {
		found := false
		for _, tl := range toolSchemas(mode) {
			if tl["function"].(map[string]any)["name"] == "read_image" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: read_image is missing", mode)
		}
		if why, denied := modeDenies(mode, "read_image"); denied {
			t.Errorf("%s: read_image is refused at runtime: %s", mode, why)
		}
	}
	if needsApproval("read_image") {
		t.Error("reading a file must not need an approval card; read_file does not")
	}
	if risk := classifyTool("read_image", `{"path":"logo.png"}`); risk.Level != riskSafe {
		t.Errorf("risk level %q, want safe", risk.Level)
	}
}

// A file the model asked to look at is not a fresh capture, and the transcript
// must not say it is: "the screen says X" and "this picture says X" are
// different claims.
func TestAttachReadImageSaysItIsAFileNotTheScreen(t *testing.T) {
	a := newTestAgent(t)
	a.cfg.Model = "deepseek-flash"
	path := copyFixture(t, t.TempDir(), "mockup.png")

	var events []string
	a.attachReadImage(path, func(kind, text string) { events = append(events, kind) })

	a.mu.Lock()
	msgs := append([]Msg(nil), a.messages...)
	a.mu.Unlock()

	if len(msgs) != 1 || msgs[0].Role != "user" || len(msgs[0].Images) != 1 {
		t.Fatalf("no user message with an image: %+v", msgs)
	}
	if !strings.Contains(msgs[0].Content, "image attached: mockup.png") {
		t.Fatalf("the message does not say what was attached: %q", msgs[0].Content)
	}
	if strings.Contains(msgs[0].Content, "as it is right now") {
		t.Fatalf("a file on disk was announced as the live screen: %q", msgs[0].Content)
	}
	if !contains(events, "image") {
		t.Fatalf("the browser was not sent the picture: %v", events)
	}
}

// The whole path end to end: the model calls read_image, the file is attached as
// a user message with an image block, and the very next request carries it.
func TestReadImageReachesTheNextRequest(t *testing.T) {
	a := newTestAgent(t)
	a.cfg.APIKey = "sk-test-key"
	a.cfg.Model = "deepseek-flash"
	copyFixture(t, a.cfg.WorkDir, "shot.png")

	var mu sync.Mutex
	var calls int
	var imageOnSecond bool
	handler := func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls++
		n := calls
		if n > 1 && strings.Contains(string(body), `"image_url"`) {
			imageOnSecond = true
		}
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		send := func(s string) {
			io.WriteString(w, "data: "+s+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
		if n == 1 {
			send(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"img1","type":"function","function":{"name":"read_image","arguments":"{\"path\":\"shot.png\"}"}}]}}]}`)
		} else {
			send(`{"choices":[{"delta":{"content":"I can see it."}}]}`)
		}
		send(`[DONE]`)
	}
	srv := httptest.NewServer(http.HandlerFunc(handler))
	defer srv.Close()
	oldURL := baseURL
	baseURL = srv.URL
	defer func() { baseURL = oldURL }()

	var events []string
	a.run(context.Background(), "look at shot.png", ModeAgent, nil, func(kind, text string) {
		events = append(events, kind)
	})

	mu.Lock()
	got := imageOnSecond
	ran := calls
	mu.Unlock()

	if ran < 2 {
		t.Fatalf("the run stopped after %d request(s): %v", ran, events)
	}
	if !got {
		t.Fatalf("the picture never reached a request: %v", events)
	}
	if !contains(events, "image") {
		t.Fatalf("the browser was not shown the picture: %v", events)
	}
}

// Regression: a tool result that merely quotes a line containing the SHOT tag -
// the agent reading its own source, a log, a diff - used to be parsed as a path
// and attached, which put a note about a file named `" + p)` into the transcript.
func TestShotFromResultIgnoresTextThatOnlyQuotesTheTag(t *testing.T) {
	quoted := "	path, ok := shotFromResult(\"SHOT:\" + p)\n	if !ok {\n		t.Fatal(\"tag not recognised\")\n	}"
	if _, ok := shotFromResult(quoted); ok {
		t.Fatal("a quoted SHOT line was read as an attachment")
	}
	// A capture that produced a file is still an attachment.
	p := filepath.Join(t.TempDir(), "real.png")
	if err := os.WriteFile(p, []byte{0x89, 'P', 'N', 'G'}, 0600); err != nil {
		t.Fatal(err)
	}
	if got, ok := shotFromResult("Captured the screen.\nSHOT:" + p + "\n"); !ok || got != p {
		t.Fatalf("a real capture was ignored: %q ok=%v", got, ok)
	}
}
