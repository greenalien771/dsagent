package main

import (
	"encoding/json"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// This file is the command-risk layer. It answers one question for every
// mutating tool call: how bad would it be if this ran unattended?
//
//	riskSafe      - no side effect worth a prompt
//	riskElevated  - changes state, but recoverable (installs, network, git push)
//	riskDangerous - irreversible, privilege-related, or credential-touching
//
// The answer is enforced in execTool: a dangerous call is NEVER covered by
// "Always Allow". It always produces an approval prompt, and that prompt cannot
// be accepted without an explicit typed confirmation the server verifies.

const (
	riskSafe      = "safe"
	riskElevated  = "elevated"
	riskDangerous = "dangerous"
)

// confirmWord is what the human has to type into the approval card before the
// server accepts an approval for a dangerous call. It is checked server side,
// so "click allow" alone is never enough for the genuinely destructive stuff.
const confirmWord = "allow"

type Risk struct {
	Level   string   `json:"level"`
	Reasons []string `json:"reasons,omitempty"`
}

func (r Risk) isDangerous() bool { return r.Level == riskDangerous }

type riskRule struct {
	re    *regexp.Regexp
	level string
	why   string
}

// commandRules is deliberately weighted toward false positives: an extra
// prompt costs a click, a missed rule can cost the user their working tree.
var commandRules = []riskRule{
	// ---- irreversible deletion ----------------------------------------
	{regexp.MustCompile(`(?i)\brm\b[^|;&]*\s-{1,2}[a-z]*[rf]`), riskDangerous, "recursive or forced delete (rm -r / -f)"},
	{regexp.MustCompile(`(?i)\b(rmdir|rd)\b[^|;&]*\s/s\b`), riskDangerous, "recursive directory delete (rmdir /s)"},
	{regexp.MustCompile(`(?i)\b(del|erase)\b[^|;&]*\s/[a-z]*[fsq]`), riskDangerous, "forced/quiet file delete (del /f /s /q)"},
	{regexp.MustCompile(`(?i)\b(rm|del|erase|rd|rmdir|remove-item)\b[^|;&]*\*`), riskDangerous, "delete with a wildcard"},
	{regexp.MustCompile(`(?i)remove-item\b[^|;&]*-(recurse|force)`), riskDangerous, "recursive PowerShell delete"},
	{regexp.MustCompile(`(?i)\b(shred|wipefs|truncate)\b`), riskDangerous, "destroys file contents"},

	// ---- disks, partitions, boot records -------------------------------
	{regexp.MustCompile(`(?i)\b(mkfs(\.\w+)?|diskpart|fdisk|parted|gparted|bootrec|bcdedit)\b`), riskDangerous, "partition or boot-disk operation"},
	{regexp.MustCompile(`(?i)\bformat\b\s+[a-z]:`), riskDangerous, "formats a drive"},
	{regexp.MustCompile(`(?i)\bdd\b[^|;&]*\bof=`), riskDangerous, "raw disk write (dd of=...)"},
	{regexp.MustCompile(`(?i)\b(vssadmin|wbadmin)\b`), riskDangerous, "shadow copy / backup destruction"},
	{regexp.MustCompile(`(?i)\bcipher\s+/w`), riskDangerous, "wipes free space"},

	// ---- host power / session -------------------------------------------
	{regexp.MustCompile(`(?i)\b(shutdown|reboot|halt|poweroff|logoff)\b`), riskDangerous, "shuts down or logs off the machine"},
	{regexp.MustCompile(`(?i)\b(stop|restart)-computer\b`), riskDangerous, "shuts down or reboots the machine"},
	{regexp.MustCompile(`(?i)\binit\s+[06]\b`), riskDangerous, "changes the host runlevel"},

	// ---- registry, services, accounts ------------------------------------
	{regexp.MustCompile(`(?i)\breg(\.exe)?\s+(delete|add|import|restore|save|load)\b`), riskDangerous, "registry write, delete or hive dump"},
	{regexp.MustCompile(`(?i)\bsc(\.exe)?\s+(delete|config|create|stop)\b`), riskDangerous, "service create/delete/config"},
	{regexp.MustCompile(`(?i)\bnet(\.exe)?\s+(user|localgroup|accounts)\b`), riskDangerous, "account or group modification"},
	{regexp.MustCompile(`(?i)\b(schtasks|at)\b[^|;&]*/(create|delete|add)`), riskDangerous, "scheduled task creation/deletion"},
	{regexp.MustCompile(`(?i)\bnetsh\b`), riskDangerous, "changes network or firewall configuration"},
	{regexp.MustCompile(`(?i)\b(taskkill|killall|pkill|tskill)\b`), riskElevated, "terminates processes"},

	// ---- privilege escalation --------------------------------------------
	{regexp.MustCompile(`(?i)(^|[|;&]\s*)\s*(sudo|doas|pkexec|gsudo|runas)\b`), riskDangerous, "asks for elevated privileges"},
	{regexp.MustCompile(`(?i)\b(icacls|takeown)\b[^|;&]*(/grant|/reset|/r\b)`), riskDangerous, "rewrites filesystem permissions"},
	{regexp.MustCompile(`(?i)\bchown\b[^|;&]*-R\b|\bchmod\b[^|;&]*-R\b`), riskDangerous, "recursive permission change"},
	{regexp.MustCompile(`(?i)\bchmod\b[^|;&]*(777|u\+s|\+s)\b`), riskDangerous, "widens file permissions"},
	{regexp.MustCompile(`(?i)\battrib\b[^|;&]*-r\b`), riskElevated, "clears read-only attributes"},

	// ---- remote code execution / obfuscation ------------------------------
	{regexp.MustCompile(`(?i)\b(curl|wget|iwr|invoke-webrequest|invoke-restmethod)\b[^|;&]*\|\s*(sudo\s+)?(ba|z|k|da)?sh\b`), riskDangerous, "pipes a download straight into a shell"},
	{regexp.MustCompile(`(?i)\|\s*(iex|invoke-expression)\b`), riskDangerous, "executes downloaded text as code"},
	{regexp.MustCompile(`(?i)powershell(\.exe)?\b[^|;&]*\s-(e|enc|encodedcommand)\b`), riskDangerous, "runs obfuscated PowerShell (base64)"},
	{regexp.MustCompile(`(?i)\bcertutil\b[^|;&]*-urlcache\b`), riskDangerous, "downloads a payload via certutil"},
	{regexp.MustCompile(`(?i)\bbase64\b[^|;&]*(-d|--decode)[^|;&]*\|`), riskDangerous, "decodes and executes a payload"},
	{regexp.MustCompile(`:\s*\(\s*\)\s*\{[^}]*\}\s*;\s*:`), riskDangerous, "fork bomb"},

	// ---- version control damage -------------------------------------------
	{regexp.MustCompile(`(?i)\bgit\b[^|;&]*\bpush\b[^|;&]*(--force|\s-f\b)`), riskDangerous, "force-pushes and can erase remote history"},
	{regexp.MustCompile(`(?i)\bgit\b[^|;&]*\breset\b[^|;&]*--hard`), riskDangerous, "git reset --hard discards uncommitted work"},
	{regexp.MustCompile(`(?i)\bgit\b[^|;&]*\bclean\b[^|;&]*-[a-z]*[xdf]`), riskDangerous, "git clean deletes untracked files"},
	{regexp.MustCompile(`(?i)\bgit\b[^|;&]*\b(filter-branch|update-ref\s+-d|reflog\s+expire)\b`), riskDangerous, "rewrites or expires git history"},

	// ---- credentials and persistence --------------------------------------
	{regexp.MustCompile(`(?i)(\.ssh\b|id_rsa|id_ed25519|\.aws[/\\]credentials|\.netrc|\.git-credentials)`), riskDangerous, "touches SSH / cloud credential material"},
	{regexp.MustCompile(`(?i)\b(crontab|systemctl\s+enable|launchctl)\b`), riskDangerous, "installs persistence"},
	{regexp.MustCompile(`(?i)(>>?|tee|set-content|out-file)\s*["']?([a-z]:\\windows|/etc/|/usr/|/bin/|/boot/|/system/)`), riskDangerous, "writes into a system path"},

	// ---- system paths ------------------------------------------------------
	{regexp.MustCompile(`(?i)([a-z]:\\windows\b|%systemroot%|%windir%|/etc/|/boot/|/usr/bin|/system/library)`), riskElevated, "references system directories"},

	// ---- environment inspection --------------------------------------------
	{regexp.MustCompile(`(?i)^\s*(env|printenv|set)\s*$`), riskElevated, "dumps the environment"},

	// ---- ordinary but state-changing ----------------------------------------
	{regexp.MustCompile(`(?i)\b(npm|pnpm|yarn|pip3?|apt(-get)?|brew|choco|winget|gem|cargo)\b[^|;&]*\b(install|add|upgrade|remove|uninstall)\b`), riskElevated, "installs or removes software"},
	{regexp.MustCompile(`(?i)\bdocker\b[^|;&]*\b(run|rm|system|prune)\b`), riskElevated, "manages containers"},
	{regexp.MustCompile(`(?i)\bgit\b[^|;&]*\b(push|clone|remote|config)\b`), riskElevated, "changes git remotes or history"},
	{regexp.MustCompile(`(?i)\b(curl|wget|iwr|invoke-webrequest)\b`), riskElevated, "downloads over the network"},
	{regexp.MustCompile(`(?i)\b(ssh|scp|sftp|rsync)\b`), riskElevated, "reaches another machine"},
	{regexp.MustCompile(`(?i)\b(mv|move|rename|ren)\b\s`), riskElevated, "moves or renames files"},
	{regexp.MustCompile(`(?i)\b(rm|del|erase|rmdir|rd)\b`), riskElevated, "deletes files"},
	{regexp.MustCompile(`(?i)\b(kill|stop-process)\b`), riskElevated, "stops processes"},
}

// classifyCommand scores a shell command line.
func classifyCommand(cmd string) Risk {
	risk := Risk{Level: riskSafe}
	text := strings.TrimSpace(cmd)
	if text == "" {
		return risk
	}

	add := func(level, why string) {
		if level == riskDangerous {
			risk.Level = riskDangerous
		} else if level == riskElevated && risk.Level == riskSafe {
			risk.Level = riskElevated
		}
		for _, r := range risk.Reasons {
			if r == why {
				return
			}
		}
		risk.Reasons = append(risk.Reasons, why)
	}

	for _, rule := range commandRules {
		if rule.re.MatchString(text) {
			add(rule.level, rule.why)
		}
	}

	// The agent's own sealed key lives in the config folder; nothing good comes
	// from a command walking into it.
	if dir := strings.ToLower(configDir()); dir != "" && strings.Contains(strings.ToLower(text), strings.ToLower(dir)) {
		add(riskDangerous, "touches the dsagent config folder (holds the sealed API key)")
	}
	if strings.Contains(text, "..\\..") || strings.Contains(text, "../../") {
		add(riskElevated, "reaches outside the workspace")
	}
	return risk
}

// classifyTool scores a whole tool call (name + raw JSON arguments).
func classifyTool(name, rawArgs string) Risk {
	var args map[string]any
	_ = json.Unmarshal([]byte(rawArgs), &args)
	str := func(k string) string {
		s, _ := args[k].(string)
		return s
	}

	switch name {
	case "run_shell":
		return classifyCommand(str("command"))

	case "write_file":
		p := filepath.ToSlash(strings.ToLower(str("path")))
		switch {
		case p == ".git" || strings.HasPrefix(p, ".git/") || strings.Contains(p, "/.git/"):
			return Risk{Level: riskDangerous, Reasons: []string{"writes inside .git (hooks, config or history)"}}
		case strings.Contains(p, ".ssh/"), strings.HasSuffix(p, "id_rsa"), strings.HasSuffix(p, "id_ed25519"),
			strings.HasSuffix(p, ".env"), strings.Contains(p, ".aws/credentials"),
			strings.HasSuffix(p, ".netrc"), strings.HasSuffix(p, ".git-credentials"):
			return Risk{Level: riskDangerous, Reasons: []string{"writes a credentials file"}}
		case strings.Contains(p, "api_key"):
			return Risk{Level: riskDangerous, Reasons: []string{"targets the stored API key"}}
		case strings.HasSuffix(p, "package.json"), strings.HasSuffix(p, "go.mod"), strings.HasPrefix(p, ".vscode/"),
			strings.HasSuffix(p, "makefile"):
			return Risk{Level: riskElevated, Reasons: []string{"edits a project manifest"}}
		}
		return Risk{Level: riskSafe}

	case "screenshot":
		// Not destructive, but it does hand the contents of the user's desktop to
		// the remote model, so the approval card says exactly that.
		return Risk{Level: riskElevated, Reasons: []string{"captures the screen and sends it to the API"}}

	case "web_fetch":
		u := strings.TrimSpace(str("url"))
		if u == "" {
			return Risk{Level: riskSafe}
		}
		parsed, err := url.Parse(u)
		if err != nil {
			return Risk{Level: riskSafe}
		}
		scheme := strings.ToLower(parsed.Scheme)
		if scheme != "http" && scheme != "https" {
			return Risk{Level: riskDangerous, Reasons: []string{"non-HTTP URL scheme (" + scheme + ":)"}}
		}
		if isLANHost(parsed.Hostname()) {
			return Risk{Level: riskElevated, Reasons: []string{"fetches from this machine or the local network"}}
		}
		return Risk{Level: riskSafe}
	}
	return Risk{Level: riskSafe}
}

// isLocalAddr is isLANHost made address-aware: it also catches IPv6 loopback,
// link-local and unique-local literals and 0.0.0.0, which a name-only check
// misses. Used by the web_fetch dialer, where the address is already resolved.
func isLocalAddr(h string) bool {
	if ip := net.ParseIP(strings.Trim(h, "[]")); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
			ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast()
	}
	return isLANHost(h)
}

// isLANHost reports loopback/link-local/private addresses, which an agent has no
// business fetching without the user watching.
func isLANHost(h string) bool {
	h = strings.ToLower(strings.Trim(h, "[]"))
	if h == "" {
		return false
	}
	if h == "localhost" || h == "::1" || strings.HasSuffix(h, ".localhost") || strings.HasSuffix(h, ".local") {
		return true
	}
	if strings.HasPrefix(h, "127.") || strings.HasPrefix(h, "169.254.") || strings.HasPrefix(h, "192.168.") ||
		strings.HasPrefix(h, "10.") {
		return true
	}
	if strings.HasPrefix(h, "172.") {
		parts := strings.Split(h, ".")
		if len(parts) > 1 {
			if n, err := strconv.Atoi(parts[1]); err == nil {
				return n >= 16 && n <= 31
			}
		}
	}
	return false
}
