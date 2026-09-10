<#
.SYNOPSIS
  Installs sion-backup on a Windows machine as a scheduled task.

.DESCRIPTION
  Run this from an elevated PowerShell, once per machine, while you are
  standing at it.

  It registers a SCHEDULED TASK rather than a Windows service, and it runs as
  the signed-in user. Both are worth understanding, because one of the reasons
  is out of date.

    * The credentials are NO LONGER sealed with DPAPI. That used to be the
      reason a service running as LocalSystem was impossible, and it is not
      true any more: nothing is cached on this machine, and every run fetches
      the S3 keys and the repository password from Eumaeus and discards them.
      The one secret on disk is the machine token, in %LOCALAPPDATA%.

      Which means running as SYSTEM, or as a dedicated local account, is now
      open to us -- and it is better. The token would sit behind an ACL the
      signed-in user cannot read, so ransomware running as that user could not
      reach it, and Volume Shadow Copy would work without making the person an
      administrator. It needs machine-scope paths first: foundation/paths
      resolves %LOCALAPPDATA%, which under SYSTEM lands somewhere nobody would
      look. Until then, this.

    * A proper Windows service also needs the binary to implement the service
      control handler (golang.org/x/sys/windows/svc). It does not yet, and a
      scheduled task with "restart on failure" gets the same practical result.
      See "Known gaps" in the README.

  USE -Elevated. It registers the task to run with highest privileges, which is
  what Volume Shadow Copy needs, and VSS is not optional on a machine with
  Outlook on it: without it, every open file is backed up from the live tree,
  every run is recorded degraded, and a .pst that is open all day is backed up
  in whatever state it happened to be in. The legacy Windows script used
  --use-fs-snapshot from the start, for exactly this reason.

  Without -Elevated the install still works and backups still run. They are
  just worse, quietly, until somebody reads the status page.
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

# The task runs as the signed-in user, because that is whose profile is being
# backed up and whose %LOCALAPPDATA% holds the machine token. Not because of
# DPAPI: see the note at the top of this file.
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

Every file that is open during a backup -- Outlook data files above all -- will
be read from the live tree, and every run will be recorded as degraded. This is
the one setting on Windows that is worth going back for:

    .\install.ps1 -Elevated

"sion-backup doctor" will keep saying so until it is fixed.
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
