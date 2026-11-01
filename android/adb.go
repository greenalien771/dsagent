// Package android is a prototype of the Android side of dsagent: it drives a
// phone over adb the way the desktop tools drive the local machine.
//
// It is deliberately small and self-contained. It knows how to list devices,
// capture the screen, tap, swipe and read a slice of logcat, and nothing else
// yet. See README.md next to this file for the design and for what is missing
// before this could become a set of agent tools.
package android

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Runner is the only thing this package needs from the outside world: run adb
// with some arguments and hand back its stdout. It is an interface so tests can
// answer without an attached phone or an adb binary on the machine.
type Runner interface {
	Run(ctx context.Context, args ...string) ([]byte, error)
}

// ErrNoDevices means adb answered and nothing was attached.
var ErrNoDevices = errors.New("android: no devices attached")

// ErrNoDevice means the serial asked for is not in the attached list.
var ErrNoDevice = errors.New("android: device not found")

// ErrDeviceNotReady means the device is attached but not usable - offline,
// unauthorized, or refusing due to udev permissions.
var ErrDeviceNotReady = errors.New("android: device not ready")

// CommandError is what a failing adb call returns. It keeps adb's own words,
// because adb reports most failures as exit status 1 with the reason on stderr
// ("error: device offline", "error: no devices/emulators found").
type CommandError struct {
	Args   []string
	Err    error
	Output string
}

func (e *CommandError) Error() string {
	switch e.Output {
	case "":
		return fmt.Sprintf("android: adb %s: %v", strings.Join(e.Args, " "), e.Err)
	default:
		return fmt.Sprintf("android: adb %s: %s", strings.Join(e.Args, " "), e.Output)
	}
}

func (e *CommandError) Unwrap() error { return e.Err }

// Device is one line of `adb devices -l`.
type Device struct {
	Serial      string
	State       string // "device", "offline", "unauthorized", "no permissions", ...
	Model       string
	Product     string
	Device      string
	TransportID string
}

// Ready reports whether commands against this device can be expected to work.
func (d Device) Ready() bool { return d.State == "device" }

// Point is a screen coordinate in device pixels.
type Point struct {
	X int
	Y int
}

// Client talks to one adb server, optionally pinned to one device.
type Client struct {
	// Runner runs adb. A nil Runner means the real adb on PATH, or Adb below.
	Runner Runner
	// Adb is the executable name or path used when Runner is nil.
	Adb string
	// Serial pins every call to one device, the way `adb -s` does. Commands
	// that need a device fail with ErrNoDevice when it is empty and more than
	// one device is attached, but this prototype does not resolve that for
	// you - it is the caller's job to pick a serial.
	Serial string
	// Timeout caps one adb call. Zero means DefaultTimeout.
	Timeout time.Duration
}

// DefaultTimeout is the per-call cap when Client.Timeout is zero. adb can block
// for a long time when a device is wedged, which is exactly when a prototype
// hanging would be most confusing.
const DefaultTimeout = 20 * time.Second

// Devices runs `adb devices -l` and returns everything attached, including
// devices that are not usable yet - State says which, so the caller decides
// whether to wait or to report.
func (c *Client) Devices(ctx context.Context) ([]Device, error) {
	out, err := c.run(ctx, "devices", "-l")
	if err != nil {
		return nil, err
	}
	devices := parseDevices(out)
	if len(devices) == 0 {
		return nil, ErrNoDevices
	}
	return devices, nil
}

// Device looks one serial up and refuses it unless it is ready, so a later
// command cannot fail with a vaguer message.
func (c *Client) Device(ctx context.Context, serial string) (Device, error) {
	if serial == "" {
		serial = c.Serial
	}
	devices, err := c.Devices(ctx)
	if err != nil {
		return Device{}, err
	}
	for _, d := range devices {
		if d.Serial != serial {
			continue
		}
		if !d.Ready() {
			return d, fmt.Errorf("android: device %q is %s: %w", d.Serial, d.State, ErrDeviceNotReady)
		}
		return d, nil
	}
	return Device{}, fmt.Errorf("android: device %q: %w", serial, ErrNoDevice)
}

