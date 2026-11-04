# Tools

Fourteen built-in tools, plus whatever your MCP servers advertise. The model is
only ever offered the ones it may actually use in the current mode - a tool that
would only be refused is not in the schema at all.

## Availability

`A` = Agent, `P` = Plan, `Q` = Ask. W = Windows only.

| tool | A | P | Q | approval |
|---|:---:|:---:|:---:|---|
| `list_dir` | yes | yes | yes | none |
| `read_file` | yes | yes | yes | none |
| `read_image` | yes | yes | yes | none |
| `web_fetch` | yes | yes | yes | asks when the risk layer flags the URL |
| `list_windows` W | yes | yes | yes | always asks - the listing discloses your desktop |
| `wait` | yes | yes | yes | none |
| `ask_user` | yes | yes | yes | none - there is nothing to approve |
| `done` | yes | yes | yes | none |
| `screenshot` W | yes | yes | yes | always outside Agent mode; otherwise unless auto-approve |
| `run_shell` | yes | yes | - | always |
| `write_file` | yes | - | - | always |
| `edit_file` | yes | - | - | always |
| `window_control` W | yes | - | - | always; `close` is never covered by Always Allow |
| `mcp_control` | yes | - | - | asks when the action changes something |
| `mcp__<server>__<tool>` | yes | - | - | always, in Agent mode only |

On Linux and macOS the three Win32 tools (`list_windows`, `window_control`,
`screenshot`) are dropped from the schema, because every one of them would refuse
before reaching a window. That leaves eleven built-ins there.

## Reading

### `list_dir`

`path` (required) - relative to `workdir`, `.` for the root.

### `read_file`

- `path` (required)
- `offset` - byte offset, default 0

Returns up to 8KB per call. Page a large file with `offset` rather than dumping it.

### `read_image`

- `path` (required) - PNG, JPEG, GIF or WebP, inside `workdir`

The file is attached to the conversation the way a live capture is, and the model
sees it on the next step. `read_file` is for text; this is for a mockup, a diagram,
a saved capture or a photo. Offered in every mode, because reading is free.

### `web_fetch`

- `url` (required)

Returns plain text, truncated to 8KB. Refuses targets that resolve to this machine
or the local network, re-checks every redirect hop and every resolved IP, and uses
no proxy. That check is what makes it prompt: a public URL is silent, a URL aimed
at loopback or your LAN raises the card.

## Acting

### `run_shell`

- `command` (required)

One shell line, handed to `cmd.exe` on Windows and `/bin/sh` elsewhere, in the
working directory. Returns stdout+stderr, truncated. The child gets the
environment **minus every provider's API key variable**, so a bare `set` /
`env` / `printenv` cannot print a key into the transcript.

- Always asks, in every mode, unless auto-approve is on in Agent mode.
- Killed at `shell_timeout_sec` (60s by default). A command the limit kills says
  so, and says which setting to raise.
- Raises a Windows notification from the process itself, not from the page - see
  [Safety and approvals](Safety-and-Approvals#notifications).

### `write_file`

- `path`, `content` (both required)

Creates a file or replaces one whole. Paths resolve through symlinks and NTFS
junctions before use, so a link inside the workspace cannot walk a write outside
it.

### `edit_file`

- `path`, `old`, `new` (all required), `all` (optional)

Replaces one exact piece of text in an existing file and leaves every other byte
alone - no re-encoding, no reformatting, no BOM added, no trailing newline
invented or lost.

| rule | behaviour |
|---|---|
| unique match | `old` must appear exactly once. If it appears more, the call is refused, the refusal names the lines it found, and nothing is written |
| `all: true` | replaces every occurrence on purpose |
| literal | no patterns, no whitespace folding |
| empty `new` | deletes the text |
| line endings | the one mismatch it forgives: if `old` has LF and the file has CRLF, the search retries the file's own convention and says so; the replacement is converted the same way |
| a miss | explained - if nothing matches but something matches with whitespace ignored, the result says so |
| the result | names the line, how many places, and the byte size before and after, so the change can be described without reading the file back |

It is gated exactly like `write_file`, and classified by the same path rules, so an
edit into `.git/hooks` or a credentials file is as dangerous one line at a time as
it is wholesale.

### `window_control` (Windows)

- `action` (required) - `focus`, `minimize`, `maximize`, `restore`, `close`
- `window` (required) - title substring or an hwnd from `list_windows`

`close` posts a close request (`WM_CLOSE`), not a kill: a window with unsaved work
can refuse and will still be open. Check with `list_windows` and say so rather than
claiming it closed. The process hosting dsagent is refused by pid.

### `mcp_control`

- `action` - `status` (default), `stop`, `start`, `restart`
- `name` - a server name; empty or `all` means every server

`status` is free. `stop` is a real stop: the server is asked to leave through its
stdin, killed only if it ignores that, and it stays down - the automatic restart a
crashed server gets does not undo it - until `start`. `restart` is how a rebuilt
server binary is picked up, because a running executable cannot be replaced.

## Conversational

### `ask_user`

- `question` (required), `options` (optional array of suggested answers, drawn as
  buttons; the field is still there)

The run pauses - it does not end - and the answer arrives as the tool result, so
"your move" or "which file did you mean" costs one step instead of a fresh task.
It never asks for approval, because there is nothing to approve.

### `done`

- `summary` (required) - one or two sentences: what changed (which files) and how
  it was verified

Ends the task on purpose. Without it the only ending is the model answering with
no tool calls, which has the same shape as "I stopped here" - neither the UI nor
the user can tell a finished task from a run that gave up.

### `wait`

- `seconds` (number, fractions allowed - `0.5` is half a second), **or**
- `window` (title substring or hwnd) plus `until` (`appear` | `gone`), with
  `timeout_ms` (default 10000)

A deliberate wait reports to the watchdog as activity, so a pause is never
mistaken for a stalled run. `seconds` needs nothing from the platform, which is why
`wait` survives on Linux and macOS when the three desktop tools do not.

## MCP tools

A tool from a configured server arrives as `mcp__<server>__<tool>`, sanitised to
`[A-Za-z0-9_-]` and cut to the API's 64-character limit with a hash of the full
name, so two tools that differ only past the cut stay two tools. See
[MCP servers](MCP-Servers).

## Limits that apply to every tool

- **8KB per result**, MCP results included.
- **Valid JSON or a retry.** Bad arguments return
  `BAD ARGS: ... - retry with valid JSON matching the schema` instead of a crash.
- **The fourth identical call in one run is refused**, so a model cannot spin on a
  command that keeps failing.
- **`max_steps`** (25 by default) caps the number of tool calls per task.
- A call that hangs is bounded by the tool's own timeout: `shell_timeout_sec` for
  `run_shell`, `timeout_sec` or ten minutes for an MCP call, and the watchdog for
  everything else.
