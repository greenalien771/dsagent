package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// withTempConfig points the config dir at a scratch directory for one test.
func withTempConfig(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("APPDATA", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv(keyEnvVar, "")
	// os.UserConfigDir reads APPDATA on Windows; make sure the sub-dir exists.
	if err := os.MkdirAll(filepath.Join(dir, "dsagent"), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestConfigFileNeverContainsTheKey(t *testing.T) {
	withTempConfig(t)
	secret := "sk-abcdefghijklmnopqrstuvwxyz0123456789"

	c := loadConfig()
	if err := applyPatch(c, configPatch{APIKey: &secret}); err != nil {
		t.Fatalf("applyPatch: %v", err)
	}

	raw, err := os.ReadFile(configPath())
	if err != nil {
		t.Fatalf("read config.json: %v", err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("plaintext key leaked into %s:\n%s", configPath(), raw)
	}
	if strings.Contains(string(raw), "api_key") {
		t.Fatalf("api_key field present in config.json:\n%s", raw)
	}

	// The sealed blob must not contain the key either.
	blob, err := os.ReadFile(secretPath())
	if err != nil {
		t.Fatalf("read sealed key: %v", err)
	}
	if strings.Contains(string(blob), secret) {
		t.Fatal("sealed key file contains the plaintext key")
	}

	// A fresh load must recover it.
	got := loadConfig().APIKey
	if got != secret {
		t.Fatalf("reloaded key = %q, want the original secret", got)
	}
}

func TestConfigJSONNeverExposesTheKey(t *testing.T) {
	withTempConfig(t)
	secret := "sk-abcdefghijklmnopqrstuvwxyz0123456789"

	c := loadConfig()
	if err := applyPatch(c, configPatch{APIKey: &secret}); err != nil {
		t.Fatal(err)
	}

	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), secret) {
		t.Fatalf("Config marshalled the key: %s", b)
	}

	vb, err := json.Marshal(viewOf(c))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(vb), secret) {
		t.Fatalf("configView marshalled the key: %s", vb)
	}
	if !strings.Contains(string(vb), `"api_key_set":true`) {
		t.Fatalf("view should report the key as set: %s", vb)
	}
	// Only a masked hint may appear.
	if !strings.Contains(string(vb), hintOf(secret)) {
		t.Fatalf("expected masked hint %q in view: %s", hintOf(secret), vb)
	}
	if strings.Contains(hintOf(secret), secret[:len(secret)-4]) {
		t.Fatal("hint reveals too much of the key")
	}
}

func TestBlankKeyKeepsExistingKey(t *testing.T) {
	withTempConfig(t)
	secret := "sk-abcdefghijklmnopqrstuvwxyz0123456789"

	c := loadConfig()
	if err := applyPatch(c, configPatch{APIKey: &secret}); err != nil {
		t.Fatal(err)
	}

	blank := "   "
	model := "deepseek-reasoner"
	if err := applyPatch(c, configPatch{APIKey: &blank, Model: &model}); err != nil {
		t.Fatal(err)
	}
	if c.APIKey != secret {
		t.Fatalf("a blank field wiped the key: got %q", c.APIKey)
	}

	// Explicit delete still works.
	yes := true
	if err := applyPatch(c, configPatch{ClearAPIKey: &yes}); err != nil {
		t.Fatal(err)
	}
	if c.APIKey != "" {
		t.Fatal("clear_api_key did not remove the key")
	}
	if _, err := os.Stat(secretPath()); !os.IsNotExist(err) {
		t.Fatal("sealed key file still on disk after removal")
	}
}

func TestEnvKeyWinsAndIsReadOnly(t *testing.T) {
	withTempConfig(t)
	const envKey = "sk-envenvenvenvenvenvenvenvenvenv1234"
	t.Setenv(keyEnvVar, envKey)

	c := loadConfig()
	if c.APIKey != envKey || c.KeySource != "env" {
		t.Fatalf("env key not honoured: source=%q", c.KeySource)
	}
	if viewOf(c).APIKeyEditable {
		t.Fatal("env-sourced key must be read-only in the UI")
	}

	other := "sk-otherotherotherotherotherother9999"
	if err := applyPatch(c, configPatch{APIKey: &other}); err == nil {
		t.Fatal("expected an error when trying to override an env key")
	}
	if c.APIKey != envKey {
		t.Fatal("env key was replaced by a stored one")
	}
	if _, err := os.Stat(secretPath()); !os.IsNotExist(err) {
		t.Fatal("stored a key file even though the env var is set")
	}
}

func TestLegacyPlaintextKeyIsMigrated(t *testing.T) {
	withTempConfig(t)
	secret := "sk-legacylegacylegacylegacylegacy42"

	legacy := `{"api_key":"` + secret + `","model":"deepseek-flash","max_steps":25}`
	if err := os.WriteFile(configPath(), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	c := loadConfig()
	if c.APIKey != secret {
		t.Fatalf("legacy key not loaded: %q", c.APIKey)
	}
	raw, _ := os.ReadFile(configPath())
	if strings.Contains(string(raw), secret) {
		t.Fatalf("legacy plaintext key survived migration:\n%s", raw)
	}
	if _, err := os.Stat(secretPath()); err != nil {
		t.Fatalf("sealed key not written during migration: %v", err)
	}
}

func TestRedactScrubsKeyEverywhere(t *testing.T) {
	registerSecret("sk-live-live-live-live-live-live-0001")

	cases := []string{
		"Authorization: Bearer sk-live-live-live-live-live-live-0001",
		"my key is sk-live-live-live-live-live-live-0001 ok",
		"unseen sk-ZZZZZZZZZZZZZZZZZZZZZZZZZZZZ1234 pattern",
	}
	for _, in := range cases {
		out := redact(in)
		if strings.Contains(out, "sk-live-live-live-live-live-live-0001") {
			t.Fatalf("known key survived redaction: %s", out)
		}
		if !strings.Contains(out, redacted) && !strings.Contains(out, "REDACTED") {
			t.Fatalf("nothing redacted in %q -> %q", in, out)
		}
	}
	if redact("") != "" {
		t.Fatal("redact should pass empty strings through")
	}
}

func TestToolOutputIsRedactedBeforeTheTranscript(t *testing.T) {
	if got := redact("token = sk-abcdefghijklmnopqrstuvwxyz0123"); strings.Contains(got, "abcdefghijklmnopqrstuvwxyz") {
		t.Fatalf("tool output redaction failed: %s", got)
	}
}

func TestScrubbedEnvDropsCredentials(t *testing.T) {
	t.Setenv(keyEnvVar, "sk-should-not-be-inherited-000000")
	t.Setenv("OPENAI_API_KEY", "sk-openai-should-not-be-inherited")
	t.Setenv("PATH", os.Getenv("PATH"))

	env := strings.Join(scrubbedEnv(), "\n")
	if strings.Contains(env, "should-not-be-inherited") {
		t.Fatalf("credential leaked into child environment:\n%s", env)
	}
	if !strings.Contains(strings.ToUpper(env), "PATH=") {
		t.Fatal("scrubbedEnv dropped unrelated variables")
	}
}

func TestGuardRejectsForeignHostAndCrossSite(t *testing.T) {
	allowedHosts = map[string]bool{}
	addAllowedHost("127.0.0.1:8080")
	addAllowedHost("localhost:8080")

	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := guard(ok)

	tests := []struct {
		name   string
		host   string
		origin string
		site   string
		method string
		want   int
	}{
		{"loopback GET", "127.0.0.1:8080", "", "", "GET", http.StatusOK},
		{"localhost GET", "localhost:8080", "", "", "GET", http.StatusOK},
		{"other port", "127.0.0.1:9999", "", "", "GET", http.StatusForbidden},
		{"dns rebinding", "evil.example.com", "", "", "GET", http.StatusForbidden},
		{"cross-site post", "127.0.0.1:8080", "http://evil.example.com", "cross-site", "POST", http.StatusForbidden},
		{"same-origin post", "127.0.0.1:8080", "http://127.0.0.1:8080", "same-origin", "POST", http.StatusOK},
		{"origin mismatch", "127.0.0.1:8080", "http://localhost:9999", "", "POST", http.StatusForbidden},
	}

	for _, tc := range tests {
		r := httptest.NewRequest(tc.method, "http://"+tc.host+"/api/config", nil)
		r.Host = tc.host
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		if tc.site != "" {
			r.Header.Set("Sec-Fetch-Site", tc.site)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Errorf("%s: got %d want %d", tc.name, w.Code, tc.want)
		}
	}
}

func TestGuardSetsSecurityHeaders(t *testing.T) {
	allowedHosts = map[string]bool{}
	addAllowedHost("127.0.0.1:8080")

	h := guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	r := httptest.NewRequest("GET", "http://127.0.0.1:8080/", nil)
	r.Host = "127.0.0.1:8080"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	csp := w.Header().Get("Content-Security-Policy")
	for _, want := range []string{"connect-src 'self'", "frame-ancestors 'none'", "default-src 'self'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP missing %q: %s", want, csp)
		}
	}
	if got := w.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q", got)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q", got)
	}
	// Headers must also be present on rejected requests.
	w2 := httptest.NewRecorder()
	r2 := httptest.NewRequest("GET", "http://evil.example.com/", nil)
	r2.Host = "evil.example.com"
	h.ServeHTTP(w2, r2)
	if w2.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w2.Code)
	}
	if w2.Header().Get("Content-Security-Policy") == "" {
		t.Error("rejected request is missing the CSP header")
	}
}

