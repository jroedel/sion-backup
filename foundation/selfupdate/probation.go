package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// # Why a new version is on probation
//
// Everything in selfupdate.go happens before the new binary has ever run as a
// service. The hash proves the download arrived intact; the smoke test proves
// it is the right architecture and answers `version`. Neither proves it works,
// and `version` in particular is answered from main's dispatch before the
// database is opened or the config is read — so a build with a broken
// migration passes every check and then dies on the command the service
// manager actually runs.
//
// That machine is bricked in the way that matters most: it is not running the
// code that reports to the dashboard, and it is not running the code that
// could replace itself. Somebody has to go and stand in front of it.
//
// So a swap is not the end of an update. The new version starts on probation,
// and if it cannot stay running, the previous one is put back.
//
// # What counts as working
//
// Staying up. Not "taking a backup" — a laptop can legitimately go a week
// without one, and three reboots in that week must not be mistaken for a crash
// loop. A daemon that has been running for [probationGrace] is not
// crash-looping, and that is the whole claim being made.
//
// The cost of that choice is a build that starts, runs for an hour and then
// fails at backup time: probation will have settled, and nothing here helps.
// That failure is already visible — it is a failed run on the dashboard,
// reported by a machine that is still up and can still be updated — which is
// exactly the difference that makes it somebody's Tuesday rather than a drive
// to another building.
//
// # Why the state is a file beside the binary
//
// Because the database is the thing most likely to be broken. The failure this
// exists to survive is `wire()` returning an error, so nothing that depends on
// wire() having succeeded can be part of the mechanism — which rules out the
// database, and rules out being called anywhere but the top of a command.

// probationStarts is how many attempts a new version gets.
//
// Three, with systemd's RestartSec=30s, is ninety seconds of trying before the
// previous version is put back. Two would risk giving up on a version that
// lost a race with the network at boot; ten would be five minutes of a machine
// not backing up to learn something known after ninety seconds.
const probationStarts = 3

// probationGrace is how long a new version must stay up to be believed.
//
// Longer than any plausible crash loop and shorter than the gap between a
// person rebooting a laptop and using it. A daemon still running after ten
// minutes has opened its database, read its config, resolved restic and served
// the status page.
const probationGrace = 10 * time.Minute

// maxRefused bounds the list of versions that failed. Ten is more than a fleet
// will ever accumulate; the point of the cap is that nothing beside a binary
// grows without limit.
const maxRefused = 10

// probation is the state a swap leaves behind, as JSON beside the binary.
//
// Readable on purpose. Somebody looking at a machine that is a version behind
// should be able to see why in a text editor, without this program's help.
type probation struct {
	// Installed is the version being tried.
	Installed string `json:"installed"`

	// Previous is what it replaced, and what .old should report.
	Previous string `json:"previous"`

	// Starts counts attempts, including the one in progress.
	Starts int `json:"starts"`

	Began time.Time `json:"began"`
}

func (u *Updater) probationPath() string { return u.exe + ".probation" }
func (u *Updater) refusedPath() string   { return u.exe + ".refused" }

