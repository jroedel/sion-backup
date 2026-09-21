<#
.SYNOPSIS
  Takes a real backup on a real Windows machine, restores it, and checks it.

.DESCRIPTION
  The Windows half of the backup gate. scripts/backup-e2e/run does this on
  Linux, in containers; this does it on the hosted windows-latest runner
  itself, which is a real Windows machine and free on a public repository.

  It asserts the same contract as run-machine, and one thing that script
  cannot: that Volume Shadow Copy actually worked. Every machine in this fleet
  that matters is a Windows machine with Outlook on it, VSS is the reason the
  task is registered with highest privileges, and until this script nothing
  had ever confirmed a shadow copy was taken. A run that quietly falls back is
  recorded degraded, which is amber on a dashboard and a .pst backed up
  mid-write in reality.

  EXIT CODES, carrying the same distinction as the Linux gate:

    0  it backed up, restored and verified
    1  it ran and the program misbehaved -- BLOCK the release
    2  it could not run at all -- DEGRADE, and say so

  The line between 1 and 2 is one question: did this tell us anything about
  the release? A restore that comes back wrong did. A bucket nobody could
  reach did not, and failing a release over somebody else's outage turns a
  safety measure into the thing that stops work.

  It writes under gate/<date>/<run>/windows, which is where the Linux
  reaper already looks, so nothing here deletes from the bucket.
#>

[CmdletBinding()]
param(
  [string] $Region   = $env:WASABI_REGION,
  [string] $Bucket   = $env:WASABI_BUCKET,
  [string] $KeyId    = $env:WASABI_ACCESS_KEY_ID,
  [string] $Secret   = $env:WASABI_SECRET_ACCESS_KEY,
  [string] $Prefix,
  [string] $NodeId   = 'gate-windows',
  [int]    $CorpusMb = 4
)

$ErrorActionPreference = 'Stop'

$script:Fail = 0

function Say  { param($m) Write-Host "`n-- $m" -ForegroundColor White }
function OK   { param($m) Write-Host "   ok   $m" -ForegroundColor Green }
function Bad  { param($m) Write-Host "   FAIL $m" -ForegroundColor Red; $script:Fail = 1 }
function Note { param($m) Write-Host "        $m" -ForegroundColor DarkGray }

