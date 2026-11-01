package android

import (
	"context"
	"strings"
	"testing"
)

// TestUnlockWillNotClaimSuccessWhenItCannotReadTheKeyguard covers the honest
// failure: the swipe went out, the answer never came back, and Unlock says so
// instead of reporting an unlocked phone it cannot see.
func TestUnlockWillNotClaimSuccessWhenItCannotReadTheKeyguard(t *testing.T) {
	runner := &scriptedRunner{reply: func(args []string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "dumpsys power"):
			return []byte(powerAwake), nil
		case strings.Contains(joined, "wm size"):
			return []byte("Physical size: 720x1600\n"), nil
		}
		// dumpsys window answers nothing at all: the keyguard stays unknown.
		return nil, nil
	}}

	screen, err := (&Client{Runner: runner}).Unlock(context.Background())
	if err == nil || !strings.Contains(err.Error(), "keyguard state could not be read") {
		t.Fatalf("err = %v, want a refusal to claim success", err)
	}
	if !calledWith(runner, "input swipe") {
		t.Errorf("calls = %v, want the gesture to have been attempted", runner.calls)
	}
	if screen.KeyguardKnown {
		t.Errorf("screen = %+v, want the keyguard still unknown", screen)
	}
}
