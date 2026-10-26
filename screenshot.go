package main

import (
	"context"
	"encoding/json"
	"fmt"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// shotTag marks the line of a screenshot tool result that names the file the
// model has to be able to see. It is parsed out of the result and the file is
// attached to the conversation (see attachShot), because the API only accepts
// image blocks in a user message - a tool result cannot carry one.
const shotTag = "SHOT:"

// shotMaxScale / shotMinScale bound the zoom factor. 1 keeps every pixel, which
// is what reading small text out of a screenshot needs; below 0.1 the picture is
// worthless.
const (
	shotMinScale = 0.1
	shotMaxScale = 1.0
)

// captureScript is the whole Windows capture path: no dependency, no extra Go
// module, and the user's own PowerShell does the work.
//
// It exists because Go cannot grab the screen without cgo bindings, and the
// interesting part - cropping to a region and scaling it down - is what turns a
// 1366x768 wallpaper of desktop into a legible picture of one dialog.
const captureScript = `param(
  [Parameter(Mandatory=$true)][string]$Out,
  [int]$X = 0, [int]$Y = 0, [int]$W = 0, [int]$H = 0,
  [double]$Scale = 1.0
)
$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.Drawing, System.Windows.Forms
$vs = [System.Windows.Forms.SystemInformation]::VirtualScreen
if ($W -le 0) { $W = $vs.Width }
if ($H -le 0) { $H = $vs.Height }

# Clamp the requested rectangle into the desktop. A region the caller guessed
# wrong must produce a picture, not a black rectangle or a thrown exception.
$cx = [Math]::Max($vs.X, [Math]::Min($X, $vs.X + $vs.Width - 1))
$cy = [Math]::Max($vs.Y, [Math]::Min($Y, $vs.Y + $vs.Height - 1))
$cw = [Math]::Min($W, $vs.X + $vs.Width - $cx)
$ch = [Math]::Min($H, $vs.Y + $vs.Height - $cy)
if ($cw -lt 1 -or $ch -lt 1) { throw "the requested region is outside the desktop ($($vs.Width)x$($vs.Height) at $($vs.X),$($vs.Y))" }

$src = New-Object System.Drawing.Bitmap($cw, $ch)
$g = [System.Drawing.Graphics]::FromImage($src)
$g.CopyFromScreen($cx, $cy, 0, 0, $src.Size)
$g.Dispose()

$dw = [int][Math]::Round($cw * $Scale); if ($dw -lt 1) { $dw = 1 }
$dh = [int][Math]::Round($ch * $Scale); if ($dh -lt 1) { $dh = 1 }
if ($dw -ne $cw -or $dh -ne $ch) {
  $dst = New-Object System.Drawing.Bitmap($dw, $dh)
  $gd = [System.Drawing.Graphics]::FromImage($dst)
  $gd.InterpolationMode = [System.Drawing.Drawing2D.InterpolationMode]::HighQualityBicubic
  $gd.PixelOffsetMode = [System.Drawing.Drawing2D.PixelOffsetMode]::HighQuality
  $gd.DrawImage($src, 0, 0, $dw, $dh)
  $gd.Dispose()
  $src.Dispose()
  $src = $dst
}
$src.Save($Out, [System.Drawing.Imaging.ImageFormat]::Png)
$src.Dispose()
Write-Output ("CAPTURED " + $cw + "x" + $ch + " at " + $cx + "," + $cy + " -> " + $dw + "x" + $dh)
Write-Output ("DESKTOP " + $vs.X + "," + $vs.Y + " " + $vs.Width + "x" + $vs.Height)
`

// shotArgs is the decoded argument set of one screenshot call.
type shotArgs struct {
	X, Y, W, H int
	Scale      float64
}

// parseShotArgs reads the region and zoom out of a tool call. Absent values mean
// "the whole desktop at full resolution", which is the honest default: the
// caller either knows what it wants to see or it is about to look and find out.
func parseShotArgs(raw string) (shotArgs, error) {
	a := shotArgs{Scale: 1}
	var args map[string]any
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return a, fmt.Errorf("BAD ARGS: %v", err)
	}
	num := func(k string) (int, bool, error) {
		v, ok := args[k]
		if !ok || v == nil {
			return 0, false, nil
		}
		f, ok := v.(float64)
		if !ok {
			return 0, false, fmt.Errorf("%s must be a number", k)
		}
		return int(f), true, nil
	}
	x, _, err := num("x")
	if err != nil {
		return a, err
	}
	y, _, err := num("y")
	if err != nil {
		return a, err
	}
	w, hasW, err := num("width")
	if err != nil {
		return a, err
	}
	h, hasH, err := num("height")
	if err != nil {
		return a, err
	}
	if hasW != hasH {
		return a, fmt.Errorf("width and height must be given together")
	}
	if w < 0 || h < 0 {
		return a, fmt.Errorf("width and height cannot be negative")
	}
	if _, ok := args["scale"].(float64); ok {
		a.Scale = args["scale"].(float64)
	}
	if a.Scale <= 0 {
		a.Scale = 1
	}
	if a.Scale < shotMinScale {
		a.Scale = shotMinScale
	}
	if a.Scale > shotMaxScale {
		a.Scale = shotMaxScale
	}
	a.X, a.Y, a.W, a.H = x, y, w, h
	return a, nil
}

