package android

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// awakeDevice answers the two dumps the preflight reads, so a test about input
// is not also a test about parsing. A field the caller set is left alone: the
// locked cases need to keep their own window dump.
func awakeDevice(extra deviceDumps) deviceDumps {
	if extra.power == "" {
		extra.power = powerAwake
	}
	if extra.window == "" {
		extra.window = windowUnlocked
	}
	if extra.size == "" {
		extra.size = "Physical size: 720x1600\n"
	}
	if extra.density == "" {
		extra.density = "Physical density: 320\n"
	}
	return extra
}

// silentDumps fails every dump and wm call but lets input commands through: the
// case where the state cannot be read and the touch itself still has to be
// attempted.
func silentDumps(args []string) ([]byte, error) {
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "input ") {
		return nil, nil
	}
	return nil, errors.New("error: closed")
}

func calledWith(runner *scriptedRunner, contains string) bool {
	for _, call := range runner.calls {
		if strings.Contains(strings.Join(call, " "), contains) {
			return true
		}
	}
	return false
}

func TestTapRefusesOnASleepDevice(t *testing.T) {
	runner := &scriptedRunner{reply: deviceDumps{
		power:  powerAsleep,
		window: windowLocked,
	}.run}

	err := (&Client{Runner: runner}).Tap(context.Background(), 100, 200)
	if !errors.Is(err, ErrScreenOff) {
		t.Fatalf("err = %v, want ErrScreenOff", err)
	}
	if !strings.Contains(err.Error(), "Asleep") || !strings.Contains(err.Error(), "Wake") {
		t.Errorf("err = %q, want it to name the state and the remedy", err)
	}
	if calledWith(runner, "input tap") {
		t.Error("a tap on a sleeping device still reached adb: it would exit 0 and do nothing")
	}
}

func TestTapRefusesOnALockedDevice(t *testing.T) {
	runner := &scriptedRunner{reply: awakeDevice(deviceDumps{window: windowLocked}).run}

	err := (&Client{Runner: runner}).Tap(context.Background(), 100, 200)
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("err = %v, want ErrLocked", err)
	}
	if !strings.Contains(err.Error(), "Unlock") {
		t.Errorf("err = %q, want it to name the remedy", err)
	}
	if calledWith(runner, "input tap") {
		t.Error("a tap while locked still reached adb: on a phone with no credential it would unlock it")
	}
}

func TestSwipeRefusesOnALockedDevice(t *testing.T) {
	runner := &scriptedRunner{reply: awakeDevice(deviceDumps{window: windowLocked}).run}

	err := (&Client{Runner: runner}).Swipe(context.Background(), Point{1, 2}, Point{3, 4}, 0)
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("err = %v, want ErrLocked", err)
	}
	if calledWith(runner, "input swipe") {
		t.Error("a swipe while locked still reached adb")
	}
}

func TestAllowInputWhileLockedIsAnExplicitChoice(t *testing.T) {
	runner := &scriptedRunner{reply: awakeDevice(deviceDumps{window: windowLocked}).run}
	c := &Client{Runner: runner, AllowInputWhileLocked: true}

	if err := c.Tap(context.Background(), 100, 200); err != nil {
		t.Fatalf("Tap with AllowInputWhileLocked: %v", err)
	}
	if !calledWith(runner, "input tap 100 200") {
		t.Errorf("calls = %v, want the tap", runner.calls)
	}
}

func TestInputProceedsWhenTheDumpsSayNothing(t *testing.T) {
	// A device that answers no dump at all is not a device known to be locked.
	// Blocking here would refuse input on phones whose dumpsys output this
	// parser has never seen; the tap itself will report the real failure.
	runner := &scriptedRunner{reply: deviceDumps{}.run}

	if err := (&Client{Runner: runner}).Tap(context.Background(), 10, 20); err != nil {
		t.Fatalf("Tap: %v", err)
	}
	if !calledWith(runner, "input tap 10 20") {
		t.Errorf("calls = %v, want the tap", runner.calls)
	}
}

func TestInputProceedsWhenTheDumpsFail(t *testing.T) {
	runner := &scriptedRunner{reply: silentDumps}

	if err := (&Client{Runner: runner}).Tap(context.Background(), 10, 20); err != nil {
		t.Fatalf("Tap: %v", err)
	}
	if !calledWith(runner, "input tap 10 20") {
		t.Errorf("calls = %v, want the tap", runner.calls)
	}
}

func TestWakeSendsKeycode224(t *testing.T) {
	runner := &scriptedRunner{reply: awakeDevice(deviceDumps{}).run}
	c := &Client{Runner: runner, Serial: "R58M12345"}

	screen, err := c.Wake(context.Background())
	if err != nil {
		t.Fatalf("Wake: %v", err)
	}
	if !screen.Awake() {
		t.Errorf("screen after wake = %+v", screen)
	}
	if !calledWith(runner, "shell input keyevent 224") {
		t.Errorf("calls = %v, want KEYCODE_WAKEUP", runner.calls)
	}
}

