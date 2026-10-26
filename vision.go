package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Image input limits.
//
// The API documents its own caps (32 MiB per inline image, 48 MiB per request
// body, 600 images per request). Those are sized for a server-side caller; this
// agent keeps the bytes in memory on the user's own machine, so the numbers here
// are deliberately smaller and chosen so a request stays inside the documented
// body limit after base64 inflation (4/3): 20 MiB of pixels is ~27 MiB on the
// wire, which leaves room for the transcript itself.
const (
	maxImagesPerRequest = 8
	maxImageBytes       = 10 << 20 // decoded, per image
	maxImagesTotalBytes = 20 << 20 // decoded, per request
	maxInlineImageBytes = 12 << 20 // base64 kept in the transcript
	maxChatBody         = 32 << 20 // whole POST /api/chat body
)

// imageMIME is exactly what the Chat Completions API accepts today: JPEG, PNG,
// GIF and WebP. Any other type is refused here rather than by a 400 mid-run.
var imageMIME = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
}

// imagePart is one image riding along with a user turn. DataURL is a base64
// data: URL, which is how an inline image is handed to the API.
type imagePart struct {
	Name    string
	DataURL string
}

// chatImage is the shape the browser posts in POST /api/chat.
type chatImage struct {
	Name string `json:"name"`
	Data string `json:"data"`
}

// normalizeImages validates an upload and rebuilds each data URL from what the
// bytes actually are rather than from what the file was called. The API sniffs
// the content as well, so a mislabelled file would otherwise only surface as an
// error in the middle of a run.
func normalizeImages(in []chatImage) ([]imagePart, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if len(in) > maxImagesPerRequest {
		return nil, fmt.Errorf("%d images attached; at most %d are accepted per task", len(in), maxImagesPerRequest)
	}

	out := make([]imagePart, 0, len(in))
	total := 0
	for i, im := range in {
		name := strings.TrimSpace(im.Name)
		if name == "" {
			name = fmt.Sprintf("image-%d", i+1)
		}
		payload, err := base64Payload(im.Data)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", name, err)
		}
		raw, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return nil, fmt.Errorf("%s: the image data is not valid base64", name)
		}
		if len(raw) == 0 {
			return nil, fmt.Errorf("%s: the image is empty", name)
		}
		if len(raw) > maxImageBytes {
			return nil, fmt.Errorf("%s: %.1f MB is over the %d MB per-image limit",
				name, float64(len(raw))/(1<<20), maxImageBytes>>20)
		}
		total += len(raw)
		if total > maxImagesTotalBytes {
			return nil, fmt.Errorf("the attached images are over the %d MB total limit; attach fewer or smaller ones",
				maxImagesTotalBytes>>20)
		}
		mime := http.DetectContentType(raw)
		if !imageMIME[mime] {
			return nil, fmt.Errorf("%s: %s is not a JPEG, PNG, GIF or WebP image", name, mime)
		}
		out = append(out, imagePart{Name: name, DataURL: "data:" + mime + ";base64," + payload})
	}
	return out, nil
}

// base64Payload returns the base64 half of a data: URL. A bare payload is
// refused: the browser always sends the URL form, and without a declared type
// there is nothing for the caller to have formed an intention about.
func base64Payload(s string) (string, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(strings.ToLower(s), "data:") {
		return "", errors.New("image data must be a base64 data: URL")
	}
	comma := strings.IndexByte(s, ',')
	if comma < 0 {
		return "", errors.New("malformed data URL")
	}
	if !strings.HasSuffix(strings.ToLower(s[:comma]), ";base64") {
		return "", errors.New("only base64 data URLs are supported")
	}
	payload := s[comma+1:]
	if payload == "" {
		return "", errors.New("the image is empty")
	}
	return payload, nil
}

// supportsVision reports whether a model name accepts image input.
//
// Taken from the Models & Pricing page: deepseek-flash (DeepSeek-V4.1-Flash-1017)
// has Vision, deepseek-v4-pro (DeepSeek-V4-Pro-0813) does not. The retired names
// deepseek-v4-flash and deepseek-v4-flash-vision-exp are still served by the
// Flash model, so they inherit its support.
//
// It is a deny-list of the text-only families rather than an allow-list of the
// known vision ones: an unfamiliar or newer Flash revision should be tried and,
// if it really cannot see, answered by the API's own error - not refused here
// because this list was out of date.
func supportsVision(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return false
	}
	if strings.Contains(m, "v4-pro") || strings.Contains(m, "reasoner") {
		return false
	}
	return true
}

