# HTTP API

The UI is a single static page served by the same process, so these endpoints are
not a public API - but they are the whole contract between the front end and the
agent, and they are usable from a script on the same machine.

- **Loopback only.** The listener is on `127.0.0.1`, and only the host authorities
  the process created are accepted. A request with a foreign `Origin`/`Host` is
  rejected.
- **JSON in, JSON out**, `Cache-Control: no-store`, `X-Content-Type-Options:
  nosniff`. Request bodies are size-capped.
- **Nothing secret comes back.** `/api/config` and `/api/key` answer with the same
  masked view; the key itself is never echoed.

## Endpoints

| endpoint | methods | body | what it does |
|---|---|---|---|
| `/api/chat` | POST | `{task, mode, images[]}` | starts a task; the response is `text/event-stream` |
| `/api/stop` | POST | - | asks the current run to stop |
| `/api/abort` | POST | - | force-aborts it; bumps the run token so a late goroutine cannot clear a newer run |
| `/api/status` | GET | - | busy flag, mode, idle and elapsed time, the limits |
| `/api/approve` | POST | `{id, ok, always, confirm}` | answers an approval card |
| `/api/answer` | POST | `{id, answer}` | answers an `ask_user` question |
| `/api/clear` | POST | - | starts a fresh server-side conversation |
| `/api/config` | GET, POST | config patch | reads or patches the non-secret configuration |
| `/api/key` | POST, DELETE | `{api_key}` | stores or clears the key |
| `/api/mcp` | GET, POST | `{action, name}` | reports the servers, or reloads / stops / starts / restarts them |

`/api/approve` is deliberately not `/api/answer`: a question is not an approval,
and mixing the two would allow a card to be answered without being read.

## `/api/chat`

```json
{"task": "add a retry to fetch.go", "mode": "agent", "images": [{"name": "shot.png", "data": "<base64>"}]}
```

- An empty task is `400`.
- Images are validated here; more than 8, or more than 20 MiB decoded in total, is
  `400`. So is an image on a model that cannot see one - refused before the run, not
  in the middle of it.
- A second task while one is running is `429` with how long the current run has
  been going. One task at a time is a design limit, not a queue.
- `mode` is `agent`, `plan` or `ask`; anything else normalises to `agent`.
- The run is detached from the HTTP request, so a browser refresh does not kill it
  - but the request is still watched, and if the tab really goes away the run is
  cancelled instead of burning steps into a dead socket.

### The event stream

One JSON object per `data:` line, `{"type": ..., "text": ...}`, flushed as it
happens. Every string goes through the same redaction the transcript gets.

| type | meaning |
|---|---|
| `token` | a chunk of model output |
| `tool` | a tool is about to run (name and arguments) |
| `tool_result` | its result |
| `approval` | a permission card, as `{id, tool, args, level, reasons, confirm}` |
| `question` | an `ask_user` card, as `{id, question, options}` |
| `notify` | a `run_shell` command line and its risk level, for the notification and the activity list |
| `status` | a line about what the run is doing |
| `error` | a fatal message for this run |
| `abort` | the run was cancelled, with the reason |
| `done` | the run ended (`""`, or a note when the ending was `done` with a summary) |

The stream carries its own failure mode: a missing key emits `error` before
anything else, and says whether there is no key at all or a stored key that could
not be loaded.

## `/api/status`

```json
{"busy": true, "mode": "agent", "idle_ms": 1200, "elapsed_ms": 41000,
 "idle_limit_ms": 120000, "run_limit_ms": 1500000, "step_limit": 25}
```

This is what the UI polls to draw the watchdog bar and to know whether a run is in
flight.

## `/api/config`

`GET` returns the masked view: provider, model, `base_url`, `workdir`, every limit,
the effort, the notification switches, the MCP list, the provider catalogue and
where the key came from (`env` or `store`) - never the key.

`POST` takes a partial patch: only the fields present are changed, which is why
every field is a pointer on the receiving side. Values are validated and clamped;
an invalid one is refused with a message rather than stored. Bad JSON is `400`.

`/api/key` is the only endpoint that accepts key material. `POST` stores it sealed,
`DELETE` clears it, and both answer with the same masked view.

## `/api/mcp`

- `GET` returns `{servers, tools, configured}`: the configured list, each entry's
  runtime state and its tool count.
- `POST` with **no body** is the "Reload servers" button - it re-reads the
  configuration without restarting the agent.
- `POST` with `{"action": "stop|start|restart", "name": "sparrow"}` acts on one
  server; `name` may be empty or `all`.
- `stop` is the same stop the agent's own `mcp_control` tool does: stdin is closed
  so the server notices, it is killed only if it ignores that, and it stays down
  until started again.

## `/api/clear`

Starts a fresh server-side conversation and resets the repeat-call ledger -
"this exact call already ran three times" belongs to the task just thrown away.
It answers `409` while a run is in flight: wiping the transcript halfway through a
step would leave an in-flight request with nothing but orphan tool results, which
is a request the API rejects.
