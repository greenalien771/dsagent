<!-- Logo and title share one block so the title sits right under the mark, and
     the style attribute grows it where the renderer keeps inline CSS. -->
<h1 align="center" style="font-size:2.6em;">
  <img src="ui/logo.svg" width="132" height="132" alt="dsagent"><br>
  dsagent
</h1>

<p align="center">
  <img src="https://img.shields.io/badge/Go-1.21%2B-blue" alt="Go 1.21+">
  <img src="https://img.shields.io/badge/License-MIT-green" alt="License: MIT">
  <img src="https://img.shields.io/badge/Platform-Windows%20%7C%20macOS%20%7C%20Linux-lightgrey" alt="Platform: Windows | macOS | Linux">
  <img src="https://img.shields.io/badge/tools-9-blue" alt="9 tools">
</p>

A coding agent in one Go binary. It serves its own UI on loopback, talks to an
OpenAI-compatible `/chat/completions` endpoint - DeepSeek by default, with Qwen
(DashScope) and z.ai (GLM) built in - and runs tools - list, read, write, shell,
fetch, screenshot, window control, plus the tools of any MCP server you
configure - inside a working directory you choose.
Mutating calls stop at an approval card before they run, and a call classified
dangerous needs the confirmation word typed into that card.

No telemetry, no hosted backend, no CDN. The UI, its vendored JavaScript and the
agent loop are all inside the executable.

## Requirements

- Go 1.21 or newer to build (developed and tested against Go 1.27 on Windows).
- An API key for one of the built-in providers. Put it in that provider's
  environment variable (`DEEPSEEK_API_KEY`, `DASHSCOPE_API_KEY` or
  `ZAI_API_KEY`), or paste it into Settings > Connection on first run.

## Build and run

```sh
go build -o dsagent.exe .     # Windows
go build -o dsagent .         # macOS / Linux
```

Start it:

```sh
./dsagent.exe
```

It binds `http://127.0.0.1:8787` and opens that URL in your browser. If the port
is taken it falls back to a free one and prints the URL it actually got (chat
history lives in browser `localStorage`, which is partitioned per port, so a
different port means a fresh transcript).

Startup prints a short banner: the mark, then the provider, the model, the
sandbox directory, whether a key is loaded, the approval posture, the limits and
where the config lives - the state that is otherwise invisible until a tool call
fails or an approval card does not appear. It is plain 7-bit ASCII, so it renders
the same in cmd.exe, PowerShell and any other terminal. The last line stays
`dsagent listening on <url>`, which is the line to scrape for the port actually
bound.

`-no-browser` skips launching the browser - useful for scripts and headless runs:

```sh
./dsagent.exe -no-browser
```

On Windows the running `dsagent.exe` is locked by the OS. To rebuild, stop it
first, or build somewhere else to check compilation:

```sh
go build -o "%TEMP%\check.exe" .
```

## Providers

Every endpoint speaks the same OpenAI Chat Completions shape, so a provider is a
URL plus the vendor details a user has to know. The table lives in
`providers.go` and is the single source of truth: the launch banner, the model
suggestions and the image-capability check all read it.

| `provider` | endpoint | key variable | default model |
|---|---|---|---|
| `deepseek` (default) | `https://api.deepseek.com/chat/completions` | `DEEPSEEK_API_KEY` | `deepseek-flash` |
| `qwen` | `https://dashscope-intl.aliyuncs.com/compatible-mode/v1/chat/completions` | `DASHSCOPE_API_KEY` | `qwen-plus` |
| `zai` | `https://api.z.ai/api/paas/v4/chat/completions` | `ZAI_API_KEY` | `glm-5.3` |
| `zai-coding` | `https://api.z.ai/api/coding/paas/v4/chat/completions` | `ZAI_API_KEY` | `glm-5.3` |

- The two z.ai entries run the same models on the same key; `zai-coding` is the
  path z.ai asks coding tools to use when the key belongs to a GLM Coding Plan
  subscription instead of pay-per-token billing.
- The Qwen default is the international DashScope region. For the
  mainland-China one, set `base_url` to
  `https://dashscope.aliyuncs.com/compatible-mode/v1/chat/completions`.
