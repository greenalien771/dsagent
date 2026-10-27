package main

import (
	"context"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// slowCommand blocks for about eight seconds without needing a tool the test
// would have to guess is installed: ping is present on every platform that has
// a shell.
func slowCommand() string {
	if runtime.GOOS == "windows" {
		return "ping -n 8 127.0.0.1 > nul"
	}
	return "sleep 8"
}

// shellCall builds the run_shell ToolCall the runner would build.
func shellCall(command string) ToolCall {
	tc := ToolCall{ID: "sh1", Type: "function"}
	tc.Function.Name = "run_shell"
	tc.Function.Arguments = `{"command":` + mustJSONText(command) + `}`
	return tc
}

// mustJSONText quotes a string as a JSON string literal.
func mustJSONText(s string) string {
	b := strings.Builder{}
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// The per-command cap is a setting now, and it is the number the runner uses:
// with a 2s limit an 8s command dies, and a command that finishes inside the
// limit does not. Before this, the 60s limit was a constant in tools.go and a
// long build inside the agent was simply impossible.
func TestShellTimeoutIsHonouredPerCommand(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns real processes")
	}
	newTestAgent(t)
	// Not a t.TempDir(): a killed shell can leave its child holding the working
	// directory for a moment, and the test would then fail in cleanup rather
	// than on what it is checking.
	root := os.TempDir()

	// 2s: short enough to keep the test quick, long enough to start a shell.
	start := time.Now()
	out := runTool(context.Background(), root, shellCall(slowCommand()), 2*time.Second)
	elapsed := time.Since(start)

	if !strings.Contains(out, "TIMEOUT") {
		t.Fatalf("a command past the limit did not report a timeout: %q", out)
	}
	if !strings.Contains(out, "shell_timeout_sec") {
		t.Fatalf("the timeout message does not name the setting to raise: %q", out)
	}
	if elapsed > 7*time.Second {
		t.Fatalf("the limit was not enforced: took %v", elapsed)
	}

	// The same command with room to finish must not be reported as killed.
	quick := "ping -n 2 127.0.0.1 > nul"
	if runtime.GOOS != "windows" {
		quick = "sleep 1"
	}
	out = runTool(context.Background(), root, shellCall(quick), 30*time.Second)
	if strings.Contains(out, "TIMEOUT") {
		t.Fatalf("a command inside its limit was killed: %q", out)
	}
}

// A cancelled run still wins over a long per-command limit, and says so: the
// two endings must not be confused in the transcript.
func TestCancelBeatsTheShellTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns real processes")
	}
	newTestAgent(t)
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	start := time.Now()
	out := runTool(ctx, os.TempDir(), shellCall(slowCommand()), 10*time.Minute)
	if elapsed := time.Since(start); elapsed > 6*time.Second {
		t.Fatalf("the parent context did not bound the command: %v", elapsed)
	}
	// The parent's own deadline surfaces as a deadline on the child context too,
	// so either wording is honest here; what matters is that it did not run to
	// completion and did not claim success.
	if strings.Contains(out, "EXIT: exit status 0") {
		t.Fatalf("a cancelled command reported success: %q", out)
	}
}

// The limit the run uses comes from the config, and a mid-run save governs the
// next command rather than the next task.
func TestShellTimeoutComesFromConfig(t *testing.T) {
	a := newTestAgent(t)

	if got := a.cfg.shellTimeout(); got != defaultShellTimeoutSec*time.Second {
		t.Fatalf("default shellTimeout() = %v, want %v", got, defaultShellTimeoutSec*time.Second)
	}

	ninety := 90
	if err := applyPatch(a.cfg, configPatch{ShellTimeoutSec: &ninety}); err != nil {
		t.Fatal(err)
	}
	if got := a.cfg.shellTimeout(); got != 90*time.Second {
		t.Fatalf("shellTimeout() = %v, want 1m30s", got)
	}

	// A hand-edited zero or negative must not become "kill instantly".
	a.cfg.ShellTimeoutSec = 0
	if got := a.cfg.shellTimeout(); got != defaultShellTimeoutSec*time.Second {
		t.Fatalf("zero timeout fell back to %v, want the default", got)
	}
}

// The system prompt has to state the number the run will actually enforce,
// otherwise the model plans against a limit that is not the one in force.
func TestSystemPromptStatesTheConfiguredShellLimit(t *testing.T) {
	prompt := buildSystemPrompt(ModeAgent, "/tmp/work", 5*time.Minute)
	if !strings.Contains(prompt, "5m0s per shell command") {
		t.Fatalf("the prompt does not state the configured limit:\n%s", prompt)
	}
	if strings.Contains(prompt, "60 seconds per command") {
		t.Fatal("the prompt still carries the old hardcoded 60s")
	}
	// A zero must render as the default, never as "0s".
	if got := buildSystemPrompt(ModeAgent, "/tmp/work", 0); !strings.Contains(got, "1m0s per shell command") {
		t.Fatalf("an empty limit did not fall back to the default")
	}
}
