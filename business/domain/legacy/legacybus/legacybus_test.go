package legacybus_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jroedel/sion-backup/business/domain/legacy/legacybus"
)

// linux11 is the shape of the script the 1.1 install notes produce, with the
// bucket and the account renamed. Kept verbatim otherwise, quirks included:
// this is what the parser has to cope with on a real machine.
const linux11 = `#!/bin/bash

unset HISTFILE

echo Preparation backup verification file
BACKUP_DIR=/home/restic/bin
VERIFICATION_FILE="backup_verification_$(date +%Y%m%d_%H%M%S).txt"

echo Setting restic environment variables
export RESTIC_REPOSITORY="s3:https://s3.us-central-1.wasabisys.com/examplebucket"
export AWS_ACCESS_KEY_ID="AHT3050MB6QDREI1L65B"
export AWS_SECRET_ACCESS_KEY="xxxx"
export RESTIC_PASSWORD="xxxx"
export RESTIC_PACK_SIZE=16
export RESTIC_READ_CONCURRENCY=5
NODE_ID=dell3-backup

$BACKUP_DIR/nodes put ${NODE_ID}-start
# /home/restic/bin/restic init
$BACKUP_DIR/restic -o rest.connections=2 --exclude={/dev,/media,/mnt,/proc,/run,/sys,/tmp,/var/tmp} --exclude-file /home/user/Desk/Documents/backup-dell3/excludes.txt backup /

RESTIC_STATUS=$?
`

// windows13 is the .bat, which differs in every way that matters to a
// parser: set rather than export, no quotes, %VAR% references, and a
// password in a file rather than in the script.
const windows13 = `:: sion backup Windows v1.3

set AWS_ACCESS_KEY_ID=xxxx
set AWS_SECRET_ACCESS_KEY=xxxx
set RESTIC_REPOSITORY=s3:https://s3.us-central-1.wasabisys.com/examplebucket
set BACKUP_PATH=C:\Users\backup\Documents\backup
set RESTIC_PASSWORD_FILE=%BACKUP_PATH%\backup.txt
set RESTIC_PACK_SIZE=16
set RESTIC_READ_CONCURRENCY=4
set NODE=gonzalo-backup

%BACKUP_PATH%\restic.exe backup --exclude-file %BACKUP_PATH%\excludes.txt --use-fs-snapshot -o rest.connections=2 C:\Users\Gonzalo
set RESTIC_STATUS=%ERRORLEVEL%
`

func TestParseTheLinuxScript(t *testing.T) {
	got := legacybus.Parse(linux11)

	if want := "s3:https://s3.us-central-1.wasabisys.com/examplebucket"; got.RepositoryURL != want {
		t.Errorf("repository = %q", got.RepositoryURL)
	}

	if got.NodeID != "dell3-backup" {
		t.Errorf("node id = %q", got.NodeID)
	}

	if got.PackSizeMiB != 16 || got.ReadConcurrency != 5 {
		t.Errorf("tuning = %d MiB, %d", got.PackSizeMiB, got.ReadConcurrency)
	}

	if want := "/home/user/Desk/Documents/backup-dell3/excludes.txt"; got.ExcludeFile != want {
		t.Errorf("exclude file = %q, want %q", got.ExcludeFile, want)
	}

	// The whole filesystem, and nothing from --exclude or --exclude-file
	// mistaken for a target.
	if len(got.Targets) != 1 || got.Targets[0] != "/" {
		t.Errorf("targets = %v, want [/]", got.Targets)
	}

	// A real access key ID is a credential even though the secret beside it
	// was scrubbed: somebody migrating this machine needs to go and get both.
	if !got.HasCredentials {
		t.Error("did not notice the credentials in the script")
	}

	if got.UsesFSSnapshot {
		t.Error("reported a filesystem snapshot on a Linux script")
	}
}

