package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// keyEnvVar lets the user keep the key completely off disk.
// When it is set it wins over the stored key and is never persisted.
const keyEnvVar = "DEEPSEEK_API_KEY"

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

	// AutoApprove skips the per-tool approval prompt (run_shell, write_file).
	// Dangerous: the agent then runs unattended. Toggled from Tools Settings.
	AutoApprove bool `json:"auto_approve"`

	// KeySource records where the key came from ("env" or "store") so the UI
	// can explain why edits are ignored. Never contains the key itself.
	KeySource string `json:"-"`
}

// maxStepLimit matches the max="200" the settings form offers.
const maxStepLimit = 200

var cfgMu sync.Mutex

func configDir() string {
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		dir = "."
	}
	return filepath.Join(dir, "dsagent")
}

func configPath() string { return filepath.Join(configDir(), "config.json") }
func secretPath() string { return filepath.Join(configDir(), "api_key.bin") }

// legacyConfig is the shape older builds wrote, back when the key sat in
// plaintext inside config.json.
type legacyConfig struct {
	APIKey *string `json:"api_key"`
}

func loadConfig() *Config {
	c := &Config{Model: "deepseek-flash", MaxSteps: 25}

	raw, err := os.ReadFile(configPath())
	if err == nil {
		if err := json.Unmarshal(raw, c); err != nil {
			// A corrupt file must not take the agent down; start clean but keep
			// the unreadable original for forensics.
			os.Rename(configPath(), configPath()+".corrupt")
			c = &Config{Model: "deepseek-flash", MaxSteps: 25}
		} else {
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

	if c.Model == "" {
		c.Model = "deepseek-flash"
	}
	if c.MaxSteps <= 0 {
		c.MaxSteps = 25
	}
	if c.WorkDir == "" {
		c.WorkDir, _ = os.Getwd()
	}

	// Environment wins: nothing has to be written to disk at all.
	if k := strings.TrimSpace(os.Getenv(keyEnvVar)); k != "" {
		c.APIKey = k
		c.KeySource = "env"
	} else if k, err := loadStoredKey(); err == nil && k != "" {
		c.APIKey = k
		c.KeySource = "store"
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "dsagent: stored API key could not be decrypted: %v\n", err)
	}

	registerSecret(c.APIKey)
	return c
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
	if err := writeFileAtomic(secretPath(), sealed); err != nil {
		return err
	}
	c.APIKey = key
	c.KeySource = "store"
	registerSecret(key)
	return nil
}

func loadStoredKey() (string, error) {
	b, err := os.ReadFile(secretPath())
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
	err := os.Remove(secretPath())
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	return err
}

// ---- request/response shapes -------------------------------------------

// configView is everything the browser is allowed to know. Note the absence of
// any key material: only a boolean and a masked fingerprint ever leave here.
type configView struct {
	Model          string `json:"model"`
	WorkDir        string `json:"workdir"`
	MaxSteps       int    `json:"max_steps"`
	AutoApprove    bool   `json:"auto_approve"`
	APIKeySet      bool   `json:"api_key_set"`
	APIKeyHint     string `json:"api_key_hint,omitempty"`
	APIKeySource   string `json:"api_key_source,omitempty"`
	APIKeyEditable bool   `json:"api_key_editable"`
}

func viewOf(c *Config) configView {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	return configView{
		Model:          c.Model,
		WorkDir:        c.WorkDir,
		MaxSteps:       c.MaxSteps,
		AutoApprove:    c.AutoApprove,
		APIKeySet:      c.APIKey != "",
		APIKeyHint:     hintOf(c.APIKey),
		APIKeySource:   c.KeySource,
		APIKeyEditable: c.KeySource != "env",
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
	APIKey      *string `json:"api_key"`
	ClearAPIKey *bool   `json:"clear_api_key"`
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

	if p.APIKey != nil {
		k := strings.TrimSpace(*p.APIKey)
		if k != "" {
			if c.KeySource == "env" {
				errs = append(errs, keyEnvVar+" is set in the environment and overrides stored keys; unset it to manage the key from the UI")
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

	if p.Model != nil && strings.TrimSpace(*p.Model) != "" {
		c.Model = strings.TrimSpace(*p.Model)
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

	if err := saveConfig(c); err != nil {
		errs = append(errs, "could not save config: "+err.Error())
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// ---- runtime accessors (always under cfgMu) ----------------------------

func autoApprove(c *Config) bool {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	return c.AutoApprove
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

// credentials returns the model and key together under one lock, so a
// concurrent /api/config save cannot tear the pair apart mid-request.
func credentials(c *Config) (model, key string) {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	model, key = c.Model, c.APIKey
	registerSecret(key)
	return model, key
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
