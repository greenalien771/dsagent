# Modes

A mode is a posture, chosen per task with the composer's switch: what the agent is
allowed to do, not how hard it thinks. The mode is remembered by the server, so a
later "continue" keeps the same restrictions.

## The three modes

| mode | sees | can do |
|---|---|---|
| **Agent** | all tools | everything, each mutating call behind an approval card |
| **Plan** | every tool but the mutating ones | reads and investigates; `run_shell` always prompts, `write_file`, `edit_file`, `window_control` and `mcp_control` are not offered |
| **Ask** | read-only tools | answers questions, changes nothing; `list_windows` still asks before it lists what you have open |

Two rules hold in every mode:

- **Auto-approve is ignored outside Agent.** It cannot be switched on to loosen
  Plan or Ask.
- **A dangerous call always prompts.** It is never covered by auto-approve and
  needs the confirmation word - see [Safety and approvals](Safety-and-Approvals).

An unknown or missing `mode` in a request normalises to `agent`.

## Task effort

The complaint `effort` answers: a one-line instruction - "shoot that enemy", "open
this page", "where is X" - should not cost five steps of surveying. It is a
setting because the right answer depends on what you are doing with the agent
today.

| value | what the prompt tells the model |
|---|---|
| `direct` | The instruction is an action: make the call on the first step, do not survey, do not plan out loud. Verification of a change to your own code still happens - writing files and running commands still end with the command that proves the result. A task that turns out to be genuinely complex switches to the thorough approach and says so. |
| `balanced` (default) | Match the work to the request: a plain action is one call, a change to your code is looked at, made and verified. A task that is a conversation (a game move, a choice) uses `ask_user` and keeps state instead of re-deriving what you already said. |
| `thorough` | Read the surrounding code, state the plan, change, verify, report the evidence - even when the change looks small. |

Settings > Agent switches it, and it applies to the next task immediately.

**Effort changes the system prompt and nothing else.** The same tools, the same
limits, the same approval cards and the same risk classifier apply at every level.
"Be quick" must never be a way to make a destructive call easier to reach.

## Effort is not the only lever

A short interactive task that needs an answer does not have to cost a new run:

| tool | what it does |
|---|---|
| `ask_user` | Asks a question and waits. The run pauses - it does not finish - and the answer arrives as the tool result. Suggested options are drawn as buttons; the field is still there. It never asks for approval, because there is nothing to approve. |

The distinction that matters for a model: ending the run to ask a question throws
away the transcript's thread and the task's context; `ask_user` keeps both.