// lockingDevice is a phone whose state changes when it is woken and swiped, so
// Unlock can be tested end to end rather than one call at a time.
func lockingDevice() (*scriptedRunner, *bool, *bool) {
	awake, unlocked := false, false
	runner := &scriptedRunner{reply: func(args []string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "dumpsys power"):
			if awake {
				return []byte(powerAwake), nil
			}
			return []byte(powerAsleep), nil
		case strings.Contains(joined, "dumpsys window"):
			if unlocked {
				return []byte(windowUnlocked), nil
			}
			return []byte(windowLocked), nil
		case strings.Contains(joined, "wm size"):
			return []byte("Physical size: 720x1600\n"), nil
		case strings.Contains(joined, "wm density"):
			return []byte("Physical density: 320\n"), nil
		case strings.Contains(joined, "input keyevent 224"):
			awake = true
			return nil, nil
		case strings.Contains(joined, "input swipe"):
			unlocked = true
			return nil, nil
		}
		return nil, nil
	}}
	return runner, &awake, &unlocked
}

func TestUnlockWakesSwipesUpAndReportsTheResult(t *testing.T) {
	runner, awake, unlocked := lockingDevice()
	c := &Client{Runner: runner}

	screen, err := c.Unlock(context.Background())
	if err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if !*awake || !*unlocked {
		t.Fatalf("awake=%v unlocked=%v, want both", *awake, *unlocked)
	}
	if !screen.Awake() || screen.Keyguard {
		t.Errorf("screen = %+v, want awake and unlocked", screen)
	}
	// The gesture is sized from the display: 720 wide, 1600 tall.
	if !calledWith(runner, "input swipe 360 1280 360 320 300") {
		t.Errorf("calls = %v, want a swipe from 4/5 height to 1/5 height", runner.calls)
	}
}

func TestUnlockIsANoOpWhenAlreadyUnlocked(t *testing.T) {
	runner := &scriptedRunner{reply: awakeDevice(deviceDumps{}).run}

	screen, err := (&Client{Runner: runner}).Unlock(context.Background())
	if err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if !screen.Awake() || screen.Keyguard {
		t.Errorf("screen = %+v", screen)
	}
	if calledWith(runner, "input swipe") || calledWith(runner, "keyevent") {
		t.Errorf("calls = %v, want no gestures on an already unlocked phone", runner.calls)
	}
}

func TestUnlockRefusesToClaimSuccessWhenACredentialIsSet(t *testing.T) {
	// Awake, locked, and the swipe changes nothing: the phone has a pin.
	runner := &scriptedRunner{reply: awakeDevice(deviceDumps{window: windowLocked}).run}

	screen, err := (&Client{Runner: runner}).Unlock(context.Background())
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("err = %v, want ErrLocked", err)
	}
	if !strings.Contains(err.Error(), "pin") {
		t.Errorf("err = %q, want it to say a credential has to be entered on the device", err)
	}
	if !screen.Keyguard {
		t.Errorf("screen = %+v, want the locked state reported alongside the error", screen)
	}
}

// TestNoCallPipesIntoTheDeviceShell is the regression net for the mistake that
// cost the first hardware run a step: `adb shell "dumpsys window | findstr ..."`
// runs findstr on the phone, where it does not exist. Every call this package
// makes must pass its arguments through untouched and filter on the host.
func TestNoCallPipesIntoTheDeviceShell(t *testing.T) {
	runner := &scriptedRunner{reply: awakeDevice(deviceDumps{
		png: homeScreenPNG(t, 720, 1600),
	}).run}
	c := &Client{Runner: runner, Serial: "R58M12345"}

	ctx := context.Background()
	c.Devices(ctx)
	c.Screen(ctx)
	c.Display(ctx)
	cp, err := c.Capture(ctx, 0.5)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if err := c.Tap(ctx, 10, 20); err != nil {
		t.Fatalf("Tap: %v", err)
	}
	if err := c.TapCapture(ctx, cp, Point{100, 200}); err != nil {
		t.Fatalf("TapCapture: %v", err)
	}
	if err := c.Swipe(ctx, Point{1, 2}, Point{3, 4}, time.Second); err != nil {
		t.Fatalf("Swipe: %v", err)
	}
	if _, err := c.Wake(ctx); err != nil {
		t.Fatalf("Wake: %v", err)
	}
	if _, err := c.Unlock(ctx); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if _, err := c.Logcat(ctx, 10); err != nil {
		t.Fatalf("Logcat: %v", err)
	}

	if len(runner.calls) == 0 {
		t.Fatal("no calls were recorded")
	}
	forbidden := []string{"|", "&", ";", ">", "<", "`", "$("}
	filters := []string{"grep", "findstr", "awk", "sed", "head", "tail"}
	for _, call := range runner.calls {
		joined := strings.Join(call, " ")
		for _, bad := range forbidden {
			if strings.Contains(joined, bad) {
				t.Errorf("call %q contains the shell metacharacter %q: filtering belongs on the host", joined, bad)
			}
		}
		for _, word := range strings.Fields(joined) {
			for _, filter := range filters {
				if word == filter {
					t.Errorf("call %q runs %q on the device: filtering belongs on the host", joined, filter)
				}
			}
		}
	}
}
