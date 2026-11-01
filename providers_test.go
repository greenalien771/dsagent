package main

import (
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"
)

// The registry is what the Settings pane, the banner and the vision checks all
// read, so its shape is worth pinning: a provider with no endpoint, no key
// variable or no default model would fail at the first request rather than at
// startup.
func TestProvidersAreWellFormed(t *testing.T) {
	if len(providers) == 0 {
		t.Fatal("no providers registered")
	}
	seenID := map[providerID]bool{}
	for _, p := range providers {
		if p.ID == "" || p.Label == "" || p.Vendor == "" {
			t.Errorf("provider %+v has no id, label or vendor", p.ID)
		}
		if seenID[p.ID] {
			t.Errorf("duplicate provider id %q", p.ID)
		}
		seenID[p.ID] = true

		u, err := url.Parse(p.ChatURL)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			t.Errorf("%s: endpoint %q is not an https URL", p.ID, p.ChatURL)
		}
		if !strings.HasSuffix(u.Path, "/chat/completions") {
			t.Errorf("%s: endpoint %q is not a chat-completions path", p.ID, p.ChatURL)
		}
		if p.KeyEnv == "" || p.KeyID == "" {
			t.Errorf("%s: needs a key env var and a key file id", p.ID)
		}
		if p.Model == "" {
			t.Errorf("%s: needs a default model", p.ID)
		}
		if len(p.Models) == 0 {
			t.Errorf("%s: needs model suggestions", p.ID)
		}
		if p.Note == "" {
			t.Errorf("%s: needs the one line the settings pane shows", p.ID)
		}
	}
	if !seenID[defaultProviderID] {
		t.Errorf("the default provider %q is not in the registry", defaultProviderID)
	}
}

// An unknown or empty value must resolve to the default rather than leave the
// agent with no endpoint at all; a Settings save, on the other hand, is refused.
func TestProviderLookup(t *testing.T) {
	for _, in := range []string{"", "  ", "openai", "DeepSeek-typo"} {
		if got := providerFor(in); got.ID != defaultProviderID {
			t.Errorf("providerFor(%q) = %q, want the default %q", in, got.ID, defaultProviderID)
		}
	}
	if got := providerFor(" ZAI "); got.ID != providerZAI {
		t.Errorf("providerFor(%q) = %q, want %q", " ZAI ", got.ID, providerZAI)
	}
	if _, ok := providerByID("openai"); ok {
		t.Error("providerByID accepted a provider that is not registered")
	}
	if _, ok := providerByID("qwen"); !ok {
		t.Error("providerByID rejected a registered provider")
	}
}

// Every provider's key variable has to be stripped from child processes, which
// is what tools.go builds secretEnvVars from.
func TestEveryProviderKeyIsScrubbedFromChildEnvironments(t *testing.T) {
	for _, name := range providerKeyEnvVars() {
		if !secretEnvVars[name] {
			t.Errorf("%s would be inherited by a tool-launched process", name)
		}
	}
	for _, p := range providers {
		if !secretEnvVars[p.KeyEnv] {
			t.Errorf("%s's variable %s is not scrubbed", p.ID, p.KeyEnv)
		}
	}
}

