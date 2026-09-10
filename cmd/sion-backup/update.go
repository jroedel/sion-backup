package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strings"
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

// supervisor builds an updater for the one job that must be done before
// anything else: deciding what to do about the update this process is already
// running.
//
// Separate from deps.updater because it must not depend on wire(). The failure
// it exists to survive is wire() itself returning an error on a new version —
// a schema migration that does not apply, a config field that no longer
// parses — so nothing that needs a database, a config file or a token can be
// part of it. It needs the path to this binary and nothing else.
//
// It has no Source, which also means it ignores whether self-update is
// switched on. That is deliberate: somebody who turns updates off after a bad
// one has been installed still wants the machine to come back.
func supervisor(log *slog.Logger) *selfupdate.Updater {
	u, err := selfupdate.New(selfupdate.Config{Current: version, Log: log})
	if err != nil {
		log.Warn("cannot supervise the last update", "err", err)

		return nil
	}

	return u
}

// superviseLastUpdate is the first thing a long-running command does.
//
// It reports whether the caller should stop. True means the binary on the disk
// is no longer this one: the previous version has been put back, and exiting
// is how it starts running.
func superviseLastUpdate(ctx context.Context, log *slog.Logger) (*selfupdate.Updater, selfupdate.Start, bool) {
	u := supervisor(log)
	if u == nil {
		return nil, selfupdate.Start{}, false
	}

	start := u.Start(ctx)

	switch start.Outcome {
	case selfupdate.RolledBack:
		// Reported, and this is the report that matters most in the whole
		// program: a machine that gave up on a release is the first warning
		// that the release is bad, and it arrives from a machine that is
		// running again and can say so.
		//
		// diagnostics() rather than deps.diag, because deps does not exist
		// yet and may not be able to.
		_ = diagnostics(log).Record(diagbus.Report{
			Kind: diagbus.KindUpdateRolledBack,
			Step: "self-update",
			Detail: fmt.Sprintf("%s would not stay running after %d starts; went back to %s",
				start.Failed, start.Starts, start.Version),
			PriorVersion: start.Failed,
		})

		return u, start, true

	case selfupdate.Stuck:
		// Nothing was changed, so there is nothing to exit into. Carried on
		// with, because a machine running a bad version is still better than
		// one running nothing — and this process may be about to fail at
		// wire() anyway, which is its own report.
		_ = diagnostics(log).Record(diagbus.Report{
			Kind: diagbus.KindUpdateRolledBack,
			Step: "self-update",
			Detail: fmt.Sprintf("%s would not stay running after %d starts, "+
				"and there is no working previous binary to go back to",
				start.Failed, start.Starts),
			PriorVersion: start.Failed,
		})
	}

	return u, start, false
}

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
  sion-backup update [--check] [--forget]

The daemon does this on its own after each backup, so this command is for
installing a version now rather than tonight, and for seeing why an update
is not happening.

  --check    say what would be installed, and install nothing
  --forget   try a version this machine previously gave up on

A version that installs and then will not stay running is put back, and this
machine will not install it again. --forget is how to say the version was
fine and the machine was not -- a full disk, a half-written database -- or
that the release has been fixed and re-tagged under the same version.
`

func updateCmd(args []string) error {
	check := false
	forget := false

	for _, a := range args {
		switch a {
		case "--check", "-check":
			check = true
		case "--forget", "-forget":
			forget = true
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

	if forget {
		if err := u.Forget(); err != nil {
			return err
		}

		fmt.Println("cleared the list of versions this machine had given up on")
	}

	// Said before anything is installed, because it is the answer to "why is
	// this machine a version behind" and somebody who typed this command is
	// asking exactly that.
	if refused := u.Refused(); len(refused) > 0 {
		fmt.Printf("this machine has given up on: %s\n", strings.Join(refused, ", "))
		fmt.Printf("  it will not install those again; --forget clears the list\n\n")
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
	fmt.Printf("  it is on probation until it has stayed running: if it will not\n")
	fmt.Printf("  start, %s is put back automatically\n", version)

	return nil
}
