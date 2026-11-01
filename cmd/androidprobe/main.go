// Command androidprobe is a small CLI over the android prototype package. It
// exists so the package can be driven by hand, on a real phone, before it grows
// into agent tools with approval cards: the same calls, no model in the loop.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"dsagent/android"
)

const usageText = `androidprobe - drive the android prototype by hand

usage:
  androidprobe devices
  androidprobe state   [-serial S]
  androidprobe shot    [-serial S] [-o screen.png] [-scale 0.5 | -width 400]
  androidprobe tap     [-serial S] [-frame 400x889] X Y
  androidprobe swipe   [-serial S] [-frame 400x889] X1 Y1 X2 Y2 [DURATION_MS]
  androidprobe wake    [-serial S]
  androidprobe unlock  [-serial S]
  androidprobe logcat  [-serial S] [-n 50]

-serial pins adb to one device, the way adb -s does; adb refuses every other
command when more than one device is attached. adb must be on PATH.

-frame says the coordinates were read off an image of that size, and maps them
back to device pixels. shot prints the size of the image it wrote, so the pair is
"shot -width 400 -o s.png" then "tap -frame 400x889 200 340".

Tap and swipe refuse while the screen is off or the keyguard is up: input in both
states looks successful and does nothing. unlock is the wake-and-swipe-up
gesture, and it fails with a clear message when a pin, pattern or password is set.

exit status: 0 on success, 1 when adb refused or failed, 2 on a usage mistake.
`

func main() {
	if len(os.Args) < 2 {
		usageError("missing command")
	}

	command, args := os.Args[1], os.Args[2:]
	var err error
	switch command {
	case "devices":
		err = runDevices(args)
	case "state":
		err = runState(args)
	case "shot":
		err = runShot(args)
	case "tap":
		err = runTap(args)
	case "swipe":
		err = runSwipe(args)
	case "wake":
		err = runWake(args)
	case "unlock":
		err = runUnlock(args)
	case "logcat":
		err = runLogcat(args)
	case "help", "-h", "-help", "--help":
		fmt.Print(usageText)
		return
	default:
		usageError(fmt.Sprintf("unknown command %q", command))
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "androidprobe: %v\n", err)
		os.Exit(1)
	}
}

// runDevices prints everything attached, including the devices that are not
// usable yet - "offline" and "unauthorized" are the states a user is usually
// looking at when nothing works.
func runDevices(args []string) error {
	newFlagSet("devices").Parse(args)

	devices, err := newClient("").Devices(context.Background())
	if err != nil {
		return err
	}
	for _, d := range devices {
		state := d.State
		if !d.Ready() {
			state += " (not ready)"
		}
		fmt.Printf("%-16s %-22s %s\n", d.Serial, state, describe(d))
	}
	return nil
}

// runState prints what the input calls are about to check: whether the panel is
// on, whether the keyguard is up, and the size the coordinates live in.
func runState(args []string) error {
	fs := newFlagSet("state")
	serial := fs.String("serial", "", "device serial to pin adb to")
	fs.Parse(args)

	c := newClient(*serial)
	ctx := context.Background()

	screen, err := c.Screen(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "androidprobe: %v\n", err)
	}
	fmt.Printf("screen:  %s\n", screen.String())

	display, err := c.Display(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("display: %dx%d at %d dpi\n", display.Width, display.Height, display.Density)
	return nil
}

// runShot captures the screen to a PNG file. It prints the size of the image it
// wrote and what the device was doing, because those two facts are what the next
// call needs: without them a tap coordinate is a guess.
func runShot(args []string) error {
	fs := newFlagSet("shot")
	serial := fs.String("serial", "", "device serial to pin adb to")
	out := fs.String("o", "screen.png", "file to write the PNG capture to")
	scale := fs.Float64("scale", 0, "image pixels per device pixel in 0.05..1, 0 for native size")
	width := fs.Int("width", 0, "shrink the image to this many pixels wide (overrides -scale)")
	fs.Parse(args)

	c := newClient(*serial)
	ctx := context.Background()

	captureScale := *scale
	if *width > 0 {
		display, err := c.Display(ctx)
		if err != nil {
			return err
		}
		captureScale = float64(*width) / float64(display.Width)
		if captureScale > 1 {
			captureScale = 1
		}
	}

	cp, err := c.Capture(ctx, captureScale)
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, cp.PNG, 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s (%d bytes)\n", *out, len(cp.PNG))
	fmt.Println(cp.Describe())
	return nil
}

func runTap(args []string) error {
	fs := newFlagSet("tap")
	serial := fs.String("serial", "", "device serial to pin adb to")
	frame := fs.String("frame", "", "WxH of the image the coordinates were read from")
	fs.Parse(args)
	if fs.NArg() != 2 {
		usageError("tap takes an X and a Y")
	}
	x, err := atoi("X", fs.Arg(0))
	if err != nil {
		return err
	}
	y, err := atoi("Y", fs.Arg(1))
	if err != nil {
		return err
	}

	c := newClient(*serial)
	ctx := context.Background()
	at := android.Point{X: x, Y: y}

	if *frame == "" {
		if err := c.Tap(ctx, x, y); err != nil {
			return err
		}
		fmt.Printf("tapped %d,%d (device pixels)\n", x, y)
		return nil
	}

	cp, err := captureFor(c, *frame)
	if err != nil {
		return err
	}
	if err := c.TapCapture(ctx, cp, at); err != nil {
		return err
	}
	mapped, _ := cp.Map(at)
	fmt.Printf("tapped %d,%d in the %dx%d image = %d,%d on the device\n", x, y, cp.Width, cp.Height, mapped.X, mapped.Y)
	return nil
}

