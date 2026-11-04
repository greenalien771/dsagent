# Safety and approvals

The approval card is the security boundary. Not the prompt, not the tool list, not
the risk classifier - the human reading the card. Everything else in this page
exists to make sure the right card appears, that it says enough to decide, and
that the answer cannot be skipped by accident.

**What it is not:** a sandbox against a hostile model. Once you approve a command,
that command runs with your privileges.

## How a mutating call flows

1. The model emits a tool call.
2. If it uses an MCP server set to `block`, it is refused before anything else.
3. The call is classified: `safe`, `elevated` or `dangerous`.
4. `run_shell` raises the Windows notification here, if one is configured.
5. The agent decides whether a card is needed:
   - the tool always asks (`run_shell`, `write_file`, `edit_file`, `screenshot`,
     `window_control`, any MCP tool), **or**
   - the tool asks on risk (`web_fetch`, `list_windows`, `mcp_control`) and the
     classifier is unhappy with this particular call.
6. The card is emitted with the tool, its arguments, the level and the reasons.
7. The run is parked until the browser posts an approval, a denial, or the
   watchdog cancels.
8. A denial returns to the model as `DENIED by user. Do not retry this exact
   call; ask or choose another approach.`

## Which calls a card covers

| tool | when it asks |
|---|---|
| `run_shell` | always |
| `write_file`, `edit_file` | always |
| `window_control` | always |
| `screenshot` | always in Plan and Ask; in Agent it follows auto-approve, because it uploads your desktop |
| `mcp__<server>__<tool>` | always, unless that server's `approval` is `allow` (Agent mode only) |
| `web_fetch` | when the URL is flagged - one that resolves to this machine or the local network |
| `list_windows` | in practice always: the listing is classified `elevated` because the titles it returns - document names, URLs, message previews - are sent to the API |
| `mcp_control` | when flagged: `status` is free, `stop` and `start` run or end a third-party process |
| `close` on a window | always, and it is the one call **Always Allow can never answer** - closing a window can throw away unsaved work, and unlike every other elevated call there is no "run it again" that brings the work back |

## Risk classes

`classifyTool` answers one question per call: how bad would it be if this ran
unattended?

| level | meaning | effect |
|---|---|---|
| `safe` | no side effect worth a prompt | runs for the tools that only ask on risk |
| `elevated` | changes state, but recoverable | prompt for the risk-gated tools; covered by Always Allow in Agent mode |
| `dangerous` | irreversible, privilege-related or credential-touching | **never** covered by auto-approve or Always Allow; requires the confirmation word |

Shell commands are matched against a rule table that is deliberately weighted
toward false positives: an extra prompt costs a click, a missed rule can cost the
user their working tree. The categories:

- **irreversible deletion** - `rm -r`/`-f`, `rmdir /s`, `del /f /s /q`, a delete
  with a wildcard, `Remove-Item -Recurse`, `shred`, `wipefs`
- **disks, partitions, boot records** - `mkfs`, `diskpart`, `fdisk`, `format X:`,
  `dd of=`, `vssadmin`, `cipher /w`
- **host power and session** - `shutdown`, `reboot`, `logoff`,
  `Stop-Computer`/`Restart-Computer`, `init 0/6`
- **registry, services, accounts** - `reg add/delete/import`,
  `sc create/delete/config`, `net user`, `schtasks /create`, `netsh`
- **privilege escalation** - `sudo`, `doas`, `runas`, `icacls /grant`,
  `chmod -R`, `chmod 777`, `chmod +s`
- **remote code execution and obfuscation** - a download piped into a shell,
  `| iex`, `powershell -enc`, `certutil -urlcache`, base64-decode piped to a shell,
  a fork bomb
- **version control damage** - `git push --force`, `git reset --hard`,
  `git clean -xdf`, `filter-branch`, `reflog expire`
- **credentials and persistence** - anything touching `.ssh`, `id_rsa`,
  `.aws/credentials`, `.netrc`, `.git-credentials`, plus `crontab`,
  `systemctl enable`, `launchctl`, and any redirect into a system path
