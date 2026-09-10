package legacybus_test

import (
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
