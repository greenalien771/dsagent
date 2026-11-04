# MCP servers

dsagent is a [Model Context Protocol](https://modelcontextprotocol.io) **client**.
A server is a child process you list in the config, speaking JSON-RPC 2.0 over its
own stdin and stdout, one message per line - the stdio transport. Nothing here
reaches the network on its own. A server runs because it is in the list, and it is
stopped when the agent stops.

Deliberately not implemented: sampling, roots and prompts. Those would let a
server drive this agent or ask the user questions through it, and none of that is
worth the attack surface for the tool calling this exists for. A server that asks
for one gets a method-not-found answer instead of silence, so it does not hang.

## Configuration

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
| `approval` | `ask` (default), `allow` or `block` - the per-server rule for the permission card |
| `tools` | an allow-list of tool names; empty means every tool the server advertises |
| `timeout_sec` | cap on one call to this server, 1-3600; empty means the built-in ten minutes |

## The three per-server settings

Each one answers a specific annoyance:

- **`approval`** is the one a server earns over time. A browser driver you trust to
  read pages should not cost a click per step, while a file-system server pointed
  at a real folder should keep asking.
  - `allow` runs that server's calls without the card, **in Agent mode only** - Plan
    and Ask still refuse them - and the model is told so in their descriptions, so
    it plans around the missing card instead of being surprised by it.
  - `block` stops offering the tools at all, so the model never spends a step on a
    call that will be refused. A call to a blocked server's tool is refused before
    the risk layer even runs.
- **`tools`** is the fix for a server that advertises forty tools and gets the wrong
  one picked. Name the four that matter and the rest are not in the request at all.
- **`timeout_sec`** is per server because "how long may this take" is a property of
  the server, not of the agent: a screenshot is seconds, a browser automation run is
  minutes, and the built-in ten-minute cap is wrong for both ends.

## Editing the list

- **Settings > MCP** is the service desk: one row per server with its state, tool
  count and policy chip, a **Settings** button that folds out the three fields, and
  **Apply and reload** - which patches the saved list and reloads the catalogue,
  because a filter and a deadline only mean something to a catalogue built from
  them. Start/stop/restart per server live here too.
- **Settings > Agent** edits the same list as one JSON document, with a **Reload
  servers** button beside it. That button, and `POST /api/mcp`, are how a
  hand-edited file or a dead server is picked up without a restart.
- A list that does not validate is refused on save rather than partly applied. A
  bad entry in a hand-edited file is dropped with a note on stderr, instead of
  being started on every task and failing there.
- The agent's own `mcp_control` tool offers `status`, `stop`, `start` and
  `restart`, so the pane and the tool cannot drift.

## What the model sees

- Tools arrive as `mcp__<server>__<tool>`, sanitised to `[A-Za-z0-9_-]` and cut to
  the API's 64-character limit with a hash of the full name, so two tools that
  differ only past the cut stay two tools.
- Every description is prefixed `[MCP <server>]`, and the approval card names the
  same server.
- A call is gated like `run_shell`: the card says it is third-party code outside
  the workspace. **Always Allow** covers it in Agent mode only. Plan and Ask refuse
  it in words, because a tool that runs someone else's code cannot promise it
  changed nothing.
- The answer is flattened to text: text and resource blocks come through, an image
  or audio block is named instead of silently dropped, and the usual 8KB truncation
  applies.

## Lifecycle

- Servers start in the background at launch - the slowest part of a cold start, so
  it does not wait for the first task - and again at the top of a task when the
  list changed, so the model is only ever offered a tool that can answer.
- One server failing to start is reported and skipped: it never costs the run the
  other servers, let alone the built-in tools.
- A server that dies mid-call is noticed when its stdout ends, and the call fails
  with the end of its stderr as the reason, instead of hanging until the
  ten-minute tool timeout. The handshake and `tools/list` get 20 seconds each.
- The child gets your environment **minus** this agent's provider key variables - a
  server needs `PATH`, not your model key - plus whatever its `env` names.
- On Windows a server is started with `CREATE_NO_WINDOW`, so none of this flashes a
  console window over what you are doing.
- `Ctrl+C` stops the servers with the agent. A console window closed outright never
  delivers that signal, so they are left to notice that stdin is gone - which is
  why stdin is closed first on the way out.
