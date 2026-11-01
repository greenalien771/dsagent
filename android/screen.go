package android

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Screen is the display state that decides whether input can do anything at
// all: an off panel swallows every tap, and a showing keyguard swallows
// everything except the unlock gesture. Both are read here, on the host.
//
// That last part is the point. The first version of this prototype discovered
// the hard way that filtering inside the phone's shell is not portable - the
// phone has grep, not findstr, and `/system/bin/sh: findstr: inaccessible or not
// found` is a whole wasted step. Every dumpsys call returns its full output and
// the parsing happens in Go, where the same code runs against every device and
// against a fake in the tests.
type Screen struct {
	// Wakefulness is mWakefulness from `dumpsys power`: "Awake", "Asleep",
	// "Dozing" or "Dreaming". Empty when the dump said nothing usable.
	Wakefulness string
	// Keyguard is whether the lock screen is up. Only meaningful when
	// KeyguardKnown is true.
	Keyguard bool
	// KeyguardKnown reports whether the dump answered the question at all. An
	// unknown keyguard is not the same thing as an unlocked phone.
	KeyguardKnown bool
}

// Awake reports whether the device is out of sleep with the panel on. Only
// "Awake" counts: a dozing or dreaming device is not showing an app that a tap
// would reach, and calling that awake is how a caller ends up tapping a
// screensaver.
func (s Screen) Awake() bool { return s.Wakefulness == "Awake" }

// Known reports whether wakefulness was actually read.
func (s Screen) Known() bool { return s.Wakefulness != "" }

// String is the one-line form a log or a tool result can carry.
func (s Screen) String() string {
	var parts []string
	switch {
	case !s.Known():
		parts = append(parts, "wakefulness unknown")
	default:
		parts = append(parts, strings.ToLower(s.Wakefulness))
	}
	switch {
	case !s.KeyguardKnown:
		parts = append(parts, "keyguard unknown")
	case s.Keyguard:
		parts = append(parts, "locked")
	default:
		parts = append(parts, "unlocked")
	}
	return strings.Join(parts, ", ")
}

// Display is the device screen in device pixels, which is the space input
// commands actually address.
type Display struct {
	Width   int
	Height  int
	Density int // dpi; 0 when it could not be read, which costs nothing
}

// Screen reads the power and keyguard state with two dumpsys calls.
//
// The keyguard lives in `dumpsys window` and the wakefulness in `dumpsys power`;
// there is no single cheap dump that carries both on every Android version, and
// guessing from one of them is what produces a "locked" phone that is not. When
// only one of the two calls works, the error is returned along with whatever was
// read, so a caller that only cares about input can carry on and a caller that
// needs both can stop.
func (c *Client) Screen(ctx context.Context) (Screen, error) {
	var s Screen
	powerErr := error(nil)
	windowErr := error(nil)

	if power, err := c.dump(ctx, "power"); err != nil {
		powerErr = err
	} else {
		s.Wakefulness = parseWakefulness(power)
	}
	if window, err := c.dump(ctx, "window"); err != nil {
		windowErr = err
	} else {
		s.Keyguard, s.KeyguardKnown = parseKeyguard(window)
	}

	switch {
	case powerErr != nil && windowErr != nil:
		return s, fmt.Errorf("android: screen state: %v; %v", powerErr, windowErr)
	case windowErr != nil:
		return s, windowErr
	}
	return s, nil
}

// Display reads the size and density with `wm size` and `wm density`. An override
// wins over the physical panel size, because that is the size commands and
// captures use.
func (c *Client) Display(ctx context.Context) (Display, error) {
	sizeOut, err := c.run(ctx, "shell", "wm", "size")
	if err != nil {
		return Display{}, err
	}
	d, err := parseSize(sizeOut)
	if err != nil {
		return Display{}, err
	}
	// Density is a nicety - a model sizing a capture needs the pixel count, not
	// the dpi - so a failure here must not lose the size that was just read.
	if densityOut, err := c.run(ctx, "shell", "wm", "density"); err == nil {
		d.Density = parseDensity(densityOut)
	}
	return d, nil
}

