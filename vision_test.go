package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// testdata/vision holds a real PNG and a real JPEG (fetched bytes, not
// synthesised headers), which is what makes the content sniffing below a real
// test rather than a test of my own guess at a magic number.
func fixtureDataURL(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join("testdata", "vision", name)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("fixture %s: %v", p, err)
	}
	mime := http.DetectContentType(b)
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(b)
}

func TestNormalizeImagesAcceptsRealImages(t *testing.T) {
	cases := []struct{ file, wantMIME string }{
		{"sample.png", "image/png"},
		{"sample.jpg", "image/jpeg"},
	}
	for _, c := range cases {
		got, err := normalizeImages([]chatImage{{Name: c.file, Data: fixtureDataURL(t, c.file)}})
		if err != nil {
			t.Fatalf("%s: %v", c.file, err)
		}
		if len(got) != 1 {
			t.Fatalf("%s: %d parts", c.file, len(got))
		}
		if !strings.HasPrefix(got[0].DataURL, "data:"+c.wantMIME+";base64,") {
			t.Fatalf("%s: data URL starts %q, want the %s prefix", c.file, got[0].DataURL[:40], c.wantMIME)
		}
		// The bytes must survive the round trip: a re-encode would corrupt them.
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(got[0].DataURL, "data:"+c.wantMIME+";base64,"))
		if err != nil {
			t.Fatalf("%s: payload is not base64: %v", c.file, err)
		}
		src, _ := os.ReadFile(filepath.Join("testdata", "vision", c.file))
		if len(raw) != len(src) || string(raw) != string(src) {
			t.Fatalf("%s: payload does not match the file", c.file)
		}
	}
}