# What is actually in the repository. A count that is wrong by one is a
# question; this is the answer to it.
function Show-Snapshots {
  param($Restic)

  $all = @(& $Restic snapshots --json 2>$null | ConvertFrom-Json)

  foreach ($snap in $all) {
    Note ("{0}  {1}  host={2}  paths={3}" -f `
      $snap.short_id, $snap.time, $snap.hostname, ($snap.paths -join ' '))
  }

  return $all.Count
}

# Unreachable is not failure. See the exit codes above.
function Unreachable {
  param($m)

  Write-Host "`n-- could not run: $m" -ForegroundColor Yellow

  exit 2
}

if (-not $Region -or -not $Bucket -or -not $KeyId -or -not $Secret) {
  Unreachable "Wasabi is not configured for this run"
}

if (-not $Prefix) {
  $day = (Get-Date).ToUniversalTime().ToString('yyyy-MM-dd')
  $run = if ($env:GITHUB_RUN_ID) { $env:GITHUB_RUN_ID } else { [guid]::NewGuid().ToString('N').Substring(0, 8) }
  $Prefix = "gate/$day/$run/windows"
}

$endpoint = "s3.$Region.wasabisys.com"
$repoUrl  = "s3:https://$endpoint/$Bucket/$Prefix"
$stubAddr = '127.0.0.1:8088'

$work    = Join-Path $env:RUNNER_TEMP 'sion-gate'
if (-not $env:RUNNER_TEMP) { $work = Join-Path $env:TEMP 'sion-gate' }

$data    = Join-Path $work 'data'
$logs    = Join-Path $work 'logs'
$journal = Join-Path $work 'journal.jsonl'
$ready   = Join-Path $work 'stub-ready'

Remove-Item -Recurse -Force $work -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force -Path $data, $logs | Out-Null

# The real path, not a redirected one. foundation/paths resolving
# %LOCALAPPDATA% is part of what is under test on this platform.
$dataDir = Join-Path $env:LOCALAPPDATA 'sion-backup'

Write-Host "repository: $repoUrl"
Write-Host "data dir:   $dataDir"

# ---------------------------------------------------------------------------
# 1. The programs.
# ---------------------------------------------------------------------------

Say 'Building'

$built = Join-Path $work 'sion-backup.exe'

$logresticinstall = Join-Path $logs 'restic-install.log'
$loginit          = Join-Path $logs 'init.log'
$logenroll        = Join-Path $logs 'enroll.log'
$logrunOne        = Join-Path $logs 'run1.log'
$logrunTwo        = Join-Path $logs 'run2.log'
$logrestore       = Join-Path $logs 'restore.log'
$logcheck         = Join-Path $logs 'check.log'
$logcheckdata     = Join-Path $logs 'checkdata.log'
$stub = Join-Path $work 'eumaeusstub.exe'

& go build -o $built ./cmd/sion-backup
if ($LASTEXITCODE -ne 0) { Bad 'the binary did not build'; exit 1 }

& go build -o $stub ./scripts/backup-e2e/eumaeusstub
if ($LASTEXITCODE -ne 0) { Bad 'the stub did not build'; exit 1 }

OK 'built sion-backup.exe and eumaeusstub.exe'

# ---------------------------------------------------------------------------
# 1b. Install it, with the installer, as a person would.
#
# Not a loose binary run out of a build directory: the thing under test is the
# machine a person is left with, and that machine's program was put there by
# install.ps1 and is started by a scheduled task. -Elevated because the runner
# is an administrator and every real machine here will be installed that way,
# Volume Shadow Copy being the reason.
# ---------------------------------------------------------------------------

Say 'Installing'

& ./deploy/windows/install.ps1 -Binary $built -Elevated -Yes

$exe = Join-Path $dataDir 'sion-backup.exe'

if (-not (Test-Path $exe)) { Bad "the installer left no binary at $exe"; exit 1 }

OK "installed at $exe"

$task = Get-ScheduledTask -TaskName 'sion-backup' -ErrorAction SilentlyContinue

if (-not $task) {
  Bad 'the installer registered no scheduled task'
} elseif ($task.Actions[0].Execute -ne $exe) {
  Bad "the task runs $($task.Actions[0].Execute), not $exe"
} else {
  OK "task registered, running $($task.Principal.UserId) at $($task.Principal.RunLevel)"
}

# ---------------------------------------------------------------------------
# 2. A corpus.
#
# Compressible text and incompressible bytes, because a repository of one is
# not a repository of the other, and a canary the program has no idea is
# special.
# ---------------------------------------------------------------------------

Say 'A corpus'

$rand = [System.Random]::new(20260921)

1..8 | ForEach-Object {
  $buf = [byte[]]::new(64KB)
  $rand.NextBytes($buf)
  [System.IO.File]::WriteAllBytes((Join-Path $data "random-$_.bin"), $buf)
}

1..8 | ForEach-Object {
  Set-Content -Path (Join-Path $data "text-$_.txt") -Value ("the same line over and over`n" * 2000)
}

$canary = [byte[]]::new($CorpusMb * 1MB)
$rand.NextBytes($canary)
[System.IO.File]::WriteAllBytes((Join-Path $data 'canary.bin'), $canary)

Set-Content -Path (Join-Path $data 'changes.txt') -Value 'version one'
Set-Content -Path (Join-Path $data 'skipped.tmp') -Value 'this is excluded by the plan'

$count = (Get-ChildItem $data -File).Count
OK "$count files in $data"

# ---------------------------------------------------------------------------
# 3. A server that answers as Eumaeus.
# ---------------------------------------------------------------------------

Say 'Eumaeus, on loopback'

New-Item -ItemType File -Force -Path $journal | Out-Null

$stubProc = Start-Process -FilePath $stub -PassThru -NoNewWindow `
  -RedirectStandardOutput (Join-Path $logs 'stub.log') `
  -RedirectStandardError  (Join-Path $logs 'stub.err') `
  -ArgumentList @(
    '-addr',    $stubAddr,
    '-repo',    $repoUrl,
    '-key-id',  $KeyId,
    '-secret',  $Secret,
    '-node',    $NodeId,
    '-journal', $journal,
    '-ready',   $ready
  )

for ($i = 0; $i -lt 100; $i++) {
  if (Test-Path $ready) { break }
  Start-Sleep -Milliseconds 100
}

if (-not (Test-Path $ready)) {
  Bad 'the stub never came up'
  Get-Content (Join-Path $logs 'stub.err') -ErrorAction SilentlyContinue | Select-Object -Last 10
  exit 1
}

$resticPw = (Get-Content $ready -Raw).Trim()

OK "serving $repoUrl"

# ---------------------------------------------------------------------------
# 4. The machine's own config.
#
# use_fs_snapshot is left unset on purpose: it defaults to on for Windows, and
# the default is the thing every real machine will run with.
# ---------------------------------------------------------------------------

# Far enough ahead that the backups below are done before it comes round, and
# close enough that this script can wait for it. The daemon's scheduler ticks
# once a minute and reads local time.
$slot = (Get-Date).AddMinutes(5)
$scheduledAt = $slot.ToString('HH:mm')

New-Item -ItemType Directory -Force -Path $dataDir | Out-Null

# A TOML literal string, single-quoted: a Windows path is mostly backslashes
# and a basic string would need every one of them doubled.
@"
node_id    = "$NodeId"
repository = "$repoUrl"
targets    = ['$data']
excludes   = ["**/*.tmp"]
confirmed  = true

[eumaeus]
url = "http://$stubAddr"

[schedule]
times = ["$scheduledAt"]
jitter_minutes = 0
min_interval = "0s"

[update]
enabled = false

[server]
addr = "127.0.0.1:7391"
"@ | Set-Content -Path (Join-Path $dataDir 'config.toml') -Encoding UTF8

OK "wrote $dataDir\config.toml"

# ---------------------------------------------------------------------------
# 5. restic, and the repository.
#
# The binary installs its own pinned restic, verified against a hash compiled
# into it. On Windows that is a zip with a versioned .exe inside, which is a
# different code path from every other platform and has never been exercised
# by anything but a person doing it by hand.
# ---------------------------------------------------------------------------

Say 'restic'

& $exe restic *> $logresticinstall

if ($LASTEXITCODE -ne 0) {
  Get-Content $logresticinstall | Select-Object -Last 5 | ForEach-Object { Note $_ }
  Unreachable 'restic could not be downloaded'
}

$restic = Join-Path $dataDir 'bin\restic.exe'

if (-not (Test-Path $restic)) { Bad "no restic at $restic"; exit 1 }

OK ((& $restic version) | Select-Object -First 1)

$env:RESTIC_REPOSITORY     = $repoUrl
$env:RESTIC_PASSWORD       = $resticPw
$env:AWS_ACCESS_KEY_ID     = $KeyId
$env:AWS_SECRET_ACCESS_KEY = $Secret

& $restic init *> $loginit

if ($LASTEXITCODE -ne 0) {
  Get-Content $loginit | Select-Object -Last 5 | ForEach-Object { Note $_ }
  Unreachable "the repository could not be created at $repoUrl"
}

OK "repository created at $repoUrl"

# ---------------------------------------------------------------------------
# 6. Enrol, and back up.
# ---------------------------------------------------------------------------

Say 'Enrolling'

& $exe enroll --code GATE-TEST *> $logenroll

if ($LASTEXITCODE -ne 0) {
  Bad 'enroll failed'
  Get-Content $logenroll | Select-Object -Last 20 | ForEach-Object { Note $_ }
  exit 1
}

OK "enrolled as $NodeId"

Say 'First backup'

& $exe run -v *> $logrunOne

if ($LASTEXITCODE -ne 0) {
  Bad 'run 1 failed'
  Get-Content $logrunOne | Select-Object -Last 20 | ForEach-Object { Note $_ }
} else {
  OK 'run 1 finished'
}

$count = Show-Snapshots $restic

if ($count -eq 1) {
  OK 'one snapshot in the repository'
} else {
  Bad "expected 1 snapshot, found $count"
}

# ---------------------------------------------------------------------------
# 7. What the machine reported, and whether the shadow copy happened.
# ---------------------------------------------------------------------------

Say 'What was reported'

$events = Get-Content $journal | Where-Object { $_ } | ForEach-Object { $_ | ConvertFrom-Json }
$runs   = $events | Where-Object { $_.kind -eq 'run' -and $_.body.phase -eq 'finished' }

if (-not $runs) {
  Bad 'the machine reported no finished run'
} else {
  $last = @($runs)[-1]

  if ($last.body.verified -eq $true) {
    OK 'the run reported itself verified'
  } else {
    Bad 'the run did not report a successful verification'
    Note 'the program restores a nonce file and compares bytes on every run'
  }

  # The Windows assertion, and the reason this script exists. Volume Shadow
  # Copy is what a machine with Outlook on it needs; a run that could not take
  # one carries on without it and records degraded, which nothing before this
  # would have noticed.
  switch ($last.body.outcome) {
    'success' {
      OK 'outcome success: the shadow copy was taken'
    }
    'degraded' {
      Bad 'outcome degraded: Volume Shadow Copy fell back'
      Note $last.body.message
      Note 'every open file in this run was read from the live tree'
    }
    default {
      Bad "outcome $($last.body.outcome): $($last.body.message)"
    }
  }

  if ($last.body.os -match 'windows') {
    OK "reported itself as $($last.body.os)"
  } else {
    Bad "reported its platform as '$($last.body.os)'"
  }
}

# ---------------------------------------------------------------------------
# 8. Change things, and back up again.
#
# The second backup is the valuable one: a parent snapshot to find, unchanged
# files to skip, and new bytes to add.
# ---------------------------------------------------------------------------

Say 'Changing the corpus'

Set-Content -Path (Join-Path $data 'changes.txt') -Value 'version two'
Remove-Item (Join-Path $data 'text-8.txt')

$fresh = [byte[]]::new(128KB)
$rand.NextBytes($fresh)
[System.IO.File]::WriteAllBytes((Join-Path $data 'added.bin'), $fresh)

OK 'changed one file, deleted one, added one'

Say 'Second backup'

& $exe run -v *> $logrunTwo

if ($LASTEXITCODE -ne 0) {
  Bad 'run 2 failed'
  Get-Content $logrunTwo | Select-Object -Last 20 | ForEach-Object { Note $_ }
} else {
  OK 'run 2 finished'
}

$count = Show-Snapshots $restic

if ($count -eq 2) {
  OK 'two snapshots in the repository'
} else {
  Bad "expected 2 snapshots, found $count"
}

# ---------------------------------------------------------------------------
# 9. Restore, and compare bytes.
#
# The assertion nothing else makes. The program restores its own nonce every
# night; this restores a file the program has no idea is special.
# ---------------------------------------------------------------------------

Say 'Restore'

$restored = Join-Path $work 'restored'
New-Item -ItemType Directory -Force -Path $restored | Out-Null

& $restic restore latest --target $restored *> $logrestore

if ($LASTEXITCODE -ne 0) {
  Bad 'restore failed'
  Get-Content $logrestore | Select-Object -Last 5 | ForEach-Object { Note $_ }
} else {
  OK 'restore completed'
}

# Found by name rather than by rebuilding the path: restic rewrites a Windows
# path on its way into a snapshot, and a test that hard-codes the rewriting is
# a test of this script's guess about it.
$back = Get-ChildItem -Path $restored -Recurse -Filter 'canary.bin' -ErrorAction SilentlyContinue |
  Select-Object -First 1

if (-not $back) {
  Bad 'the restored tree has no canary.bin in it'
} else {
  $before = (Get-FileHash (Join-Path $data 'canary.bin') -Algorithm SHA256).Hash
  $after  = (Get-FileHash $back.FullName -Algorithm SHA256).Hash

  if ($before -eq $after) {
    OK 'the restored file is byte-for-byte identical to the original'
  } else {
    Bad 'THE RESTORED FILE DIFFERS FROM THE ORIGINAL'
    Note 'a backup that cannot be restored exactly is not a backup'
  }
}

$changed = Get-ChildItem -Path $restored -Recurse -Filter 'changes.txt' -ErrorAction SilentlyContinue |
  Select-Object -First 1

if ($changed -and (Get-Content $changed.FullName -Raw) -match 'version two') {
  OK 'the changed file restored at its latest version'
} else {
  Bad 'the changed file did not restore at its latest version'
}

$excluded = Get-ChildItem -Path $restored -Recurse -Filter 'skipped.tmp' -ErrorAction SilentlyContinue

if ($excluded) {
  Bad 'a file the plan excludes is in the backup'
} else {
  OK 'the excluded file is not in the backup'
}

# ---------------------------------------------------------------------------
# 10. The command the binary never calls.
#
# restic check is the only thing that catches a pack file that is present,
# correctly named, and wrong.
# ---------------------------------------------------------------------------

Say 'Repository integrity'

& $restic check *> $logcheck

if ($LASTEXITCODE -eq 0) {
  OK 'restic check passes'
} else {
  Bad 'restic check FAILED - the repository is not sound'
  Get-Content $logcheck | Select-Object -Last 10 | ForEach-Object { Note $_ }
}

& $restic check --read-data-subset=1/52 *> $logcheckdata

if ($LASTEXITCODE -eq 0) {
  OK 'check --read-data-subset=1/52 passes (a week''s slice)'
} else {
  Bad 'reading back a week''s slice of pack data failed'
  Get-Content $logcheckdata | Select-Object -Last 10 | ForEach-Object { Note $_ }
}

# ---------------------------------------------------------------------------
# 11. The scheduled task, and a backup nobody asked for.
#
# Everything above drove the program by hand. A real machine is not driven by
# hand: a task starts the daemon at logon, the daemon watches the clock, and
# the backup happens while nobody is looking. That is the product. Nothing had
# ever watched it happen.
#
# Two claims, and they are separate. That the task starts the daemon is about
# install.ps1 -- the path it registered, the account, the privileges. That a
# backup then happens on its own is about the daemon's scheduler, which is
# reading a slot this script put five minutes into the future before any of
# the backups above ran.
# ---------------------------------------------------------------------------

Say 'The scheduled task'

$before = @(& $restic snapshots --json 2>$null | ConvertFrom-Json).Count

Start-ScheduledTask -TaskName 'sion-backup'

$daemon = $null

for ($i = 0; $i -lt 60; $i++) {
  Start-Sleep -Seconds 1

  $daemon = Get-Process -Name 'sion-backup' -ErrorAction SilentlyContinue

  if ($daemon) { break }
}

if (-not $daemon) {
  Bad 'the task did not start the program'
  Note "task state: $((Get-ScheduledTask -TaskName 'sion-backup').State)"
  Note "last result: $((Get-ScheduledTaskInfo -TaskName 'sion-backup').LastTaskResult)"
} else {
  OK "the task started the program (pid $($daemon[0].Id))"

  $listening = $null

  for ($i = 0; $i -lt 30; $i++) {
    $listening = Get-NetTCPConnection -LocalPort 7391 -State Listen -ErrorAction SilentlyContinue

    if ($listening) { break }

    Start-Sleep -Seconds 1
  }

  if ($listening) {
    OK 'the status page is listening on 127.0.0.1:7391'
  } else {
    Bad 'the program is running but nothing is listening on 127.0.0.1:7391'
  }
}

# And now the clock. The slot is at $scheduledAt; the scheduler ticks once a
# minute, so the wait is until four minutes past it before giving up.
Say "Waiting for the scheduled backup at $scheduledAt"

$deadline = $slot.AddMinutes(4)
$after    = $before

while ((Get-Date) -lt $deadline) {
  Start-Sleep -Seconds 15

  $after = @(& $restic snapshots --json 2>$null | ConvertFrom-Json).Count

  if ($after -gt $before) { break }
}

if ($after -gt $before) {
  OK "the machine backed itself up with nobody driving it ($before -> $after snapshots)"
} else {
  Bad "no scheduled backup by $($deadline.ToString('HH:mm:ss')); still $after snapshots"
  Note "the task starts the daemon; the daemon's own scheduler is what runs a backup"
  Note "slot was $scheduledAt, jitter 0, tick one minute, now $(Get-Date -Format 'HH:mm:ss')"

  $info = Get-ScheduledTaskInfo -TaskName 'sion-backup' -ErrorAction SilentlyContinue

  if ($info) {
    Note "task: last run $($info.LastRunTime), result $($info.LastTaskResult)"
  }

  $still = Get-Process -Name 'sion-backup' -ErrorAction SilentlyContinue

  Note $(if ($still) { "the daemon is still running (pid $($still[0].Id))" } else { "THE DAEMON IS GONE" })

  # What the machine itself says the schedule is. The plan the daemon reads
  # comes from its database, not from the config file this script wrote, and
  # the two disagreeing is the first thing to rule out.
  try {
    $page = Invoke-WebRequest -Uri 'http://127.0.0.1:7391/' -UseBasicParsing -TimeoutSec 10

    foreach ($line in ($page.Content -split "`n")) {
      if ($line -match 'Next scheduled run|Backing up|last backup|paused') {
        Note ($line -replace '<[^>]+>', ' ' -replace '\s+', ' ').Trim()
      }
    }
  } catch {
    Note "the status page did not answer: $_"
  }

  Note '--- doctor ---'
  & $exe doctor 2>&1 | Select-Object -Last 14 | ForEach-Object { Note $_ }
}

Stop-ScheduledTask -TaskName 'sion-backup' -ErrorAction SilentlyContinue
Unregister-ScheduledTask -TaskName 'sion-backup' -Confirm:$false -ErrorAction SilentlyContinue
Get-Process -Name 'sion-backup' -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue

# ---------------------------------------------------------------------------
# Done. The repository is left for the Linux reaper, which deletes everything
# under gate/ older than a week and is the only thing here allowed to delete.
# ---------------------------------------------------------------------------

if ($stubProc -and -not $stubProc.HasExited) {
  Stop-Process -Id $stubProc.Id -Force -ErrorAction SilentlyContinue
}

Write-Host ''

if ($script:Fail -ne 0) {
  Write-Host 'FAILED - do not publish this build' -ForegroundColor Red

  exit 1
}

Write-Host "backed up, restored and verified on Windows: $repoUrl" -ForegroundColor Green

exit 0
