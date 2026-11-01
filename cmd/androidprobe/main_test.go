package main

import (
	"context"
	"strings"
	"testing"

	"dsagent/android"
)

// fixedRunner answers the one call captureFor makes, so the CLI's -frame path
// can be tested without a phone.
type fixedRunner struct{}

func (fixedRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	if strings.Contains(strings.Join(args, " "), "wm size") {
		return []byte("Physical size: 720x1600\n"), nil
	}
	return nil, nil
}

func TestParseFrame(t *testing.T) {
	good := []struct {
		in   string
		w, h int
	}{
		{"400x889", 400, 889},
		{"400X889", 400, 889},
		{" 360x800 ", 360, 800},
	}
	for _, tc := range good {
		w, h, err := parseFrame(tc.in)
		if err != nil {
			t.Errorf("parseFrame(%q): %v", tc.in, err)
			continue
		}
		if w != tc.w || h != tc.h {
			t.Errorf("parseFrame(%q) = %dx%d, want %dx%d", tc.in, w, h, tc.w, tc.h)
		}
	}

	for _, bad := range []string{"", "400", "x889", "0x100", "400x0", "400xbig"} {
		if _, _, err := parseFrame(bad); err == nil {
			t.Errorf("parseFrame(%q) must be refused", bad)
		}
	}
}

func TestCaptureForMapsTheFrameOntoTheDevice(t *testing.T) {
	c := &android.Client{Runner: fixedRunner{}}

	cp, err := captureFor(c, "400x889")
	if err != nil {
		t.Fatalf("captureFor: %v", err)
	}
	if cp.Width != 400 || cp.Height != 889 {
		t.Errorf("frame = %dx%d, want 400x889", cp.Width, cp.Height)
	}
	if cp.Display.Width != 720 || cp.Display.Height != 1600 {
		t.Errorf("display = %+v, want the device size read from wm size", cp.Display)
	}
	at, err := cp.Map(android.Point{X: 200, Y: 340})
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if at.X != 360 || at.Y != 612 {
		t.Errorf("Map(200,340) = %v, want 360,612", at)
	}
}
