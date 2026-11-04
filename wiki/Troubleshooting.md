# Troubleshooting

## Start-up

| symptom | cause | fix |
|---|---|---|
| the URL in the browser is not `8787` | the port was taken, so a free one was used | read the banner's last line for the port actually bound. A different port means a fresh transcript - `localStorage` is per origin |
| the old transcript is gone after a restart on another port | same cause | free `8787` and restart, or accept the newer origin |
| `go build -o dsagent.exe .` fails with access-denied | the running executable is locked by Windows | stop dsagent, or `go build -o "%TEMP%\check.exe" .` to prove the build |
| banner says no key | no `DEEPSEEK_API_KEY`/`DASHSCOPE_API_KEY`/`ZAI_API_KEY` and nothing stored | paste a key in Settings > Connection |
| banner says a stored key could not be loaded | the sealed file was written by another Windows account, is truncated, or is a format this build does not know | the message quotes the reason; paste a new key, or set the environment variable |
| a hand-edited `config.json` did nothing | it is read at start | restart, or use the **Reload servers** button for the MCP list only |

## Runs

| symptom | cause | fix |
|---|---|---|
| `Agent is busy (running for ...)` | one task at a time; this one is still going | stop it, then send the task again |
| the run just stops, with no "Task finished" | the model answered without calling `done` | that is a stop, not a completion - read the last message |
| `Step limit reached. Type 'continue' to keep going.` | `max_steps` (25 by default) | type "continue", or raise the budget in Settings > Agent (up to 200) |
| `watchdog: no activity for ...` | nothing reached the browser for `idle_timeout_sec` | raise `idle_timeout_sec` in Settings > Agent if the task is genuinely slow |
| `watchdog: this task exceeded the ... limit` | wall clock over `run_timeout_min` | raise it, or split the task |
| a shell command was killed | `shell_timeout_sec` (60s) | the result names the setting; raise it up to 3600 |
| the same tool call is refused on the fourth try | the repeat-call ledger | the command is failing for a reason; change the approach rather than the retry count |
| `Could not reach ... Retrying, attempt N of 3` | a network or 5xx failure from the provider | it retries three times, then fails; check the endpoint |

## Approvals

| symptom | cause |
|---|---|
| every shell command asks, even with auto-approve on | auto-approve is **Agent mode only**; Plan and Ask ignore it |
| a dangerous command asks even with Always Allow on | by design - dangerous calls are never covered, and need the word `allow` typed into the card |
| `close` asks and Always Allow will not answer | closing a window is not recoverable by re-running it |
| `list_windows` asks | the titles it returns are the names of what you have open |
| a `web_fetch` asks | the URL resolves to this machine or the local network |
| a call is refused outright, no card | it targets an MCP server set to `block` |

## Files

| symptom | cause |
|---|---|
| `path escapes working dir` | the path resolved outside `workdir`, often through a symlink or junction; the message names the target |
| `cannot resolve <path>` | the deepest existing ancestor could not be resolved for a write target |
| `edit_file` refuses a match | `old` appears more than once; the refusal names the lines. Include the line above or below, or pass `"all": true` |
| `edit_file` says it matched after ignoring whitespace | the text is there but indented differently - copy it out of the file again |
| a write into `.git`, `.ssh` or a credentials file asks twice as hard | path classification: those are `dangerous`, not merely gated |

## Images

| symptom | cause |
|---|---|
| "the model X does not accept images" | the model is text-only for its provider - see [Providers](Providers#which-models-see-images) |
| "N images attached; at most 8 are accepted per task" | per-task image limit |
| an older image in the conversation became a line of text | the inline budget replaced the oldest payloads instead of resending megabytes every step |

## MCP servers

| symptom | cause | fix |
|---|---|---|
| a server is not in the tool list | it failed to start, or it is `disabled` | Settings > MCP shows the state; a bad entry in a hand-edited file is dropped with a note on stderr |
| a configured server's tools are refused | its `approval` is `block`, or the mode is Plan/Ask - MCP tools are Agent-only | change the policy, or switch mode |
| a call hangs for ten minutes | no `timeout_sec` on that server | set one; the built-in cap is ten minutes |
| the wrong tool is picked from a big server | forty tools compete for attention | set `tools` to an allow-list of the ones that matter |
| a server that died is still listed | the process ended | `POST /api/mcp` or **Reload servers**, or `mcp_control` with `restart` |
| a rebuilt server binary is not being used | the old process still holds the executable | `mcp_control` with `restart` |

## Windows desktop tools

| symptom | cause |
|---|---|
| `list_windows`, `window_control` or `screenshot` is not in the tool list | not on Windows - they are Win32 through PowerShell and are dropped elsewhere |
| a window "closed" is still open | `close` posts `WM_CLOSE`; a window with unsaved work can refuse. Check with `list_windows` |
| the tool refuses a particular window | it is the console hosting dsagent, refused by pid |
| a capture shows the wrong thing | pass `window` to capture a single window even when minimised, or crop with `x`/`y`/`width`/`height` |
