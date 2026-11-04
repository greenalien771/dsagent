# Interface

One static page, served from inside the binary. No build step, no framework: the
front end is `ui/index.html` plus four vendored libraries.

## Layout

- **Left rail** - sessions. `Alt+N` starts another.
- **Right panel** - the activity list for the current session: tool calls, their
  results, approval cards, status lines.
- **Composer** - the task, the `+` attachment menu, and the Agent / Plan / Ask
  switch.

## Keyboard

| binding | action |
|---|---|
| `Ctrl+K` | command palette |
| `Ctrl+,` | Settings |
| `Ctrl+O` | workspace window |
| `Alt+N` | new session |
| `F1` | every binding, listed |

## Settings

`Ctrl+,` opens seven panes:

| pane | what lives there |
|---|---|
| **Connection** | provider (read-only here), model, API key, `base_url` |
| **Agent** | effort, step budget, the three timeouts, and the MCP list as JSON with **Reload servers** |
| **MCP** | one row per server: state, tool count, policy chip, start/stop/restart, and the per-server `approval` / `tools` / `timeout_sec` fields under **Settings** |
| **Interface** | browser preferences only - Enter sends, timestamps, follow the run, fold successful tool cards, warn before closing mid-run, a chime |
| **Safety** | auto-approve, and the two notification switches |
| **Data** | what browser storage costs, and a button to clear it |
| **About** | version and links |

The MCP pane and the agent's `mcp_control` tool drive the same state, so they
cannot drift. Settings writes through `/api/config`, so a change applies without a
restart for every field the pane owns.

## What lives in the browser

Transcripts, UI preferences and the setup checklist are in this browser's
`localStorage` - per origin, which means per port. Settings > Data reports the cost
and clears them; the workspace, the API key and the server config are untouched by
that.

Because of that split, clearing browser data loses your transcripts but not your
configuration, and moving to a different port (`8787` taken by something else)
shows an empty transcript while the old one still exists at the old origin.

## Transcript conventions

- A run that ends with `done` shows **Task finished** and the agent's summary.
- A run that ends without `done` just stops - that is the visual difference between
  "finished" and "gave up".
- A cancelled run shows the reason: stopped by you, a watchdog limit, or the tab
  going away.
- Skipped approvals are announced each time auto-approve lets a call through, so
  unattended execution is never silent.