// captureScreenshot writes one PNG and reports what it grabbed. The picture
// itself reaches the model through attachShot, not through this return value.
func captureScreenshot(ctx context.Context, raw string) (string, error) {
	if runtime.GOOS != "windows" {
		return "ERROR: the screenshot tool is only implemented on Windows. On this machine, capture the screen another way and attach the file with read_file (text) or the composer's Add Image.", nil
	}
	args, err := parseShotArgs(raw)
	if err != nil {
		return "ERROR: " + err.Error(), nil
	}
	dir := filepath.Join(os.TempDir(), "dsagent-shots")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "ERROR: " + err.Error(), nil
	}
	sweepShots(dir)

	script := filepath.Join(dir, "capture.ps1")
	if err := os.WriteFile(script, []byte(captureScript), 0600); err != nil {
		return "ERROR: " + err.Error(), nil
	}

	out := filepath.Join(dir, fmt.Sprintf("shot-%d.png", time.Now().UnixNano()))
	cmdCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c := exec.CommandContext(cmdCtx, "powershell",
		"-NoProfile", "-ExecutionPolicy", "Bypass", "-File", script,
		"-Out", out,
		"-X", strconv.Itoa(args.X), "-Y", strconv.Itoa(args.Y),
		"-W", strconv.Itoa(args.W), "-H", strconv.Itoa(args.H),
		"-Scale", strconv.FormatFloat(args.Scale, 'f', 2, 64))
	c.Env = scrubbedEnv()
	c.WaitDelay = 3 * time.Second
	outBytes, err := c.CombinedOutput()
	text := clip(strings.TrimSpace(string(outBytes)))
	if err != nil {
		return "ERROR: screen capture failed: " + err.Error() + "\n" + text, nil
	}
	if st, err := os.Stat(out); err != nil || st.Size() == 0 {
		return "ERROR: the capture produced no image\n" + text, nil
	}

	info := text
	if size := imageSize(out); size != "" {
		info = strings.TrimSpace(text + "\nSaved image: " + size + " px")
	}
	return fmt.Sprintf("Captured the screen.\n%s\nFile: %s (%s)\nThe image is attached to the conversation - you will see it on the next step; describe what is actually visible rather than what you expect.\n%s%s",
		info, out, humanBytes(fileSize(out)), shotTag, out), nil
}

// imageSize reads just the PNG header, so the result can tell the model the
// pixel dimensions without decoding the whole bitmap.
func imageSize(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	cfg, err := png.DecodeConfig(f)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%dx%d", cfg.Width, cfg.Height)
}

func fileSize(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.Size()
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d B", n)
}

// sweepShots deletes captures older than an hour. They are screen content on
// disk, so leaving them there forever is both a privacy and a disk problem.
func sweepShots(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-time.Hour)
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "shot-") {
			continue
		}
		if info, err := e.Info(); err == nil && info.ModTime().Before(cutoff) {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// shotFromResult pulls the captured file out of a tool result.
func shotFromResult(out string) (string, bool) {
	i := strings.LastIndex(out, shotTag)
	if i < 0 {
		return "", false
	}
	path := strings.TrimSpace(out[i+len(shotTag):])
	if nl := strings.IndexByte(path, '\n'); nl >= 0 {
		path = strings.TrimSpace(path[:nl])
	}
	if path == "" {
		return "", false
	}
	return path, true
}
