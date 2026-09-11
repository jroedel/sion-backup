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
  [switch] $Elevated,

  # Look at the machine and report; change nothing. Run this first on a
  # computer that has been backing up with the old .bat for two years.
  [switch] $ReconOnly,

  # Where the old install is, if it is somewhere the notes never mentioned.
  # These were installed by hand, so it happens.
  [string] $LegacyDir,

  # Disable the legacy scheduled task. LAST, after the new install has taken
  # one verified backup -- two backup systems for one night is untidy; none
  # is worse. Reversible: schtasks /Change /TN <name> /ENABLE.
  [switch] $DisableLegacyTask,

  # Do not stop to ask.
  [switch] $Yes
)

$ErrorActionPreference = 'Stop'

# Ties every report from this run of the installer together, including the
# ones from before the binary was in place.
$InstallId = [guid]::NewGuid().ToString()

# What a failure is reported as. Set it before anything that can fail.
$Script:Step = 'starting'

# Empty rather than unset: an unset variable can be dropped from a native
# command's arguments instead of passed as an empty one, which would silently
# shift --detail into --prior-version.
$Script:PriorVersion = ''

# Report tells Eumaeus that this install did not finish.
#
# The failures worth hearing about are the ones nobody will type up: an
# install that fell over at nine in the evening on somebody's laptop, which
# will fall over the same way on the next machine unless it is seen. Sending
# needs no token -- see jroedel/eumaeus#113.
function Report-Failure {
  param([string] $Detail)

  $exe = Join-Path $InstallDir 'sion-backup.exe'

  if (-not (Test-Path $exe)) { return }

  try {
    & $exe report --kind install-failed --step "$Script:Step" `
        --install-id "$InstallId" --prior-version "$Script:PriorVersion" `
        --detail "$Detail" 2>&1 | Out-Null

    Write-Host "Reported it (or queued the report): $exe doctor"
  } catch {
    # An installer that fails while reporting that it failed helps nobody.
  }
}

if (-not ([Security.Principal.WindowsPrincipal] [Security.Principal.WindowsIdentity]::GetCurrent()
    ).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
  throw "Run this from an elevated PowerShell."
}

trap {
  Write-Warning "install failed at step: $($Script:Step)"
  Report-Failure -Detail "install.ps1 failed at $($Script:Step): $_"

  break
}

$Script:Step = 'install-binary'

New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null

Copy-Item -Force $Binary "$InstallDir\sion-backup.exe"

# ---------------------------------------------------------------------------
# What is already on this machine.
#
# Almost every machine in this fleet is already backing up with the old .bat
# file, installed by hand from a PDF, with a bucket and a schedule and two
# years of history. Installing over the top of that without looking gives it
# two backup systems and two opinions about which bucket is current.
# ---------------------------------------------------------------------------

$Script:Step = 'recon'

$reconArgs = @('recon')
if ($LegacyDir) { $reconArgs += @('--legacy-dir', $LegacyDir) }

& "$InstallDir\sion-backup.exe" @reconArgs

$reconJson = & "$InstallDir\sion-backup.exe" @($reconArgs + '--json') | ConvertFrom-Json
$legacy    = $reconJson.legacy

$Script:PriorVersion = if ($legacy) { "legacy-$($legacy.layout)-$($legacy.version)" } else { '' }

if ($ReconOnly) {
  Write-Host ""
  Write-Host "Nothing was changed (-ReconOnly)."

  exit 0
}

if ($legacy -and -not $Yes) {
  Write-Host ""
  Write-Warning @"
This machine is already backing up with the old scripts.

Read the plan above. Nothing below touches the legacy install, its bucket or
its credentials -- but decide the bucket question BEFORE enrolling, because
enrolling is what fixes the answer.
"@

  if ((Read-Host "Continue? [y/N]") -notmatch '^[Yy]') {
    Write-Host "Stopped. The binary is installed and nothing else was changed."

    exit 0
  }
}

