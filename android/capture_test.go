package android

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"strings"
	"testing"
)

// scriptedRunner answers each adb call from a function, so a test can play a
// whole device: a power dump, a window dump, a size and a screenshot are
// different commands with different output, and a single canned reply cannot
// stand in for them.
type scriptedRunner struct {
	calls [][]string
	reply func(args []string) ([]byte, error)
}

func (s *scriptedRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	s.calls = append(s.calls, append([]string(nil), args...))
	if s.reply == nil {
		return nil, nil
	}
	return s.reply(args)
}

func (s *scriptedRunner) lastCall(t *testing.T) []string {
	t.Helper()
	if len(s.calls) == 0 {
		t.Fatal("no adb call was made")
	}
	return s.calls[len(s.calls)-1]
}

// deviceDumps is a fake phone built from the four dumps plus one frame.
type deviceDumps struct {
	power   string
	window  string
	size    string
	density string
	png     []byte
}

func (d deviceDumps) run(args []string) ([]byte, error) {
	joined := strings.Join(args, " ")
	switch {
	case strings.Contains(joined, "dumpsys power"):
		return []byte(d.power), nil
	case strings.Contains(joined, "dumpsys window"):
		return []byte(d.window), nil
	case strings.Contains(joined, "wm size"):
		return []byte(d.size), nil
	case strings.Contains(joined, "wm density"):
		return []byte(d.density), nil
	case strings.Contains(joined, "screencap"):
		return d.png, nil
	}
	return nil, nil
}

// solidPNG is the black frame a sleeping phone hands back: a valid PNG with
// nothing in it, which is exactly the case the signature check cannot catch.
func solidPNG(t *testing.T, w, h int, c color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: c}, image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encoding the test PNG: %v", err)
	}
	return buf.Bytes()
}

// homeScreenPNG stands in for a real capture: mostly background with a few
// icons drawn on it.
func homeScreenPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: color.RGBA{200, 210, 220, 255}}, image.Point{}, draw.Src)
	for i := 0; i < 4; i++ {
		icon := image.Rect(40+i*160, 200, 140+i*160, 300)
		draw.Draw(img, icon, &image.Uniform{C: color.RGBA{250, 60, 90, 255}}, image.Point{}, draw.Src)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encoding the test PNG: %v", err)
	}
	return buf.Bytes()
}

func TestCaptureReportsSizeStateAndScale(t *testing.T) {
	runner := &scriptedRunner{reply: deviceDumps{
		power:   powerAwake,
		window:  windowUnlocked,
		size:    "Physical size: 720x1600\n",
		density: "Physical density: 320\n",
		png:     homeScreenPNG(t, 720, 1600),
	}.run}
	c := &Client{Runner: runner, Serial: "R58M12345"}

	cp, err := c.Capture(context.Background(), 0.5)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	if cp.Width != 360 || cp.Height != 800 {
		t.Errorf("image size = %dx%d, want 360x800 (720x1600 at scale 0.5)", cp.Width, cp.Height)
	}
	if cp.Scale != 0.5 {
		t.Errorf("scale = %v, want 0.5", cp.Scale)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(cp.PNG))
	if err != nil {
		t.Fatalf("the returned PNG does not decode: %v", err)
	}
	if cfg.Width != 360 || cfg.Height != 800 {
		t.Errorf("returned PNG is %dx%d, want 360x800", cfg.Width, cfg.Height)
	}
	if cp.Display != (Display{Width: 720, Height: 1600, Density: 320}) {
		t.Errorf("display = %+v", cp.Display)
	}
	if !cp.Screen.Awake() || cp.Screen.Keyguard {
		t.Errorf("screen = %+v, want awake and unlocked", cp.Screen)
	}
	if cp.Blank {
		t.Errorf("a drawn frame was reported blank: %s", cp.BlankReason)
	}
	if got := cp.Describe(); !strings.Contains(got, "scale 0.50") || !strings.Contains(got, "image 360x800 of a 720x1600 device") {
		t.Errorf("Describe = %q, want the coordinate space spelled out", got)
	}
}

func TestCaptureFlagsABlackFrameOnASleepingPhone(t *testing.T) {
	runner := &scriptedRunner{reply: deviceDumps{
		power:  powerAsleep,
		window: windowLocked,
		size:   "Physical size: 720x1600\n",
		png:    solidPNG(t, 720, 1600, color.RGBA{0, 0, 0, 255}),
	}.run}
	c := &Client{Runner: runner}

	cp, err := c.Capture(context.Background(), 0)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if !cp.Blank {
		t.Fatal("a black frame from a sleeping phone must be flagged blank")
	}
	for _, want := range []string{"uniform colour", "mWakefulness=Asleep"} {
		if !strings.Contains(cp.BlankReason, want) {
			t.Errorf("BlankReason = %q, want it to mention %q", cp.BlankReason, want)
		}
	}
	if cp.Scale != 1 {
		t.Errorf("scale = %v, want 1 for a native capture", cp.Scale)
	}
}

func TestCaptureFlagsABlackFrameOnAnAwakePhone(t *testing.T) {
	// The pixels alone are enough here: the device says it is awake, and it is
	// still showing nothing.
	runner := &scriptedRunner{reply: deviceDumps{
		power:  powerAwake,
		window: windowUnlocked,
		size:   "Physical size: 720x1600\n",
		png:    solidPNG(t, 720, 1600, color.RGBA{0, 0, 0, 255}),
	}.run}

	cp, err := (&Client{Runner: runner}).Capture(context.Background(), 0)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if !cp.Blank || !strings.Contains(cp.BlankReason, "uniform colour") {
		t.Fatalf("Blank = %v, reason = %q", cp.Blank, cp.BlankReason)
	}
}

