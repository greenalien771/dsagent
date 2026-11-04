# dsagent wiki

dsagent is a coding agent in a single Go binary. It serves its own UI on loopback,
talks to an OpenAI-compatible `/chat/completions` endpoint (DeepSeek by default,
Qwen/DashScope and z.ai/GLM built in), and runs tools - list, read, write, patch,
shell, fetch, screenshot, window control, plus the tools of any MCP server you
configure - inside a working directory you choose.

Mutating calls stop at an approval card before they run. A call the risk layer
classifies *dangerous* needs the confirmation word typed into that card. A task
ends with the `done` tool, which stops the run and puts "Task finished" plus the
agent's own summary in the transcript.

No telemetry, no hosted backend, no CDN: the UI, its vendored JavaScript and the
agent loop all live inside the executable.

## Quick start

```sh
go build -o dsagent.exe .     # Windows
go build -o dsagent .         # macOS / Linux
./dsagent.exe
```

It binds `http://127.0.0.1:8787`, prints a startup banner ending in
`dsagent listening on <url>`, and opens that URL in your browser. Paste an API
key into Settings > Connection on first run, or set `DEEPSEEK_API_KEY` /
`DASHSCOPE_API_KEY` / `ZAI_API_KEY` in the environment.

## Pages

| Page | What it covers |
|---|---|
| [Getting started](Getting-Started) | Build, run, flags, the banner, the first task, where state lives |
| [Configuration](Configuration) | Every `config.json` key, its default and its range |
| [Providers](Providers) | The built-in endpoints, key families, `base_url`, vision rules |
| [Tools](Tools) | All fourteen built-in tools, their arguments and their limits |
| [Modes](Modes) | Agent / Plan / Ask, and the `effort` ladder |
| [Safety and approvals](Safety-and-Approvals) | The approval gate, risk classes, sandbox, redaction, watchdog |
| [MCP servers](MCP-Servers) | Configuring third-party tool servers, naming, gating, lifecycle |
| [Desktop and images](Desktop-and-Images) | Vision, `screenshot`, `list_windows`, `window_control`, `wait` |
| [HTTP API](HTTP-API) | The loopback endpoints the UI is built on |
| [Interface](Interface) | Panes, keyboard bindings, what lives in `localStorage` |
| [Architecture](Architecture) | File layout, the run loop, how the binary is put together |
| [Development](Development) | Tests, cross-vet, CI, updating the vendored front-end |
| [Troubleshooting](Troubleshooting) | Port, key, locked exe, 400s, watchdog stops, MCP that will not start |

## The rules the design follows

These are the constraints most decisions in this tree come from. They are worth
knowing before reading any other page.

- **The model is never offered a tool it would only be refused later.** That is
  why Plan and Ask drop `write_file` and `edit_file` from the schema rather than
  answering "no" to a call, and why the three Win32 tools disappear on Linux and
  macOS. A refusal costs a step and teaches the model nothing.
- **The approval card is the security boundary.** Not the prompt, not the tool
  list, not the risk classifier - the human reading the card. The classifier only
  decides how loud the card is. Once a command is approved it runs with your
  privileges.
- **Settings are numbers, not constants.** Timeouts, step budgets and effort are
  in `config.json` because "how long may this take" is a property of the task, not
  of the binary.
- **Nothing is started that you did not name.** No MCP server launches except
  from your list, no request goes anywhere but the configured endpoint, and the
  server listens on loopback only.

## Limits worth stating up front

One task at a time, one user, loopback only. Tool output is truncated at 8KB. The
fourth identical tool call in one run is refused. Shell commands get
`shell_timeout_sec` (60s by default); a run that goes silent for
`idle_timeout_sec` (120s) or runs longer than `run_timeout_min` (25 minutes) is
cancelled. See [Limits](Configuration#limits) for the numbers and their bounds.
