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

  WHERE THE BINARY LIVES

  In %LOCALAPPDATA%\sion-backup, which is the account's own profile, and that
  is a decision with a cost on both sides.

  It is there so the machine can update itself. Self-update replaces the
  binary in place, so it needs a directory the account running the program can
  write to. %ProgramFiles% is not one: the task runs as the user, the
  directory is owned by administrators, and Writable() refuses -- correctly.
  A machine installed there takes the version it was installed with and keeps
  it until somebody comes back and reinstalls by hand. Every fix in every
  release after that one is a fix that machine does not get, and this fleet's
  machines are not ones anybody revisits.

  The cost: with -Elevated the task runs with highest privileges, and a binary
  in a directory the user can write to is a binary that anything running as
  that user can replace and have run as an administrator. That is a real
  escalation path and it is the reason %ProgramFiles% was here first. It is
  the same account whose files are being backed up and whose machine token
  sits in the same profile, so an attacker who has it already has a great
  deal -- but not that.

  The fix for both is the one this script's header already describes: machine
  scope paths and a task running as SYSTEM or a dedicated account, with the
  binary somewhere the user cannot write. Until then, a machine that keeps
  itself patched is worth more than one that cannot be reached.

  RUN THIS AS THE ACCOUNT BEING BACKED UP. The task is registered for whoever
  runs this script, the machine token goes in that profile, restic is
  installed into it by "sion-backup enroll", and now the binary lives there
  too. Elevating into a DIFFERENT administrator account puts all four in the
  wrong place -- a backup of the administrator's profile, running as the
  administrator. If the person is not a local administrator, install without
  -Elevated rather than elevating as somebody else.
#>

[CmdletBinding()]
param(
  # The user's own profile, not %ProgramFiles%, so that the machine can
  # update itself. See "Where the binary lives" at the top of this file.
  [string] $InstallDir = "$env:LOCALAPPDATA\sion-backup",
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

# Administrator is needed for what needs it, and not for the install itself.
#
# It used to be needed for all of it, because the binary went into
# %ProgramFiles%. It now goes into the account's own profile, and registering
# a task for yourself is not a privileged act either. What still needs
# elevation is a task that runs with highest privileges, and touching the
# legacy task, which this account did not create.
#
# The distinction matters on exactly the machines this fleet has: a person who
# is not a local administrator would otherwise be told to elevate, would
# elevate as somebody else, and would install a backup of the wrong profile.
$isAdmin = ([Security.Principal.WindowsPrincipal] [Security.Principal.WindowsIdentity]::GetCurrent()
  ).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)

if (-not $isAdmin) {
  if ($Elevated) {
    throw "-Elevated registers the task to run with highest privileges, which needs an elevated PowerShell. Run this elevated AS THIS SAME ACCOUNT, or install without -Elevated and accept that Volume Shadow Copy will be unavailable."
  }

  if ($DisableLegacyTask) {
    throw "-DisableLegacyTask changes a scheduled task this account did not create, which needs an elevated PowerShell."
  }

  Write-Host "Not running as an administrator. Installing into the profile and registering the task with limited privileges."
}

if ($Elevated) {
  Write-Warning "-Elevated runs the task with highest privileges, and $InstallDir is writable by this account. Anything running as this account can replace the binary and have it run as an administrator. See ""Where the binary lives"" in this script."
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

"sion-backup adopt-enroll" is the command that decides it in the direction
that keeps the history. It is step 2 at the end of this script.
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
# which is the part that matters on Windows. Fetching restic from here would
# put it wherever this script happens to be running, which is the right place
# only when this script is being run by the account the task will run as. It
# is left to enrolment so that it cannot be anywhere else.
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

# At logon, and only at logon.
#
# Not at startup as well, which this said for a while and never did: the task
# runs as the signed-in user with LogonType Interactive, and at startup there
# is no interactive session for it to run in. A machine that reboots starts
# the daemon when somebody signs in, and one left signed in never stopped it
# -- the settings below restart it 999 times at five-minute intervals.
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
# Repeated on the command lines below so that a machine whose old install is
# somewhere the notes never mentioned gets an instruction that works as typed.
$legacyDirArg = if ($LegacyDir) { " --legacy-dir `"$LegacyDir`"" } else { "" }

Write-Host "Next:"
Write-Host "  1. Nothing to configure: the server is built in. To see where this"
Write-Host "     machine keeps its files:"
Write-Host "       $InstallDir\sion-backup.exe paths"
if ($legacy) {
Write-Host "  2. Take the old install over. This writes its plan as this machine's"
Write-Host "     own and prints the Eumaeus commands to run, filled in:"
Write-Host "       $InstallDir\sion-backup.exe adopt-enroll$legacyDirArg"
Write-Host "     Run those on the server -- adopt, NOT provision -- then come back"
Write-Host "     with the code. This checks that the bucket handed back is the"
Write-Host "     legacy one, and prints the restore card for the owner:"
Write-Host "       $InstallDir\sion-backup.exe adopt-enroll --code XXXX-XXXX"
} else {
Write-Host "  2. In Eumaeus, choose 'Enrol a computer', pick the owner and the"
Write-Host "     bucket, and bring the code over. It lasts fifteen minutes:"
Write-Host "       $InstallDir\sion-backup.exe enroll --code XXXX-XXXX"
Write-Host "     Print the restore card it produces and give it to the owner."
}
Write-Host "  3. Enrolling starts the task and opens the set-up page. NOTHING is"
Write-Host "     backed up until somebody at this computer answers it -- that is"
Write-Host "     where the folders, the schedule and the first backup are chosen:"
Write-Host "       http://127.0.0.1:7391/setup"
Write-Host "     If it did not start, or enroll was given --no-start:"
Write-Host "       Start-ScheduledTask -TaskName sion-backup"
Write-Host "       $InstallDir\sion-backup.exe doctor"
if ($legacy -and -not $DisableLegacyTask) {
Write-Host "  4. ONLY after that page has produced a verified backup, turn the"
Write-Host "     old one off:"
Write-Host "       .\install.ps1 -DisableLegacyTask"
}
