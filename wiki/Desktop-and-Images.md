# Desktop and images

Two things share this page because they share one pipeline: a picture reaches the
model either from a capture or from a file, and it travels the same way.

## Attaching an image

- **`+` in the composer > Add Image...** - PNG, JPEG, GIF or WebP, up to **8 per
  task** and **20 MiB of decoded pixels** in total (10 MiB per image). **Add
  File...** in the same menu adds a text file as context instead. Both apply to the
  next task only.
- The model must actually see images. That is a per-provider rule over model names,
  not a constant - see [Providers](Providers#which-models-see-images). A text-only
  model is refused **up front**, with a message naming the model and the setting to
  change, rather than failing mid-run with a 400.
- Inline images are kept in the conversation so a follow-up question still works,
  but the total is capped: the oldest payloads become a line naming the image
  rather than several megabytes resent on every step.
- `read_image` is the file counterpart of `screenshot`: it attaches an existing
  PNG/JPEG/GIF/WebP from the workspace, and the model sees it on the next step. It
  is offered in every mode, because reading is free.

## The three desktop tools

Win32 through PowerShell, so they are offered **on Windows only**. Elsewhere every
one of them would refuse before reaching a window, which is exactly the wasted step
the tool schemas exist to avoid.

### `list_windows`

Enumerates the visible windows on the desktop: title, process, state
(minimized/maximized/normal), focus and rect, each with an hwnd. This is how the
agent finds a window to capture or control without you opening or focusing
anything. Optional `title` filter.

It asks every time, because the titles it returns - document names, URLs, message
previews - are sent to the remote model. The risk layer classifies the listing as
`elevated` for that reason, and `elevated` is enough to raise the card.

### `screenshot`

Captures the desktop, one region, or one window.

| argument | meaning |
|---|---|
| `window` | title substring or hwnd from `list_windows`: capture just that window, even when minimised or behind others, without the user opening or focusing it |
| `x`, `y` | left/top edge in pixels; **offsets inside the window** when `window` is set |
| `width`, `height` | crop size; defaults to the whole desktop |
| `scale` | 0.1-1.0, a zoom factor - the cheap way to shrink a big region |

A full screen at `scale: 0.5` is how you find something; a tight region at
`scale: 1` is how you read it. Capture is approval-gated: it uploads what is on
your screen to the API, so it prompts in Agent mode unless auto-approve is on, and
always in Plan and Ask.

### `window_control`

`focus`, `minimize`, `maximize`, `restore` or `close`, by title substring or hwnd.

`close` posts a close request (`WM_CLOSE`), not a kill - a window with unsaved work
can refuse and will still be open. Check with `list_windows` and say so rather than
claiming it closed. It is also the one call Always Allow can never answer.

Two windows are off limits by design:

- **The console hosting dsagent** - the script refuses it by pid, because closing
  it is closing the run.
- **The browser tab showing the UI** - it belongs to another process, which the
  tool cannot recognise, so that one is on you.

## `wait`

`wait` is what makes "open, let it load, capture, close" reliable instead of a
guess:

- `seconds` - fractions allowed, `0.5` is half a second
- `window` plus `until: appear | gone`, with `timeout_ms` (default 10000)

Prefer waiting on the window over sleeping a fixed time. The pause reports to the
watchdog as activity, so a deliberate wait is never mistaken for a stalled run.
`wait` needs nothing from the platform, which is why it survives on Linux and
macOS when the three desktop tools do not.

## Why only Windows

The rule is the OS, not the code.

- **macOS** grants the same powers through Screen Recording and Accessibility,
  which the user switches on once in System Settings. The backend would be
  `screencapture` plus CoreGraphics. Not written here, not blocked.
- **X11** grants them too, through `xwd` and `xdotool`. Not written here, not
  blocked.
- **Wayland** is the case that is actually blocked: no protocol lets one client
  enumerate or drive another client's toplevels, so `list_windows` and
  `window_control` have no counterpart at any budget. `screenshot` would degrade to
  `org.freedesktop.portal.Screenshot` - a consent dialog, and a source the user
  picks rather than a window the agent names. Shelling out does not rescue it: a
  portal call returns a `Request` object path and the real answer arrives later as
  a D-Bus signal, which `gdbus call` exits before ever seeing. A Wayland backend
  means a D-Bus client and a PipeWire consumer - the first Go dependency this tree
  would carry, and cgo for the second.

## Captures on disk

A capture is written to a temporary file, attached, and swept afterwards. The
temporary-file sweep is in `screenshot.go`; if you see captures accumulating in the
temp directory, that is the file to look at.
