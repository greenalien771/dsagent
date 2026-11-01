package android

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"math"
	"time"
)

// MinScale is the smallest capture scale accepted. Below this the image is too
// small to read anything in, so a caller asking for it is asking the wrong
// question rather than asking for a small image.
const MinScale = 0.05

// Capture is one screenshot plus the two things the PNG bytes cannot tell you:
// how big the device's screen is, and what the device was doing when the frame
// was taken.
//
// It exists because of two failures seen on real hardware. A phone with the
// display off returns a perfectly valid, entirely black PNG - the signature
// check passes and the caller has no way to know it is looking at nothing. And a
// model that reads a downscaled image has no idea what coordinate space it is
// looking at, so it taps plausible nonsense. Width, Height, Scale and Display
// are the answer to the second; Screen and Blank are the answer to the first.
type Capture struct {
	// PNG is the image itself: exactly what Screenshot returned, or a scaled
	// copy of it when Scale < 1.
	PNG []byte
	// Width and Height are the image's own pixel size - the space a model reads
	// coordinates in.
	Width  int
	Height int
	// Scale is image pixels per device pixel. 0.5 means the image is half size.
	Scale float64
	// Display is the device screen in device pixels.
	Display Display
	// Screen is the power and keyguard state read just before the capture.
	Screen Screen
	// Blank means the frame carries no information: the panel was off, or
	// nothing was drawn. It is a flag, not an error, because a caller may
	// still want the bytes - but nobody should describe a blank frame as a
	// picture of an app.
	Blank bool
	// BlankReason says why Blank is set, in words a tool result can print.
	BlankReason string
}

// Capture takes a screenshot and reads the device state around it.
//
// scale is image pixels per device pixel in 0.05..1; 0 means native size. The
// state is read before the pixels on purpose: it is what turns "some bytes" into
// an honest answer, and it has to be known before the caller is handed the frame.
//
// The state and display reads are best effort. A device that will not answer
// dumpsys still has a screen worth showing, so a failure there leaves the fields
// zeroed and Capture reports what it could: a capture that refuses to happen
// because a dump failed would be worse than one that admits it does not know.
func (c *Client) Capture(ctx context.Context, scale float64) (Capture, error) {
	if scale != 0 && (scale < MinScale || scale > 1) {
		return Capture{}, fmt.Errorf("android: capture: scale %.3f is outside %.2f..1", scale, MinScale)
	}

	screen, _ := c.Screen(ctx)
	display, _ := c.Display(ctx)

	raw, err := c.Screenshot(ctx)
	if err != nil {
		return Capture{}, err
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return Capture{}, fmt.Errorf("android: capture: %w", err)
	}

	cp := Capture{
		PNG:     raw,
		Width:   cfg.Width,
		Height:  cfg.Height,
		Scale:   1,
		Display: display,
		Screen:  screen,
	}
	if scale > 0 && scale < 1 {
		scaled, width, height, err := resizePNG(raw, scale)
		if err != nil {
			return Capture{}, fmt.Errorf("android: capture: resize: %w", err)
		}
		cp.PNG, cp.Width, cp.Height, cp.Scale = scaled, width, height, scale
	}

	cp.noteBlank()
	return cp, nil
}

// Map converts a point in this capture's own pixels into device pixels, which is
// the space adb input commands address.
//
// This is the whole fix for coordinate guessing: a model that read a 400-pixel
// wide image says "tap at 200,340" in that image, and Map turns it into the real
// touch. A point outside the image is refused rather than clamped - a tap just
// off the edge is a silent miss, and a silent miss costs another capture, another
// screenshot upload and another step.
func (cp Capture) Map(p Point) (Point, error) {
	if cp.Width <= 0 || cp.Height <= 0 {
		return Point{}, fmt.Errorf("android: capture has no size (%dx%d)", cp.Width, cp.Height)
	}
	if p.X < 0 || p.Y < 0 || p.X >= cp.Width || p.Y >= cp.Height {
		return Point{}, fmt.Errorf("android: %d,%d is outside the %dx%d capture", p.X, p.Y, cp.Width, cp.Height)
	}
	w, h := cp.DeviceSize()
	return Point{
		X: (p.X*w + cp.Width/2) / cp.Width,
		Y: (p.Y*h + cp.Height/2) / cp.Height,
	}, nil
}

// DeviceSize is the device screen size this capture was taken from: the real
// display when it could be read, and otherwise what the image size and scale
// imply.
func (cp Capture) DeviceSize() (int, int) {
	if cp.Display.Width > 0 && cp.Display.Height > 0 {
		return cp.Display.Width, cp.Display.Height
	}
	if cp.Scale > 0 {
		return int(math.Round(float64(cp.Width) / cp.Scale)), int(math.Round(float64(cp.Height) / cp.Scale))
	}
	return cp.Width, cp.Height
}

