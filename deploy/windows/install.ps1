<#
.SYNOPSIS
  Installs sion-backup on a Windows machine as a scheduled task.

.DESCRIPTION
  Run this from an elevated PowerShell, once per machine, while you are
  standing at it.

  It registers a SCHEDULED TASK rather than a Windows service, and that is a
  deliberate limitation worth understanding:

    * The credentials are sealed with DPAPI to the user's account, so whatever
      runs the daemon must run AS THAT USER. A service running as
      LocalSystem could not open them.

    * A proper Windows service also needs the binary to implement the service
      control handler (golang.org/x/sys/windows/svc). It does not yet, and a
      scheduled task with "restart on failure" gets the same practical result.
      See "Known gaps" in the README.

  -Elevated registers the task to run with highest privileges, which is what
  Volume Shadow Copy needs. Without it, backups still run, and files that are
  open at the time -- Outlook's .pst above all -- are backed up from the live
  tree and marked degraded. See restic.BackupOptions.AllowVSSFallback.
#>

[CmdletBinding()]
param(
  [string] $InstallDir = "$env:ProgramFiles\sion-backup",
  [string] $Binary     = ".\sion-backup-windows-amd64.exe",
  [string] $Pin        = ".\restic.pin",
  [switch] $Elevated
)

$ErrorActionPreference = 'Stop'

if (-not ([Security.Principal.WindowsPrincipal] [Security.Principal.WindowsIdentity]::GetCurrent()
    ).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
  throw "Run this from an elevated PowerShell."
}

New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null

Copy-Item -Force $Binary "$InstallDir\sion-backup.exe"

# ---------------------------------------------------------------------------
# restic, downloaded from upstream and verified before it is ever run.
#
# This binary reads every file on the machine and holds the credentials to the
# off-site copy, so an unverified one is a compromise of both. The expected
# version and hash come from restic.pin, which ships with the release and is
# copied from a signed upstream SHA256SUMS.
# ---------------------------------------------------------------------------

if (-not (Test-Path $Pin)) {
  throw "Cannot find $Pin. It ships alongside the binary in the release."
}

$pinText = Get-Content $Pin -Raw

if ($pinText -notmatch '(?m)^RESTIC_VERSION=(\S+)') {
  throw "$Pin does not name a restic version."
}
$resticVersion = $Matches[1]

if ($pinText -notmatch '(?m)^RESTIC_SHA256_windows_amd64=(\S+)') {
  throw "$Pin has no hash for windows/amd64."
}
$want = $Matches[1].ToLower()

$asset = "restic_${resticVersion}_windows_amd64.zip"
$url   = "https://github.com/restic/restic/releases/download/v$resticVersion/$asset"
$work  = Join-Path ([System.IO.Path]::GetTempPath()) ([System.Guid]::NewGuid().ToString())

New-Item -ItemType Directory -Force -Path $work | Out-Null

try {
  Write-Host "Fetching  $asset"

  # TLS 1.2 explicitly: Windows PowerShell 5.1 still defaults to older
  # protocols that GitHub refuses, and the failure looks like a network fault.
  [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
  Invoke-WebRequest -Uri $url -OutFile "$work\$asset" -UseBasicParsing

  $got = (Get-FileHash "$work\$asset" -Algorithm SHA256).Hash.ToLower()

  if ($got -ne $want) {
    throw @"
REFUSING $asset - hash does not match the pin.
  expected $want
  got      $got

Do not work around this. Either the pin is stale (bump it from a signed
upstream SHA256SUMS) or the download was tampered with.
"@
  }

  Write-Host "Verified  sha256 $got"

  Expand-Archive -Path "$work\$asset" -DestinationPath $work -Force

  $extracted = Get-ChildItem -Path $work -Filter *.exe -Recurse | Select-Object -First 1
  if (-not $extracted) {
    throw "Nothing executable was extracted from $asset."
  }

  Copy-Item -Force $extracted.FullName "$InstallDir\restic.exe"
}
finally {
  # Whatever happened, leave nothing behind that a later step could run.
  Remove-Item -Recurse -Force $work -ErrorAction SilentlyContinue
}

Write-Host "Installed to $InstallDir (restic $resticVersion)"

# The task runs as the logged-in user, because that is whose profile is being
# backed up and whose DPAPI keys seal the credentials.
$user = "$env:USERDOMAIN\$env:USERNAME"

$action = New-ScheduledTaskAction `
  -Execute "$InstallDir\sion-backup.exe" `
  -Argument "daemon" `
  -WorkingDirectory $InstallDir

# At logon, and again at startup for a machine that is left signed in.
$triggers = @(
  New-ScheduledTaskTrigger -AtLogOn -User $user
)

$settings = New-ScheduledTaskSettingsSet `
  -AllowStartIfOnBatteries `
  -DontStopIfGoingOnBatteries `
  -StartWhenAvailable `
  -RestartCount 999 `
  -RestartInterval (New-TimeSpan -Minutes 5) `
  -ExecutionTimeLimit ([TimeSpan]::Zero)

$level = if ($Elevated) { 'Highest' } else { 'Limited' }

$principal = New-ScheduledTaskPrincipal -UserId $user -LogonType Interactive -RunLevel $level

Register-ScheduledTask `
  -TaskName 'sion-backup' `
  -Action $action `
  -Trigger $triggers `
  -Settings $settings `
  -Principal $principal `
  -Description 'Keeps this computer backed up with restic.' `
  -Force | Out-Null

Write-Host "Registered the scheduled task 'sion-backup' as $user (run level: $level)."

if (-not $Elevated) {
  Write-Warning @"
Registered WITHOUT elevation, so Volume Shadow Copy is unavailable.
Files that are open during a backup -- Outlook data files especially -- will be
read from the live tree and the run will be marked degraded.
Re-run with -Elevated to fix that.
"@
}

Write-Host ""
Write-Host "Next:"
Write-Host "  1. Point it at Eumaeus. Copy config.example.toml to the data"
Write-Host "     directory and set [eumaeus] url:"
Write-Host "       $InstallDir\sion-backup.exe paths"
Write-Host "  2. In Eumaeus, choose 'Enrol a computer', pick the owner and the"
Write-Host "     bucket, and bring the code over. It lasts fifteen minutes:"
Write-Host "       $InstallDir\sion-backup.exe enroll --code XXXX-XXXX"
Write-Host "     Print the restore card it produces and give it to the owner."
Write-Host "  3. Start it:"
Write-Host "       Start-ScheduledTask -TaskName sion-backup"
Write-Host "  4. Check it:"
Write-Host "       $InstallDir\sion-backup.exe doctor"
Write-Host "       http://127.0.0.1:7391/"
