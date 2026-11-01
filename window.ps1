param(
  [Parameter(Mandatory=$true)][ValidateSet('list','act','capture','wait')][string]$Action,
  [string]$Title = '',
  [double]$Id = 0,
  [string]$Act = '',
  [string]$Out = '',
  [double]$Scale = 1.0,
  [int]$OffsetX = 0, [int]$OffsetY = 0,
  [int]$CropW = 0, [int]$CropH = 0,
  [string]$Until = 'appear',
  [int]$TimeoutMs = 10000,
  [int]$SelfPid = 0
)
# Every variable in this file is case-insensitive, so the parameters above must
# never be named $w or $h: those are the window handle and width used below, and
# a typed parameter would coerce them (a [int]$H silently turned every handle
# into an integer, and .ToInt64() then had nothing to run on).
$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.Drawing
Add-Type -TypeDefinition @"
using System;
using System.Text;
using System.Collections.Generic;
using System.Runtime.InteropServices;

public class Win32 {
  public delegate bool EnumProc(IntPtr h, IntPtr l);
  [DllImport("user32.dll")] public static extern bool EnumWindows(EnumProc cb, IntPtr l);
  [DllImport("user32.dll")] public static extern bool IsWindowVisible(IntPtr h);
  [DllImport("user32.dll")] public static extern bool IsWindow(IntPtr h);
  [DllImport("user32.dll", CharSet=CharSet.Unicode)] public static extern int GetWindowText(IntPtr h, StringBuilder sb, int max);
  [DllImport("user32.dll")] public static extern bool GetWindowRect(IntPtr h, out RECT r);
  [DllImport("user32.dll")] public static extern uint GetWindowThreadProcessId(IntPtr h, out uint pid);
  [DllImport("user32.dll")] public static extern bool IsIconic(IntPtr h);
  [DllImport("user32.dll")] public static extern bool IsZoomed(IntPtr h);
  [DllImport("user32.dll")] public static extern IntPtr GetForegroundWindow();
  [DllImport("user32.dll")] public static extern bool SetForegroundWindow(IntPtr h);
  [DllImport("user32.dll")] public static extern bool BringWindowToTop(IntPtr h);
  [DllImport("user32.dll")] public static extern bool ShowWindow(IntPtr h, int cmd);
  [DllImport("user32.dll")] public static extern bool PostMessage(IntPtr h, uint msg, IntPtr w, IntPtr l);
  [DllImport("user32.dll")] public static extern bool PrintWindow(IntPtr h, IntPtr hdc, uint flags);
  [DllImport("dwmapi.dll")] public static extern int DwmGetWindowAttribute(IntPtr h, int attr, out int val, int size);
  [StructLayout(LayoutKind.Sequential)] public struct RECT { public int Left; public int Top; public int Right; public int Bottom; }

  // Visible, titled, top-level windows in z-order, topmost first. Cloaked
  // windows (a suspended UWP app, another virtual desktop) are skipped: they
  // cannot be captured and their titles would only mislead the caller.
  public static List<IntPtr> List() {
    var res = new List<IntPtr>();
    EnumWindows(delegate(IntPtr h, IntPtr l) {
      if (!IsWindowVisible(h)) return true;
      var sb = new StringBuilder(512);
      GetWindowText(h, sb, 512);
      if (sb.Length == 0) return true;
      int cloaked;
      if (DwmGetWindowAttribute(h, 14, out cloaked, 4) == 0 && cloaked != 0) return true;
      res.Add(h);
      return true;
    }, IntPtr.Zero);
    return res;
  }

  public static string TitleOf(IntPtr h) {
    var sb = new StringBuilder(1024);
    GetWindowText(h, sb, 1024);
    return sb.ToString();
  }

  public static uint PidOf(IntPtr h) {
    uint p;
    GetWindowThreadProcessId(h, out p);
    return p;
  }

  // left, top, width, height
  public static int[] RectOf(IntPtr h) {
    RECT r;
    if (!GetWindowRect(h, out r)) return new int[] { 0, 0, 0, 0 };
    return new int[] { r.Left, r.Top, r.Right - r.Left, r.Bottom - r.Top };
  }
}
"@

function Get-All {
  return @([Win32]::List())
}

function Get-State($win) {
  if ([Win32]::IsIconic($win)) { return 'minimized' }
  if ([Win32]::IsZoomed($win)) { return 'maximized' }
  return 'normal'
}

function Format-Win($win) {
  $r = [Win32]::RectOf($win)
  $procId = [Win32]::PidOf($win)
  $proc = '?'
  try { $proc = (Get-Process -Id $procId -ErrorAction Stop).ProcessName } catch { $proc = '?' }
  $focused = '0'
  if ([Win32]::GetForegroundWindow() -eq $win) { $focused = '1' }
  $t = ([Win32]::TitleOf($win)) -replace '\s+', ' '
  return ("hwnd=0x{0:X8} pid={1} proc={2} state={3} focused={4} rect={5},{6} {7}x{8} title={9}" -f $win.ToInt64(), $procId, $proc, (Get-State $win), $focused, $r[0], $r[1], $r[2], $r[3], $t)
}

