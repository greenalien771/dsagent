# dsagent

A coding agent in one Go binary. It serves its own UI on loopback, talks to the
DeepSeek `/chat/completions` API, and runs tools - list, read, write, shell,
fetch - inside a working directory you choose. Every mutating tool call stops at
an approval card before it runs.

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

## Security model

These are the reasons to run this locally rather than hand a shell to a remote
service. They are also the parts worth reading before you trust it:

- **Loopback only.** The server binds `127.0.0.1` and accepts only the host
  authorities it created. Requests with a foreign `Origin`/`Host` (the shape of a
  DNS-rebinding attack from a hostile web page) are rejected, and responses carry
  a strict CSP.
- **Approval gate.** `run_shell` and `write_file` always prompt unless you enable
  auto-approve, and auto-approve never covers a call classified *dangerous* -
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
- **Watchdog.** A run with no progress for 120s, or one that exceeds 25 minutes,
  is cancelled. Stop and force-abort are both available from the UI.

What it is *not*: a sandbox against a hostile model. Once you approve a command,
that command runs with your privileges. The approval card is the boundary - read
it.

## Limits

- One task at a time, one user, loopback only.
- Shell commands get 60s; tool results are truncated at 8KB.
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

## License

MIT - see `LICENSE`. Vendored front-end libraries keep their own licenses, listed
in `ui/vendor/README.md`.
