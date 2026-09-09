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
  [string] $Restic     = ".\restic.exe",
  [switch] $Elevated
)

$ErrorActionPreference = 'Stop'

if (-not ([Security.Principal.WindowsPrincipal] [Security.Principal.WindowsIdentity]::GetCurrent()
    ).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
  throw "Run this from an elevated PowerShell."
}

New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null

Copy-Item -Force $Binary "$InstallDir\sion-backup.exe"
Copy-Item -Force $Restic "$InstallDir\restic.exe"

Write-Host "Installed to $InstallDir"

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
Write-Host "  1. Copy config.example.toml to the data directory and edit it:"
Write-Host "       $InstallDir\sion-backup.exe paths"
Write-Host "  2. Enroll this machine:"
Write-Host "       $InstallDir\sion-backup.exe enroll -init"
Write-Host "  3. Start it:"
Write-Host "       Start-ScheduledTask -TaskName sion-backup"
Write-Host "  4. Check it:"
Write-Host "       $InstallDir\sion-backup.exe doctor"
Write-Host "       http://127.0.0.1:7391/"
