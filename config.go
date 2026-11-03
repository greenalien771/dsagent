package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// redacted is what the UI/transcript sees in place of a secret.
const redacted = "sk-[REDACTED]"

// Config holds runtime configuration.
//
// APIKey is deliberately tagged `json:"-"`: it can never be marshalled into
// config.json (no plaintext on disk) and it can never be marshalled into an
// /api/config response (no leak to the browser, so an XSS, a stray extension or
// a local page cannot steal it). The key lives in memory only and, when
// persisted, is sealed by the OS key store (DPAPI on Windows) in a separate
// file that only this user account can open.
type Config struct {
	APIKey   string `json:"-"` // secret: memory only
	Model    string `json:"model"`
	WorkDir  string `json:"workdir"`
	MaxSteps int    `json:"max_steps"`

	// Provider picks which vendor endpoint the agent talks to (providers.go).
	// A plain string rather than a providerID: a hand-edited config.json with an
	// unknown value must load as the default instead of failing to unmarshal.
	Provider string `json:"provider"`

	// BaseURL optionally replaces the provider's own endpoint. That is how the
	// other DashScope region, a self-hosted gateway or a proxy is used without a
	// new build; empty means "the URL the provider entry names". Setting it
	// sends this provider's key wherever it points, which is why Settings keeps
	// it as an advanced field.
	BaseURL string `json:"base_url,omitempty"`

	// MCPServers are Model Context Protocol servers this agent may use as extra
	// tool providers (mcp.go). Each entry is a child process the user configured;
	// a server is started only because it is listed here, and this agent never
	// connects to one over the network on its own.
	MCPServers []MCPServerConfig `json:"mcp_servers,omitempty"`

	// AutoApprove skips the per-tool approval prompt (run_shell, write_file).
	// Dangerous: the agent then runs unattended. Set from Safety in Settings.
	AutoApprove bool `json:"auto_approve"`

	// NotifyAny and NotifyDanger are the two command-notification switches in
	// Settings > Safety, and they are one choice: any command, or only the ones
	// the risk layer calls dangerous. They live in the server config rather than
	// in the browser because the notification is raised by this process as a
	// Windows toast - the tab may not even be open, which is the whole point.
	NotifyAny    bool `json:"notify_any"`
	NotifyDanger bool `json:"notify_danger"`

	// IdleTimeoutSec and RunTimeoutMin size the run watchdog: a task that
	// produces no output for IdleTimeoutSec, or that runs longer than
	// RunTimeoutMin of wall clock, is cancelled. They are the numbers the
	// Agent pane shows, so "how long may a task run" is a setting and not a
	// constant in the binary.
	IdleTimeoutSec int `json:"idle_timeout_sec"`
	RunTimeoutMin  int `json:"run_timeout_min"`

	// ShellTimeoutSec is the per-command cap for run_shell. It is deliberately
	// separate from the two watchdog numbers above: those end a whole task,
	// while this one ends a single child process. A build, a test suite or a
	// package install routinely needs longer than a model turn, and the old
	// hardcoded 60s meant the only way to run one was to not use the agent.
	ShellTimeoutSec int `json:"shell_timeout_sec"`

	// KeySource records where the key came from ("env" or "store") so the UI
	// can explain why edits are ignored. Never contains the key itself.
	KeySource string `json:"-"`

	// KeyError explains why a key that exists on disk was not loaded: a sealed
	// file written by another Windows account, a truncated file, a format this
	// build does not know. It is what the banner and the "no key" refusal quote,
	// so the user is told the real reason instead of being asked to paste a key
	// that is already there. Never persisted and never sent to the browser.
	KeyError string `json:"-"`
}

// maxStepLimit matches the max="200" the settings form offers.
const maxStepLimit = 200

// Watchdog defaults and bounds. The bounds exist so no single field in the
// settings form can turn the watchdog off: a run always ends on its own.
const (
	defaultIdleTimeoutSec = 120
	defaultRunTimeoutMin  = 25
	minIdleTimeoutSec     = 10
	maxIdleTimeoutSec     = 1800
	minRunTimeoutMin      = 1
	maxRunTimeoutMin      = 240

	defaultShellTimeoutSec = 60
	minShellTimeoutSec     = 5
	maxShellTimeoutSec     = 3600
)