// Describe is the one-line summary a tool result prints next to the image, so
// the model knows the coordinate space and whether the frame is worth reading.
func (cp Capture) Describe() string {
	w, h := cp.DeviceSize()
	out := fmt.Sprintf("image %dx%d of a %dx%d device", cp.Width, cp.Height, w, h)
	if cp.Display.Density > 0 {
		out += fmt.Sprintf(" at %d dpi", cp.Display.Density)
	}
	if cp.Scale > 0 && cp.Scale != 1 {
		out += fmt.Sprintf(" (scale %.2f: pass coordinates in image pixels)", cp.Scale)
	}
	out += "; screen " + cp.Screen.String()
	if cp.Blank {
		out += "; BLANK - " + cp.BlankReason
	}
	return out
}

// TapCapture taps at a point given in the capture's own pixel space.
func (c *Client) TapCapture(ctx context.Context, cp Capture, p Point) error {
	at, err := cp.Map(p)
	if err != nil {
		return err
	}
	return c.Tap(ctx, at.X, at.Y)
}

// SwipeCapture drags between two points given in the capture's own pixel space.
// The gesture is still gesture-controlled when the keyguard is up: a swipe up
// from the bottom of the screen is how a locked phone is opened, and it is how
// Unlock does its work.
func (c *Client) SwipeCapture(ctx context.Context, cp Capture, from, to Point, duration time.Duration) error {
	a, err := cp.Map(from)
	if err != nil {
		return err
	}
	b, err := cp.Map(to)
	if err != nil {
		return err
	}
	return c.swipe(ctx, a, b, duration, false)
}

// noteBlank sets Blank when the frame cannot be a view of anything.
//
// Two witnesses, because each sees something the other misses: the pixels catch
// a black frame on an awake device, and the power state catches a sleeping
// device whose frame happens to vary - an always-on clock, or a stale
// framebuffer left behind by the last app.
func (cp *Capture) noteBlank() {
	var reasons []string
	if reason, blank := uniformFrame(cp.PNG); blank {
		reasons = append(reasons, reason)
	}
	if cp.Screen.Known() && !cp.Screen.Awake() {
		reasons = append(reasons, fmt.Sprintf("the device reports mWakefulness=%s, so this frame is not a view of an app", cp.Screen.Wakefulness))
	}
	if len(reasons) == 0 {
		return
	}
	cp.Blank = true
	cp.BlankReason = joinReasons(reasons)
}

func joinReasons(reasons []string) string {
	out := reasons[0]
	for _, r := range reasons[1:] {
		out += "; " + r
	}
	return out
}

// uniformFrame reports whether a frame carries no drawing at all. It samples on
// a stride that is coprime with the common tile sizes, so a small drawn detail
// has a fair chance of being seen instead of falling between samples.
func uniformFrame(raw []byte) (string, bool) {
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return "", false
	}
	b := img.Bounds()
	const stride = 7

	var firstR, firstG, firstB uint32
	var sum uint64
	var n uint64
	sampled := false
	uniform := true

	for y := b.Min.Y; y < b.Max.Y; y += stride {
		for x := b.Min.X; x < b.Max.X; x += stride {
			r, g, bl, _ := img.At(x, y).RGBA()
			r, g, bl = r>>8, g>>8, bl>>8
			sum += uint64(r+g+bl) / 3
			n++
			if !sampled {
				firstR, firstG, firstB = r, g, bl
				sampled = true
				continue
			}
			if !nearByte(r, firstR, 2) || !nearByte(g, firstG, 2) || !nearByte(bl, firstB, 2) {
				uniform = false
			}
		}
	}
	if !sampled {
		return "", false
	}
	if uniform {
		return "the frame is a single uniform colour, so nothing was drawn", true
	}
	if sum/n < 8 {
		return "the frame is almost entirely black", true
	}
	return "", false
}

func nearByte(a, b, tolerance uint32) bool {
	if a > b {
		return a-b <= tolerance
	}
	return b-a <= tolerance
}

// resizePNG shrinks a PNG to scale, nearest neighbour. The dependency budget for
// this prototype is the standard library, and a nearest-neighbour shrink of a
// screenshot is not the place to spend it: the image is being made small enough
// to read, not archived.
func resizePNG(raw []byte, scale float64) ([]byte, int, int, error) {
	src, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, 0, 0, err
	}
	b := src.Bounds()
	w := int(math.Round(float64(b.Dx()) * scale))
	h := int(math.Round(float64(b.Dy()) * scale))
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}

	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		sy := b.Min.Y + y*b.Dy()/h
		for x := 0; x < w; x++ {
			sx := b.Min.X + x*b.Dx()/w
			dst.Set(x, y, src.At(sx, sy))
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, dst); err != nil {
		return nil, 0, 0, err
	}
	return buf.Bytes(), w, h, nil
}