// The declared type is ignored; the bytes decide. A .txt full of JPEG is a JPEG,
// and a text file renamed to .png is not an image at all.
func TestNormalizeImagesSniffsContentNotName(t *testing.T) {
	jpeg, err := os.ReadFile(filepath.Join("testdata", "vision", "sample.jpg"))
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	doc := []chatImage{{Name: "really-a-jpeg.txt", Data: "data:image/png;base64," + base64.StdEncoding.EncodeToString(jpeg)}}
	got, err := normalizeImages(doc)
	if err != nil {
		t.Fatalf("a JPEG declared as PNG was refused: %v", err)
	}
	if !strings.HasPrefix(got[0].DataURL, "data:image/jpeg;base64,") {
		t.Fatalf("content type not taken from the bytes: %q", got[0].DataURL[:40])
	}

	text := []chatImage{{Name: "fake.png", Data: "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("just some text, not a picture"))}}
	if _, err := normalizeImages(text); err == nil {
		t.Fatal("text pretending to be a PNG was accepted")
	} else if !strings.Contains(err.Error(), "not a JPEG, PNG, GIF or WebP") {
		t.Fatalf("unhelpful error: %v", err)
	}
}

func TestNormalizeImagesRejectsMalformedPayloads(t *testing.T) {
	png := fixtureDataURL(t, "sample.png")
	cases := map[string]string{
		"bare base64":    strings.TrimPrefix(png, "data:image/png;base64,"),
		"not a data URL": "https://example.com/cat.png",
		"unencoded body": "data:image/png,rawbytes",
		"empty":          "data:image/png;base64,",
		"broken base64":  "data:image/png;base64,!!!!not-base64!!!!",
	}
	for name, data := range cases {
		if _, err := normalizeImages([]chatImage{{Name: "x.png", Data: data}}); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestNormalizeImagesEnforcesLimits(t *testing.T) {
	png := fixtureDataURL(t, "sample.png")

	many := make([]chatImage, maxImagesPerRequest+1)
	for i := range many {
		many[i] = chatImage{Name: "x.png", Data: png}
	}
	if _, err := normalizeImages(many); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("count limit not enforced: %v", err)
	}

	// A real PNG header with padding past the per-image cap: detection still
	// says PNG, so this exercises the size check rather than the type check.
	huge := append([]byte{}, png...)
	raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(png, "data:image/png;base64,"))
	_ = raw
	huge = make([]byte, maxImageBytes+1024)
	copy(huge, []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
	big := []chatImage{{Name: "huge.png", Data: "data:image/png;base64," + base64.StdEncoding.EncodeToString(huge)}}
	if _, err := normalizeImages(big); err == nil || !strings.Contains(err.Error(), "per-image limit") {
		t.Fatalf("size limit not enforced: %v", err)
	}
}

func TestSupportsVisionFollowsTheModelTable(t *testing.T) {
	cases := map[string]bool{
		"deepseek-flash":               true,
		"deepseek-v4-flash-vision-exp": true, // retired name, served by Flash
		"DeepSeek-Flash":               true,
		"deepseek-v4-pro":              false,
		"deepseek-reasoner":            false,
		"":                             false,
	}
	for model, want := range cases {
		if got := supportsVision(model); got != want {
			t.Errorf("supportsVision(%q) = %v, want %v", model, got, want)
		}
	}
}

// The wire shape is the contract: an image is only legal as a content block in a
// user message, so it must never be sent as a plain string field or attached to
// an assistant/tool turn.
func TestWireMessagesCarryImagesAsUserBlocks(t *testing.T) {
	a := newTestAgent(t)
	img := imagePart{Name: "screen.png", DataURL: "data:image/png;base64,AAAA"}

	a.mu.Lock()
	a.messages = []Msg{
		{Role: "system", Content: "sys"},
		{Role: "assistant", Content: "look", Images: nil},
		{Role: "user", Content: "what is this?", Images: []imagePart{img}},
	}
	a.mu.Unlock()

	wire := a.wireMessages()
	raw, err := json.Marshal(wire)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if string(decoded[1].Content) != `"look"` {
		t.Fatalf("a message without images must keep the plain string form, got %s", decoded[1].Content)
	}

	var blocks []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(decoded[2].Content, &blocks); err != nil {
		t.Fatalf("user content is not a block array: %v (%s)", err, decoded[2].Content)
	}
	if len(blocks) != 2 || blocks[0].Type != "text" || blocks[1].Type != "image_url" {
		t.Fatalf("unexpected blocks: %+v", blocks)
	}
	if !strings.Contains(blocks[0].Text, "what is this?") || !strings.Contains(blocks[0].Text, "screen.png") {
		t.Fatalf("text block must carry the task and the image name: %q", blocks[0].Text)
	}
	if blocks[1].ImageURL.URL != img.DataURL {
		t.Fatalf("image block lost its payload")
	}
}

// History stays sendable and the image survives normalizeHistory, which rebuilds
// every assistant/tool pairing on the way out.
func TestImagesSurviveHistoryRepair(t *testing.T) {
	a := newTestAgent(t)
	img := imagePart{Name: "a.png", DataURL: "data:image/png;base64,AAAA"}
	a.mu.Lock()
	a.messages = []Msg{
		{Role: "system", Content: "sys"},
		user("look"),
		{Role: "assistant", ToolCalls: []ToolCall{call("a", "screenshot")}},
		toolMsg("a", "SHOT:"+filepath.Join(t.TempDir(), "x.png")),
		{Role: "user", Content: "here it is", Images: []imagePart{img}},
	}
	a.mu.Unlock()

	out := normalizeHistory(a.messages)
	if err := checkGrammar(out); err != nil {
		t.Fatalf("history with an attached image is unusable: %v", err)
	}
	if len(out) != 5 || len(out[4].Images) != 1 {
		t.Fatalf("image lost in repair: %+v", out)
	}
}

// A long session must not resend every screenshot forever: the oldest payloads
// are dropped, and the text says so rather than pretending the image is there.
func TestPruneImagesKeepsOneBudget(t *testing.T) {
	a := newTestAgent(t)
	big := strings.Repeat("A", maxInlineImageBytes/2+1)
	a.mu.Lock()
	a.messages = []Msg{
		{Role: "user", Content: "first", Images: []imagePart{{Name: "one.png", DataURL: big}}},
		{Role: "assistant", Content: "ok"},
		{Role: "user", Content: "second", Images: []imagePart{{Name: "two.png", DataURL: big}}},
	}
	a.pruneImages()
	first := a.messages[0]
	second := a.messages[2]
	a.mu.Unlock()

	if len(first.Images) != 0 {
		t.Fatal("oldest image payload was kept past the budget")
	}
	if !strings.Contains(first.Content, "one.png") || !strings.Contains(first.Content, "no longer sent") {
		t.Fatalf("the model is not told the image is gone: %q", first.Content)
	}
	if len(second.Images) != 1 {
		t.Fatal("the newest image was dropped as well")
	}
}

func TestShotFromResult(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "shot-1.png")
	if _, ok := shotFromResult("Captured the screen.\nSHOT:" + p + "\n"); !ok {
		t.Fatal("tag not recognised")
	}
	got, ok := shotFromResult("SHOT:" + p)
	if !ok || got != p {
		t.Fatalf("got %q, ok=%v", got, ok)
	}
	if _, ok := shotFromResult("ERROR: screen capture failed"); ok {
		t.Fatal("a failure was read as a screenshot")
	}
}

// attachShot is the bridge between the tool result and the API: the picture has
// to land as a user message with image blocks, or the model never sees it.
func TestAttachShotAppendsUserImage(t *testing.T) {
	a := newTestAgent(t)
	a.cfg.Model = "deepseek-flash"

	src, err := os.ReadFile(filepath.Join("testdata", "vision", "sample.png"))
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	path := filepath.Join(t.TempDir(), "shot-1.png")
	if err := os.WriteFile(path, src, 0600); err != nil {
		t.Fatalf("write: %v", err)
	}

	var events []string
	a.attachShot(path, func(kind, text string) { events = append(events, kind) })

	a.mu.Lock()
	msgs := append([]Msg(nil), a.messages...)
	a.mu.Unlock()

	if len(msgs) != 1 || msgs[0].Role != "user" || len(msgs[0].Images) != 1 {
		t.Fatalf("no user message with an image: %+v", msgs)
	}
	if !strings.HasPrefix(msgs[0].Images[0].DataURL, "data:image/png;base64,") {
		t.Fatalf("unexpected data URL: %q", msgs[0].Images[0].DataURL[:40])
	}
	if !contains(events, "image") {
		t.Fatalf("the browser was not sent the picture: %v", events)
	}
}

// A model that cannot see must not be handed a picture: the API answers with a
// 400 mid-run, which reads like a broken app instead of a setting.
func TestAttachShotRefusesTextOnlyModel(t *testing.T) {
	a := newTestAgent(t)
	a.cfg.Model = "deepseek-v4-pro"

	src, _ := os.ReadFile(filepath.Join("testdata", "vision", "sample.png"))
	path := filepath.Join(t.TempDir(), "shot-1.png")
	os.WriteFile(path, src, 0600)

	var events []string
	a.attachShot(path, func(kind, text string) { events = append(events, kind) })

	a.mu.Lock()
	msgs := append([]Msg(nil), a.messages...)
	a.mu.Unlock()

	if len(msgs) != 1 || len(msgs[0].Images) != 0 {
		t.Fatalf("a picture was attached to a text-only model: %+v", msgs)
	}
	if !strings.Contains(msgs[0].Content, "could not be attached") {
		t.Fatalf("the model was not told it cannot see: %q", msgs[0].Content)
	}
	if contains(events, "image") {
		t.Fatal("the browser was shown a picture the model never received")
	}
}

// handleChat refuses images for a text-only model before any run starts, so the
// composer gets one clear error instead of a failed task.
func TestHandleChatRefusesImagesForTextOnlyModel(t *testing.T) {
	a := newTestAgent(t)
	a.cfg.APIKey = "sk-[REDACTED]"
	a.cfg.Model = "deepseek-v4-pro"

	body, _ := json.Marshal(map[string]any{
		"task":   "what is on screen?",
		"mode":   "agent",
		"images": []chatImage{{Name: "a.png", Data: fixtureDataURL(t, "sample.png")}},
	})
	w := httptest.NewRecorder()
	a.handleChat(w, httptest.NewRequest("POST", "/api/chat", strings.NewReader(string(body))))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "deepseek-flash") {
		t.Fatalf("the error does not say what to switch to: %s", w.Body.String())
	}
	a.mu.Lock()
	busy := a.isRunning
	a.mu.Unlock()
	if busy {
		t.Fatal("a refused request left the agent marked as running")
	}
}

// The whole path, end to end: the model calls the screenshot tool, the run
// captures the screen, and the very next request to the API carries the picture
// as an image block on a user message.
//
// Windows only, because that is the only platform captureScreenshot implements;
// elsewhere it returns an explanatory error and no image exists to attach.
func TestScreenshotReachesTheNextRequest(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("screen capture is implemented on Windows only")
	}

	var mu sync.Mutex
	var withImage int
	var sawToolResult bool
	handler := func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		json.Unmarshal(body, &req)

		mu.Lock()
		n := withImage
		for _, m := range req.Messages {
			if m.Role == "user" && strings.Contains(string(m.Content), `"image_url"`) {
				withImage++
				break
			}
			if m.Role == "tool" && strings.Contains(string(m.Content), shotTag) {
				sawToolResult = true
			}
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
		if n == 0 && !sawToolResult {
			// Step one: ask for a small, cheap capture.
			send(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"shot1","type":"function","function":{"name":"screenshot","arguments":"{\"width\":64,\"height\":48,\"scale\":1}"}}]}}]}`)
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

	a := newTestAgent(t)
	a.cfg.APIKey = "sk-[REDACTED]"
	a.cfg.Model = "deepseek-flash"
	setAutoApprove(a.cfg, true) // the capture is approved by "Always Allow"

	var events []string
	a.run(context.Background(), "look at my screen", ModeAgent, nil, func(kind, text string) {
		events = append(events, kind)
	})

	mu.Lock()
	got := withImage
	sawTool := sawToolResult
	mu.Unlock()

	if !sawTool {
		t.Fatalf("the screenshot tool did not run: %v", events)
	}
	if got == 0 {
		t.Fatalf("the captured image never reached a request: %v", events)
	}
	if !contains(events, "image") {
		t.Fatalf("the browser was not shown the capture: %v", events)
	}
}
