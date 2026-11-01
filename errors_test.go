package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// call builds the ToolCall the runner would build for one tool.
func callTool(name string, args map[string]any) ToolCall {
	tc := ToolCall{ID: "c1", Type: "function"}
	tc.Function.Name = name
	b, _ := json.Marshal(args)
	tc.Function.Arguments = string(b)
	return tc
}

// These are the answers a caller has to be able to act on. Every case here was
// previously either a bare OS string with no context, or a sentence that named
// the wrong thing - and both of those send the reader looking in the wrong place.
func TestToolErrorsSayWhatHappened(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "folder"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "empty"), 0755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		tc   ToolCall
		// want is what the sentence has to contain; the parts that make it
		// actionable rather than merely descriptive.
		want []string
	}{
		{"a missing file names the path and the next step",
			callTool("read_file", map[string]any{"path": "nope.txt"}),
			[]string{"there is no file at nope.txt", "list_dir"}},
		{"a folder is not an access-denied error",
			callTool("read_file", map[string]any{"path": "folder"}),
			[]string{"folder is a folder, not a file", "list_dir"}},
		{"an empty file is not a failure",
			callTool("read_file", map[string]any{"path": "empty.txt"}),
			[]string{"0 bytes"}},
		{"a negative offset explains what an offset is",
			callTool("read_file", map[string]any{"path": "notes.txt", "offset": -5}),
			[]string{"negative", "0 means the start"}},
		{"an offset past the end states the real length",
			callTool("read_file", map[string]any{"path": "notes.txt", "offset": 999}),
			[]string{"past the end of notes.txt", "5 bytes long", "offset=0"}},
		{"list_dir on a file is not an empty listing",
			callTool("list_dir", map[string]any{"path": "notes.txt"}),
			[]string{"notes.txt is a file, not a folder", "read_file"}},
		{"an empty folder says so",
			callTool("list_dir", map[string]any{"path": "empty"}),
			[]string{"empty folder"}},
		{"a path outside the workspace is quoted back",
			callTool("read_file", map[string]any{"path": "../escape.txt"}),
			[]string{"../escape.txt"}},
	}
	// The empty-file case needs a file that exists and is empty.
	if err := os.WriteFile(filepath.Join(root, "empty.txt"), nil, 0600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range cases {
		out := runTool(context.Background(), root, tc.tc, 0, nil)
		for _, want := range tc.want {
			if !strings.Contains(out, want) {
				t.Errorf("%s: %q does not mention %q", tc.name, clip(out), want)
			}
		}
		if tc.name != "an empty file is not a failure" && tc.name != "an empty folder says so" {
			if !strings.HasPrefix(out, "ERROR") {
				t.Errorf("%s: not reported as an error: %q", tc.name, clip(out))
			}
		}
	}
}

// A command that finishes with a non-zero code has to say which code. Go's own
// "exit status 1" names neither the command nor the meaning, and it was being
// pasted into the transcript verbatim.
func TestExitNoteNamesTheCode(t *testing.T) {
	out := runTool(context.Background(), os.TempDir(), shellCall("exit 3"), 30e9, nil)
	if !strings.Contains(out, "code 3") {
		t.Fatalf("the exit code was not carried through: %q", out)
	}
	if strings.Contains(out, "exit status") {
		t.Fatalf("Go's own wording reached the transcript: %q", out)
	}
}

// A 404 must never come back looking like the page: the server's error document
// is not the content, and quoting it as an answer was a real failure mode.
func TestWebFetchRefusesAnErrorPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("<html><body><h1>Not Found</h1></body></html>"))
	}))
	defer srv.Close()

	out := runTool(context.Background(), t.TempDir(), callTool("web_fetch", map[string]any{"url": srv.URL}), 0, nil)
	if !strings.HasPrefix(out, "ERROR") {
		t.Fatalf("an error page was returned as content: %q", out)
	}
	for _, want := range []string{"HTTP 404", "not fetched", "server's own response"} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal does not say %q: %s", want, out)
		}
	}
}