// clampInt pins a hand-edited or posted value into [lo, hi].
func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

var cfgMu sync.Mutex

func configDir() string {
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		dir = "."
	}
	return filepath.Join(dir, "dsagent")
}

func configPath() string { return filepath.Join(configDir(), "config.json") }

// secretPath is the sealed key for one provider family. The family is part of
// the file name so choosing another provider can never hand the previous
// vendor's key to a different endpoint.
func secretPath(keyID string) string {
	return filepath.Join(configDir(), "api_key_"+keyID+".bin")
}

// legacySecretPath is the single file builds before providers wrote. It always
// belonged to DeepSeek, which was then the only endpoint, so it is read as
// DeepSeek's fallback and removed along with DeepSeek's key.
func legacySecretPath() string { return filepath.Join(configDir(), "api_key.bin") }

// legacyConfig is the shape older builds wrote, back when the key sat in
// plaintext inside config.json.
type legacyConfig struct {
	APIKey *string `json:"api_key"`
}

// defaultConfig is what a first launch runs on, and the shape a corrupt or
// partial file is repaired into.
func defaultConfig() *Config {
	prov := providerFor(string(defaultProviderID))
	return &Config{
		Provider:        string(prov.ID),
		Model:           prov.Model,
		MaxSteps:        25,
		NotifyDanger:    true,
		IdleTimeoutSec:  defaultIdleTimeoutSec,
		RunTimeoutMin:   defaultRunTimeoutMin,
		ShellTimeoutSec: defaultShellTimeoutSec,
	}
}

func loadConfig() *Config {
	c := defaultConfig()

	raw, err := os.ReadFile(configPath())
	if err == nil {
		if err := json.Unmarshal(raw, c); err != nil {
			// A corrupt file must not take the agent down; start clean but keep
			// the unreadable original for forensics.
			os.Rename(configPath(), configPath()+".corrupt")
			c = defaultConfig()
		} else {
			// A config file written before the notification switches existed has
			// neither key. The dangerous-only default belongs to that case and not
			// to a file that says false on purpose, so presence is what is checked
			// here, not the value.
			var notify struct {
				NotifyAny    *bool `json:"notify_any"`
				NotifyDanger *bool `json:"notify_danger"`
			}
			if json.Unmarshal(raw, &notify) == nil && notify.NotifyAny == nil && notify.NotifyDanger == nil {
				c.NotifyDanger = true
			}

			// Migrate a plaintext key out of config.json as soon as we see one.
			var legacy legacyConfig
			if json.Unmarshal(raw, &legacy) == nil && legacy.APIKey != nil {
				if k := strings.TrimSpace(*legacy.APIKey); k != "" {
					if err := c.storeAPIKey(k); err != nil {
						fmt.Fprintf(os.Stderr, "dsagent: could not migrate stored key: %v\n", err)
						c.APIKey = k
					}
				}
				// Rewrite the file without the plaintext field (APIKey is `-`,
				// so marshalling drops it).
				saveConfig(c)
			}
		}
	}

	// The provider decides the model fallback: a vendor rejects another vendor's
	// model name with a 400, so the default has to follow the choice.
	c.Provider = string(providerFor(c.Provider).ID)
	if c.Model == "" {
		c.Model = providerFor(c.Provider).Model
	}
	if c.MaxSteps <= 0 {
		c.MaxSteps = 25
	}
	// A config written before these existed carries zeroes, and a hand-edited
	// file can carry nonsense; both end up on the documented default or inside
	// the documented range.
	if c.IdleTimeoutSec <= 0 {
		c.IdleTimeoutSec = defaultIdleTimeoutSec
	}
	if c.RunTimeoutMin <= 0 {
		c.RunTimeoutMin = defaultRunTimeoutMin
	}
	if c.ShellTimeoutSec <= 0 {
		c.ShellTimeoutSec = defaultShellTimeoutSec
	}
	c.IdleTimeoutSec = clampInt(c.IdleTimeoutSec, minIdleTimeoutSec, maxIdleTimeoutSec)
	c.RunTimeoutMin = clampInt(c.RunTimeoutMin, minRunTimeoutMin, maxRunTimeoutMin)
	c.ShellTimeoutSec = clampInt(c.ShellTimeoutSec, minShellTimeoutSec, maxShellTimeoutSec)

	if c.WorkDir == "" {
		c.WorkDir, _ = os.Getwd()
	}

	// A hand-edited mcp_servers list is validated like one that came from the
	// settings form. An entry that cannot work is dropped with a note on stderr
	// instead of being started on every task and failing there.
	if cleaned, problems := normalizeMCPServers(c.MCPServers); len(problems) > 0 || len(cleaned) != len(c.MCPServers) {
		c.MCPServers = cleaned
		for _, p := range problems {
			fmt.Fprintf(os.Stderr, "dsagent: %s\n", p)
		}
	}

	// Environment wins: nothing has to be written to disk at all.
	c.resolveKeyLocked()
	return c
}

