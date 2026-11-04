# Architecture

## File layout

| file | role |
|---|---|
| `main.go` | server setup, embedded UI, loopback binding, config/key endpoints |
| `agent.go` | the run loop, streaming, approvals, watchdog, abort |
| `tools.go` | tool schemas and implementations, path sandbox, fetch policy |
| `edit.go` | `edit_file`: exact matching, the uniqueness rule, line-ending tolerance |
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
| `notify_windows.go`, `notify_other.go` | the Windows toast raised for a shell command |
| `shell_windows.go`, `shell_other.go` | how one shell line is handed to `cmd.exe` or `/bin/sh` |
| `secret_windows.go`, `secret_other.go` | key sealing per platform |
| `ui/index.html` | the whole front end, no build step |
| `ui/vendor/` | vendored marked, DOMPurify, highlight.js - see its own README |

Everything above is `package main` in one directory. Splitting by platform is done
with file suffixes, which is why a clean `go vet` on Windows says nothing about
darwin or linux.

## The process

```
main()
  loadConfig()                    config.json, sealed key, defaults, migration
  NewAgent(cfg)                   run state, MCP manager
  http.Serve(guard(mux), 127.0.0.1:8787)
  printBanner(...)                the launch state, ending in the bound URL
  go ag.warmMCP()                 start configured servers before the first task
  open(url)                       unless -no-browser
  <-signal; ag.mcp.close()        Ctrl+C stops the child processes
```

The UI is embedded with `//go:embed ui/*`, so the served page, the vendored
JavaScript and the agent loop are all in the executable. There is no CDN and no
network access at runtime beyond the model endpoint and whatever a tool call
deliberately reaches.

## The run loop

One task at a time. `POST /api/chat` sets up a run and streams events; the run
itself is detached from the request but watches it, so a closed tab cancels a run
instead of letting it burn steps.

```
handleChat
  validate task and images, refuse early on a text-only model
  refuse a second task (429)
  bump runToken, snapshot limits
  ctx, cancel = WithCancel(detached)      watchdog + disconnect watcher
  emit := redacted SSE writer
  a.run(ctx, task, mode, images, emit)
```

Inside `a.run`:

- the system prompt is built from the mode and `effort`,
- the tool catalogue is built from the mode, the platform, and the MCP servers,
- each step posts to the provider, streams `token` events, and executes the tool
  calls it gets back, one at a time, through `execTool`,
- `execTool` classifies, gates, executes, truncates and reports,
- `max_steps` ends the loop with "Step limit reached. Type 'continue' to keep
  going.",
- `done` ends it for good.

Retries, mark-ups and the repeat-call ledger all live here: the fourth identical
tool call in one run is refused, and a run that keeps failing on the same command
cannot spin.

## Watchdog and abort

Two independent ways out of a wedged run, plus the human one:

| mechanism | bound |
|---|---|
| idle watchdog | `idle_timeout_sec` with no activity |
| wall-clock watchdog | `run_timeout_min` |
| stop | asks the run to end |
| force-abort | releases it, and bumps the run token so a goroutine that wakes up late cannot clear a newer run's state |

`touch()` is called by every event that reaches the browser, which is what the idle
watchdog measures - so a deliberate `wait` counts as activity.

## Guard

`guard.go` wraps the mux: it accepts only the host authorities this process
created, rejects a foreign `Origin`/`Host` (the shape of DNS rebinding from a
hostile page), and sets a strict CSP. The listener is on `127.0.0.1` and the port is
preferred (`8787`) rather than random, because `localStorage` is partitioned by
origin.

## Adding a tool

1. Add its schema in `toolSchemas` (`tools.go`), inside the right mode block - a
   tool the model may not use should not be in the schema at all.
2. Add its case in `runTool`, returning a string. Results are truncated centrally.
3. Add it to `needsApproval` if it must always ask, or to `promptsOnRisk` if it
   should ask only when the classifier is unhappy, or to `neverAuto` if Always
   Allow must not cover it.
4. Add it to `desktopTool` if it needs Win32, so it disappears elsewhere.
5. Say in the description what the tool does *not* do - the descriptions are the
   model's only instruction about limits, and a tool that never mentions its own
   gate gets used as though it had none.