// web_fetch on a real 200 still returns the text, so the refusal above is about
// status, not about being cautious with every fetch.
func TestWebFetchReturnsAPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html><body><p>Hello there</p></body></html>"))
	}))
	defer srv.Close()

	out := runTool(context.Background(), t.TempDir(), callTool("web_fetch", map[string]any{"url": srv.URL}), 0, nil)
	if strings.HasPrefix(out, "ERROR") {
		t.Fatalf("a working page was refused: %q", out)
	}
	if !strings.Contains(out, "Hello there") {
		t.Fatalf("the page text did not come through: %q", out)
	}
}

// The API's own error must be quoted, and the sentence must name the endpoint
// and the setting to change - "api 401: {...}" did none of that.
func TestAPIErrorExplainsItself(t *testing.T) {
	a := newTestAgent(t)
	a.cfg.Provider = string(providerZAI)

	cases := []struct {
		status int
		body   string
		want   []string
	}{
		{http.StatusUnauthorized, `{"error":{"message":"invalid api key","type":"authentication_error"}}`,
			[]string{"api.z.ai", "HTTP 401", "the API key was refused", "invalid api key", "Settings > Connection"}},
		{http.StatusNotFound, `{"message":"no such route"}`,
			[]string{"HTTP 404", "no endpoint at that address", "no such route", "base_url"}},
		{http.StatusTooManyRequests, `{"error":"rate limit reached"}`,
			[]string{"HTTP 429", "rate-limited", "rate limit reached"}},
		{http.StatusInternalServerError, ``,
			[]string{"HTTP 500", "server failed"}},
		{http.StatusBadGateway, `<html>Bad Gateway</html>`,
			[]string{"HTTP 502", "Bad Gateway"}},
	}
	for _, tc := range cases {
		err := a.apiError("https://api.z.ai/api/paas/v4/chat/completions", tc.status, []byte(tc.body), 1)
		msg := err.Error()
		for _, want := range tc.want {
			if !strings.Contains(msg, want) {
				t.Errorf("status %d: %q does not mention %q", tc.status, msg, want)
			}
		}
		if strings.Contains(msg, `"error":`) {
			t.Errorf("status %d: the raw wire body leaked into the error: %q", tc.status, msg)
		}
	}
}

// A body that is not an event stream - an HTML page, a JSON error - has to be
// reported as that, not as "closed unexpectedly".
func TestParseStreamSaysWhatArrived(t *testing.T) {
	a := newTestAgent(t)

	_, err := a.parseStream(strings.NewReader("<html><body>login required</body></html>\n"), func(string, string) {})
	if err == nil {
		t.Fatal("a non-stream body was accepted")
	}
	if !strings.Contains(err.Error(), "did not send an event stream") || !strings.Contains(err.Error(), "login required") {
		t.Fatalf("the error does not say what arrived: %v", err)
	}

	// A stream that simply stops mid-answer is an incomplete reply, not a
	// mystery: the amount of text already received says what was lost.
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"half an ans\"}}]}\n\n"
	_, err = a.parseStream(strings.NewReader(body), func(string, string) {})
	if err == nil {
		t.Fatal("a truncated stream was accepted as a complete reply")
	}
	if !strings.Contains(err.Error(), "type continue") {
		t.Fatalf("the error does not say how to recover: %v", err)
	}
}

// A stored key that exists but cannot be loaded is a different situation from no
// key at all, and the banner used to call both of them MISSING.
func TestBannerDistinguishesAnUnloadableKey(t *testing.T) {
	a := newTestAgent(t)
	a.cfg.KeyError = "CryptUnprotectData failed: Access is denied."

	lines := strings.Join(bannerLines(a.cfg), "\n")
	if !strings.Contains(lines, "NOT LOADED") {
		t.Fatalf("an unloadable key was reported as missing: %s", lines)
	}
	if !strings.Contains(lines, "Access is denied") {
		t.Fatalf("the reason the key could not be loaded is gone: %s", lines)
	}
	if strings.Contains(lines, "MISSING") {
		t.Fatalf("a key that exists was called MISSING: %s", lines)
	}
}