# How many windows the last Find-Win matched: >1 means the title was ambiguous
# and the topmost one was used, which the caller is told about.
$script:MatchCount = 0

function Find-Win {
  $all = Get-All
  if ($Id -gt 0) {
    $want = [IntPtr][int64]$Id
    foreach ($win in $all) { if ($win -eq $want) { $script:MatchCount = 1; return $win } }
    if ([Win32]::IsWindow($want)) { $script:MatchCount = 1; return $want }
    throw ("no window with hwnd 0x{0:X8}; run list_windows to see what is open" -f [int64]$Id)
  }
  if ($Title.Trim() -eq '') { throw 'give a title (substring) or an hwnd id' }
  $hits = @()
  foreach ($win in $all) {
    if (([Win32]::TitleOf($win)).ToLower().Contains($Title.ToLower())) { $hits += $win }
  }
  if ($hits.Count -eq 0) {
    throw ("no visible window title contains '" + $Title + "'; run list_windows to see what is open")
  }
  $script:MatchCount = $hits.Count
  return $hits[0]
}

# A capture that came back one flat colour is not a picture of anything: it is
# what PrintWindow returns for a window it could not render. Sampling a grid is
# enough to tell the two apart, and cheap enough to run on every capture.
function Test-Blank($bmp) {
  $bw = $bmp.Width; $bh = $bmp.Height
  if ($bw -lt 1 -or $bh -lt 1) { return $true }
  $first = $bmp.GetPixel(0, 0).ToArgb()
  $sx = [Math]::Max(1, [int]($bw / 12))
  $sy = [Math]::Max(1, [int]($bh / 12))
  for ($x = 0; $x -lt $bw; $x += $sx) {
    for ($y = 0; $y -lt $bh; $y += $sy) {
      if ($bmp.GetPixel($x, $y).ToArgb() -ne $first) { return $false }
    }
  }
  return $true
}