func TestCaptureStillReturnsTheFrameWhenTheDumpsFail(t *testing.T) {
	// The dumps and wm fail; the frame itself arrives. A capture that refused to
	// happen because a dump failed would be worse than one that admits it does
	// not know the state.
	runner := &scriptedRunner{reply: func(args []string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "screencap") {
			return homeScreenPNG(t, 720, 1600), nil
		}
		return nil, errors.New("error: closed")
	}}

	cp, err := (&Client{Runner: runner}).Capture(context.Background(), 0)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if cp.Width != 720 || cp.Height != 1600 {
		t.Errorf("image size = %dx%d, want the frame that did arrive", cp.Width, cp.Height)
	}
	if cp.Screen.Known() || cp.Display.Width != 0 {
		t.Errorf("unreadable state must stay zeroed, got screen %+v display %+v", cp.Screen, cp.Display)
	}
	if cp.Blank {
		t.Errorf("an unknown state must not be reported as blank: %q", cp.BlankReason)
	}
	if w, h := cp.DeviceSize(); w != 720 || h != 1600 {
		t.Errorf("DeviceSize = %dx%d, want the image size as the fallback", w, h)
	}
}

func TestCaptureRejectsAnOutOfRangeScale(t *testing.T) {
	runner := &scriptedRunner{reply: deviceDumps{}.run}
	c := &Client{Runner: runner}

	for _, scale := range []float64{0.01, 1.5, -1} {
		if _, err := c.Capture(context.Background(), scale); err == nil {
			t.Errorf("scale %v must be refused", scale)
		}
	}
	if len(runner.calls) != 0 {
		t.Errorf("an out-of-range scale still reached adb: %v", runner.calls)
	}
}

func TestMapTurnsImagePixelsIntoDevicePixels(t *testing.T) {
	cp := Capture{Width: 400, Height: 889, Display: Display{Width: 720, Height: 1600}}

	got, err := cp.Map(Point{X: 200, Y: 340})
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if want := (Point{X: 360, Y: 612}); got != want {
		t.Errorf("Map(200,340) = %v, want %v", got, want)
	}

	if _, err := cp.Map(Point{X: -1, Y: 0}); err == nil {
		t.Error("a negative point must be refused")
	}
	if _, err := cp.Map(Point{X: 400, Y: 0}); err == nil {
		t.Error("a point on the right edge is outside the image and must be refused")
	}
}

func TestMapFallsBackToTheScaleWhenTheDisplayIsUnknown(t *testing.T) {
	cp := Capture{Width: 360, Height: 800, Scale: 0.5}

	if w, h := cp.DeviceSize(); w != 720 || h != 1600 {
		t.Fatalf("DeviceSize = %dx%d, want 720x1600 from the scale", w, h)
	}
	got, err := cp.Map(Point{X: 180, Y: 400})
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if want := (Point{X: 360, Y: 800}); got != want {
		t.Errorf("Map(180,400) = %v, want %v", got, want)
	}
}

func TestTapCaptureMapsBeforeTapping(t *testing.T) {
	runner := &scriptedRunner{reply: deviceDumps{
		power:  powerAwake,
		window: windowUnlocked,
		size:   "Physical size: 720x1600\n",
	}.run}
	c := &Client{Runner: runner}

	cp := Capture{Width: 400, Height: 889, Display: Display{Width: 720, Height: 1600}}
	if err := c.TapCapture(context.Background(), cp, Point{X: 200, Y: 340}); err != nil {
		t.Fatalf("TapCapture: %v", err)
	}
	want := "shell input tap 360 612"
	if got := strings.Join(runner.lastCall(t), " "); got != want {
		t.Errorf("args = %q, want %q", got, want)
	}
}

func TestTapCaptureRefusesAPointOutsideTheImage(t *testing.T) {
	runner := &scriptedRunner{reply: deviceDumps{}.run}
	c := &Client{Runner: runner}

	cp := Capture{Width: 400, Height: 889}
	if err := c.TapCapture(context.Background(), cp, Point{X: 900, Y: 10}); err == nil {
		t.Fatal("a point outside the capture must be refused")
	}
	if len(runner.calls) != 0 {
		t.Errorf("an out-of-image point still reached adb: %v", runner.calls)
	}
}

func TestResizePNGKeepsTheAspectRatio(t *testing.T) {
	out, w, h, err := resizePNG(homeScreenPNG(t, 720, 1600), 0.25)
	if err != nil {
		t.Fatalf("resizePNG: %v", err)
	}
	if w != 180 || h != 400 {
		t.Fatalf("resized to %dx%d, want 180x400", w, h)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("the resized PNG does not decode: %v", err)
	}
	if cfg.Width != w || cfg.Height != h {
		t.Errorf("encoded size = %dx%d, want %dx%d", cfg.Width, cfg.Height, w, h)
	}
}

func TestUniformFrame(t *testing.T) {
	if _, blank := uniformFrame(homeScreenPNG(t, 200, 400)); blank {
		t.Error("a frame with icons drawn on it is not uniform")
	}
	reason, blank := uniformFrame(solidPNG(t, 200, 400, color.RGBA{0, 0, 0, 255}))
	if !blank || reason == "" {
		t.Errorf("a black frame must be uniform, got (%q, %v)", reason, blank)
	}
	if _, blank := uniformFrame([]byte("not a png")); blank {
		t.Error("an undecodable frame must not be called blank")
	}
}