func TestHintIsShortAndMasked(t *testing.T) {
	cases := map[string]string{
		"":      "",
		"short": "sk-...",
		"sk-abcdefghijklmnopqrstuvwxyz0123456789": "sk-...6789",
	}
	for in, want := range cases {
		if got := hintOf(in); got != want {
			t.Errorf("hintOf(%q) = %q, want %q", in, got, want)
		}
	}
	// The mask must never expose more than 4 trailing characters.
	k := "sk-abcdefghijklmnopqrstuvwxyz0123456789"
	if strings.Contains(k[:len(k)-4], strings.TrimPrefix(hintOf(k), "sk-...")) {
		t.Fatal("hint exposes too much of the key")
	}
}
func TestConfigEndpointNeverReturnsTheKey(t *testing.T) {
	withTempConfig(t)
	secret := "sk-abcdefghijklmnopqrstuvwxyz0123456789"

	c := loadConfig()
	if err := applyPatch(c, configPatch{APIKey: &secret}); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	handleConfig(c)(w, httptest.NewRequest("GET", "/api/config", nil))
	if strings.Contains(w.Body.String(), secret) {
		t.Fatalf("GET /api/config leaked the key: %s", w.Body.String())
	}

	// The write path must not be usable to read the key back either.
	body := strings.NewReader(`{"model":"deepseek-chat"}`)
	w2 := httptest.NewRecorder()
	handleConfig(c)(w2, httptest.NewRequest("POST", "/api/config", body))
	if w2.Code != http.StatusOK {
		t.Fatalf("POST /api/config: %d %s", w2.Code, w2.Body.String())
	}
	if strings.Contains(w2.Body.String(), secret) {
		t.Fatalf("POST /api/config leaked the key: %s", w2.Body.String())
	}
}

