package main

import (
	"fmt"
	"io"
	"strings"
)

// The launch banner.
//
// ASCII only, on purpose. `/ \ | _` and the letters are the same bytes in
// cmd.exe on cp437, in PowerShell, in Windows Terminal, on Terminal.app and on
// a bare Linux console, so this block needs no ANSI escapes, no UTF-8 box
// drawing and no per-platform branch - which is one less thing that renders as
// mojibake on somebody else's machine.
//
// The shape is ui/logo.svg: a hexagon standing on a point (the sandbox cell)
// with a shell prompt inside it (chevron + caret). It is the same mark as the
// UI's title bar and hero, drawn in characters. Change it with the SVG.
//
// A terminal cell is about twice as tall as it is wide, so a hexagon that is
// 16.8 x 18.8 units in the SVG has to be about twice as wide as it is tall in
// characters to keep its proportions: 16 columns by 9 rows.
var bannerLogo = []string{
	`         /\`,
	`       /    \`,
	`     /        \`,
	`    |          |`,
	`    |    >_    |`,
	`    |          |`,
	`     \        /`,
	`       \    /`,
	`         \/`,
}

// bannerGutter is the space between the mark and the text beside it.
const bannerGutter = "   "

// bannerLabelWidth pads the labels so every value starts in the same column.
const bannerLabelWidth = 10

// bannerRow is one "label   value" line.
func bannerRow(label, value string) string {
	if len(label) < bannerLabelWidth {
		label += strings.Repeat(" ", bannerLabelWidth-len(label))
	}
	return label + value
}

// bannerLines is what the banner says about this launch, in the order it is
// printed beside the mark. Every line is read from the config the agent is
// about to run with, so the banner cannot claim something the agent will not do:
// a key that is not loaded, a workspace that is not the sandbox, an approval
// gate that is already off - those are the things worth knowing before you type
// a task, and the ones that are invisible from the UI until it is too late.
func bannerLines(cfg *Config) []string {
	key := "MISSING - paste one in Settings, or set " + keyEnvVar
	switch cfg.KeySource {
	case "env":
		key = "from " + keyEnvVar + " (never written to disk)"
	case "store":
		key = "stored, sealed by the OS keystore"
	}

	approvals := "ask before every write and shell run"
	if cfg.AutoApprove {
		approvals = "auto-approved - dangerous calls still ask"
	}

	return []string{
		"dsagent",
		"local coding agent - one directory, no telemetry",
		"",
		bannerRow("model", cfg.Model),
		bannerRow("workdir", cfg.WorkDir),
		bannerRow("key", key),
		bannerRow("approvals", approvals),
		bannerRow("limits", fmt.Sprintf("%d steps, %d min, %ds idle, %ds per command",
			cfg.MaxSteps, cfg.RunTimeoutMin, cfg.IdleTimeoutSec, cfg.ShellTimeoutSec)),
		bannerRow("config", configPath()),
	}
}

// printBanner writes the mark with the launch facts beside it, then a blank
// line, then the URL.
//
// The last line is deliberately the same bytes earlier builds printed:
// "dsagent listening on <url>" is what scripts and headless checks scrape to
// find the port the fallback happened to pick, so the banner goes above that
// line rather than in place of it. Trailing spaces are trimmed off every line,
// because a redirected log should not carry a wall of invisible whitespace.
func printBanner(w io.Writer, url string, cfg *Config) {
	lines := bannerLines(cfg)

	markWidth := 0
	for _, l := range bannerLogo {
		if len(l) > markWidth {
			markWidth = len(l)
		}
	}

	var b strings.Builder
	b.WriteString("\n")
	for i := 0; i < len(bannerLogo) || i < len(lines); i++ {
		var mark, text string
		if i < len(bannerLogo) {
			mark = bannerLogo[i]
		}
		if i < len(lines) {
			text = lines[i]
		}
		padding := strings.Repeat(" ", markWidth-len(mark)) + bannerGutter
		b.WriteString(strings.TrimRight("  "+mark+padding+text, " "))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString("dsagent listening on " + url + "\n")

	fmt.Fprint(w, b.String())
}
