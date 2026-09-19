package main

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"time"

	"github.com/jroedel/sion-backup/app/domain/statusapp"
	"github.com/jroedel/sion-backup/business/domain/diag/diagbus"
	"github.com/jroedel/sion-backup/foundation/selfupdate"
)

// The status page's two program buttons, wired to the machinery that already
// existed for the scheduled path.
//
// Both live here rather than in app/domain/statusapp because both are facts
// about how this program was started and by what — which service manager,
// under which restart policy, with which binary on the disk. The page knows
// none of that and should not learn it.

// restartGrace is how long the process stays alive after a restart is asked
// for.
//
// It exists so the response can be written. The handler returns, Go's server
// flushes the page, and only then does the daemon exit — a page that said
// "your daemon is coming back" and was cut off mid-body would leave somebody
// looking at a broken tab wondering what they had just done.
//
// A second is far more than a loopback response needs and is not worth
// shaving: nothing is waiting on it except the restart it is about to do.
const restartGrace = time.Second

// restartDelay is how long this machine's service manager is expected to take
// to start the replacement.
//
// Three different answers, and the differences are large enough to be worth
// telling somebody about rather than averaging:
//
//	systemd    RestartSec=30s in deploy/systemd/sion-backup.service
//	launchd    immediately — KeepAlive with SuccessfulExit=false
//	Windows    a scheduled task retrying on a five-minute interval
//
// The Windows figure is the reason the restart is a button of its own rather
// than something an update does for you. Five minutes with no daemon is a
// perfectly reasonable thing to choose and a poor thing to have chosen for
// you.
func restartDelay() time.Duration {
	switch runtime.GOOS {
	case "windows":
		return 5 * time.Minute
	case "darwin":
		return 5 * time.Second
	default:
		return 30 * time.Second
	}
}

// updateSource describes where updates come from, for the settings page.
func (d *deps) updateSource() (string, []string) {
	if !d.cfg.Update.On() {
		return "", nil
	}

	u := d.updater()
	if u == nil {
		return d.cfg.Update.Repository, nil
	}

	return d.cfg.Update.Repository, u.Refused()
}

// checkForUpdate is selfUpdate without the throttle and without the silence.
//
// Not a call to selfUpdate, deliberately. That one is written for a machine
// deciding on its own: it checks at most once an hour, and every outcome that
// is not an installed version is a log line nobody reads. A person who has
// just pressed a button is owed the opposite — this minute, and a sentence
// back whatever happens, including "there was nothing to do", which is an
// answer rather than an absence.
//
// What it keeps from selfUpdate is the one refusal that is not about
// impatience: it will not swap the binary while a backup is running. See
// selfUpdate for why a version upgrade must not interrupt somebody's files
// being read.
func (d *deps) checkForUpdate(ctx context.Context) (statusapp.UpdateOutcome, error) {
	if !d.cfg.Update.On() {
		return statusapp.UpdateOutcome{
			Unavailable: "Updates are switched off for this machine in its config.toml.",
		}, nil
	}

	u := d.updater()
	if u == nil {
		return statusapp.UpdateOutcome{
			Unavailable: "This build cannot update itself.",
		}, nil
	}

	if _, running := d.backups.Running(); running {
		return statusapp.UpdateOutcome{
			Unavailable: "A backup is running. Replacing the program while it is " +
				"reading your files is a needless risk, so this waits until the " +
				"run has finished.",
		}, nil
	}

	// The throttle is for the automatic path. Somebody who pressed this means
	// it — the same reasoning, and the same line, as the command in update.go.
	lastUpdateCheck = time.Time{}

	release, swapped, err := u.Apply(ctx)

	switch {
	case errors.Is(err, selfupdate.ErrNotWritable):
		// A deployment fact rather than a failure, exactly as the scheduled
		// path treats it — but said out loud here, because somebody is
		// waiting for an answer and "nothing happened" is not one.
		return statusapp.UpdateOutcome{
			Unavailable: fmt.Sprintf(
				"This copy of the program is installed somewhere it may not "+
					"replace itself (%s). Updating it is the administrator's to do.",
				d.paths.DataDir),
		}, nil

	case errors.Is(err, selfupdate.ErrNoRelease):
		return statusapp.UpdateOutcome{
			Unavailable: "There is no installable release for this platform.",
		}, nil

	case err != nil:
		// Reported as well as returned, on the same argument the scheduled
		// path makes: the failure this guards against is a fleet that quietly
		// stops updating, and a machine whose owner pressed the button and
		// saw it fail is the best evidence anyone will get.
		_ = d.diag.Record(diagbus.Report{
			Kind:   diagbus.KindUpdateFailed,
			Step:   "self-update (asked for)",
			Detail: err.Error(),
		})

		return statusapp.UpdateOutcome{}, err

	case !swapped:
		return statusapp.UpdateOutcome{}, nil
	}

	d.log.Info("updated on request", "was", version, "now", release.Version)

	applied = true

	return statusapp.UpdateOutcome{Installed: release.Version}, nil
}

// errNoBackupInterrupted is the refusal a running backup earns.
var errBackupRunning = errors.New(
	"a backup is running. Restarting now would abandon it part-way and the next " +
		"run would start again from the beginning; this page will let you restart " +
		"as soon as it has finished")

// restart books the exit that the service manager turns into a start.
//
// # Why an exit and not an exec
//
// The service manager is already what starts this program, on all three
// platforms, and all three restart it when it stops. Re-execing would leave
// this process alive as a parent the supervisor does not know about, and a
// supervisor watching the wrong pid is how a service becomes one nobody can
// restart.
//
// The exit is deliberately the update one, exit code 75. launchd's KeepAlive
// is SuccessfulExit=false, so a clean exit on macOS means staying stopped —
// the code that already carries "start me again" across all three platforms
// is the one the self-update path has been using since the beginning, and it
// is proved by scripts/selfupdate-e2e on each of them.
func (d *deps) restart(_ context.Context) (time.Duration, error) {
	if d.restarting == nil {
		return 0, errors.New(
			"this program is not running as a service, so nothing would start it " +
				"again. Stop and start it the way it was started")
	}

	if _, running := d.backups.Running(); running {
		return 0, errBackupRunning
	}

	d.log.Info("restart asked for from the status page",
		"in", restartGrace, "expected_back", restartDelay())

	// After the response. See restartGrace.
	go func() {
		time.Sleep(restartGrace)

		select {
		case d.restarting <- struct{}{}:
		default:
		}
	}()

	return restartDelay(), nil
}