func TestParseTheWindowsScript(t *testing.T) {
	got := legacybus.Parse(windows13)

	if want := "s3:https://s3.us-central-1.wasabisys.com/examplebucket"; got.RepositoryURL != want {
		t.Errorf("repository = %q", got.RepositoryURL)
	}

	if got.NodeID != "gonzalo-backup" {
		t.Errorf("node id = %q", got.NodeID)
	}

	// %BACKUP_PATH% expanded, because "the password is in %BACKUP_PATH%\
	// backup.txt" is not a path anybody can open.
	if want := `C:\Users\backup\Documents\backup\backup.txt`; got.PasswordFile != want {
		t.Errorf("password file = %q, want %q", got.PasswordFile, want)
	}

	if want := `C:\Users\backup\Documents\backup\excludes.txt`; got.ExcludeFile != want {
		t.Errorf("exclude file = %q, want %q", got.ExcludeFile, want)
	}

	if len(got.Targets) != 1 || got.Targets[0] != `C:\Users\Gonzalo` {
		t.Errorf("targets = %v", got.Targets)
	}

	// The one that matters most on Windows. Losing it in the migration would
	// be a silent regression on every machine with Outlook open.
	if !got.UsesFSSnapshot {
		t.Error("did not notice --use-fs-snapshot")
	}

	// Every key in this file was replaced with xxxx before it was filed. A
	// placeholder is not a credential, and reporting it as one sends
	// somebody looking for keys that are not there.
	if got.HasCredentials {
		t.Error("reported scrubbed placeholders as credentials")
	}
}

// TestVersionTellsTheVintagesApart. Which notes an install came from decides
// where its files are, and the scripts carry no version of their own.
func TestVersionTellsTheVintagesApart(t *testing.T) {
	if got := legacybus.Version(legacybus.LayoutLinux, linux11, "/home/restic/bin"); got != "1.1" {
		t.Errorf("linux 1.1 read as %q", got)
	}

	v10 := strings.ReplaceAll(linux11, "BACKUP_DIR=/home/restic/bin", "BACKUP_DIR=/home/backup/bin")
	if got := legacybus.Version(legacybus.LayoutLinux, v10, "/home/backup/bin"); got != "1.0" {
		t.Errorf("linux 1.0 read as %q", got)
	}

	v02 := strings.ReplaceAll(v10, "BACKUP_DIR=/home/backup/bin", "")
	if got := legacybus.Version(legacybus.LayoutLinux, v02, "/home/backup/bin"); got != "0.2" {
		t.Errorf("linux 0.2 read as %q", got)
	}

	if got := legacybus.Version(legacybus.LayoutWindows, windows13, `C:\Users\backup`); got != "1.3" {
		t.Errorf("windows read as %q", got)
	}
}

// TestAScriptWithNothingInItSaysNothing: a half-edited template must not
// produce confident wrong answers.
func TestAScriptWithNothingInItSaysNothing(t *testing.T) {
	got := legacybus.Parse("#!/bin/bash\necho hello\n")

	if got.RepositoryURL != "" || got.NodeID != "" || got.HasCredentials || len(got.Targets) != 0 {
		t.Errorf("got %+v from a script with nothing in it", got)
	}
}

// TestParseTakesTheExcludesOffTheCommandLine. The 1.1 script keeps the
// pseudo-filesystems in a brace expansion on the backup command and
// everything else in a file. A migration that carried the file across and not
// this line would spend its first night backing up /proc and /sys, and the
// person who wrote the exclude list would be told their excludes had been
// preserved.
func TestParseTakesTheExcludesOffTheCommandLine(t *testing.T) {
	got := legacybus.Parse(linux11)

	want := []string{"/dev", "/media", "/mnt", "/proc", "/run", "/sys", "/tmp", "/var/tmp"}

	if len(got.Excludes) != len(want) {
		t.Fatalf("excludes = %v, want %v", got.Excludes, want)
	}

	for i := range want {
		if got.Excludes[i] != want[i] {
			t.Errorf("exclude %d = %q, want %q", i, got.Excludes[i], want[i])
		}
	}

	// The exclude FILE is a different thing and must not have been swept up
	// as a pattern: --exclude-file is not --exclude.
	for _, pattern := range got.Excludes {
		if strings.Contains(pattern, "excludes.txt") {
			t.Errorf("the exclude file %q was read as an exclude pattern", pattern)
		}
	}
}

