package main

import (
	"regexp"
	"strings"
)

// Providers.
//
// Every endpoint this agent can talk to speaks the OpenAI Chat Completions
// shape that agent.go already emits, so a provider is mostly a URL plus the
// vendor-specific details a user has to know: which environment variable can
// hold the key, which model names are worth suggesting, and which of those
// models can see an image.
//
// The registry is the single source of truth for all of that. The Settings pane
// is built from it (configView carries the list), the launch banner reads it,
// and vision checks ask it rather than pattern-matching a DeepSeek name in
// three places.
type providerID string

const (
	providerDeepSeek  providerID = "deepseek"
	providerQwen      providerID = "qwen"
	providerZAI       providerID = "zai"
	providerZAICoding providerID = "zai-coding"
)

// Key families. Two entries that bill the same key share one, so switching
// between z.ai's standard and coding-plan endpoints does not ask for the key
// twice.
const (
	keyIDDeepSeek = "deepseek"
	keyIDQwen     = "qwen"
	keyIDZAI      = "zai"
)

// provider is one choice in Settings > Connection.
type provider struct {
	ID     providerID `json:"id"`
	Label  string     `json:"label"`
	Vendor string     `json:"vendor"`
	// ChatURL is the full chat-completions endpoint. All four are
	// OpenAI-compatible, which is what lets one request builder serve them.
	ChatURL string `json:"-"`
	// KeyEnv is the environment variable that overrides the stored key for this
	// provider. It also names the variable the banner and error text point at.
	KeyEnv string `json:"key_env"`
	// KeyID names the sealed key file (api_key_<KeyID>.bin).
	KeyID string `json:"-"`
	// Model is what the model field is set to when this provider is chosen; a
	// vendor rejects another vendor's model name with a 400.
	Model string `json:"model"`
	// Models are suggestions for the model field, not a closed list: the field
	// stays free text because vendors ship new names faster than this binary.
	Models []string `json:"models"`
	// Note is the one line of vendor-specific advice the pane shows.
	Note string `json:"note"`
}

// providers is in the order the Settings dropdown offers them.
//
// Endpoints verified 2026-08: DeepSeek (unchanged), z.ai standard
// /api/paas/v4 and its GLM Coding Plan twin /api/coding/paas/v4 - both answered
// 401 "authentication parameter not received" to an anonymous POST, which is
// how an endpoint proves it exists without a key. The DashScope entries could
// not be reached from the machine this was written on (TLS reset), so base_url
// is exposed in Settings: a wrong region is a field to fix, not a rebuild.
var providers = []provider{
	{
		ID:      providerDeepSeek,
		Label:   "DeepSeek",
		Vendor:  "DeepSeek",
		ChatURL: "https://api.deepseek.com/chat/completions",
		KeyEnv:  "DEEPSEEK_API_KEY",
		KeyID:   keyIDDeepSeek,
		Model:   "deepseek-flash",
		Models:  []string{"deepseek-flash", "deepseek-v4-pro"},
		Note:    "deepseek-flash sees images; deepseek-v4-pro does not.",
	},
	{
		ID:      providerQwen,
		Label:   "Qwen (DashScope)",
		Vendor:  "Alibaba Cloud Model Studio",
		ChatURL: "https://dashscope-intl.aliyuncs.com/compatible-mode/v1/chat/completions",
		KeyEnv:  "DASHSCOPE_API_KEY",
		KeyID:   keyIDQwen,
		Model:   "qwen-plus",
		Models:  []string{"qwen-plus", "qwen-max", "qwen3-coder-plus", "qwen3-vl-plus", "qwen-vl-max"},
		Note: "OpenAI-compatible DashScope. For the mainland-China region set " +
			"the endpoint to https://dashscope.aliyuncs.com/compatible-mode/v1/chat/completions. " +
			"Only the VL/omni models see images.",
	},
	{
		ID:      providerZAI,
		Label:   "Z.ai (GLM)",
		Vendor:  "Z.ai",
		ChatURL: "https://api.z.ai/api/paas/v4/chat/completions",
		KeyEnv:  "ZAI_API_KEY",
		KeyID:   keyIDZAI,
		Model:   "glm-5.3",
		Models:  []string{"glm-5.3", "glm-5.3-flash", "glm-5.3-flashx", "glm-5.2", "glm-4.7", "glm-4.6v"},
		Note: "Standard API key from z.ai/manage-apikey. glm-5.3 is text-only; " +
			"glm-5.3-flash and the -v models see images.",
	},
	{
		// Same models and same key as above, but billed against a GLM Coding
		// Plan subscription instead of pay-per-token. z.ai asks coding tools to
		// use this path, so it is a choice rather than something to configure.
		ID:      providerZAICoding,
		Label:   "Z.ai (GLM Coding Plan)",
		Vendor:  "Z.ai",
		ChatURL: "https://api.z.ai/api/coding/paas/v4/chat/completions",
		KeyEnv:  "ZAI_API_KEY",
		KeyID:   keyIDZAI,
		Model:   "glm-5.3",
		Models:  []string{"glm-5.3", "glm-5.3-flash", "glm-5.3-flashx"},
		Note: "Dedicated endpoint for a GLM Coding Plan subscription. Plan keys " +
			"and standard API keys are issued separately - use the plan key here.",
	},
}