// One vendor's key must never end up on another vendor's endpoint: storing a
// key per provider is the whole reason the sealed file is namespaced.
func TestProviderKeysAreIsolated(t *testing.T) {
	withTempConfig(t)
	for _, p := range providers {
		t.Setenv(p.KeyEnv, "")
	}

	c := loadConfig()
	deepKey := "sk-deepseek-0123456789abcdef"
	if err := applyPatch(c, configPatch{APIKey: &deepKey}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(secretPath(keyIDDeepSeek)); err != nil {
		t.Fatalf("deepseek key was not sealed: %v", err)
	}

	qwen := string(providerQwen)
	if err := applyPatch(c, configPatch{Provider: &qwen}); err != nil {
		t.Fatal(err)
	}
	if c.APIKey != "" || c.KeySource != "" {
		t.Fatalf("switching provider kept a key for the new endpoint: source=%q", c.KeySource)
	}
	if _, err := os.Stat(secretPath(keyIDQwen)); !os.IsNotExist(err) {
		t.Fatal("switching provider invented a key file for the new provider")
	}

	qwenKey := "sk-qwen-0123456789abcdef"
	if err := applyPatch(c, configPatch{APIKey: &qwenKey}); err != nil {
		t.Fatal(err)
	}
	if got := loadConfig().APIKey; got != qwenKey {
		t.Fatalf("reloaded key = %q, want the Qwen key", got)
	}

	back := string(providerDeepSeek)
	if err := applyPatch(c, configPatch{Provider: &back}); err != nil {
		t.Fatal(err)
	}
	if c.APIKey != deepKey {
		t.Fatal("switching back did not restore the DeepSeek key")
	}

	// The Qwen key survives untouched for the next switch.
	again := loadConfig()
	if again.APIKey != deepKey {
		t.Fatalf("reloaded = %q, want the DeepSeek key", again.APIKey)
	}
	if err := applyPatch(again, configPatch{Provider: &qwen}); err != nil {
		t.Fatal(err)
	}
	if again.APIKey != qwenKey {
		t.Fatalf("the Qwen key was lost by the round trip: %q", again.APIKey)
	}
}

// A model name and an endpoint override belong to one vendor, so a switch drops
// both rather than sending z.ai's endpoint a DeepSeek model name.
func TestProviderSwitchResetsModelAndEndpoint(t *testing.T) {
	withTempConfig(t)
	for _, p := range providers {
		t.Setenv(p.KeyEnv, "")
	}

	c := loadConfig()
	custom := "https://gateway.example.test/v1/chat/completions"
	if err := applyPatch(c, configPatch{BaseURL: &custom}); err != nil {
		t.Fatal(err)
	}
	if got := c.chatEndpoint(); got != custom {
		t.Fatalf("chatEndpoint() = %q, want the override", got)
	}

	zai := string(providerZAI)
	if err := applyPatch(c, configPatch{Provider: &zai}); err != nil {
		t.Fatal(err)
	}
	if c.BaseURL != "" {
		t.Fatalf("the endpoint override survived a provider switch: %q", c.BaseURL)
	}
	if want := providerFor(zai).ChatURL; c.chatEndpoint() != want {
		t.Fatalf("chatEndpoint() = %q, want the z.ai endpoint %q", c.chatEndpoint(), want)
	}
	if want := providerFor(zai).Model; c.Model != want {
		t.Fatalf("model = %q, want the new provider's default %q", c.Model, want)
	}

	// A switch that names the model keeps it: the caller said what it wants.
	model := "glm-5.3-flash"
	back := string(providerDeepSeek)
	if err := applyPatch(c, configPatch{Provider: &back, Model: &model}); err != nil {
		t.Fatal(err)
	}
	if c.Model != model {
		t.Fatalf("model = %q, want the one the patch named", c.Model)
	}

	// A typo is refused, and nothing moves.
	typo := "z.ai"
	if err := applyPatch(c, configPatch{Provider: &typo}); err == nil {
		t.Fatal("an unknown provider was accepted")
	}
	if c.Provider != back {
		t.Fatalf("a refused provider left the config on %q", c.Provider)
	}
}

// The browser gets the whole registry, so the dropdown is built from one source
// of truth - and still no key material.
func TestConfigViewCarriesProvidersButNotTheKey(t *testing.T) {
	withTempConfig(t)
	for _, p := range providers {
		t.Setenv(p.KeyEnv, "")
	}

	c := loadConfig()
	secret := "sk-test-0123456789abcdef0123"
	if err := applyPatch(c, configPatch{APIKey: &secret}); err != nil {
		t.Fatal(err)
	}

	b, err := json.Marshal(viewOf(c))
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	if strings.Contains(out, secret) {
		t.Fatalf("the view carried the key: %s", out)
	}
	for _, p := range providers {
		if !strings.Contains(out, string(p.ID)) {
			t.Errorf("the view does not offer %q to the Settings pane", p.ID)
		}
	}
	if !strings.Contains(out, `"key_env":"DEEPSEEK_API_KEY"`) {
		t.Errorf("the view does not name the key variable: %s", out)
	}
	if !strings.Contains(out, `"provider_url":"https://api.deepseek.com/chat/completions"`) {
		t.Errorf("the view does not name the endpoint: %s", out)
	}
}
