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

$exe  = Join-Path $work 'sion-backup.exe'

$logresticinstall = Join-Path $logs 'restic-install.log'
$loginit          = Join-Path $logs 'init.log'
$logenroll        = Join-Path $logs 'enroll.log'
$logrunOne        = Join-Path $logs 'run1.log'
$logrunTwo        = Join-Path $logs 'run2.log'
$logrestore       = Join-Path $logs 'restore.log'
$logcheck         = Join-Path $logs 'check.log'
$logcheckdata     = Join-Path $logs 'checkdata.log'
$stub = Join-Path $work 'eumaeusstub.exe'

& go build -o $exe ./cmd/sion-backup
if ($LASTEXITCODE -ne 0) { Bad 'the binary did not build'; exit 1 }

& go build -o $stub ./scripts/backup-e2e/eumaeusstub
if ($LASTEXITCODE -ne 0) { Bad 'the stub did not build'; exit 1 }

OK 'built sion-backup.exe and eumaeusstub.exe'

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
times = ["03:00"]
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

$snapshots = & $restic snapshots --json 2>$null | ConvertFrom-Json

if (@($snapshots).Count -eq 1) {
  OK 'one snapshot in the repository'
} else {
  Bad "expected 1 snapshot, found $(@($snapshots).Count)"
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

$snapshots = & $restic snapshots --json 2>$null | ConvertFrom-Json

if (@($snapshots).Count -eq 2) {
  OK 'two snapshots in the repository'
} else {
  Bad "expected 2 snapshots, found $(@($snapshots).Count)"
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
