# android - prototype, not a feature

A first cut at driving an Android device over `adb`, in the shape the desktop
tools already have on `main`: the machine dsagent runs on is captured and poked
through `screenshot` and `window_control`, so a phone would be captured and
poked through the same two verbs plus a log.

Nothing here is wired into the agent loop yet. There is no tool, no approval
card, no schema, no config key. It is a package and a throwaway CLI, so that the
next step is a decision about tools rather than a decision about parsing.

## What works today

| call | adb behind it |
|---|---|
| `Client.Devices` | `adb devices -l` |
| `Client.Device` | the same listing, filtered to one serial and refused unless ready |
| `Client.Screenshot` | `adb -s S exec-out screencap -p` |
| `Client.Tap` | `adb -s S shell input tap X Y` |
| `Client.Swipe` | `adb -s S shell input swipe X1 Y1 X2 Y2 [MS]` |
| `Client.Logcat` | `adb -s S logcat -d -t N` |

Deliberate choices, each of which cost someone an afternoon at some point:

- **`exec-out`, not `shell`.** `adb shell screencap -p` translates `\n` to
  `\r\n` and hands back a PNG that no decoder accepts. `Screenshot` also checks
  the 8-byte PNG signature, so a device that answers with `error: closed` fails
  as a capture error instead of as a corrupt file later.
- **adb's own words survive.** adb reports almost everything as exit status 1
  with the reason on stderr. `CommandError` keeps that text, because
  "device offline" and "no devices/emulators found" want different responses.
- **Devices are not resolved for you.** An empty `Serial` means adb decides, and
  adb refuses when several devices are attached. Guessing the first device is
  how a test meant for the emulator taps on someone's phone.
- **State is checked before the call.** `Device` refuses `offline`,
  `unauthorized` and `no permissions` up front (`ErrDeviceNotReady`) instead of
  letting the next command fail with a vaguer message.
- **Nothing can hang.** Every call is bounded by `Client.Timeout`
  (`DefaultTimeout`, 20s), and `Logcat` refuses an unbounded line count. A whole
  logcat buffer is megabytes - unreadable in a transcript and expensive to send
  to a model.
- **Every non-test path goes through `Runner`.** That interface is the whole
  test seam: the tests answer without a phone, an adb binary or a Windows
  machine, which is why they can run in CI.

## Using it

`adb` must be on `PATH` and a device has to be attached:

```
go build -o %TEMP%\androidprobe.exe .\cmd\androidprobe
%TEMP%\androidprobe.exe devices
%TEMP%\androidprobe.exe shot -serial R58M12345 -o screen.png
%TEMP%\androidprobe.exe tap -serial R58M12345 540 1200
%TEMP%\androidprobe.exe logcat -serial R58M12345 -n 80
```

The CLI is not the product. It is the thing that makes the package falsifiable
by hand before any of it is offered to a model.

## What is missing before this becomes a tool

In rough order of how much design each one needs:

1. **A tool, not a package.** On `main` this would be `android.go` next to
   `screenshot.go`: two or three tools - `android_devices` (read-only),
   `android_screenshot` (capture, uploads pixels), `android_input` (mutating) -
   registered through the same table as the rest, so Plan and Ask mode can drop
   the mutating one and the read-only one still lists.
2. **Approval posture.** Tapping and swiping run on someone's personal phone:
   `android_input` belongs in the dangerous class, not merely in the
   approval-card class. Screenshots upload the screen, so they follow
   `screenshot`: prompt unless auto-approve, always in Plan and Ask mode.
3. **Images into the vision path.** `Screenshot` returns PNG bytes, which is
   exactly what the vision code already takes. The device's pixel size is not
   part of the payload, so scaling hints (`x`/`y`/`width`/`height` on the
   desktop `screenshot` tool) have nowhere to come from yet -
   `adb shell wm size` would supply it.
4. **Coordinates.** Tap takes device pixels, but a model reading a downscaled
   capture will guess in the wrong space. A real tool needs the capture size
   attached to the image it sends, or it will tap plausible nonsense.
5. **`adb` discovery and failure.** Today a missing `adb` is an error message.
   A shipping version needs to know whether the platform-tools are installed,
   offer the same Windows-only honesty the desktop tools have, and not claim a
   tool exists where it cannot run.
6. **Multi-device UX.** A serial list, a chosen default, and a clear refusal
   when several are attached - the config key that holds it.
7. **Emulator as the test bed.** The unit tests need no device, but the
   e2e path needs a booted emulator and a skip when there is none, the way
   `window_e2e_test.go` skips on the wrong `GOOS`.
8. **Only then**: install/pull/logcat-to-file, app launch, and anything that
   writes to the device.
