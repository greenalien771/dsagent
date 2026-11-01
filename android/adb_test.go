package android

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeRunner stands in for adb: it records the arguments it was handed and
// answers with whatever the test set up.
type fakeRunner struct {
	calls [][]string
	out   []byte
	err   error
}

func (f *fakeRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string(nil), args...))
	return f.out, f.err
}

func (f *fakeRunner) lastCall(t *testing.T) []string {
	t.Helper()
	if len(f.calls) == 0 {
		t.Fatal("no adb call was made")
	}
	return f.calls[len(f.calls)-1]
}

// adbDevicesListing is the shape `adb devices -l` prints in practice, including
// the daemon chatter and the three states that matter.
const adbDevicesListing = `* daemon not running; starting now at tcp:5037
* daemon started successfully
List of devices attached
R58M12345              device product:beyond1lte model:SM_G973F device:beyond1lte transport_id:1
emulator-5554          offline
98AY00L0J8             no permissions (user in plugdev group; are your udev rules wrong?); see [http://developer.android.com/tools/device.html]

`

func TestDevicesParsesListing(t *testing.T) {
	fake := &fakeRunner{out: []byte(adbDevicesListing)}
	c := &Client{Runner: fake}

	devices, err := c.Devices(context.Background())
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}
	if len(devices) != 3 {
		t.Fatalf("got %d devices, want 3: %+v", len(devices), devices)
	}
	if got, want := strings.Join(fake.calls[0], " "), "devices -l"; got != want {
		t.Errorf("args = %q, want %q", got, want)
	}

	first := devices[0]
	if first.Serial != "R58M12345" || first.State != "device" {
		t.Errorf("first device = %+v", first)
	}
	if first.Model != "SM_G973F" || first.Product != "beyond1lte" || first.Device != "beyond1lte" {
		t.Errorf("first device details = %+v", first)
	}
	if first.TransportID != "1" {
		t.Errorf("transport id = %q, want 1", first.TransportID)
	}
	if !first.Ready() {
		t.Error("a device in state \"device\" must be ready")
	}

	if devices[1].State != "offline" || devices[1].Ready() {
		t.Errorf("emulator = %+v, want offline and not ready", devices[1])
	}
	if devices[2].State != "no permissions" {
		t.Errorf("third device state = %q, want \"no permissions\"", devices[2].State)
	}
}

func TestDevicesWithoutDevices(t *testing.T) {
	fake := &fakeRunner{out: []byte("List of devices attached\n\n")}
	c := &Client{Runner: fake}

	if _, err := c.Devices(context.Background()); !errors.Is(err, ErrNoDevices) {
		t.Fatalf("err = %v, want ErrNoDevices", err)
	}
}

func TestDevicesPropagatesAdbFailure(t *testing.T) {
	fake := &fakeRunner{err: errors.New("adb: server did not acknowledge")}
	c := &Client{Runner: fake}

	_, err := c.Devices(context.Background())
	if err == nil || !strings.Contains(err.Error(), "did not acknowledge") {
		t.Fatalf("err = %v, want adb's message", err)
	}
}

func TestSerialPinsEveryCall(t *testing.T) {
	fake := &fakeRunner{}
	c := &Client{Runner: fake, Serial: "R58M12345"}

	if err := c.Tap(context.Background(), 100, 200); err != nil {
		t.Fatalf("Tap: %v", err)
	}
	want := "-s R58M12345 shell input tap 100 200"
	if got := strings.Join(fake.lastCall(t), " "); got != want {
		t.Errorf("args = %q, want %q", got, want)
	}
}

func TestTapRejectsNegativeCoordinate(t *testing.T) {
	fake := &fakeRunner{}
	c := &Client{Runner: fake}

	if err := c.Tap(context.Background(), -1, 200); err == nil {
		t.Fatal("a negative coordinate must be refused")
	}
	if len(fake.calls) != 0 {
		t.Errorf("negative coordinate still reached adb: %v", fake.calls)
	}
}

func TestSwipeArgs(t *testing.T) {
	fake := &fakeRunner{}
	c := &Client{Runner: fake, Serial: "emulator-5554"}

	if err := c.Swipe(context.Background(), Point{10, 20}, Point{30, 40}, 300*time.Millisecond); err != nil {
		t.Fatalf("Swipe: %v", err)
	}
	want := "-s emulator-5554 shell input swipe 10 20 30 40 300"
	if got := strings.Join(fake.lastCall(t), " "); got != want {
		t.Errorf("args = %q, want %q", got, want)
	}

	// No duration is the two-point form: let input pick its own speed.
	if err := c.Swipe(context.Background(), Point{1, 2}, Point{3, 4}, 0); err != nil {
		t.Fatalf("Swipe without duration: %v", err)
	}
	want = "-s emulator-5554 shell input swipe 1 2 3 4"
	if got := strings.Join(fake.lastCall(t), " "); got != want {
		t.Errorf("args = %q, want %q", got, want)
	}
}

