package main

import (
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"

	"github.com/jroedel/sion-backup/foundation/paths"
)

// Where this program keeps its things is a per-user question, and sudo answers
// it wrong.
//
// # The evening this cost
//
// A laptop with two accounts. Somebody ran `sudo sion-backup adopt-enroll`
// from the second one — reasonably, because adopt.go tells them to re-run
// elevated when the old backup script cannot be read. sudo reset HOME,
// foundation/paths resolved to /root/.local/share/sion-backup, and the claim
// was spent writing a machine token there. Running the same binary as
// themselves afterwards found no token and reported a machine that was not
// enrolled. The service would not start either, because it is a user service
// and they were in a `su -` shell with no session bus. Nothing had failed
// loudly at any point, and an enrollment code is good once.
//
// handoff already knew — it declines to start the service when SUDO_USER is
// set, and prints the command to run as the right account. It knew too late:
// by the time it printed that, the code had been spent.
//
// So the question is asked at the front of the two commands that write a
// token, where the answer is still worth something.

// elevatedBySudo reports whether this process is root because somebody typed
// sudo, rather than because it is a service.
//
// It matters because the daemon is a per-user service reading a per-user data
// directory: `systemctl --user` from a root shell addresses root's own
// services, and a browser opened as root lands on the wrong desktop or on
// none.
func elevatedBySudo() bool {
	return os.Getenv("SUDO_USER") != ""
}

// refuseElevatedEnrollment stops an enrolment that would be written by the
// wrong account.
//
// Only sudo, and only when nobody has named a data directory. Being root is
// not itself the mistake: a system-wide install is what this should grow into,
// and deploy/systemd/sion-backup.service says what it needs first — a machine
// account, cap_dac_read_search on restic, and machine-scope paths. Being root
// *because somebody typed sudo* is different. It means the files to be backed
// up belong to the account they typed it from.
//
// SION_BACKUP_DATA_DIR is the way past it rather than a flag, because it is
// exactly the thing being got wrong. Somebody who names the directory has said
// where this machine keeps its enrolment, and there is nothing left to guess.
func refuseElevatedEnrollment(command string) error {
	if !elevatedBySudo() || os.Getenv(paths.DataDirEnv) != "" {
		return nil
	}

	dir := "root's own home directory"
	if p, err := paths.Resolve(); err == nil {
		dir = p.DataDir
	}

	return fmt.Errorf(`refusing to enrol under sudo.

The enrolment would be written to

    %s

as root, and the daemon does not run as root — it runs as the person whose
files are backed up. So it would find either nothing there at all, or files it
cannot write. Either way the enrollment code would be spent and this machine
would still report that it is not enrolled.

Run it as %s, the account you typed sudo from:

    sion-backup %s ...

If root was only needed to read the old backup script, this reports on what is
there and writes nothing:

    sudo sion-backup recon

And if root's own files really are what should be backed up, say where this
machine keeps its enrolment and that will be taken at its word:

    SION_BACKUP_DATA_DIR=... sudo -E sion-backup %s ...`,
		dir, os.Getenv("SUDO_USER"), command, command)
}

// handBackToSudoUser gives the data directory to the account that typed sudo.
//
// The companion to the refusal above, for the one case that legitimately needs
// both: a legacy install only root can read, being adopted on behalf of a
// person who is not root. Naming SION_BACKUP_DATA_DIR gets past the refusal —
// it says which account's installation is meant — and without this, what it
// would get past the refusal to produce is root-owned files in somebody's home
// that the daemon, running as them, cannot write. That is the same bug wearing
// a different hat.
//
// Best effort, and deliberately not fatal. The enrolment has already happened
// by the time this runs; failing the command here would report a machine as
// unenrolled when its token is on the disk and its code is spent. Doctor will
// say so, and a chown fixes it.
func handBackToSudoUser(p paths.Paths, log *slog.Logger) {
	if !elevatedBySudo() {
		return
	}

	uid, uerr := strconv.Atoi(os.Getenv("SUDO_UID"))
	gid, gerr := strconv.Atoi(os.Getenv("SUDO_GID"))

	if uerr != nil || gerr != nil {
		return
	}

	err := filepath.WalkDir(p.DataDir, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		return os.Chown(path, uid, gid)
	})
	if err != nil {
		log.Warn("the data directory could not be given back to the account that will "+
			"use it", "dir", p.DataDir, "account", os.Getenv("SUDO_USER"), "err", err)

		return
	}

	fmt.Printf("\n%s now owns %s, so the service can read it as that account.\n",
		os.Getenv("SUDO_USER"), p.DataDir)
}
