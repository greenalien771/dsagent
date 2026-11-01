# test-android-branch

This branch is a **test** branch and it is **in development**.

Nothing on it is finished, stable or safe to build on. It exists only for
experimentation; the real code lives on `main`.

## What is here now

The first piece of a prototype for driving an Android device over `adb` - the
same two verbs the desktop tools already use on `main` (capture a screen, poke
it), pointed at a phone instead of the local machine:

| path | what it is |
|---|---|
| `android/` | the prototype package: list devices, capture the screen, tap, swipe, read a slice of logcat |
| `android/README.md` | design notes, the deliberate choices, and what is missing before this becomes a tool |
| `cmd/androidprobe/` | a throwaway CLI over that package, so it can be driven by hand before a model ever sees it |
| `go.mod` | the prototype's own module, so this branch does not have to agree with `main` |

It is not wired into the agent yet: no tool, no approval card, no schema, no
config key. That is the point of this commit - the parsing, the failure
messages and the test seam are settled, and what to expose is still open.

## Build and test

```
go vet ./...
go test ./...
go build -o "%TEMP%\androidprobe.exe" ./cmd/androidprobe
```

`adb` is not needed for any of that: the package takes its `adb` behind a
`Runner` interface, so the tests answer without a device. To drive it against a
real phone, `adb` has to be on `PATH`:

```
%TEMP%\androidprobe.exe devices
%TEMP%\androidprobe.exe shot -serial <serial> -o screen.png
%TEMP%\androidprobe.exe logcat -serial <serial> -n 80
```

## Where this is going

`android/README.md` ends with the list, in order: turn the package into
approval-gated tools, feed captures into the existing vision path, resolve
coordinates and device pixel size properly, be honest about a missing `adb`,
and only then let it write anything to the device.

`main` is unaffected by anything on this branch. Copy the package over when it is
worth copying; nothing here imports it.
