# Getting started

## Requirements

- **Go 1.21 or newer** to build. `go.mod` pins `go 1.21`, and CI builds with the
  version in that file, so the stated minimum is the one actually exercised.
- **An API key for one of the built-in providers.** Put it in that provider's
  environment variable, or paste it into Settings > Connection on first run:

  | provider | variable |
  |---|---|
  | DeepSeek | `DEEPSEEK_API_KEY` |
  | Qwen (DashScope) | `DASHSCOPE_API_KEY` |
  | z.ai (GLM) | `ZAI_API_KEY` |

## Build

```sh
go build -o dsagent.exe .     # Windows
go build -o dsagent .         # macOS / Linux
```

On Windows the *running* `dsagent.exe` is locked by the OS, so a rebuild over it
fails with access-denied. That is the OS protecting the process, not a bug. Either
stop dsagent first, or prove the build somewhere else:

```sh
go build -o "%TEMP%\check.exe" .
```

## Run

```sh
./dsagent.exe
```

What happens, in order:

1. `config.json` is loaded from the OS config directory (see [Configuration](Configuration)).
2. The HTTP server binds **`127.0.0.1:8787`**. If that port is taken it falls back
   to a free one and the banner prints the URL it actually got.
3. The banner is printed (see below).
4. Configured MCP servers are started in the background, so the tool catalogue is
   usually ready before the first task.
5. The URL is opened in your browser.

Stop it with `Ctrl+C`. That is the only exit, and it is where MCP child processes
are stopped. A console window closed outright never delivers the signal, so the
servers are left to notice their stdin is gone - which is why stdin is closed
first on the way out.

### `-no-browser`

```sh
./dsagent.exe -no-browser
```

Skips launching the browser. It is the only command-line flag; it exists for
scripts and headless runs.

### Why the port matters

Chat history lives in browser `localStorage`, which is partitioned by
scheme + host + port. A launch on a different port is therefore a fresh
transcript, and the old one is still there but invisible. `8787` is the preferred
port for exactly that reason.

## The startup banner

The banner is plain 7-bit ASCII, so it renders identically in cmd.exe, PowerShell
and any other terminal. It shows the state that is otherwise invisible until a
tool call fails or an approval card does not appear:

- the provider and the model,
- the sandbox directory (`workdir`),
- whether a key is loaded, and from where (`env` or the sealed store),
- the approval posture (whether auto-approve is on),
- the limits (steps, idle timeout, run timeout, shell timeout),
- where `config.json` lives,
- and finally `dsagent listening on <url>` - the line to scrape for the port
  actually bound.

## Your first task

1. Open the printed URL.
2. Settings (`Ctrl+,`) > Connection: pick a model and paste the key. The provider
   switch is not in the UI yet; that is a `config.json` edit - see
   [Providers](Providers).
3. Check the composer's mode switch is on **Agent** if you want the task to be
   able to change files. See [Modes](Modes).
4. Type the task. Watch the activity panel on the right.
5. A mutating call raises an approval card. Read it - it names the tool, its
   arguments and, for shell commands, why the risk layer flagged it. Approve,
   reject, or use **Always Allow** for the rest of the run.
6. The task ends with the `done` tool, which prints "Task finished" and the
   agent's summary.

If the model answers without calling `done`, the run is over but nothing was
declared finished; that is a hint that it stopped rather than completed.

## Where state lives

| state | where |
|---|---|
| `config.json`, `api_key_<family>.bin` | OS config directory (see [Configuration](Configuration)) |
| transcripts, UI preferences, setup checklist | this browser's `localStorage` |
| the workspace | `workdir`, the sandbox root for every tool |
| MCP servers | child processes of dsagent, alive only while it is |

Settings > Data reports what the browser storage costs and clears it. The
workspace, the API key and the server config are untouched by that.
