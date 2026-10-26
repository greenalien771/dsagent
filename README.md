# dsagent

![Go Version](https://img.shields.io/badge/Go-1.21%2B-blue)
![License](https://img.shields.io/badge/License-MIT-green)
![Platform](https://img.shields.io/badge/Platform-Windows%20%7C%20macOS%20%7C%20Linux-lightgrey)
![RAM](https://img.shields.io/badge/Memory-4MB-brightgreen)

A coding agent in one Go binary. It serves its own UI on loopback, talks to the
DeepSeek `/chat/completions` API, and runs tools - list, read, write, shell,
fetch - inside a working directory you choose. Mutating calls stop at an approval
card before they run, and a call classified dangerous needs the confirmation word
typed into that card.

No telemetry, no hosted backend, no CDN. The UI, its vendored JavaScript and the
agent loop are all inside the executable.

## Requirements

- Go 1.21 or newer to build (developed and tested against Go 1.27 on Windows).
- A DeepSeek API key. Set `DEEPSEEK_API_KEY` or paste it into the settings panel
  on first run.

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

`-no-browser` skips launching the browser - useful for scripts and headless runs:

```sh
./dsagent.exe -no-browser
```

On Windows the running `dsagent.exe` is locked by the OS. To rebuild, stop it
first, or build somewhere else to check compilation:

```sh
go build -o "%TEMP%\check.exe" .
```

## Configuration

Stored in `config.json` under the OS config directory:

| OS | path |
|---|---|
| Windows | `%AppData%\dsagent\config.json` |
| macOS | `~/Library/Application Support/dsagent/config.json` |
| Linux | `~/.config/dsagent/config.json` |

| key | default | meaning |
|---|---|---|
| `model` | `deepseek-flash` | chat model id sent to the API |
| `workdir` | directory dsagent was first started in | sandbox root for every tool |
| `max_steps` | `25` | tool-call budget per task (form allows up to 200) |
| `auto_approve` | `false` | skip approval cards for non-dangerous calls |
| `idle_timeout_sec` | `120` | no-output watchdog, 10-1800 seconds |
| `run_timeout_min` | `25` | wall-clock cap per task, 1-240 minutes |

The API key is never written into `config.json` and never sent to the browser.
`DEEPSEEK_API_KEY` in the environment wins over anything stored and is never
persisted. A key pasted into the UI is sealed with the OS keystore (DPAPI on
Windows) in `api_key.bin` next to the config.

On macOS and Linux there is no keystore without cgo, so the key is base64-encoded
in a `0600` file. That is obfuscation, not encryption - on those platforms prefer
the environment variable and keep nothing on disk.

## Modes

| mode | sees | can do |
|---|---|---|
| Agent | all tools | everything, each mutating call behind an approval card |
| Plan | all tools | reads and investigates; `run_shell` always prompts, `write_file` is not offered |
| Ask | read-only tools | answers questions, changes nothing |

## Images and the screen

`deepseek-flash` accepts images, so a task can carry pictures as well as text.

- **Attach an image** with `+` in the composer, then **Add Image...** (PNG, JPEG,
  GIF or WebP, up to 8 per task). **Add File...** in the same menu adds a text file
  as context instead. Both apply to the next task only.
- **The agent can look at your screen** with the `screenshot` tool. It captures the
  desktop, or one region via `x`/`y`/`width`/`height`, and `scale` shrinks a big
  region so it costs fewer tokens. Capture is a separate approval-gated step: it
  uploads what is on your screen to the API, so it prompts unless auto-approve is
  on, and always in Plan and Ask mode.
- `deepseek-v4-pro` is text-only. An image is refused up front with a message that
  says so, instead of failing mid-run.
- Inline images are kept in the conversation so a follow-up question still works,
  but the total is capped - the oldest payloads become a line naming the image
  rather than several megabytes resent on every step.

## Security model

These are the reasons to run this locally rather than hand a shell to a remote
service. They are also the parts worth reading before you trust it:

- **Loopback only.** The server binds `127.0.0.1` and accepts only the host
  authorities it created. Requests with a foreign `Origin`/`Host` (the shape of a
  DNS-rebinding attack from a hostile web page) are rejected, and responses carry
  a strict CSP.
- **Approval gate.** `run_shell` and `write_file` always prompt unless you enable
  auto-approve. `web_fetch` prompts too whenever the URL is flagged - one that
  resolves to this machine or the local network - and `screenshot` always prompts
  outside Agent mode, because it uploads your desktop. Auto-approve is ignored
  entirely in Plan mode and never covers a call classified *dangerous* -
  recursive deletes, disk and registry operations, privilege escalation,
  downloading into a shell, `git push --force`, credential access and similar.
  Those need the confirmation word typed into the card, and the server verifies
  it.
- **Path sandbox.** Every tool path is resolved through symlinks and NTFS
  junctions before use, so a link inside the workspace cannot walk a read or a
  write outside it.
- **No credential inheritance.** Child processes launched by `run_shell` get the
  environment minus the LLM API keys, so a bare `set`/`env` cannot print the key
  into the transcript.
- **Redaction on the way out.** Model output, tool output and error strings are
  all scrubbed for the live key and for anything shaped like one before they
  reach the browser or the remote API.
- **Fetch policy.** `web_fetch` refuses targets that resolve to this machine or
  the local network, re-checks every redirect hop and every resolved IP, and uses
  no proxy.
- **Watchdog.** A run with no progress for `idle_timeout_sec` (120s by default),
  or one that exceeds `run_timeout_min` (25 minutes by default), is cancelled.
  Both are set in the Agent pane and are checked when a run starts. Stop and
  force-abort are both available from the UI.

What it is *not*: a sandbox against a hostile model. Once you approve a command,
that command runs with your privileges. The approval card is the boundary - read
it.

## Limits

- One task at a time, one user, loopback only.
- Shell commands get 60s; tool results are truncated at 8KB.
- The fourth identical tool call in one run is refused rather than executed, so
  a model cannot spin on a command that keeps failing.
- The API endpoint is fixed at build time (`baseURL` in `agent.go`).

## Layout

| file | role |
|---|---|
| `main.go` | server setup, embedded UI, loopback binding, config/key endpoints |
| `agent.go` | the run loop, streaming, approvals, watchdog, abort |
| `tools.go` | tool schemas and implementations, path sandbox, fetch policy |
| `safety.go` | command risk classifier |
| `guard.go` | host/origin guards and security headers |
| `config.go` | config load/save, key storage, secret redaction |
| `secret_windows.go`, `secret_other.go` | key sealing per platform |
| `ui/index.html` | the whole front end, no build step |
| `ui/vendor/` | vendored marked, DOMPurify, highlight.js - see its own README |

## Tests

```sh
go vet ./...
go test ./...
```

CI (`.github/workflows/ci.yml`) runs `gofmt`, `go vet`, `go build` and
`go test` on Linux, macOS and Windows, pinned to the Go version in `go.mod` so
the stated minimum is the version actually exercised.

## License

MIT - see `LICENSE`. Vendored front-end libraries keep their own licenses, listed
in `ui/vendor/README.md`.
