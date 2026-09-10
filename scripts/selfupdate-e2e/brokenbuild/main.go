// Command brokenbuild is a release that passes every check self-update makes
// and then does not work.
//
// It answers "version" with whatever version it was stamped with, which is the
// entire smoke test in foundation/selfupdate — `version` returns from main's
// dispatch before wire() runs, so it opens no database, reads no config and
// touches no restic. Then it fails the way a real bad build fails: on the
// command the service manager actually runs.
//
// # Why it contains the probation logic
//
// Because a real broken release would. The rollback in
// foundation/selfupdate/probation.go runs inside the NEW binary, at the top of
// the daemon command, before anything that can fail — so a build that dies at
// wire() rolls itself back, and a build that dies before reaching that code
// cannot.
//
// That is a genuine limit of the design and not an artefact of this stub:
// self-update can only recover from a version broken *after* the point where
// the check runs. What guards the other side is the smoke test, which runs the
// downloaded binary before installing it and so catches anything that fails at
// package init or in main's first few lines.
//
// So this models the case that matters and is honest about which one it is: a
// build whose database migration does not apply. It runs the probation check
// exactly as cmd/sion-backup/daemon.go does, and then fails where wire() would.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"runtime"

	"github.com/jroedel/sion-backup/foundation/selfupdate"
)

// version is stamped the same way the real binary's is.
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Printf("sion-backup %s (%s/%s, %s)\n", version, runtime.GOOS, runtime.GOARCH, runtime.Version())

		return
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	u, err := selfupdate.New(selfupdate.Config{Current: version, Log: log})
	if err != nil {
		log.Warn("cannot supervise the last update", "err", err)
	}

	if u != nil {
		switch start := u.Start(context.Background()); start.Outcome {
		case selfupdate.RolledBack:
			log.Warn("went back to the previous version; exiting so the service manager starts it",
				"gave_up_on", start.Failed, "now", start.Version)

			// 75, as cmd/sion-backup does: EX_TEMPFAIL, which a Windows
			// scheduled task restarts on and systemd does not care about.
			os.Exit(75)

		case selfupdate.Stuck:
			log.Error("gave up on this version and could not go back", "version", start.Failed)
		}
	}

	// Where a bad build actually dies: after the service manager has started
	// it, having already replaced the one that worked, and after the check
	// above has had its chance.
	fmt.Fprintln(os.Stderr, "sion-backup: opening the database: migration 7: no such column: verified_at")
	os.Exit(1)
}