switch ($Action) {

  'list' {
    $lines = @()
    foreach ($win in (Get-All)) {
      if ($Title -ne '') {
        if (-not ([Win32]::TitleOf($win)).ToLower().Contains($Title.ToLower())) { continue }
      }
      $lines += ('  ' + (Format-Win $win))
    }
    Write-Output ("WINDOWS " + $lines.Count)
    if ($lines.Count -gt 0) { $lines | Write-Output } else { Write-Output '  (none)' }
    break
  }

  'act' {
    $win = Find-Win
    if ($SelfPid -gt 0 -and [Win32]::PidOf($win) -eq $SelfPid) { throw 'refusing to touch this window: it belongs to the dsagent process itself, and acting on it would kill the run' }
    $before = Format-Win $win
    if ($script:MatchCount -gt 1) { Write-Output ("NOTE: " + $script:MatchCount + " windows matched; acting on the topmost one.") }
    switch ($Act.ToLower()) {
      'focus' {
        if ([Win32]::IsIconic($win)) { [void][Win32]::ShowWindow($win, 9) }
        [void][Win32]::BringWindowToTop($win)
        [void][Win32]::SetForegroundWindow($win)
        Start-Sleep -Milliseconds 150
        if ([Win32]::GetForegroundWindow() -ne $win) {
          Write-Output 'NOTE: Windows refused the foreground switch (foreground lock). The window was raised, but another window may still hold the focus.'
        }
      }
      'minimize' { [void][Win32]::ShowWindow($win, 6) }
      'maximize' { [void][Win32]::ShowWindow($win, 3) }
      'restore'  { [void][Win32]::ShowWindow($win, 9) }
      'close'    { [void][Win32]::PostMessage($win, 0x0010, [IntPtr]::Zero, [IntPtr]::Zero) }
      default    { throw ("unknown action '" + $Act + "'; use focus, minimize, maximize, restore or close") }
    }
    Start-Sleep -Milliseconds 200
    $verb = $Act.ToLower()
    if ($verb -eq 'close') {
      $gone = $false
      for ($i = 0; $i -lt 20; $i++) {
        if (-not [Win32]::IsWindow($win)) { $gone = $true; break }
        Start-Sleep -Milliseconds 100
      }
      if ($gone) { Write-Output 'OK close: the window is gone.' }
      else { Write-Output ("OK close: WM_CLOSE was posted, but the window is still open - it is probably asking about unsaved changes: " + (Format-Win $win)) }
      Write-Output ("WAS " + $before)
    } else {
      Write-Output ("OK " + $verb + ": " + (Format-Win $win))
      Write-Output ("WAS " + $before)
    }
    break
  }

  'capture' {
    $win = Find-Win
    if ($script:MatchCount -gt 1) { Write-Output ("NOTE: " + $script:MatchCount + " windows matched; capturing the topmost one.") }
    # A minimized window has no drawable surface: PrintWindow would return an
    # empty frame. Restoring it is the only way to get a picture, so that is
    # done and reported rather than silently producing a black image.
    $wasMin = [Win32]::IsIconic($win)
    if ($wasMin) {
      [void][Win32]::ShowWindow($win, 9)
      Start-Sleep -Milliseconds 350
    }
    $r = [Win32]::RectOf($win)
    $ww = $r[2]; $wh = $r[3]
    if ($ww -lt 1 -or $wh -lt 1) { throw ("that window has no drawable area (rect " + $r[0] + "," + $r[1] + " " + $ww + "x" + $wh + ")") }

    $src = New-Object System.Drawing.Bitmap($ww, $wh)
    $g = [System.Drawing.Graphics]::FromImage($src)
    $hdc = $g.GetHdc()
    $ok = [Win32]::PrintWindow($win, $hdc, 2)
    $g.ReleaseHdc($hdc)
    $g.Dispose()
    $method = 'printwindow'
    $note = ''
    if (-not $ok -or (Test-Blank $src)) {
      # Some windows refuse to render themselves. The screen still shows them,
      # so the fallback raises the window and copies the desktop instead.
      $src.Dispose()
      [void][Win32]::BringWindowToTop($win)
      [void][Win32]::SetForegroundWindow($win)
      Start-Sleep -Milliseconds 300
      $r = [Win32]::RectOf($win)
      $ww = $r[2]; $wh = $r[3]
      if ($ww -lt 1 -or $wh -lt 1) { throw 'the window vanished before it could be captured' }
      $src = New-Object System.Drawing.Bitmap($ww, $wh)
      $g = [System.Drawing.Graphics]::FromImage($src)
      $g.CopyFromScreen($r[0], $r[1], 0, 0, $src.Size)
      $g.Dispose()
      $method = 'screen'
      $note = 'PrintWindow returned nothing usable, so the window was raised and taken from the screen instead.'
    }

    $ox = [Math]::Min([Math]::Max(0, $OffsetX), $ww - 1)
    $oy = [Math]::Min([Math]::Max(0, $OffsetY), $wh - 1)
    $cw = $ww - $ox; $ch = $wh - $oy
    if ($CropW -gt 0) { $cw = [Math]::Min($cw, $CropW) }
    if ($CropH -gt 0) { $ch = [Math]::Min($ch, $CropH) }
    if ($cw -lt 1 -or $ch -lt 1) { throw ("the requested crop is outside the window (" + $ww + "x" + $wh + ")") }

    if ($cw -ne $ww -or $ch -ne $wh) {
      $crop = New-Object System.Drawing.Bitmap($cw, $ch)
      $gc = [System.Drawing.Graphics]::FromImage($crop)
      $gdst = New-Object System.Drawing.Rectangle(0, 0, $cw, $ch)
      $gsrc = New-Object System.Drawing.Rectangle($ox, $oy, $cw, $ch)
      $gc.DrawImage($src, $gdst, $gsrc, [System.Drawing.GraphicsUnit]::Pixel)
      $gc.Dispose()
      $src.Dispose()
      $src = $crop
    }

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

    Write-Output ("CAPTURED " + $cw + "x" + $ch + " -> " + $dw + "x" + $dh)
    Write-Output ("WINDOW " + (Format-Win $win))
    Write-Output ("METHOD " + $method)
    if ($wasMin) { Write-Output 'NOTE: the window was minimized, so it was restored to be captured, and it stays restored.' }
    if ($note -ne '') { Write-Output ('NOTE: ' + $note) }
    break
  }

  'wait' {
    $deadline = (Get-Date).AddMilliseconds($TimeoutMs)
    $wantGone = ($Until.ToLower() -eq 'gone')
    $start = Get-Date
    while ($true) {
      $win = $null
      try { $win = Find-Win } catch { $win = $null }
      $elapsed = [int]((Get-Date) - $start).TotalMilliseconds
      if ($wantGone -and $null -eq $win) { Write-Output ("GONE after " + $elapsed + "ms"); break }
      if (-not $wantGone -and $null -ne $win) { Write-Output ("FOUND after " + $elapsed + "ms " + (Format-Win $win)); break }
      if ((Get-Date) -gt $deadline) {
        if ($wantGone) { Write-Output ("TIMEOUT after " + $TimeoutMs + "ms: still open: " + (Format-Win $win)) }
        else { Write-Output ("TIMEOUT after " + $TimeoutMs + "ms: no visible window title contains '" + $Title + "'") }
        break
      }
      Start-Sleep -Milliseconds 150
    }
    break
  }
}
