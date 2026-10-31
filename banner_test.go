package main

import (
	"bytes"
	"strings"
	"testing"
)

// testBannerConfig is a launch as a fresh install would produce it: no key yet,
// the defaults, approvals on.
func testBannerConfig() *Config {
	return &Config{
		Model:           "deepseek-flash",
		WorkDir:         `C:\work\project`,
		MaxSteps:        25,
		IdleTimeoutSec:  120,
		RunTimeoutMin:   25,
		ShellTimeoutSec: 60,
	}
}

// The banner claims to render anywhere - cmd.exe on cp437, PowerShell, a mac
// terminal, a bare Linux console. That claim holds only if every byte is 7-bit
// ASCII, because those encodings agree on the first 128 code points and on
// nothing else.
func TestBannerIsPlainASCII(t *testing.T) {
	var buf bytes.Buffer
	printBanner(&buf, "http://127.0.0.1:8787", testBannerConfig())
	out := buf.String()

	for i := 0; i < len(out); i++ {
		if out[i] > 127 {
			line := strings.Split(out, "\n")[strings.Count(out[:i], "\n")]
			t.Fatalf("byte %d is %#x, not ASCII (line %q)", i, out[i], line)
		}
	}
	// No ANSI either: an escape sequence would be a colour on one terminal and
	// literal garbage in a redirected log.
	if strings.Contains(out, "\x1b") {
		t.Fatal("banner contains an escape sequence")
	}
	// A redirected log should not carry a wall of invisible whitespace.
	for _, line := range strings.Split(out, "\n") {
		if line != strings.TrimRight(line, " \t") {
			t.Fatalf("line has trailing whitespace: %q", line)
		}
	}
}

// An 80-column terminal is still the thing people open a cmd window into, and
// the line that would wrap is the tagline - the least useful one in the block -
// so the fixed part (mark, title, tagline) has to fit. The values below it are a
// workdir and a config path: those can be long, and that is the user's path, not
// this layout.
func TestBannerFitsAnEightyColumnTerminal(t *testing.T) {
	var buf bytes.Buffer
	printBanner(&buf, "http://127.0.0.1:8787", testBannerConfig())
	for i, line := range strings.Split(buf.String(), "\n") {
		if i > 3 {
			break
		}
		if len(line) > 80 {
			t.Fatalf("line %d is %d columns, too wide for 80: %q", i, len(line), line)
		}
	}
}

// The URL line is the contract scripts scrape to find the port, and it has to
// stay the last line and keep its shape whatever the banner above it says.
func TestBannerKeepsTheScrapableURLLine(t *testing.T) {
	var buf bytes.Buffer
	printBanner(&buf, "http://127.0.0.1:9123", testBannerConfig())
	out := buf.String()

	if !strings.HasSuffix(out, "\ndsagent listening on http://127.0.0.1:9123\n") {
		t.Fatalf("the URL line is not the last line:\n%s", out)
	}
}

// The point of the banner: the state you cannot see from the UI until it bites.
func TestBannerReportsTheLaunchState(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*Config)
		want  []string
		avoid []string
	}{
		{
			name:  "no key yet",
			setup: func(c *Config) {},
			want:  []string{"key       MISSING", keyEnvVar},
		},
		{
			name:  "key from the environment",
			setup: func(c *Config) { c.KeySource = "env" },
			want:  []string{"key       from DEEPSEEK_API_KEY (never written to disk)"},
			avoid: []string{"MISSING"},
		},
		{
			name:  "key sealed on disk",
			setup: func(c *Config) { c.KeySource = "store" },
			want:  []string{"key       stored, sealed by the OS keystore"},
			avoid: []string{"MISSING"},
		},
		{
			name:  "approval gate on",
			setup: func(c *Config) {},
			want:  []string{"approvals ask before every write and shell run"},
			avoid: []string{"auto-approved"},
		},
		{
			// Auto-approve never covers a dangerous call, and the banner must not
			// imply that it does.
			name:  "approval gate off",
			setup: func(c *Config) { c.AutoApprove = true },
			want:  []string{"approvals auto-approved - dangerous calls still ask"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testBannerConfig()
			tc.setup(cfg)
			var buf bytes.Buffer
			printBanner(&buf, "http://127.0.0.1:8787", cfg)
			out := buf.String()

			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("banner does not say %q:\n%s", w, out)
				}
			}
			for _, a := range tc.avoid {
				if strings.Contains(out, a) {
					t.Errorf("banner says %q, which is not true for this launch:\n%s", a, out)
				}
			}
		})
	}
}

// The workspace and the limits are read from the same config the agent runs on,
// so they have to appear verbatim rather than as a second copy of the defaults.
func TestBannerShowsTheConfiguredWorkdirAndLimits(t *testing.T) {
	cfg := testBannerConfig()
	cfg.WorkDir = `D:\other\tree`
	cfg.MaxSteps = 50
	cfg.IdleTimeoutSec = 300
	cfg.RunTimeoutMin = 60
	cfg.ShellTimeoutSec = 600

	var buf bytes.Buffer
	printBanner(&buf, "http://127.0.0.1:8787", cfg)
	out := buf.String()

	for _, w := range []string{
		`workdir   D:\other\tree`,
		"limits    50 steps, 60 min, 300s idle, 600s per command",
		"model     deepseek-flash",
		"config    " + configPath(),
	} {
		if !strings.Contains(out, w) {
			t.Errorf("banner does not say %q:\n%s", w, out)
		}
	}
}
