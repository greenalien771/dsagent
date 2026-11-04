# Configuration

Everything the agent reads at start lives in one file, `config.json`. Secrets do
not: the API key is stored separately and never written there.

## Location

| OS | path |
|---|---|
| Windows | `%AppData%\dsagent\config.json` |
| macOS | `~/Library/Application Support/dsagent/config.json` |
| Linux | `~/.config/dsagent/config.json` |

Next to it, one sealed key file per vendor family:

```
api_key_deepseek.bin
api_key_qwen.bin
api_key_zai.bin
```

## Keys

| key | default | meaning |
|---|---|---|
| `provider` | `deepseek` | which built-in endpoint to talk to - see [Providers](Providers) |
| `model` | the provider's own default | chat model id sent to the API |
| `base_url` | *(the provider's endpoint)* | full chat-completions URL that replaces the provider's own |
| `workdir` | the directory dsagent was first started in | sandbox root for every tool |
| `max_steps` | `25` | tool-call budget per task (the form allows up to 200) |
| `effort` | `balanced` | `direct`, `balanced` or `thorough` - see [Modes](Modes) |
| `auto_approve` | `false` | skip approval cards for non-dangerous calls |
| `notify_any` | `false` | Windows notification for every `run_shell` command |
| `notify_danger` | `true` | Windows notification for dangerous commands only |
| `idle_timeout_sec` | `120` | no-output watchdog, 10-1800 seconds |
| `run_timeout_min` | `25` | wall-clock cap per task, 1-240 minutes |
| `shell_timeout_sec` | `60` | cap on one `run_shell` command, 5-3600 seconds |
| `mcp_servers` | *(none)* | MCP servers to start as extra tool providers - see [MCP servers](MCP-Servers) |

A key that is absent takes its default. A key that is present but out of range is
clamped into range, and a nonsense value (non-numeric, wrong type) leaves the
default in place - a hand-edited file degrades, it does not stop the agent from
starting.

## The API key

- The environment variable wins. `DEEPSEEK_API_KEY`, `DASHSCOPE_API_KEY` or
  `ZAI_API_KEY` overrides anything stored, is never persisted, and is never sent
  to the browser. The banner reports the source as `env`, and the UI explains why
  edits in Settings are then ignored.
- A key pasted into the UI is sealed with the OS keystore - **DPAPI on Windows** -
  in `api_key_<family>.bin`. One file per vendor family, so switching provider
  cannot hand the previous vendor's key to a different endpoint.
- On macOS and Linux there is no keystore without cgo, so the key is
  base64-encoded in a `0600` file. **That is obfuscation, not encryption.** Prefer
  the environment variable on those platforms and keep nothing on disk.
- A file from before providers existed, `api_key.bin`, is read as DeepSeek's key
  and removed when that key is.
- Key material is redacted on the way out: model output, tool output and error
  strings are all scrubbed for the live keys, and for anything shaped like one,
  before they reach the browser or the remote API.
- If a sealed key exists but could not be loaded - written by another Windows
  account, truncated, unknown format - the reason is kept and quoted by the
  banner and by the "no key" refusal, so you are not asked to paste a key that is
  already there.

## Editing

- **Settings** (`Ctrl+,`) writes through the API and takes effect immediately for
  the fields it owns: model, key, limits, effort, notifications, MCP list.
- **A hand edit of `config.json`** is read at the next start. `POST /api/mcp` and
  the **Reload servers** button are the one exception - they re-read the server
  list without a restart.
- A file that does not parse is moved aside to `config.json.corrupt` and the agent
  starts from defaults rather than refusing to run.
- `provider` and `base_url` are config-file keys; the provider switch is not in
  the UI yet. Settings > Connection edits the model and the key. An unknown
  `provider` value falls back to DeepSeek instead of failing, while a bad
  `base_url` is refused with a message instead of being stored.

## Limits

Two families of timeout, deliberately separate:

| setting | scope | default | bounds |
|---|---|---|---|
| `idle_timeout_sec` | whole task, measured by silence | 120s | 10-1800 |
| `run_timeout_min` | whole task, wall clock | 25 min | 1-240 |
| `shell_timeout_sec` | a single `run_shell` child process | 60s | 5-3600 |

The first two end the run; the third kills one command. Both watchdog numbers are
snapshotted when a run starts, so a save mid-run applies to the next task. The
per-command limit is read per call, so a save applies to the next command.

The bounds exist so no settings field can turn the watchdog off: a run always ends
on its own. Other limits that are not settings: 8KB per tool result, 8 images and
20 MiB of pixels per task, 200 steps maximum, and a refusal on the fourth
identical tool call in one run.