// dump runs one dumpsys service and hands back all of it. No pipes, no greps:
// see the Screen doc comment for why that rule exists.
func (c *Client) dump(ctx context.Context, service string) ([]byte, error) {
	return c.run(ctx, "shell", "dumpsys", service)
}

// keyguardKeys are the keys different Android versions use for the same fact,
// in order of preference. isKeyguardShowing is the most direct answer and the
// one Android 11 (the test device) prints.
var keyguardKeys = []string{
	"isKeyguardShowing=",
	"mDreamingLockscreen=",
	"mKeyguardShowing=",
	"mShowingLockscreen=",
}

// parseKeyguard answers "is the lock screen up" from a `dumpsys window` dump.
//
// Several keys can appear at once and disagree: mDreamingLockscreen is about the
// dream layer, and is true on a docked screensaver that is not actually locked.
// The first key present wins, in keyguardKeys order, rather than OR-ing them.
func parseKeyguard(out []byte) (showing, known bool) {
	values := map[string]string{}
	for _, line := range dumpLines(out) {
		for _, key := range keyguardKeys {
			v, ok := dumpValue(line, key)
			if !ok {
				continue
			}
			if _, seen := values[key]; !seen {
				values[key] = v
			}
		}
	}
	for _, key := range keyguardKeys {
		if v, ok := values[key]; ok {
			return v == "true", true
		}
	}
	return false, false
}

// parseWakefulness reads mWakefulness, falling back to the display power state
// that older builds print instead.
func parseWakefulness(out []byte) string {
	fallback := ""
	for _, line := range dumpLines(out) {
		if v, ok := dumpValue(line, "mWakefulness="); ok && v != "" {
			return v
		}
		if strings.Contains(line, "Display Power") {
			v, ok := dumpValue(line, "state=")
			if !ok {
				continue
			}
			switch v {
			case "ON":
				fallback = "Awake"
			case "OFF":
				fallback = "Asleep"
			case "DOZE", "DOZING":
				fallback = "Dozing"
			}
		}
	}
	return fallback
}

// parseSize reads `wm size`, preferring an override over the physical panel.
func parseSize(out []byte) (Display, error) {
	var physical, override Display
	for _, line := range dumpLines(out) {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		w, h, ok := parseWxH(value)
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "Physical size":
			physical = Display{Width: w, Height: h}
		case "Override size":
			override = Display{Width: w, Height: h}
		}
	}
	if override.Width > 0 {
		return override, nil
	}
	if physical.Width > 0 {
		return physical, nil
	}
	return Display{}, fmt.Errorf("android: `wm size` said nothing usable (%q)", strings.TrimSpace(string(out)))
}

// parseDensity reads `wm density`, preferring an override over the physical one.
func parseDensity(out []byte) int {
	override, physical := 0, 0
	for _, line := range dumpLines(out) {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || n <= 0 {
			continue
		}
		switch strings.TrimSpace(key) {
		case "Physical density":
			physical = n
		case "Override density":
			override = n
		}
	}
	if override > 0 {
		return override
	}
	return physical
}

func parseWxH(s string) (int, int, bool) {
	s = strings.TrimSpace(s)
	i := strings.IndexAny(s, "xX")
	if i < 0 {
		return 0, 0, false
	}
	w, errW := strconv.Atoi(strings.TrimSpace(s[:i]))
	h, errH := strconv.Atoi(strings.TrimSpace(s[i+1:]))
	if errW != nil || errH != nil || w <= 0 || h <= 0 {
		return 0, 0, false
	}
	return w, h, true
}

func dumpLines(out []byte) []string { return strings.Split(string(out), "\n") }

// dumpValue finds key anywhere in a line and returns the token after it. Tokens
// end at whitespace, so "isKeyguardShowing=true" next to other keys on the same
// line still yields "true".
func dumpValue(line, key string) (string, bool) {
	i := strings.Index(line, key)
	if i < 0 {
		return "", false
	}
	rest := line[i+len(key):]
	if j := strings.IndexAny(rest, " \t\r\n"); j >= 0 {
		rest = rest[:j]
	}
	return rest, true
}