- `provider` and `base_url` are `config.json` keys. Settings > Connection edits
  the model and the key; the provider switch is not in the UI yet, so a hand
  edit of the file is how you change it today. An unknown value falls back to
  DeepSeek rather than failing to start, and a bad `base_url` is refused with a
  message instead of being stored.
- Settings are provider-scoped. Switching provider drops the endpoint override,
  loads that provider's own key and, unless you name another, its default model
  - because a vendor rejects another vendor's model name with a 400, and one
  vendor's key on another's endpoint is worse than that.

## Configuration

Stored in `config.json` under the OS config directory:

| OS | path |
|---|---|
| Windows | `%AppData%\dsagent\config.json` |
| macOS | `~/Library/Application Support/dsagent/config.json` |
| Linux | `~/.config/dsagent/config.json` |

| key | default | meaning |
|---|---|---|
| `provider` | `deepseek` | which built-in endpoint to talk to (see above) |
| `model` | the provider's own default | chat model id sent to the API |
| `base_url` | *(the provider's endpoint)* | full chat-completions URL that replaces the provider's own |
| `workdir` | directory dsagent was first started in | sandbox root for every tool |
| `max_steps` | `25` | tool-call budget per task (form allows up to 200) |
| `auto_approve` | `false` | skip approval cards for non-dangerous calls |
| `idle_timeout_sec` | `120` | no-output watchdog, 10-1800 seconds |
| `run_timeout_min` | `25` | wall-clock cap per task, 1-240 minutes |
| `shell_timeout_sec` | `60` | cap on one `run_shell` command, 5-3600 seconds |
| `mcp_servers` | *(none)* | MCP servers to start as extra tool providers - see [MCP servers](#mcp-servers) |

The API key is never written into `config.json` and never sent to the browser.
`DEEPSEEK_API_KEY`, `DASHSCOPE_API_KEY` or `ZAI_API_KEY` in the environment wins
over anything stored, and is never persisted. A key pasted into the UI is sealed
with the OS keystore (DPAPI on Windows) in `api_key_<family>.bin` next to the
config - one file per vendor family (`api_key_deepseek.bin`, `api_key_qwen.bin`,
`api_key_zai.bin`), so switching provider cannot hand the previous vendor's key
to a different endpoint. A file from before providers existed, `api_key.bin`, is
read as DeepSeek's key and removed when that key is.

On macOS and Linux there is no keystore without cgo, so the key is base64-encoded
in a `0600` file. That is obfuscation, not encryption - on those platforms prefer
the environment variable and keep nothing on disk.

## Modes

| mode | sees | can do |
|---|---|---|
| Agent | all tools | everything, each mutating call behind an approval card |
| Plan | every tool but the mutating ones | reads and investigates; `run_shell` always prompts, `write_file` and `window_control` are not offered |
| Ask | read-only tools | answers questions, changes nothing; `list_windows` still asks before it lists what you have open |

## Images and the screen

A task can carry pictures as well as text, for as long as the chosen model sees
them. Which names do is a per-provider rule in `providers.go`, not a constant.

- **Attach an image** with `+` in the composer, then **Add Image...** (PNG, JPEG,
  GIF or WebP, up to 8 per task and 20 MiB of pixels). **Add File...** in the same
  menu adds a text file as context instead. Both apply to the next task only.
- **The agent can look at your screen** with the `screenshot` tool. It captures the
  desktop, or one region via `x`/`y`/`width`/`height`, and `scale` shrinks a big
  region so it costs fewer tokens. Capture is a separate approval-gated step: it
  uploads what is on your screen to the API, so it prompts unless auto-approve is
  on, and always in Plan and Ask mode.
- **It can capture one window on its own**, without you opening or focusing
  anything: `list_windows` enumerates what is on the desktop (title, process,
  state, hwnd), and `screenshot` with `window` set to a title substring or an
  hwnd grabs just that window - including when it is minimized or behind another
  one. `x`/`y` then offset inside the window, `width`/`height` crop it. All of
  this is Win32 through PowerShell, so the three desktop tools are offered on
  Windows only - elsewhere they would be a step spent on a refusal.
- **It can drive windows too**: `window_control` focuses, minimizes, maximizes,
  restores or closes one. `close` posts a close request, not a kill - a window
  with unsaved work can refuse, and the result says the window is still open
  instead of claiming success. It never touches the console that hosts dsagent:
  the script refuses that by pid, because closing it is closing the run. The
  browser tab showing the UI belongs to another process, which the tool cannot
  recognise, so that one is on you.
- **`wait` makes a sequence reliable** rather than a guess: `seconds` (fractions
  allowed, `0.5` is half a second) for "let it load", or `window` with
  `until: appear|gone` to wait for the state you actually need. Launch, wait for
  the window, capture, minimize or close - the pause reports to the watchdog as
  activity, so a deliberate wait is never mistaken for a stalled run.
- **Why these three exist only on Windows.** The rule is the OS, not the code.
  macOS grants the same powers through Screen Recording and Accessibility, which
  the user switches on once in System Settings, and the backend there would be
  `screencapture` plus CoreGraphics; X11 grants them too, through `xwd` and
  `xdotool`. Neither is written here, but neither is blocked. Wayland is the
  case that is blocked: no protocol lets one client enumerate or drive another
  client's toplevels, so `list_windows` and `window_control` have no
  counterpart at any budget. `screenshot` would degrade to
  `org.freedesktop.portal.Screenshot` - a consent dialog, and a source the user
  picks rather than a window the agent names. Shelling out does not rescue it
  either: a portal call returns a `Request` object path and the real answer
  arrives later as a D-Bus signal, which `gdbus call` exits before ever seeing,
  so a Wayland backend means a D-Bus client and a PipeWire consumer - the first
  Go dependency this tree would carry, and cgo for the second.
- **A text-only model is refused up front**, with a message saying so, instead of
  failing mid-run: `deepseek-v4-pro` and the reasoner family, Qwen's non-VL
  models, and `glm-5.3` itself. `deepseek-flash`, `glm-5.3-flash`, and the `-v`,
  `vl` and `omni` names do see images.
- Inline images are kept in the conversation so a follow-up question still works,
  but the total is capped - the oldest payloads become a line naming the image
  rather than several megabytes resent on every step.

## MCP servers

dsagent is a [Model Context Protocol](https://modelcontextprotocol.io) *client*:
a server is a child process you list in the config, speaking JSON-RPC 2.0 over
its own stdin and stdout, one message per line - the stdio transport. Nothing
here reaches the network on its own. A server runs because it is in the list,
and it is stopped when the agent stops.

```json
"mcp_servers": [
  {"name": "fs", "command": "npx",
   "args": ["-y", "@modelcontextprotocol/server-filesystem", "C:\\work"]},
  {"name": "github", "command": "npx", "args": ["-y", "@modelcontextprotocol/server-github"],
   "env": ["GITHUB_TOKEN=..."], "dir": "C:\\work"},
  {"name": "slow", "command": "python", "args": ["server.py"], "disabled": true}
]
```

| field | meaning |
|---|---|
| `name` | how the server is named to the model and in the UI: letters, digits, `-` and `_`, up to 40 characters, unique among the entries |
| `command` | the executable, resolved through `PATH`. There is no shell, so `args` are literal and `&&` is not an operator |
| `args` | arguments, one per entry |
| `env` | extra `NAME=value` entries on top of your environment |
| `dir` | the server's working directory; empty inherits the agent's |
| `disabled` | keeps an entry configured without starting it |

Settings > Agent edits the same list as one JSON document, with a **Reload
servers** button beside it - that button, and `POST /api/mcp`, is how a
hand-edited file or a server that died is picked up without a restart. A list
that does not validate is refused on save rather than partly applied, and a bad
entry in a hand-edited file is dropped with a note on stderr instead of being
started on every task and failing there.

What the model sees, and what it costs:

- Tools arrive as `mcp__<server>__<tool>`, sanitised to `[A-Za-z0-9_-]` and cut
  to the API's 64 characters with a hash of the full name, so two tools that
  differ only past the cut stay two tools. Every description is prefixed
  `[MCP <server>]`, and the approval card names the same server.
- A call is gated like `run_shell`: it always asks, the card says it is
  third-party code outside the workspace, and **Always Allow** covers it in
  Agent mode only. Plan and Ask refuse it in words, because a tool that runs
  someone else's code cannot promise it changed nothing.
- Servers start in the background at launch, and again at the top of a task when
  the list changed, so the model is only ever offered a tool that can answer.
  One server failing to start is reported and skipped: it never costs the run
  the other servers, let alone the built-in tools.
- A server that dies mid-call is noticed when its stdout ends, and the call
  fails with the end of its stderr as the reason instead of hanging until the
  ten-minute tool timeout. The handshake and `tools/list` get 20 seconds each.
- The child gets your environment **minus** this agent's provider key variables
  - a server needs `PATH`, not your model key - plus whatever its `env` names.
- The answer is flattened to text: text and resource blocks come through, an
  image or audio block is named instead of silently dropped, and the usual 8 KB
  truncation applies.
- `Ctrl+C` stops the servers with the agent. A console window closed outright
  never delivers that signal, so they are left to notice that stdin is gone -
  which is why stdin is closed first on the way out.

Deliberately not implemented: sampling, roots and prompts. Those would let a
server drive this agent or ask the user questions through it, and none of that
is worth the attack surface for the tool calling this exists for. A server that
asks for one gets a method-not-found answer instead of silence, so it does not
hang. On Windows a server is started with `CREATE_NO_WINDOW`, so none of this
flashes a console window over what you are doing.

## Interface

- The left rail lists sessions (`Alt+N` starts another), the right panel is the
  activity list for the current one, and the composer carries the Agent / Plan /
  Ask switch.
- `Ctrl+K` is the command palette, `Ctrl+,` opens Settings (Connection, Agent,
  Interface, Safety, Data, About), `Ctrl+O` opens the workspace window, `F1`
  lists every binding.
- The Interface pane holds browser preferences only - Enter sends, timestamps,
  follow the run, fold successful tool cards, warn before closing mid-run, a
  chime - and they never travel to the model.
- Transcripts, preferences and the setup checklist live in this browser's
  `localStorage`. Settings > Data reports what they cost and clears them; the
  workspace, the API key and the server config are untouched by that.

## Security model

These are the reasons to run this locally rather than hand a shell to a remote
service. They are also the parts worth reading before you trust it:

- **Loopback only.** The server binds `127.0.0.1` and accepts only the host
  authorities it created. Requests with a foreign `Origin`/`Host` (the shape of a
  DNS-rebinding attack from a hostile web page) are rejected, and responses carry
  a strict CSP.
- **Approval gate.** `run_shell`, `write_file` and `window_control` always prompt
  unless you enable auto-approve. `web_fetch` prompts too whenever the URL is flagged - one that
  resolves to this machine or the local network - and `screenshot` always prompts
  outside Agent mode, because it uploads your desktop. `list_windows` joins them
  for the same reason at one remove: the titles it returns are the names of what
  you have open. `close` is never covered by auto-approve, because no re-run of it
  brings back unsaved work. Auto-approve is ignored
  entirely in Plan mode and never covers a call classified *dangerous* -
  recursive deletes, disk and registry operations, privilege escalation,
  downloading into a shell, `git push --force`, credential access and similar.
  Those need the confirmation word typed into the card, and the server verifies
  it.
- **Path sandbox.** Every tool path is resolved through symlinks and NTFS
  junctions before use, so a link inside the workspace cannot walk a read or a
  write outside it.
- **No credential inheritance.** Child processes launched by `run_shell` get the
  environment minus every provider's API key variable, so a bare
  `set`/`env`/`printenv` cannot print a key into the transcript.
- **Redaction on the way out.** Model output, tool output and error strings are
  all scrubbed for the live keys and for anything shaped like one before they
  reach the browser or the remote API.
- **Fetch policy.** `web_fetch` refuses targets that resolve to this machine or
  the local network, re-checks every redirect hop and every resolved IP, and uses
  no proxy.
- **MCP is third-party code, and is treated as such.** A server runs as a
  child process with your privileges, outside the path sandbox and outside
  anything this agent can classify, so its tools are offered in Agent mode
  only, every call stops at an approval card, and its environment is scrubbed
  of your provider keys first. The list is yours: nothing is started that you
  did not name in `config.json`.
- **One endpoint.** A run posts to the configured provider's URL, or the
  `base_url` set in config.json. No tool call can redirect that, and the
  endpoint is validated before it is saved.
- **Watchdog.** A run with no progress for `idle_timeout_sec` (120s by default),
  or one that exceeds `run_timeout_min` (25 minutes by default), is cancelled.
  Both are set in the Agent pane and are checked when a run starts. Stop and
  force-abort are both available from the UI. The per-command limit
  (`shell_timeout_sec`) is read per call instead, so a save mid-run applies to
  the next command rather than the next task.

What it is *not*: a sandbox against a hostile model. Once you approve a command,
that command runs with your privileges. The approval card is the boundary - read
it.

## Limits

- One task at a time, one user, loopback only.
- Shell commands get `shell_timeout_sec` (60s by default, up to an hour
  from Settings > Agent). It is a separate knob from the two watchdog timers:
  this one kills a single child process, they end the whole task. A command the
  limit kills says so, and says which setting to raise.
- Tool results are truncated at 8KB, MCP ones included. A single MCP call
  may take up to ten minutes before it is abandoned: a server that wedges is a
  slow run, not a fast failure.
- The fourth identical tool call in one run is refused rather than executed, so
  a model cannot spin on a command that keeps failing.
- `baseURL` in `agent.go` is a test hook now, not a setting: it points the client
  at a local fake endpoint for the tests and is empty in every real build.

## Layout

| file | role |
|---|---|
| `main.go` | server setup, embedded UI, loopback binding, config/key endpoints |
| `agent.go` | the run loop, streaming, approvals, watchdog, abort |
| `tools.go` | tool schemas and implementations, path sandbox, fetch policy |
| `providers.go` | the provider registry: endpoints, key variables, model defaults, vision rules |
| `safety.go` | command risk classifier and the per-tool approval rules |
| `guard.go` | host/origin guards and security headers |
| `config.go` | config load/save, per-provider key storage, secret redaction |
| `banner.go` | the ASCII startup banner: mark plus the launch state |
| `mcp.go` | the MCP client: server processes, handshake, tool catalogue, `tools/call` |
| `mcp_windows.go`, `mcp_other.go` | how a server process is started per platform (no console window on Windows) |
| `vision.go` | image validation, inline limits, attaching a capture to the conversation |
| `screenshot.go` | the desktop capture path and its temporary-file sweep |
| `window.go`, `window.ps1` | window enumeration, per-window capture, control and waits |
| `shell_windows.go`, `shell_other.go` | how one shell line is handed to cmd.exe or /bin/sh |
| `secret_windows.go`, `secret_other.go` | key sealing per platform |
| `ui/index.html` | the whole front end, no build step |
| `ui/vendor/` | vendored marked, DOMPurify, highlight.js - see its own README |

## Tests

```sh
go vet ./...
go test ./...
```

`go vet` type-checks only the target it is built for, so a clean run on Windows
says nothing about darwin or linux: an untagged `SysProcAttr.CmdLine` in
`tools.go` once passed `go vet` on Windows and failed on the first unix runner
it reached. CI therefore vets the other two targets as well, from Linux:

```sh
GOOS=darwin go vet ./...
GOOS=windows go vet ./...
```

`go vet` builds nothing, links nothing and runs nothing, so `go build` and
`go test` are the steps that actually prove the tree compiles and works.

The suite includes end-to-end tests: a real server on a loopback port for the
abort path, and on Windows a real Notepad that is launched, waited for, captured
while minimized and closed. Those windows tests are gated on `GOOS` and skip
elsewhere, because the desktop tools are.

CI (`.github/workflows/ci.yml`) runs `gofmt`, `go vet` (plus the cross-vet
above), `go build` and `go test` on Linux, macOS (`macos-15-intel`, because the
arm64 pool is the capacity-constrained one) and Windows, pinned to the Go
version in `go.mod` so the stated minimum is the version actually exercised.

## License

MIT - see `LICENSE`. Vendored front-end libraries keep their own licenses, listed
in `ui/vendor/README.md`.
