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
	"time"

	"dsagent/android"
)

const usageText = `androidprobe - drive the android prototype by hand

usage:
  androidprobe devices
  androidprobe shot    [-serial S] [-o screen.png]
  androidprobe tap     [-serial S] X Y
  androidprobe swipe   [-serial S] X1 Y1 X2 Y2 [DURATION_MS]
  androidprobe logcat  [-serial S] [-n 50]

-serial pins adb to one device, the way adb -s does; adb refuses every other
command when more than one device is attached. adb must be on PATH.

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
	case "shot":
		err = runShot(args)
	case "tap":
		err = runTap(args)
	case "swipe":
		err = runSwipe(args)
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

// runShot captures the screen to a PNG file. The bytes never go through a
// terminal, so this is also the quickest way to check that the capture path is
// binary-safe on this machine.
func runShot(args []string) error {
	fs := newFlagSet("shot")
	serial := fs.String("serial", "", "device serial to pin adb to")
	out := fs.String("o", "screen.png", "file to write the PNG capture to")
	fs.Parse(args)

	png, err := newClient(*serial).Screenshot(context.Background())
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, png, 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s (%d bytes)\n", *out, len(png))
	return nil
}

func runTap(args []string) error {
	fs := newFlagSet("tap")
	serial := fs.String("serial", "", "device serial to pin adb to")
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

	if err := newClient(*serial).Tap(context.Background(), x, y); err != nil {
		return err
	}
	fmt.Printf("tapped %d,%d\n", x, y)
	return nil
}

func runSwipe(args []string) error {
	fs := newFlagSet("swipe")
	serial := fs.String("serial", "", "device serial to pin adb to")
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

	from := android.Point{X: coords[0], Y: coords[1]}
	to := android.Point{X: coords[2], Y: coords[3]}
	if err := newClient(*serial).Swipe(context.Background(), from, to, duration); err != nil {
		return err
	}
	fmt.Printf("swiped %d,%d -> %d,%d\n", from.X, from.Y, to.X, to.Y)
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
