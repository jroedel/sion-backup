package main

import (
	"os"
	"strings"
	"testing"
)

// TestEnrollingUnderSudoIsRefused.
//
// The bug this closes cost somebody an enrollment code and an evening. `sudo
// sion-backup adopt-enroll` on a machine with two accounts wrote the machine
// token into /root/.local/share/sion-backup, because sudo resets HOME. Nothing
// said so. Run as themselves afterwards, the same binary reported a machine
// that was not enrolled, and the code — good for fifteen minutes, and once —
// was gone.
func TestEnrollingUnderSudoIsRefused(t *testing.T) {
	t.Setenv("SUDO_USER", "jeff")
	t.Setenv("SUDO_UID", "1000")
	t.Setenv("SUDO_GID", "1000")

	err := refuseElevatedEnrollment("adopt-enroll")
	if err == nil {
		t.Fatal("enrolling under sudo was allowed")
	}

	// The message has one job: get somebody to the command that works. A
	// refusal that does not say what to run instead is a wall.
	for _, want := range []string{
		"jeff",              // the account they typed sudo from
		"sion-backup recon", // reads as root and writes nothing
		"SION_BACKUP_DATA_DIR",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n\n%v", want, err)
		}
	}
}

// TestNamingTheDataDirectoryIsTakenAtItsWord.
//
// The way past the refusal, and the one case that needs it: a legacy install
// only root can read, adopted on behalf of somebody who is not root. Naming the
// directory says which account's installation is meant, and there is then
// nothing left to guess.
func TestNamingTheDataDirectoryIsTakenAtItsWord(t *testing.T) {
	t.Setenv("SUDO_USER", "jeff")
	t.Setenv("SION_BACKUP_DATA_DIR", t.TempDir())

	if err := refuseElevatedEnrollment("enroll"); err != nil {
		t.Errorf("a named data directory was still refused: %v", err)
	}
}

// TestAnOrdinaryEnrolmentIsNotRefused. The guard must be invisible to the
// hundred people who never type sudo.
func TestAnOrdinaryEnrolmentIsNotRefused(t *testing.T) {
	for _, key := range []string{"SUDO_USER", "SUDO_UID", "SUDO_GID"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}

	if err := refuseElevatedEnrollment("enroll"); err != nil {
		t.Errorf("an ordinary enrolment was refused: %v", err)
	}
}

// TestBeingRootIsNotItselfTheMistake.
//
// A system-wide install is where this is meant to go — see
// deploy/systemd/sion-backup.service, which names what it needs first. What is
// refused is being root *because somebody typed sudo*, which means the files
// to be backed up belong to the account they typed it from.
func TestBeingRootIsNotItselfTheMistake(t *testing.T) {
	for _, key := range []string{"SUDO_USER", "SUDO_UID", "SUDO_GID"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}

	if elevatedBySudo() {
		t.Error("a root shell with no SUDO_USER was taken for a sudo invocation")
	}
}
