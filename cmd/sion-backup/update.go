package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/jroedel/sion-backup/business/domain/diag/diagbus"
	"github.com/jroedel/sion-backup/foundation/selfupdate"
)

// updateExitCode is what the daemon exits with after replacing itself.
//
// Non-zero deliberately, and it is not an error. A Windows scheduled task
// restarts on failure and not on a clean exit, so exiting 0 would leave the
// machine running the old binary until somebody signed in again. systemd's
// Restart=always does not care either way. 75 is EX_TEMPFAIL: "try again",
// which is exactly what is wanted.
const updateExitCode = 75

// updateCheckEvery throttles the check.
//
// The brief is "on each run", and a run is nightly, so this bounds the
// pathological cases rather than the ordinary one: a daemon restarting in a
// loop, or somebody running `sion-backup run` a dozen times while testing.
// GitHub's unauthenticated API allows sixty requests an hour per address, and
// a whole office shares one.
const updateCheckEvery = time.Hour

// applied records that this process replaced its own binary, so a foreground
// run can say so after the backup it just printed.
var applied bool

// selfUpdated reports whether this process installed a new version.
func selfUpdated() bool { return applied }

// lastUpdateCheck is when this process last asked. Process-scoped rather than
// stored: a daemon runs for weeks, and a foreground run that checks once is
// not a problem worth a file to solve.
var lastUpdateCheck time.Time

// updater builds one, or nil when self-update is off or this build should not
// replace itself.
func (d *deps) updater() *selfupdate.Updater {
	if !d.cfg.Update.On() {
		return nil
	}

	u, err := selfupdate.New(selfupdate.Config{
		Source:  selfupdate.GitHub{Repository: d.cfg.Update.Repository},
		Current: version,
		Log:     d.log,
	})
	if err != nil {
		d.log.Debug("self-update is unavailable", "err", err)

		return nil
	}

	return u
}

// selfUpdate replaces this binary if there is a newer release.
//
// Called at the end of a run rather than the start, and never while one is in
// progress. Replacing the program that is currently reading somebody's files
// is a needless risk for a gain of one night, and a backup interrupted by a
// version upgrade is the sort of thing that erodes trust in the upgrade
// rather than in the person who scheduled it.
//
// It reports whether the binary was replaced. Nothing here restarts anything:
// the daemon exits and lets the service manager start the new one, and a
// foreground run simply finishes.
func (d *deps) selfUpdate(ctx context.Context) bool {
	u := d.updater()
	if u == nil {
		return false
	}

	if _, running := d.backups.Running(); running {
		return false
	}

	if time.Since(lastUpdateCheck) < updateCheckEvery {
		return false
	}

	lastUpdateCheck = time.Now()

	release, swapped, err := u.Apply(ctx)

	switch {
	case errors.Is(err, selfupdate.ErrNotWritable):
		// A deployment fact, not a failure: the binary is somewhere this
		// process may not write, and it will be there again in an hour.
		// Logged once per process and never reported, because a machine
		// reporting this hourly would bury the reports that matter.
		d.log.Info("this build cannot update itself", "err", err)

		return false

	case err != nil:
		d.log.Warn("could not update to a newer version", "err", err)

		// Reported, because the failure mode this guards against is a fleet
		// that quietly stops updating: every machine still backing up, every
		// machine two versions behind, and nothing anywhere saying so.
		_ = d.diag.Record(diagbus.Report{
			Kind:   diagbus.KindUpdateFailed,
			Step:   "self-update",
			Detail: err.Error(),
		})

		return false

	case !swapped:
		return false
	}

	d.log.Info("updated", "was", version, "now", release.Version)

	applied = true

	return true
}

// updateUsage documents the manual form of the same thing.
const updateUsage = `sion-backup update — replace this binary with the newest release

Usage:
  sion-backup update [--check]

The daemon does this on its own after each backup, so this command is for
installing a version now rather than tonight, and for seeing why an update
is not happening.

  --check   say what would be installed, and install nothing
`

func updateCmd(args []string) error {
	check := false

	for _, a := range args {
		switch a {
		case "--check", "-check":
			check = true
		case "-h", "--help":
			fmt.Print(updateUsage)

			return nil
		default:
			fmt.Fprint(os.Stderr, updateUsage)

			return fmt.Errorf("unknown flag %q", a)
		}
	}

	ctx, cancel := signalContext()
	defer cancel()

	d, err := wire(ctx, false)
	if err != nil {
		return err
	}
	defer d.close()

	if !d.cfg.Update.On() {
		return errors.New("self-update is switched off in this machine's config.toml")
	}

	u := d.updater()
	if u == nil {
		return errors.New("self-update is not available on this build")
	}

	if check {
		latest, err := selfupdate.GitHub{Repository: d.cfg.Update.Repository}.
			Latest(ctx, runtime.GOOS, runtime.GOARCH)
		if err != nil {
			return err
		}

		if !selfupdate.Newer(version, latest.Version) {
			fmt.Printf("running %s; %s is the latest release, so nothing to do\n",
				version, latest.Version)

			return nil
		}

		fmt.Printf("running %s; %s is available\n  %s\n  sha256 %s\n",
			version, latest.Version, latest.URL, latest.SHA256)

		return nil
	}

	// The throttle is for the automatic path. Somebody who typed this means
	// it.
	lastUpdateCheck = time.Time{}

	release, swapped, err := u.Apply(ctx)
	if err != nil {
		return err
	}

	if !swapped {
		fmt.Printf("running %s, which is current\n", version)

		return nil
	}

	fmt.Printf("updated to %s; restart the service to run it\n", release.Version)

	return nil
}