if ($legacy -and $legacy.uses_fs_snapshot -and -not $Elevated) {
  # Worth stopping for: the machine it is true of is a machine somebody
  # already decided needed shadow copies.
  Write-Warning @"
The old install on this machine uses Volume Shadow Copy (--use-fs-snapshot)
and you are installing WITHOUT -Elevated. That is a regression: every run
would read open files from the live tree and be recorded as degraded.

Re-run with -Elevated.
"@

  if (-not $Yes -and (Read-Host "Continue anyway? [y/N]") -notmatch '^[Yy]') {
    exit 0
  }
}

# ---------------------------------------------------------------------------
# restic
#
# Not here, deliberately, and this file used to do it.
#
# sion-backup installs its own restic: one pinned version, from upstream's
# release, verified against a hash compiled into the binary. That happens when
# the machine is enrolled, and it happens as the person being backed up --
# which is the part that matters on Windows. This script writes to Program
# Files, so it is running as an administrator, and an administrator may well
# not be the signed-in user. Fetching restic from here would put it in the
# wrong profile's %LOCALAPPDATA%, and the scheduled task -- which runs as the
# user -- would not find it.
#
# So it is left to "sion-backup enroll", which the user runs next, and which
# needs restic anyway to prove the bucket opens. "sion-backup restic" installs
# it on its own if you want to see it happen first.
# ---------------------------------------------------------------------------

Write-Host "Installed to $InstallDir"
Write-Host "restic is installed by ""sion-backup enroll"", into the user's own profile."

# The task runs as the signed-in user, because that is whose profile is being
# backed up and whose %LOCALAPPDATA% holds the machine token. Not because of
# DPAPI: see the note at the top of this file.
$user = "$env:USERDOMAIN\$env:USERNAME"

$Script:Step = 'register-task'

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

# ---------------------------------------------------------------------------
# The legacy schedule, if and only if asked.
# ---------------------------------------------------------------------------

if ($DisableLegacyTask) {
  $Script:Step = 'disable-legacy'

  if (-not $legacy) {
    Write-Warning "-DisableLegacyTask was given but no legacy install was found."
  } elseif (-not $legacy.schedule) {
    Write-Warning "No scheduled task was found for $($legacy.script); disable it by hand."
  } else {
    # recon reports it as "scheduled task \Name".
    $taskName = ($legacy.schedule -replace '^scheduled task\s+', '')

    Write-Host "Disabling the legacy task $taskName"
    schtasks /Change /TN "$taskName" /DISABLE | Out-Null

    Write-Host "  Re-enable with: schtasks /Change /TN `"$taskName`" /ENABLE"
  }
}

$Script:Step = 'done'

Write-Host ""
Write-Host "Next:"
Write-Host "  1. Nothing to configure: the server is built in. To see where this"
Write-Host "     machine keeps its files:"
Write-Host "       $InstallDir\sion-backup.exe paths"
if ($legacy) {
Write-Host "     If this machine's existing bucket is being adopted, that has to"
Write-Host "     be set up on the server FIRST - jroedel/eumaeus#112."
}
Write-Host "  2. In Eumaeus, choose 'Enrol a computer', pick the owner and the"
Write-Host "     bucket, and bring the code over. It lasts fifteen minutes:"
Write-Host "       $InstallDir\sion-backup.exe enroll --code XXXX-XXXX"
Write-Host "     Print the restore card it produces and give it to the owner."
Write-Host "  3. Start it:"
Write-Host "       Start-ScheduledTask -TaskName sion-backup"
Write-Host "  4. Check it:"
Write-Host "       $InstallDir\sion-backup.exe doctor"
Write-Host "       http://127.0.0.1:7391/"
if ($legacy -and -not $DisableLegacyTask) {
Write-Host "  5. ONLY after a verified backup, turn the old one off:"
Write-Host "       .\install.ps1 -DisableLegacyTask"
}
