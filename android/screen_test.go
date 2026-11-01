package android

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The dump fragments below are the lines themselves, copied from the TECNO KG5m
// this package was first run against: an Android 11 phone that had been asleep
// behind the keyguard, and the same phone awake and unlocked. They are verbatim
// so that the parser is tested against output a device actually produced, not
// against output that would be convenient.
const (
	powerAsleep = `Power Manager State:
  mDirty=true mWakefulness=Asleep
  mIsPowered=true mPlugType=2
Display Power: state=OFF
`
	powerAwake = `Power Manager State:
  mDirty=true mWakefulness=Awake
  mIsPowered=true mPlugType=2
Display Power: state=ON
`
	windowLocked = `  mCurrentFocus=Window{5472f25 u0 NotificationShade}
    mShowingDream=false mDreamingLockscreen=true    mStatusBar=Window{40fec11 u0 StatusBar}    mExpandedPanel=Window{5472f25 u0 NotificationShade} isKeyguardShowing=true
`
	windowUnlocked = `  mCurrentFocus=Window{5277be9 u0 com.transsion.hilauncher/com.android.quickstep.src.com.android.launcher3.uioverrides.QuickstepLauncher}
    mShowingDream=false mDreamingLockscreen=false    mStatusBar=Window{40fec11 u0 StatusBar}    mExpandedPanel=Window{5472f25 u0 NotificationShade} isKeyguardShowing=false
`
	// Android 9 and earlier print the display state and nothing named
	// mWakefulness; the fallback covers that.
	powerOldAsleep = `Power Manager State:
  mScreenOnEarly=false mScreenOnFully=false
Display Power: state=OFF
`
)