// Screenshot captures the device screen with `exec-out screencap -p`. exec-out
// is used rather than shell because `adb shell` would translate line endings
// and corrupt the PNG.
func (c *Client) Screenshot(ctx context.Context) ([]byte, error) {
	out, err := c.run(ctx, "exec-out", "screencap", "-p")
	if err != nil {
		return nil, err
	}
	if !bytes.HasPrefix(out, pngMagic) {
		return nil, fmt.Errorf("android: screencap returned %d bytes that are not a PNG (first bytes %q)",
			len(out), firstBytes(out, 8))
	}
	return out, nil
}

// Tap sends one touch at x,y.
func (c *Client) Tap(ctx context.Context, x, y int) error {
	if x < 0 || y < 0 {
		return fmt.Errorf("android: tap: negative coordinate %d,%d", x, y)
	}
	_, err := c.run(ctx, "shell", "input", "tap", itoa(x), itoa(y))
	return err
}

// Swipe drags from one point to another. A zero duration lets `input` use its
// own default, which is the fastest a real drag gets.
func (c *Client) Swipe(ctx context.Context, from, to Point, duration time.Duration) error {
	if from.X < 0 || from.Y < 0 || to.X < 0 || to.Y < 0 {
		return fmt.Errorf("android: swipe: negative coordinate in %v -> %v", from, to)
	}
	args := []string{"shell", "input", "swipe", itoa(from.X), itoa(from.Y), itoa(to.X), itoa(to.Y)}
	if duration > 0 {
		args = append(args, itoa(int(duration/time.Millisecond)))
	}
	_, err := c.run(ctx, args...)
	return err
}

// MaxLogcatLines is the cap on one Logcat call. A whole buffer is megabytes,
// which is neither readable in a transcript nor cheap to send to a model.
const MaxLogcatLines = 5000

// Logcat returns the last n lines of the device log and nothing older.
func (c *Client) Logcat(ctx context.Context, n int) ([]string, error) {
	if n <= 0 || n > MaxLogcatLines {
		return nil, fmt.Errorf("android: logcat: %d lines is outside 1..%d", n, MaxLogcatLines)
	}
	out, err := c.run(ctx, "logcat", "-d", "-t", itoa(n))
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		lines = append(lines, line)
	}
	return lines, nil
}

// run prepends -s when the client is pinned and bounds the call in time.
func (c *Client) run(ctx context.Context, args ...string) ([]byte, error) {
	if c.Serial != "" {
		args = append([]string{"-s", c.Serial}, args...)
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	return c.runner().Run(ctx, args...)
}

func (c *Client) runner() Runner {
	if c.Runner != nil {
		return c.Runner
	}
	if c.Adb == "" {
		return execRunner{adb: "adb"}
	}
	return execRunner{adb: c.Adb}
}

func (c *Client) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return DefaultTimeout
}

// execRunner is the real Runner: one adb process per call.
type execRunner struct {
	adb string
}

func (r execRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, r.adb, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return stdout.Bytes(), &CommandError{Args: args, Err: err, Output: msg}
	}
	return stdout.Bytes(), nil
}

// pngMagic is the 8-byte PNG signature.
var pngMagic = []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}

// parseDevices reads `adb devices -l`. The listing is line-oriented and adb
// likes to interleave daemon chatter, so anything that is not a serial
// followed by a state is dropped rather than reported as a device.
func parseDevices(out []byte) []Device {
	var devices []Device
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || strings.HasPrefix(line, "List of devices") || strings.HasPrefix(line, "*") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		// A device the host cannot open reads as
		// "SERIAL no permissions (user in plugdev group; ...)".
		state := fields[1]
		if state == "no" && len(fields) > 2 && fields[2] == "permissions" {
			state = "no permissions"
		}
		d := Device{Serial: fields[0], State: state}
		for _, field := range fields[2:] {
			key, value, ok := strings.Cut(field, ":")
			if !ok {
				continue
			}
			switch key {
			case "model":
				d.Model = value
			case "product":
				d.Product = value
			case "device":
				d.Device = value
			case "transport_id":
				d.TransportID = value
			}
		}
		devices = append(devices, d)
	}
	return devices
}

// itoa keeps strconv out of the call sites; the values are never negative here.
func itoa(n int) string { return strconv.Itoa(n) }

func firstBytes(b []byte, n int) []byte {
	if len(b) < n {
		return b
	}
	return b[:n]
}