// imageNote names the images riding on a user turn, so the model can refer to a
// screenshot by name instead of describing "the image".
func imageNote(images []imagePart) string {
	if len(images) == 0 {
		return ""
	}
	names := make([]string, 0, len(images))
	for _, im := range images {
		names = append(names, im.Name)
	}
	label := "image"
	if len(names) > 1 {
		label = "images"
	}
	return "\n\n[attached " + label + ": " + strings.Join(names, ", ") + "]"
}

// pruneImages keeps the transcript's inline image data inside one budget.
//
// An image the model has already read stays in the history, because a follow-up
// ("and what colour is the car?") is a reasonable thing to ask. What must not
// happen is every request for the rest of the session carrying megabytes of
// base64, so once the total passes maxInlineImageBytes the oldest payloads are
// replaced by a line naming them. The name survives, which tells the model the
// truth: an image existed and is no longer attached, so it can ask for it again
// instead of pretending it can still see it.
//
// Called with a.mu held.
func (a *Agent) pruneImages() {
	total := 0
	for i := len(a.messages) - 1; i >= 0; i-- {
		m := &a.messages[i]
		if len(m.Images) == 0 {
			continue
		}
		size := 0
		for _, im := range m.Images {
			size += len(im.DataURL)
		}
		if total+size <= maxInlineImageBytes {
			total += size
			continue
		}
		names := make([]string, 0, len(m.Images))
		for _, im := range m.Images {
			names = append(names, im.Name)
		}
		m.Images = nil
		m.Content = strings.TrimRight(m.Content, "\n") +
			"\n\n[attached image, no longer sent with the request: " + strings.Join(names, ", ") + "]"
	}
}

// imagePartFromFile turns a file on disk into one inline image, validating it
// the same way an upload is validated: the type is taken from the bytes, and the
// size caps are the ones normalizeImages enforces.
func imagePartFromFile(path string) (imagePart, error) {
	f, err := os.Open(path)
	if err != nil {
		return imagePart{}, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return imagePart{}, err
	}
	if st.Size() == 0 {
		return imagePart{}, errors.New("the file is empty")
	}
	if st.Size() > maxImageBytes {
		return imagePart{}, fmt.Errorf("%.1f MB is over the %d MB image limit; capture a smaller region or pass scale",
			float64(st.Size())/(1<<20), maxImageBytes>>20)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxImageBytes+1))
	if err != nil {
		return imagePart{}, err
	}
	mime := http.DetectContentType(raw)
	if !imageMIME[mime] {
		return imagePart{}, fmt.Errorf("%s is not a JPEG, PNG, GIF or WebP image", mime)
	}
	name := filepath.Base(path)
	return imagePart{Name: name, DataURL: "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(raw)}, nil
}

// attachShot hands a captured screenshot to the model.
//
// It has to be a user message: the API accepts image blocks in user messages
// only, so the picture cannot ride on the tool result that produced it. The
// message lands directly after that result, which is legal grammar and reads
// naturally - the model sees "here is the picture you asked for" on its next
// step, which is also why the tool description promises the image one step
// later rather than immediately.
func (a *Agent) attachShot(path string, emit func(string, string)) {
	model, _ := credentials(a.cfg)

	part, err := imagePartFromFile(path)
	if err == nil && !supportsVision(model) {
		err = fmt.Errorf("the model %s does not accept images", model)
	}
	if err != nil {
		// Tell the model the truth instead of leaving it to describe a screen it
		// never received.
		a.mu.Lock()
		a.messages = append(a.messages, Msg{Role: "user",
			Content: "NOTE: the screenshot was captured but could not be attached (" + err.Error() + "). You cannot see it: say so, and ask the user to attach the image themselves."})
		a.mu.Unlock()
		emit("status", "Screenshot captured but not attached: "+err.Error())
		return
	}

	a.mu.Lock()
	a.messages = append(a.messages, Msg{Role: "user",
		Content: "[screenshot attached: " + part.Name + "] This is the screen as it is right now. Describe only what is actually visible in it.",
		Images:  []imagePart{part}})
	a.mu.Unlock()

	// The same picture goes to the browser, so the transcript shows what the
	// agent was looking at instead of asking the user to trust a description.
	emit("image", part.DataURL)
	emit("status", "Screenshot attached to the conversation: "+part.Name)
}