// begin records that a swap just happened.
//
// A failure here is returned but not fatal to the update: the new binary is
// already in place, and refusing to report a successful swap because a
// bookkeeping file could not be written would be the wrong trade. The caller
// logs it; the consequence is a version that is not watched, which is where
// this package was before probation existed.
func (u *Updater) begin(installed, previous string) error {
	blob, err := json.MarshalIndent(probation{
		Installed: installed,
		Previous:  previous,
		Starts:    0,
		Began:     time.Now(),
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("selfupdate: recording probation: %w", err)
	}

	if err := os.WriteFile(u.probationPath(), append(blob, '\n'), 0o600); err != nil {
		return fmt.Errorf("selfupdate: recording probation: %w", err)
	}

	return nil
}

// read loads the probation state, or reports that there is none.
func (u *Updater) read() (probation, bool) {
	blob, err := os.ReadFile(u.probationPath())
	if err != nil {
		return probation{}, false
	}

	var p probation

	if err := json.Unmarshal(blob, &p); err != nil {
		// Unreadable is treated as absent rather than as an error. A corrupt
		// file here would otherwise be a machine that can never start again,
		// which is a worse failure than the one probation is guarding against.
		u.log.Warn("the probation file is unreadable; ignoring it",
			"path", u.probationPath(), "err", err)

		return probation{}, false
	}

	return p, p.Installed != ""
}

// Outcome is what a start found.
type Outcome int

const (
	// Nothing is the ordinary start: no update is being watched.
	Nothing Outcome = iota

	// OnProbation is a version that was installed and has not yet proved it
	// can stay running. The caller must call [Updater.Settle] once it has.
	OnProbation

	// RolledBack is a version that was given up on. The binary on the disk is
	// the previous one, and the caller should exit so that the service manager
	// starts it.
	RolledBack

	// Stuck is a version that failed probation and could not be rolled back,
	// because the previous binary is gone or does not run. Nothing was
	// changed. It is reported and then carried on with, because a machine
	// running a bad version is still better than one running nothing.
	Stuck
)

// Start is what [Updater.Start] found.
type Start struct {
	Outcome Outcome

	// Version is the version now on the disk: the one being tried, or the one
	// restored.
	Version string

	// Failed is the version given up on. Set for RolledBack and Stuck.
	Failed string

	// Starts is how many times the version on probation has been started.
	Starts int
}

// Start decides what to do about the last update, and must be called at the
// top of any long-running command before anything that can fail.
//
// The context bounds the smoke test on the binary being rolled back to, and
// nothing else here can block.
//
// Before wire(), before the database, before the config — the whole point is
// to survive a build for which those do not work. It needs nothing but the
// path to this binary.
func (u *Updater) Start(ctx context.Context) Start {
	p, on := u.read()

	if !on {
		// No update is being watched, so the previous binary — if one is still
		// beside this one, from an update that settled or a rollback that
		// happened — is no longer needed. Removed here rather than after the
		// swap because on Windows the file being replaced is the image of the
		// running process and cannot be deleted until it stops.
		u.removeOld()

		return Start{Outcome: Nothing}
	}

	p.Starts++

	if p.Starts <= probationStarts {
		if err := u.write(p); err != nil {
			u.log.Warn("could not record this start", "err", err)
		}

		u.log.Info("this version is on probation",
			"version", p.Installed, "start", p.Starts, "of", probationStarts)

		return Start{Outcome: OnProbation, Version: p.Installed, Starts: p.Starts}
	}

	// Out of attempts.
	restored, err := u.rollBack(ctx, p)

	// Either way probation is over: a machine that keeps trying to roll back
	// every thirty seconds is its own kind of broken.
	_ = os.Remove(u.probationPath())

	if err != nil {
		u.log.Error("gave up on this version and could not go back",
			"version", p.Installed, "err", err)

		return Start{Outcome: Stuck, Version: p.Installed, Failed: p.Installed, Starts: p.Starts}
	}

	u.log.Warn("gave up on a version that would not stay running; went back",
		"gave_up_on", p.Installed, "now", restored, "starts", p.Starts)

	return Start{Outcome: RolledBack, Version: restored, Failed: p.Installed, Starts: p.Starts}
}

// write saves the probation state.
func (u *Updater) write(p probation) error {
	blob, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(u.probationPath(), append(blob, '\n'), 0o600)
}

// Settle records that the version on probation works.
//
// Called once the daemon has been up for [probationGrace], or immediately
// after a foreground backup has completed — which is a stronger signal than
// staying up, and worth taking when it is available.
//
// Safe to call when there is no probation, and safe to call twice.
func (u *Updater) Settle() {
	p, on := u.read()
	if !on {
		return
	}

	if err := os.Remove(u.probationPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		u.log.Warn("could not clear probation", "err", err)

		return
	}

	// Only now, because until this moment it was the way back.
	u.removeOld()

	u.log.Info("this version is working; keeping it", "version", p.Installed)
}

// Grace is how long a caller should wait before settling. Exported so that the
// daemon's timer and this package's reasoning cannot drift apart.
func Grace() time.Duration { return probationGrace }

// rollBack puts the previous binary back, and reports which version that is.
//
// It refuses rather than trying if the previous binary is missing or does not
// run. A machine on a version that will not start is bad; a machine with no
// working binary at all cannot even be updated out of it.
func (u *Updater) rollBack(ctx context.Context, p probation) (string, error) {
	old := u.exe + ".old"

	if _, err := os.Stat(old); err != nil {
		return "", fmt.Errorf("there is no %s to go back to: %w", old, err)
	}

	// The same check the new binary had to pass, applied to the old one. It
	// passed once, but the disk it is on is the disk that just produced a
	// version which would not start.
	if err := smokeTest(ctx, old, p.Previous); err != nil {
		return "", fmt.Errorf("the previous binary does not run: %w", err)
	}

	// Recorded before the swap, so that a crash in the middle cannot leave a
	// machine that will cheerfully install the same bad version again in an
	// hour.
	if err := u.refuse(p.Installed); err != nil {
		u.log.Warn("could not record the refused version", "err", err)
	}

	// Three renames rather than two, for the same reason swap() has its
	// shape: on Windows the running image can be renamed but not overwritten,
	// and the bad binary is this process. It becomes the new .old and is
	// removed by the next start that is not on probation.
	staged := u.exe + ".back"

	if err := os.Rename(old, staged); err != nil {
		return "", fmt.Errorf("staging the previous binary: %w", err)
	}

	if err := os.Rename(u.exe, old); err != nil {
		_ = os.Rename(staged, old)

		return "", fmt.Errorf("moving the failed binary aside: %w", err)
	}

	if err := os.Rename(staged, u.exe); err != nil {
		// Put the failed one back rather than leave nothing at all. It does
		// not start, but it is still what the service manager expects to
		// find, and `sion-backup update` from a working copy can fix it.
		if back := os.Rename(old, u.exe); back != nil {
			return "", fmt.Errorf("restoring the previous binary failed (%w) "+
				"and there is now no binary at %s (%w)", err, u.exe, back)
		}

		return "", fmt.Errorf("restoring the previous binary: %w", err)
	}

	return p.Previous, nil
}

// removeOld deletes the previous binary, if one is beside this one.
func (u *Updater) removeOld() {
	if err := os.Remove(u.exe + ".old"); err != nil && !errors.Is(err, os.ErrNotExist) {
		u.log.Debug("could not remove the previous binary", "err", err)
	}
}

// refuse adds a version to the list this machine will not install again.
func (u *Updater) refuse(version string) error {
	refused := u.Refused()

	for _, r := range refused {
		if r == version {
			return nil
		}
	}

	refused = append(refused, version)

	if len(refused) > maxRefused {
		refused = refused[len(refused)-maxRefused:]
	}

	return os.WriteFile(u.refusedPath(), []byte(strings.Join(refused, "\n")+"\n"), 0o600)
}

// Refused lists the versions this machine gave up on.
//
// Exported because it is the answer to "why is this machine a version behind",
// and a machine that cannot say so is a machine somebody has to guess about.
// doctor prints it; the status page shows it.
func (u *Updater) Refused() []string {
	blob, err := os.ReadFile(u.refusedPath())
	if err != nil {
		return nil
	}

	var out []string

	for line := range strings.Lines(string(blob)) {
		if v := strings.TrimSpace(line); v != "" {
			out = append(out, v)
		}
	}

	return out
}

// refuses reports whether a version is one this machine has given up on.
func (u *Updater) refuses(version string) bool {
	for _, r := range u.Refused() {
		if r == version {
			return true
		}
	}

	return false
}

// Forget clears the refusal list, so a version that failed here can be tried
// again.
//
// For the case where the version was fine and the machine was not — a full
// disk, a half-written database — and for a release that has been fixed and
// re-tagged. `sion-backup update --forget`.
func (u *Updater) Forget() error {
	if err := os.Remove(u.refusedPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("selfupdate: clearing the refused versions: %w", err)
	}

	return nil
}