func TestScreenshotReturnsPNGBytes(t *testing.T) {
	png := append(append([]byte(nil), pngMagic...), []byte("\x00\x00\x00\rIHDR")...)
	fake := &fakeRunner{out: png}
	c := &Client{Runner: fake, Serial: "R58M12345"}

	got, err := c.Screenshot(context.Background())
	if err != nil {
		t.Fatalf("Screenshot: %v", err)
	}
	if string(got) != string(png) {
		t.Errorf("screenshot bytes were altered: %q", got)
	}
	want := "-s R58M12345 exec-out screencap -p"
	if args := strings.Join(fake.lastCall(t), " "); args != want {
		t.Errorf("args = %q, want %q", args, want)
	}
}

func TestScreenshotRejectsNonPNG(t *testing.T) {
	fake := &fakeRunner{out: []byte("error: closed\n")}
	c := &Client{Runner: fake}

	_, err := c.Screenshot(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not a PNG") {
		t.Fatalf("err = %v, want a complaint about the missing PNG signature", err)
	}
}

func TestDeviceLookup(t *testing.T) {
	c := &Client{Runner: &fakeRunner{out: []byte(adbDevicesListing)}}

	d, err := c.Device(context.Background(), "R58M12345")
	if err != nil {
		t.Fatalf("Device: %v", err)
	}
	if d.Model != "SM_G973F" {
		t.Errorf("model = %q, want SM_G973F", d.Model)
	}

	if _, err := c.Device(context.Background(), "nope"); !errors.Is(err, ErrNoDevice) {
		t.Errorf("unknown serial: err = %v, want ErrNoDevice", err)
	}
	if _, err := c.Device(context.Background(), "emulator-5554"); !errors.Is(err, ErrDeviceNotReady) {
		t.Errorf("offline serial: err = %v, want ErrDeviceNotReady", err)
	}
}

func TestDeviceFallsBackToPinnedSerial(t *testing.T) {
	c := &Client{Runner: &fakeRunner{out: []byte(adbDevicesListing)}, Serial: "R58M12345"}

	if _, err := c.Device(context.Background(), ""); err != nil {
		t.Fatalf("Device with the client's own serial: %v", err)
	}
}

func TestLogcatLines(t *testing.T) {
	fake := &fakeRunner{out: []byte("01-02 03:04:05.000  1234  1234 I ActivityManager: start\r\n\r\n01-02 03:04:06.000  1234  1234 E AndroidRuntime: boom\r\n")}
	c := &Client{Runner: fake, Serial: "R58M12345"}

	lines, err := c.Logcat(context.Background(), 20)
	if err != nil {
		t.Fatalf("Logcat: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %q", len(lines), lines)
	}
	if strings.HasSuffix(lines[0], "\r") || !strings.Contains(lines[1], "boom") {
		t.Errorf("lines = %q", lines)
	}
	want := "-s R58M12345 logcat -d -t 20"
	if args := strings.Join(fake.lastCall(t), " "); args != want {
		t.Errorf("args = %q, want %q", args, want)
	}

	if _, err := c.Logcat(context.Background(), 0); err == nil {
		t.Error("0 lines must be refused")
	}
	if _, err := c.Logcat(context.Background(), MaxLogcatLines+1); err == nil {
		t.Error("an unbounded line count must be refused")
	}
}

// TestMissingAdbIsReported goes through the real execRunner, so it proves the
// "adb is not installed" path names the binary instead of failing silently.
func TestMissingAdbIsReported(t *testing.T) {
	c := &Client{Adb: "definitely-not-adb-dsagent", Timeout: 5 * time.Second}

	_, err := c.Devices(context.Background())
	if err == nil || !strings.Contains(err.Error(), "definitely-not-adb-dsagent") {
		t.Fatalf("err = %v, want a message naming the missing binary", err)
	}
	var cmdErr *CommandError
	if !errors.As(err, &cmdErr) {
		t.Fatalf("err = %T, want *CommandError", err)
	}
}