func TestMissingKeyIsReportedNotSent(t *testing.T) {
	withTempConfig(t)
	c := loadConfig()
	_, key := credentials(c)
	if key != "" {
		t.Fatalf("expected no key, got one")
	}
	if v := viewOf(c); v.APIKeySet {
		t.Fatal("view claims a key is set when none is")
	}
}

// A side-effecting endpoint must not answer a GET: that is the shape a hostile
// page can send without a preflight (an <img> tag), and Origin is only checked
// on non-GET requests.
func TestMutatingEndpointsRefuseGet(t *testing.T) {
	allowedHosts = map[string]bool{}
	addAllowedHost("127.0.0.1:8080")

	hit := false
	h := postOnly(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	})

	get := httptest.NewRequest("GET", "http://127.0.0.1:8080/api/stop", nil)
	get.Host = "127.0.0.1:8080"
	w := httptest.NewRecorder()
	guard(h).ServeHTTP(w, get)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET on a POST-only endpoint: got %d want 405", w.Code)
	}
	if hit {
		t.Fatal("the handler ran for a GET")
	}

	post := httptest.NewRequest("POST", "http://127.0.0.1:8080/api/stop", nil)
	post.Host = "127.0.0.1:8080"
	w = httptest.NewRecorder()
	guard(h).ServeHTTP(w, post)
	if w.Code != http.StatusOK || !hit {
		t.Fatalf("POST on a POST-only endpoint: got %d, ran=%v", w.Code, hit)
	}
}

// The watchdog timers are settings, so they have to survive a save and a
// reload, and an out-of-range value must be refused rather than silently
// rewritten - a timeout the form says one thing about and the run uses another
// is worse than a save that failed loudly.
func TestWatchdogTimeoutsDefaultSaveAndValidate(t *testing.T) {
	withTempConfig(t)

	c := loadConfig()
	if c.IdleTimeoutSec != defaultIdleTimeoutSec || c.RunTimeoutMin != defaultRunTimeoutMin {
		t.Fatalf("defaults = %ds/%dmin, want %ds/%dmin",
			c.IdleTimeoutSec, c.RunTimeoutMin, defaultIdleTimeoutSec, defaultRunTimeoutMin)
	}
	if idle, hard := c.watchdogLimits(); idle != 120*time.Second || hard != 25*time.Minute {
		t.Fatalf("watchdogLimits() = %v/%v, want 2m0s/25m0s", idle, hard)
	}

	idle, hard := 45, 3
	if err := applyPatch(c, configPatch{IdleTimeoutSec: &idle, RunTimeoutMin: &hard}); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.watchdogLimits(); got != 45*time.Second {
		t.Fatalf("idle limit = %v, want 45s", got)
	}
	if _, got := c.watchdogLimits(); got != 3*time.Minute {
		t.Fatalf("hard limit = %v, want 3m", got)
	}

	// A reload proves the numbers went to disk with the rest of the config.
	again := loadConfig()
	if again.IdleTimeoutSec != 45 || again.RunTimeoutMin != 3 {
		t.Fatalf("reloaded = %ds/%dmin, want 45s/3min", again.IdleTimeoutSec, again.RunTimeoutMin)
	}

	tooSmall, tooBig := 1, maxRunTimeoutMin+1
	if err := applyPatch(c, configPatch{IdleTimeoutSec: &tooSmall}); err == nil {
		t.Fatal("an idle timeout below the minimum was accepted")
	}
	if err := applyPatch(c, configPatch{RunTimeoutMin: &tooBig}); err == nil {
		t.Fatal("a task limit above the maximum was accepted")
	}
	if c.IdleTimeoutSec != 45 || c.RunTimeoutMin != 3 {
		t.Fatalf("a refused value was written anyway: %ds/%dmin", c.IdleTimeoutSec, c.RunTimeoutMin)
	}
}