- **elevated, not dangerous** - `taskkill`, `attrib -r`, package managers
  installing or removing, `docker run/rm/prune`, `git push/clone`,
  `curl`/`wget` on their own, `ssh`/`scp`, `mv`/`ren`, deleting files, `kill`,
  and a bare `env` / `printenv` / `set` (an environment dump)

Paths are classified too, and the same rules cover `write_file` and `edit_file`:

| path | level | why |
|---|---|---|
| inside `.git` | dangerous | writes inside `.git` (hooks, config or history) |
| `.ssh`, `id_rsa`, `id_ed25519`, `.env`, `.aws/credentials`, `.netrc`, `.git-credentials` | dangerous | writes a credentials file |
| anything containing `api_key` | dangerous | targets the stored API key |
| `package.json`, `go.mod`, `.vscode/`, `makefile` | elevated | edits a project manifest |

## The confirmation word

A dangerous call sets `confirm` on the card to the word **`allow`**. The browser
must post that word back with the approval, and the server verifies it. "Click
allow" alone is never enough for the genuinely destructive stuff, and the check is
server-side, so a crafted request that skips the UI does not help either.

## Auto-approve

`auto_approve` (Settings > Safety) skips the card for calls that would otherwise
ask. Its limits are the point:

- **Agent mode only.** Plan and Ask ignore it entirely.
- **Never for a dangerous call.**
- **Never for a call that cannot be undone by re-running it** - window `close`.
- Skipping is announced in the transcript each time, so it is never silent.

## Sandbox and isolation

- **Path sandbox.** Every tool path is joined against `workdir`, then resolved
  through symlinks and NTFS junctions and checked again, because a link inside the
  workspace points somewhere else while the name still looks contained. Windows
  paths are compared case-insensitively (`filepath.Rel`), since `c:\Users` and
  `C:\Users` are one folder. An escape is refused with the resolved target named.
- **No credential inheritance.** `run_shell` children get the environment minus
  every provider key variable, so a bare `set` / `env` / `printenv` cannot print a
  key into the transcript. MCP servers get the same treatment.
- **Redaction on the way out.** Model output, tool output and error strings are
  scrubbed for the live keys and for anything shaped like one before they reach
  the browser or the remote API.
- **Fetch policy.** `web_fetch` refuses targets that resolve to this machine or the
  local network, re-checks every redirect hop and every resolved IP, and uses no
  proxy.
- **One endpoint.** A run posts to the configured provider's URL, or the
  `base_url` in `config.json`. No tool call can redirect that, and the endpoint is
  validated before it is saved.
- **Loopback only.** The server binds `127.0.0.1` and accepts only the host
  authorities it created. Requests with a foreign `Origin`/`Host` - the shape of a
  DNS-rebinding attack from a hostile web page - are rejected, and responses carry
  a strict CSP.

## Watchdog

A run that produces no output for `idle_timeout_sec` (120s) or exceeds
`run_timeout_min` (25 minutes) of wall clock is cancelled, and the transcript says
which limit fired. The limits are snapshotted when a run starts, so a save mid-run
does not move them under a task already in flight. A deliberate `wait` reports as
activity, so a pause is not mistaken for a stall.

Stop and force-abort are both available in the UI: stop asks the run to end,
force-abort releases it and bumps the run token so a goroutine that wakes up late
cannot clear a newer run's state.

## Notifications

Every `run_shell` call can raise a Windows toast **from dsagent itself**, not from
the page, so it arrives while the window is behind something else or minimised.
Two switches in Settings > Safety pick what is announced - any command
(`notify_any`), or only the ones the risk layer calls dangerous
(`notify_danger`, the default) with the reason it flagged them. Both live in
`config.json` because the process that shows the toast is the server. Only the
command line and the reasons are shown, never the output.

## MCP servers

An MCP server is third-party code running as a child process with your
privileges, outside the path sandbox and outside anything this agent can classify.
It is treated accordingly: Agent mode only, every call stopped at a card unless
that server's policy says otherwise, provider keys scrubbed from its environment,
and nothing started that you did not name. See [MCP servers](MCP-Servers).