func runSwipe(args []string) error {
	fs := newFlagSet("swipe")
	serial := fs.String("serial", "", "device serial to pin adb to")
	frame := fs.String("frame", "", "WxH of the image the coordinates were read from")
	fs.Parse(args)
	if fs.NArg() < 4 || fs.NArg() > 5 {
		usageError("swipe takes X1 Y1 X2 Y2 and an optional duration in milliseconds")
	}

	var coords [4]int
	for i, name := range []string{"X1", "Y1", "X2", "Y2"} {
		v, err := atoi(name, fs.Arg(i))
		if err != nil {
			return err
		}
		coords[i] = v
	}
	var duration time.Duration
	if fs.NArg() == 5 {
		ms, err := atoi("DURATION_MS", fs.Arg(4))
		if err != nil {
			return err
		}
		duration = time.Duration(ms) * time.Millisecond
	}

	c := newClient(*serial)
	ctx := context.Background()
	from := android.Point{X: coords[0], Y: coords[1]}
	to := android.Point{X: coords[2], Y: coords[3]}

	if *frame == "" {
		if err := c.Swipe(ctx, from, to, duration); err != nil {
			return err
		}
		fmt.Printf("swiped %d,%d -> %d,%d (device pixels)\n", from.X, from.Y, to.X, to.Y)
		return nil
	}

	cp, err := captureFor(c, *frame)
	if err != nil {
		return err
	}
	if err := c.SwipeCapture(ctx, cp, from, to, duration); err != nil {
		return err
	}
	a, _ := cp.Map(from)
	b, _ := cp.Map(to)
	fmt.Printf("swiped %d,%d -> %d,%d in the %dx%d image = %d,%d -> %d,%d on the device\n",
		from.X, from.Y, to.X, to.Y, cp.Width, cp.Height, a.X, a.Y, b.X, b.Y)
	return nil
}

func runWake(args []string) error {
	fs := newFlagSet("wake")
	serial := fs.String("serial", "", "device serial to pin adb to")
	fs.Parse(args)

	screen, err := newClient(*serial).Wake(context.Background())
	if err != nil {
		return err
	}
	fmt.Printf("screen: %s\n", screen.String())
	return nil
}

func runUnlock(args []string) error {
	fs := newFlagSet("unlock")
	serial := fs.String("serial", "", "device serial to pin adb to")
	fs.Parse(args)

	screen, err := newClient(*serial).Unlock(context.Background())
	if err != nil {
		return err
	}
	fmt.Printf("screen: %s\n", screen.String())
	return nil
}

func runLogcat(args []string) error {
	fs := newFlagSet("logcat")
	serial := fs.String("serial", "", "device serial to pin adb to")
	n := fs.Int("n", 50, "how many of the last lines to print")
	fs.Parse(args)

	lines, err := newClient(*serial).Logcat(context.Background(), *n)
	if err != nil {
		return err
	}
	for _, line := range lines {
		fmt.Println(line)
	}
	return nil
}

func newFlagSet(name string) *flag.FlagSet {
	return flag.NewFlagSet(name, flag.ExitOnError)
}

// newClient builds a client that gives up on a wedged device instead of
// hanging: a prototype that waits forever teaches nothing.
func newClient(serial string) *android.Client {
	return &android.Client{Serial: serial, Timeout: android.DefaultTimeout}
}

// captureFor builds the coordinate space a -frame argument describes: an image
// of that size, taken from this device's display.
func captureFor(c *android.Client, frame string) (android.Capture, error) {
	width, height, err := parseFrame(frame)
	if err != nil {
		return android.Capture{}, err
	}
	display, err := c.Display(context.Background())
	if err != nil {
		return android.Capture{}, err
	}
	return android.Capture{Width: width, Height: height, Display: display}, nil
}

func parseFrame(frame string) (int, int, error) {
	parts := strings.SplitN(strings.ToLower(strings.TrimSpace(frame)), "x", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("-frame %q is not WxH", frame)
	}
	width, err := strconv.Atoi(parts[0])
	if err != nil || width <= 0 {
		return 0, 0, fmt.Errorf("-frame %q has no usable width", frame)
	}
	height, err := strconv.Atoi(parts[1])
	if err != nil || height <= 0 {
		return 0, 0, fmt.Errorf("-frame %q has no usable height", frame)
	}
	return width, height, nil
}

func describe(d android.Device) string {
	switch {
	case d.Model != "" && d.Product != "":
		return d.Model + " (" + d.Product + ")"
	case d.Model != "":
		return d.Model
	case d.Product != "":
		return d.Product
	default:
		return "-"
	}
}

func atoi(name, raw string) (int, error) {
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a whole number", name, raw)
	}
	return v, nil
}

func usageError(msg string) {
	fmt.Fprintf(os.Stderr, "androidprobe: %s\n\n%s", msg, usageText)
	os.Exit(2)
}