// defaultProviderID is what an empty or unknown config resolves to. It is also
// what every build before providers existed was talking to.
const defaultProviderID = providerDeepSeek

// providerFor resolves a config value to a registry entry, falling back to the
// default for an empty or hand-edited-unknown value rather than failing: an
// unusable config file must not stop the agent from starting.
func providerFor(id string) provider {
	want := strings.TrimSpace(strings.ToLower(id))
	for _, p := range providers {
		if string(p.ID) == want {
			return p
		}
	}
	return providerFor(string(defaultProviderID))
}

// providerByID is the strict lookup: it reports whether the id is real, which
// is what a Settings save needs so a typo is refused instead of silently
// becoming DeepSeek.
func providerByID(id string) (provider, bool) {
	want := strings.TrimSpace(strings.ToLower(id))
	for _, p := range providers {
		if string(p.ID) == want {
			return p, true
		}
	}
	return provider{}, false
}

// providerIDs lists the valid ids for an error message.
func providerIDs() []string {
	out := make([]string, 0, len(providers))
	for _, p := range providers {
		out = append(out, string(p.ID))
	}
	return out
}

// providerKeyEnvVars is every environment variable that can hold a key for any
// provider. tools.go strips these from child processes.
func providerKeyEnvVars() []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(providers))
	for _, p := range providers {
		if seen[p.KeyEnv] {
			continue
		}
		seen[p.KeyEnv] = true
		out = append(out, p.KeyEnv)
	}
	return out
}

// Vision support by provider.
//
// These are deny/allow rules over model names, not an API call: the vendor's own
// error is the final word, and refusing a picture here only exists so a
// text-only model does not spend a run answering "I cannot see an image" in the
// middle of a task.
var (
	// DeepSeek: the Pro and reasoner families are text-only; a Flash revision,
	// including a retired alias, has vision. An unknown DeepSeek name is
	// allowed through - the API's 400 is a better answer than a stale list.
	deepSeekTextOnly = regexp.MustCompile(`v4-pro|reasoner`)

	// z.ai: the "V" suffix marks a vision model (glm-4.6v, glm-4.5v,
	// glm-4.6v-flash) and GLM-5.3-Flash is natively multimodal. GLM-5.3 itself
	// and the plain 4.x/4.5-Flash/4.7-Flash text models are not.
	glmVision = regexp.MustCompile(`glm-\d+(?:\.\d+)?v(?:-|$)|glm-5\.3-flash`)
)

// modelSeesImages reports whether this provider's model accepts image input.
func modelSeesImages(p providerID, model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return false
	}
	switch p {
	case providerQwen:
		// DashScope names vision models qwen-vl-max, qwen2.5-vl-*, qwen3-vl-plus
		// and qwen-omni-*; the rest of the family is text-only.
		return strings.Contains(m, "vl") || strings.Contains(m, "omni")
	case providerZAI, providerZAICoding:
		return glmVision.MatchString(m)
	case providerDeepSeek:
		return !deepSeekTextOnly.MatchString(m)
	}
	// An unknown provider is not guessed at: no picture is sent to an endpoint
	// this build knows nothing about.
	return false
}