// resolveKeyLocked fills APIKey and KeySource for the configured provider.
//
// Callers hold cfgMu, or are loadConfig before the config is shared. Both
// sources are provider-scoped - this provider's environment variable and this
// provider's sealed file - because the alternative is a DeepSeek key in memory
// being sent to z.ai's endpoint under a model name z.ai has never heard of.
func (c *Config) resolveKeyLocked() {
	prov := providerFor(c.Provider)
	c.APIKey, c.KeySource, c.KeyError = "", "", ""

	if k := strings.TrimSpace(os.Getenv(prov.KeyEnv)); k != "" {
		c.APIKey, c.KeySource = k, "env"
		registerSecret(k)
		return
	}
	k, err := loadStoredKey(prov.KeyID)
	if err == nil && k != "" {
		c.APIKey, c.KeySource = k, "store"
		registerSecret(k)
		return
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		// Remembered on the config as well as printed: stderr goes nowhere when
		// the UI was started from a shortcut, and "MISSING" is not what happened.
		c.KeyError = err.Error()
		fmt.Fprintf(os.Stderr, "dsagent: the stored %s key could not be loaded: %v\n", prov.Label, err)
	}
}

// saveConfig persists everything except the secret. The file is created with
// 0600 and replaced atomically so a crash cannot leave a half-written config.
func saveConfig(c *Config) error {
	if err := os.MkdirAll(configDir(), 0700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(configPath(), b)
}

func writeFileAtomic(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	_ = os.Chmod(name, 0600) // best effort; Windows ACLs already restrict the profile dir
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// storeAPIKey seals the key with the OS keystore and writes it to disk.
// The environment-variable case never reaches this function on purpose.
func (c *Config) storeAPIKey(key string) error {
	if err := os.MkdirAll(configDir(), 0700); err != nil {
		return err
	}
	sealed, err := sealSecret(key)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(secretPath(providerFor(c.Provider).KeyID), sealed); err != nil {
		return err
	}
	c.APIKey = key
	c.KeySource = "store"
	registerSecret(key)
	return nil
}

func loadStoredKey(keyID string) (string, error) {
	b, err := os.ReadFile(secretPath(keyID))
	if errors.Is(err, os.ErrNotExist) && keyID == keyIDDeepSeek {
		// A file from before providers existed: DeepSeek was the only endpoint,
		// so the old single file is DeepSeek's key.
		b, err = os.ReadFile(legacySecretPath())
	}
	if err != nil {
		return "", err
	}
	if len(b) == 0 {
		return "", errors.New("empty secret file")
	}
	return openSecret(b)
}

// clearAPIKey removes the sealed key from disk and from memory.
func clearAPIKey(c *Config) error {
	c.APIKey = ""
	if c.KeySource != "env" {
		c.KeySource = ""
	}
	prov := providerFor(c.Provider)
	paths := []string{secretPath(prov.KeyID)}
	if prov.KeyID == keyIDDeepSeek {
		// Remove the pre-provider file too, or "Remove stored key" would leave a
		// working key on disk that nothing on screen mentions.
		paths = append(paths, legacySecretPath())
	}
	var first error
	for _, p := range paths {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) && first == nil {
			first = err
		}
	}
	return first
}

// ---- request/response shapes -------------------------------------------

// configView is everything the browser is allowed to know. Note the absence of
// any key material: only a boolean and a masked fingerprint ever leave here.
type configView struct {
	Model         string `json:"model"`
	Provider      string `json:"provider"`
	ProviderLabel string `json:"provider_label"`
	// KeyEnv and ProviderURL are what the pane needs to explain itself: which
	// variable can hold the key and which endpoint the provider would use. None
	// of this is secret - the key itself never appears here.
	KeyEnv          string     `json:"key_env"`
	ProviderURL     string     `json:"provider_url"`
	BaseURL         string     `json:"base_url"`
	Models          []string   `json:"models"`
	Providers       []provider `json:"providers"`
	WorkDir         string     `json:"workdir"`
	MaxSteps        int        `json:"max_steps"`
	AutoApprove     bool       `json:"auto_approve"`
	NotifyAny       bool       `json:"notify_any"`
	NotifyDanger    bool       `json:"notify_danger"`
	IdleTimeoutSec  int        `json:"idle_timeout_sec"`
	RunTimeoutMin   int        `json:"run_timeout_min"`
	ShellTimeoutSec int        `json:"shell_timeout_sec"`
	// MCPServers is the configured server list, verbatim: it is the user's own
	// file, and unlike the API key it is meant to be edited from the settings
	// pane. Any token an entry carries in env is the user's, not this agent's.
	MCPServers     []MCPServerConfig `json:"mcp_servers"`
	APIKeySet      bool              `json:"api_key_set"`
	APIKeyHint     string            `json:"api_key_hint,omitempty"`
	APIKeySource   string            `json:"api_key_source,omitempty"`
	APIKeyEditable bool              `json:"api_key_editable"`
}

func viewOf(c *Config) configView {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	prov := providerFor(c.Provider)
	return configView{
		Model:           c.Model,
		Provider:        string(prov.ID),
		ProviderLabel:   prov.Label,
		KeyEnv:          prov.KeyEnv,
		ProviderURL:     prov.ChatURL,
		BaseURL:         c.BaseURL,
		Models:          prov.Models,
		Providers:       providers,
		WorkDir:         c.WorkDir,
		MaxSteps:        c.MaxSteps,
		AutoApprove:     c.AutoApprove,
		NotifyAny:       c.NotifyAny,
		NotifyDanger:    c.NotifyDanger,
		IdleTimeoutSec:  c.IdleTimeoutSec,
		RunTimeoutMin:   c.RunTimeoutMin,
		ShellTimeoutSec: c.ShellTimeoutSec,
		MCPServers:      append([]MCPServerConfig(nil), c.MCPServers...),
		APIKeySet:       c.APIKey != "",
		APIKeyHint:      hintOf(c.APIKey),
		APIKeySource:    c.KeySource,
		APIKeyEditable:  c.KeySource != "env",
	}
}

// hintOf renders a non-reversible fingerprint: the sk- prefix plus the last 4
// characters is enough for a human to recognise which key is loaded, and far too
// little to reconstruct it.
func hintOf(k string) string {
	if k == "" {
		return ""
	}
	if len(k) <= 11 {
		return "sk-..."
	}
	return k[:3] + "..." + k[len(k)-4:]
}

// configPatch is the write payload. Pointers distinguish "field absent" (leave
// alone) from "field explicitly set".
type configPatch struct {
	Model       *string `json:"model"`
	WorkDir     *string `json:"workdir"`
	MaxSteps    *int    `json:"max_steps"`
	AutoApprove *bool   `json:"auto_approve"`
	// The two command-notification switches. Sent one at a time from Safety,
	// which is why they are separate pointers rather than one "notify" enum: a
	// browser that only knows the older one can still save.
	NotifyAny    *bool `json:"notify_any"`
	NotifyDanger *bool `json:"notify_danger"`
	// Provider switches vendor; base_url overrides that vendor's endpoint.
	Provider *string `json:"provider"`
	BaseURL  *string `json:"base_url"`
	// Watchdog timers. Out-of-range values are refused rather than clamped:
	// a silently rewritten timeout is worse than one that did not save.
	IdleTimeoutSec  *int `json:"idle_timeout_sec"`
	RunTimeoutMin   *int `json:"run_timeout_min"`
	ShellTimeoutSec *int `json:"shell_timeout_sec"`
	// MCPServers replaces the whole list: the settings pane edits it as one
	// document, and merging entries by name would make a rename look like a
	// different server.
	MCPServers  *[]MCPServerConfig `json:"mcp_servers"`
	APIKey      *string            `json:"api_key"`
	ClearAPIKey *bool              `json:"clear_api_key"`
}

// applyPatch mutates the config and persists it.
//
// Key semantics:
//   - api_key absent or blank  -> keep the current key. Blank is never treated
//     as "delete", so a form that cannot read the key back cannot wipe it by
//     accident.
//   - api_key present, non-blank -> seal and store it.
//   - clear_api_key: true      -> delete the stored key.
//
// When the key comes from the environment it is read-only here: we refuse to
// shadow it with a file copy so the env var stays the single source of truth.
func applyPatch(c *Config, p configPatch) error {
	cfgMu.Lock()
	defer cfgMu.Unlock()

	var errs []string

	// Provider first: everything below is provider-scoped. The key is stored per
	// provider family, the model name has to belong to the chosen vendor, and an
	// endpoint override belongs to that vendor too - carrying either across a
	// switch is a 400 at best and one vendor's key on another's endpoint at
	// worst.
	switched := false
	if p.Provider != nil {
		if prov, ok := providerByID(*p.Provider); !ok {
			errs = append(errs, fmt.Sprintf("unknown provider %q; expected one of %s",
				strings.TrimSpace(*p.Provider), strings.Join(providerIDs(), ", ")))
		} else if c.Provider != string(prov.ID) {
			c.Provider = string(prov.ID)
			c.BaseURL = ""
			switched = true
		}
	}
	if switched {
		// Swap in whatever key this provider already has, from its environment
		// variable or its sealed file, instead of keeping the old one in memory.
		c.resolveKeyLocked()
	}

	if p.APIKey != nil {
		k := strings.TrimSpace(*p.APIKey)
		if k != "" {
			if c.KeySource == "env" {
				errs = append(errs, providerFor(c.Provider).KeyEnv+
					" is set in the environment and overrides stored keys; unset it to manage the key from the UI")
			} else if err := c.storeAPIKey(k); err != nil {
				errs = append(errs, "could not store key: "+err.Error())
			}
		}
	}
	if p.ClearAPIKey != nil && *p.ClearAPIKey && c.KeySource != "env" {
		if err := clearAPIKey(c); err != nil {
			errs = append(errs, "could not remove key: "+err.Error())
		}
	}

	if p.BaseURL != nil {
		if u := strings.TrimSpace(*p.BaseURL); u == "" {
			c.BaseURL = ""
		} else if err := validateEndpoint(u); err != nil {
			errs = append(errs, err.Error())
		} else {
			c.BaseURL = u
		}
	}

	if p.Model != nil && strings.TrimSpace(*p.Model) != "" {
		c.Model = strings.TrimSpace(*p.Model)
	} else if switched {
		// A model name belongs to one vendor, so a switch that does not name one
		// lands on the new provider's default rather than on a 400.
		c.Model = providerFor(c.Provider).Model
	}
	if p.WorkDir != nil && strings.TrimSpace(*p.WorkDir) != "" {
		wd := strings.TrimSpace(*p.WorkDir)
		// A workspace that does not exist would leave every tool call failing
		// with a confusing error, so reject it at the door instead.
		abs, err := filepath.Abs(wd)
		if err != nil {
			errs = append(errs, "invalid workspace path: "+err.Error())
		} else if st, err := os.Stat(abs); err != nil {
			errs = append(errs, "workspace does not exist: "+abs)
		} else if !st.IsDir() {
			errs = append(errs, "workspace is not a folder: "+abs)
		} else {
			c.WorkDir = abs
		}
	}
	if p.MaxSteps != nil && *p.MaxSteps > 0 {
		steps := *p.MaxSteps
		if steps > maxStepLimit {
			steps = maxStepLimit
		}
		c.MaxSteps = steps
	}
	if p.AutoApprove != nil {
		c.AutoApprove = *p.AutoApprove
	}
	// One switch on means the other is off: "any command" already contains the
	// dangerous ones, and a file that says both must still answer one question.
	if p.NotifyAny != nil || p.NotifyDanger != nil {
		any, danger := c.NotifyAny, c.NotifyDanger
		if p.NotifyAny != nil {
			any = *p.NotifyAny
		}
		if p.NotifyDanger != nil {
			danger = *p.NotifyDanger
		}
		if any && danger {
			// Both on is not a third state: the wider switch already covers the
			// narrow one, so it wins.
			danger = false
		}
		c.NotifyAny, c.NotifyDanger = any, danger
	}
	if p.IdleTimeoutSec != nil {
		if v := *p.IdleTimeoutSec; v < minIdleTimeoutSec || v > maxIdleTimeoutSec {
			errs = append(errs, fmt.Sprintf("idle timeout must be between %d and %d seconds",
				minIdleTimeoutSec, maxIdleTimeoutSec))
		} else {
			c.IdleTimeoutSec = v
		}
	}
	if p.RunTimeoutMin != nil {
		if v := *p.RunTimeoutMin; v < minRunTimeoutMin || v > maxRunTimeoutMin {
			errs = append(errs, fmt.Sprintf("task time limit must be between %d and %d minutes",
				minRunTimeoutMin, maxRunTimeoutMin))
		} else {
			c.RunTimeoutMin = v
		}
	}
	if p.ShellTimeoutSec != nil {
		if v := *p.ShellTimeoutSec; v < minShellTimeoutSec || v > maxShellTimeoutSec {
			errs = append(errs, fmt.Sprintf("shell command timeout must be between %d and %d seconds",
				minShellTimeoutSec, maxShellTimeoutSec))
		} else {
			c.ShellTimeoutSec = v
		}
	}

	if p.MCPServers != nil {
		cleaned, problems := normalizeMCPServers(*p.MCPServers)
		if len(problems) > 0 {
			// Refused rather than partly applied: a list that lost an entry
			// silently is worse than one that did not save.
			errs = append(errs, problems...)
		} else {
			c.MCPServers = cleaned
		}
	}

	if err := saveConfig(c); err != nil {
		errs = append(errs, "could not save config: "+err.Error())
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// validateEndpoint checks an endpoint override before it is stored.
//
// Only the shape is checked: whether the host answers, and whether it wants this
// provider's key, is a question for the first request. A stored nonsense URL
// would otherwise fail every run with a network error that reads like the
// agent's fault rather than a typo in Settings.
func validateEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid endpoint: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("the endpoint must start with http:// or https://")
	}
	if u.Host == "" {
		return errors.New("the endpoint has no host")
	}
	return nil
}

// ---- runtime accessors (always under cfgMu) ----------------------------

func autoApprove(c *Config) bool {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	return c.AutoApprove
}

// desktopNotifyFor answers whether a command of this risk level should raise a
// Windows notification. Read under cfgMu like autoApprove: a Save that lands in
// the middle of a run decides the next command, not the next task.
func (c *Config) desktopNotifyFor(level string) bool {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	return c.NotifyAny || (c.NotifyDanger && level == riskDangerous)
}

func setAutoApprove(c *Config, v bool) {
	cfgMu.Lock()
	c.AutoApprove = v
	saveConfig(c)
	cfgMu.Unlock()
}

// runSettings returns the two knobs a run actually uses, read under cfgMu.
//
// Taking a snapshot does two things: it makes a Settings save that lands in the
// middle of a run race-free (applyPatch writes these fields under the same
// lock), and it keeps one task on one workspace with one step budget instead of
// re-reading them every loop iteration and switching horses mid-stride.
func (c *Config) runSettings() (workDir string, maxSteps int) {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	return c.WorkDir, c.MaxSteps
}

// watchdogLimits returns the idle and wall-clock limits this run is judged by.
// Read under cfgMu like runSettings, so a Save that lands mid-run cannot make
// one tick use one timeout and the next tick another.
func (c *Config) watchdogLimits() (idle, hard time.Duration) {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	idle = time.Duration(c.IdleTimeoutSec) * time.Second
	hard = time.Duration(c.RunTimeoutMin) * time.Minute
	if idle <= 0 {
		idle = defaultIdleTimeoutSec * time.Second
	}
	if hard <= 0 {
		hard = defaultRunTimeoutMin * time.Minute
	}
	return idle, hard
}

// shellTimeout is the per-command cap run_shell applies to one child process.
//
// Read per call rather than snapshotted with runSettings: it bounds a single
// command, so a Save that lands mid-run should govern the next command rather
// than the next task. Anything nonsense falls back to the default, because a
// zero here would be an instantly-killed shell.
func (c *Config) shellTimeout() time.Duration {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	secs := c.ShellTimeoutSec
	if secs <= 0 {
		secs = defaultShellTimeoutSec
	}
	return time.Duration(clampInt(secs, minShellTimeoutSec, maxShellTimeoutSec)) * time.Second
}

// mcpServers returns the configured MCP server list. It is a copy, taken under
// cfgMu, so a Save that lands while the servers are being started cannot tear
// the list apart mid-refresh.
func (c *Config) mcpServers() []MCPServerConfig {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	return append([]MCPServerConfig(nil), c.MCPServers...)
}

// credentials returns the model and key together under one lock, so a
// concurrent /api/config save cannot tear the pair apart mid-request.
// keyLoadError reports why a stored key that exists could not be loaded, if that
// is what happened. Read under cfgMu like the key itself, so a Settings save that
// re-resolves the key cannot tear the answer apart.
func (c *Config) keyLoadError() string {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	return c.KeyError
}

func credentials(c *Config) (model, key string) {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	model, key = c.Model, c.APIKey
	registerSecret(key)
	return model, key
}

// activeProvider is the registry entry for the current setting.
func (c *Config) activeProvider() provider {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	return providerFor(c.Provider)
}

// chatEndpoint is the URL one streaming request goes to: the provider's own
// endpoint unless Settings stores an override.
func (c *Config) chatEndpoint() string {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	if u := strings.TrimSpace(c.BaseURL); u != "" {
		return u
	}
	return providerFor(c.Provider).ChatURL
}

// ---- secret redaction ---------------------------------------------------

var (
	secretMu   sync.RWMutex
	secretList []string
	registered = map[string]bool{}
	keyPattern = regexp.MustCompile(`sk-[A-Za-z0-9_\-]{20,}`)
)

// registerSecret teaches the redactor about a live secret so it can be scrubbed
// from anything that leaves the process (UI stream, error text, transcript).
func registerSecret(s string) {
	s = strings.TrimSpace(s)
	if len(s) < 8 {
		return
	}
	secretMu.Lock()
	if !registered[s] {
		registered[s] = true
		secretList = append(secretList, s)
		// Longest first so a key that contains another cannot be half-masked.
		sortByLenDesc(secretList)
	}
	secretMu.Unlock()
}

func sortByLenDesc(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && len(s[j]) > len(s[j-1]); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// redact scrubs every known secret plus anything shaped like an API key. It is
// applied to model output, tool output and error strings alike, so a key echoed
// by a shell command cannot reach the browser or the remote API.
func redact(s string) string {
	if s == "" {
		return s
	}
	secretMu.RLock()
	list := secretList
	secretMu.RUnlock()
	for _, k := range list {
		if strings.Contains(s, k) {
			s = strings.ReplaceAll(s, k, redacted)
		}
	}
	return keyPattern.ReplaceAllString(s, redacted)
}