func TestParseWakefulness(t *testing.T) {
	tests := []struct {
		name string
		dump string
		want string
	}{
		{"asleep", powerAsleep, "Asleep"},
		{"awake", powerAwake, "Awake"},
		{"old build falls back to display power", powerOldAsleep, "Asleep"},
		{"nothing usable", "Power Manager State:\n", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseWakefulness([]byte(tc.dump)); got != tc.want {
				t.Errorf("parseWakefulness = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseWakefulnessDozing(t *testing.T) {
	dump := "  mWakefulness=Dozing\n"
	got := parseWakefulness([]byte(dump))
	if got != "Dozing" {
		t.Fatalf("parseWakefulness = %q, want Dozing", got)
	}
	s := Screen{Wakefulness: got, KeyguardKnown: true}
	if s.Awake() {
		t.Error("a dozing device must not count as awake: a tap would not reach an app")
	}
}

func TestParseKeyguard(t *testing.T) {
	tests := []struct {
		name        string
		dump        string
		wantShowing bool
		wantKnown   bool
	}{
		{"locked", windowLocked, true, true},
		{"unlocked", windowUnlocked, false, true},
		{"no keyguard keys at all", "  mCurrentFocus=null\n", false, false},
		{
			// isKeyguardShowing wins over mDreamingLockscreen even when they
			// disagree: the dream layer is up but the phone is not locked.
			name:        "disagreeing keys prefer isKeyguardShowing",
			dump:        "  mDreamingLockscreen=true isKeyguardShowing=false\n",
			wantShowing: false,
			wantKnown:   true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			showing, known := parseKeyguard([]byte(tc.dump))
			if showing != tc.wantShowing || known != tc.wantKnown {
				t.Errorf("parseKeyguard = (%v, %v), want (%v, %v)", showing, known, tc.wantShowing, tc.wantKnown)
			}
		})
	}
}

func TestScreenCombinesBothDumps(t *testing.T) {
	runner := &scriptedRunner{reply: deviceDumps{
		power:  powerAsleep,
		window: windowLocked,
	}.run}
	c := &Client{Runner: runner, Serial: "R58M12345"}

	screen, err := c.Screen(context.Background())
	if err != nil {
		t.Fatalf("Screen: %v", err)
	}
	if screen.Wakefulness != "Asleep" || !screen.Keyguard || !screen.KeyguardKnown {
		t.Fatalf("screen = %+v", screen)
	}
	if screen.Awake() {
		t.Error("Asleep must not be awake")
	}
	if got, want := screen.String(), "asleep, locked"; got != want {
		t.Errorf("String = %q, want %q", got, want)
	}
	if want := "-s R58M12345 shell dumpsys power"; strings.Join(runner.calls[0], " ") != want {
		t.Errorf("first call = %q, want %q", strings.Join(runner.calls[0], " "), want)
	}
}

func TestScreenKeepsTheHalfItCouldRead(t *testing.T) {
	// `dumpsys window` fails but `dumpsys power` answers: the state still says
	// the device is awake, and the error is reported alongside it.
	runner := &scriptedRunner{reply: func(args []string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "dumpsys power"):
			return []byte(powerAwake), nil
		case strings.Contains(joined, "dumpsys window"):
			return nil, errors.New("error: closed")
		}
		return nil, nil
	}}
	c := &Client{Runner: runner}

	screen, err := c.Screen(context.Background())
	if err == nil {
		t.Fatal("a failed window dump must be reported")
	}
	if screen.Wakefulness != "Awake" {
		t.Errorf("wakefulness = %q, want Awake despite the window dump failing", screen.Wakefulness)
	}
	if screen.KeyguardKnown {
		t.Error("keyguard must stay unknown when its dump failed")
	}
}

func TestParseSize(t *testing.T) {
	tests := []struct {
		name    string
		dump    string
		want    Display
		wantErr bool
	}{
		{"physical only", "Physical size: 720x1600\n", Display{Width: 720, Height: 1600}, false},
		{
			name: "override wins",
			dump: "Physical size: 720x1600\nOverride size: 1080x1920\n",
			want: Display{Width: 1080, Height: 1920},
		},
		{"carriage returns", "Physical size: 720x1600\r\n", Display{Width: 720, Height: 1600}, false},
		{"nothing usable", "", Display{}, true},
		{"nonsense", "Physical size: big\n", Display{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseSize([]byte(tc.dump))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("parseSize = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParseDensity(t *testing.T) {
	if got := parseDensity([]byte("Physical density: 320\n")); got != 320 {
		t.Errorf("density = %d, want 320", got)
	}
	if got := parseDensity([]byte("Physical density: 320\nOverride density: 480\n")); got != 480 {
		t.Errorf("density = %d, want the 480 override", got)
	}
	if got := parseDensity([]byte("garbage\n")); got != 0 {
		t.Errorf("density = %d, want 0 for an unreadable dump", got)
	}
}

func TestDisplayReadsSizeThenDensity(t *testing.T) {
	runner := &scriptedRunner{reply: deviceDumps{
		size:    "Physical size: 720x1600\n",
		density: "Physical density: 320\n",
	}.run}
	c := &Client{Runner: runner}

	d, err := c.Display(context.Background())
	if err != nil {
		t.Fatalf("Display: %v", err)
	}
	if d != (Display{Width: 720, Height: 1600, Density: 320}) {
		t.Fatalf("Display = %+v", d)
	}
	want := "shell wm size"
	if got := strings.Join(runner.calls[0], " "); got != want {
		t.Errorf("first call = %q, want %q", got, want)
	}
}

func TestDisplaySurvivesAFailedDensityRead(t *testing.T) {
	runner := &scriptedRunner{reply: func(args []string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "wm density") {
			return nil, errors.New("error: closed")
		}
		return []byte("Physical size: 720x1600\n"), nil
	}}
	c := &Client{Runner: runner}

	d, err := c.Display(context.Background())
	if err != nil {
		t.Fatalf("Display: %v", err)
	}
	if d.Width != 720 || d.Height != 1600 || d.Density != 0 {
		t.Errorf("Display = %+v, want the size kept and no density", d)
	}
}
