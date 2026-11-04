# Providers

Every endpoint speaks the same OpenAI Chat Completions shape, so a provider is a
URL plus the vendor details a user has to know. The table lives in
`providers.go` and is the single source of truth: the launch banner, the model
suggestions in Settings, the key-storage file name and the image-capability check
all read it.

| `provider` | endpoint | key variable | default model |
|---|---|---|---|
| `deepseek` (default) | `https://api.deepseek.com/chat/completions` | `DEEPSEEK_API_KEY` | `deepseek-flash` |
| `qwen` | `https://dashscope-intl.aliyuncs.com/compatible-mode/v1/chat/completions` | `DASHSCOPE_API_KEY` | `qwen-plus` |
| `zai` | `https://api.z.ai/api/paas/v4/chat/completions` | `ZAI_API_KEY` | `glm-5.3` |
| `zai-coding` | `https://api.z.ai/api/coding/paas/v4/chat/completions` | `ZAI_API_KEY` | `glm-5.3` |

## Choosing one

`provider` is a `config.json` key; the switch is not in the UI yet, so a hand edit
is how you change it today. An unknown value falls back to DeepSeek rather than
failing to start.

Settings are provider-scoped. Switching provider:

- drops the `base_url` override,
- loads that provider's own key from its own `api_key_<family>.bin`,
- and, unless you name another, sets that provider's default model.

The reason is that a vendor rejects another vendor's model name with a 400, and
one vendor's key on another vendor's endpoint is worse than that.

## Model names

The model field is free text, with suggestions. Vendors ship new names faster than
this binary does, so an unlisted name is still sent.

| provider | suggestions |
|---|---|
| DeepSeek | `deepseek-flash`, `deepseek-v4-pro` |
| Qwen | `qwen-plus`, `qwen-max`, `qwen3-coder-plus`, `qwen3-vl-plus`, `qwen-vl-max` |
| z.ai | `glm-5.3`, `glm-5.3-flash`, `glm-5.3-flashx`, `glm-5.2`, `glm-4.7`, `glm-4.6v` |

## `base_url`

`base_url` replaces the provider's own endpoint with any full chat-completions
URL: the other DashScope region, a self-hosted gateway, a proxy. It is validated
before it is saved, and a bad value is refused with a message instead of stored.

Note what it means: setting it sends *this provider's key* wherever it points,
which is why Settings keeps it as an advanced field.

The mainland-China DashScope region is
`https://dashscope.aliyuncs.com/compatible-mode/v1/chat/completions`.

## Key families

`zai` and `zai-coding` share one key family (`zai`), so switching between z.ai's
standard and coding-plan endpoints does not ask for the key twice. The coding-plan
entry exists because z.ai asks coding tools to use that path when the key belongs
to a GLM Coding Plan subscription instead of pay-per-token billing. Plan keys and
standard API keys are issued separately.

## Which models see images

There is no API to ask, so this is a per-provider rule over model names. An image
attached to a task on a text-only model is **refused up front**, with a message
naming the model and the setting to change, rather than failing mid-run with a 400
that reads like a broken app.

| provider | rule |
|---|---|
| DeepSeek | text-only if the name matches `v4-pro` or `reasoner`; everything else, including unknown names, is allowed through |
| Qwen | vision if the name contains `vl` or `omni` (e.g. `qwen3-vl-plus`, `qwen-omni-*`) |
| z.ai | vision if the name matches `glm-<n>v` (e.g. `glm-4.6v`) or is `glm-5.3-flash`; `glm-5.3` itself is text-only |
| unknown provider | never - no picture is sent to an endpoint this build knows nothing about |

The vendor's own error is the final word: an unknown DeepSeek name is let through
on the assumption that the API's 400 is a better answer than a stale list.

See [Desktop and images](Desktop-and-Images) for what happens to an attached
image after that check.

## Adding a provider

Add an entry to the `providers` slice in `providers.go`. The rest follows from it:
the Settings dropdown, the banner, the model suggestions, the key file name, and
the vision rule if the vendor needs a new one. If the vendor's key variable is
new, add it to the same entry - `providerKeyEnvVars()` derives the strip-list that
keeps it out of `run_shell` child processes and out of MCP server environments.