// TestParseFindsNoExcludesWhereThereAreNone guards the brace-expansion
// handling against inventing patterns: the Windows script excludes only
// through a file.
func TestParseFindsNoExcludesWhereThereAreNone(t *testing.T) {
	if got := legacybus.Parse(windows13); len(got.Excludes) != 0 {
		t.Errorf("excludes = %v, want none — this script excludes only through a file", got.Excludes)
	}
}

// TestParseDoesNotCarryCredentials is the invariant that lets recon print a
// Fields, and --json hand one to an installer, without anybody having to
// check first. The secrets come out through ParseCredentials or not at all.
func TestParseDoesNotCarryCredentials(t *testing.T) {
	const script = `export AWS_ACCESS_KEY_ID="AKIAREAL"
export AWS_SECRET_ACCESS_KEY="s3cr3t"
export RESTIC_PASSWORD="hunter2"
export RESTIC_REPOSITORY="s3:https://s3.example.com/bucket"
/home/restic/bin/restic backup /
`

	got := legacybus.Parse(script)

	if !got.HasCredentials {
		t.Fatal("HasCredentials = false, want true: this script carries all three")
	}

	if rendered := fmt.Sprintf("%+v", got); strings.Contains(rendered, "s3cr3t") ||
		strings.Contains(rendered, "hunter2") || strings.Contains(rendered, "AKIAREAL") {
		t.Errorf("a credential reached Fields, which is printed: %s", rendered)
	}
}

// TestParseCredentialsReadsWhatAdoptionNeeds. Adoption opens the legacy
// repository to count what is in it, which needs all three.
func TestParseCredentialsReadsWhatAdoptionNeeds(t *testing.T) {
	const script = `export AWS_ACCESS_KEY_ID="AKIAREAL"
export AWS_SECRET_ACCESS_KEY="s3cr3t"
export RESTIC_PASSWORD="hunter2"
`

	got := legacybus.ParseCredentials(script)

	if string(got.AccessKeyID) != "AKIAREAL" || string(got.SecretAccessKey) != "s3cr3t" ||
		string(got.Password) != "hunter2" {
		t.Fatalf("ParseCredentials read %q/%q/%q", got.AccessKeyID, got.SecretAccessKey, got.Password)
	}

	if !got.Complete() {
		t.Error("Complete = false on a script carrying all three")
	}
}

// TestParseCredentialsTreatsAScrubbedScriptAsEmpty. A copy filed with the
// keys replaced by a run of x's is a thing that exists, and an operator sent
// to adopt a bucket with "xxxx" as the password gets an authentication error
// and no explanation of it.
func TestParseCredentialsTreatsAScrubbedScriptAsEmpty(t *testing.T) {
	got := legacybus.ParseCredentials(linux11)

	if got.Complete() {
		t.Fatalf("Complete = true on the scrubbed script: %q/%q/%q",
			got.AccessKeyID, got.SecretAccessKey, got.Password)
	}

	if len(got.Password) != 0 {
		t.Errorf("password = %q, want empty: a run of x's is not a credential", got.Password)
	}
}

// TestParseCredentialsFindsTheWindowsPasswordFile. The .bat keeps the
// password in a file beside the script and refers to it through %BACKUP_PATH%.
func TestParseCredentialsFindsTheWindowsPasswordFile(t *testing.T) {
	got := legacybus.ParseCredentials(windows13)

	if want := `C:\Users\backup\Documents\backup\backup.txt`; got.PasswordFile != want {
		t.Errorf("password file = %q, want %q", got.PasswordFile, want)
	}

	if len(got.Password) != 0 {
		t.Errorf("password = %q, want empty: it is in the file, and reading that "+
			"file is the caller's decision", got.Password)
	}
}
