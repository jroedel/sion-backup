package statusapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jroedel/sion-backup/business/domain/plan/planbus"
)

// The program's own two buttons: install a newer build, and restart into it.
//
// # Why these are a pair and not one
//
// Installing and running are separate events here, and the gap between them
// is not an implementation detail. `Updater.Apply` downloads the release,
// verifies its hash, smoke-tests the binary and swaps it into place — and the
// process carries on running the old one, because a program cannot replace
// itself while executing. The new build starts when this one exits and the
// service manager starts it again.
//
// That exit is the thing worth putting behind its own button. It is not
// instant and it is not uniform: systemd waits RestartSec=30s, launchd starts
// the replacement immediately, and the Windows scheduled task retries on a
// five-minute interval. For those minutes there is no daemon, no status page
// and no scheduled backup. That is a fine thing to do on purpose and a poor
// thing to have happen while somebody is reading a page.
//
// # Why the restart is an exit and not an exec
//
// Because the service manager is already the thing that starts this program,
// on all three platforms, and it restarts on exit. Re-execing would mean
// this process staying alive as a parent that the service manager does not
// know about, and a supervisor supervising the wrong pid is how a service
// ends up unrestartable. See cmd/sion-backup/update.go and updateExitCode:
// the exit is non-zero deliberately, because launchd's KeepAlive is
// SuccessfulExit=false and a clean exit there would mean staying stopped.

// UpdateOutcome is what a manual check did.
//
// Three states from two fields, and none of them is an error:
//
//	Unavailable != ""   no check happened, and this says why
//	Installed   != ""   a newer build is on the disk, awaiting a restart
//	both empty          this machine is running the newest release
//
// A reason rather than a sentinel error because none of these is a failure.
// "Updates are switched off" is a deployment somebody chose, and rendering it
// through the same path as "the download was corrupt" would make the page
// shout at a person about a setting they meant.
type UpdateOutcome struct {
	// Installed is the version now on the disk, and empty when nothing was.
	Installed string

	// Unavailable says why no check was possible, in a sentence fit to show
	// somebody. Empty when a check did happen.
	Unavailable string
}

// programView is the settings page's last section.
type programView struct {
	// Version is what is running now, which after an install is NOT the
	// newest version on the disk. Both are shown for exactly that reason.
	Version string

	// Source is where updates come from, or empty when none do.
	Source string

	// Installed is a build waiting for a restart, and the whole reason the
	// restart button is worth pressing.
	Installed string

	// Newest says the check found nothing to do.
	Newest bool

	// Unavailable is why the check that just ran installed nothing, in a
	// sentence fit to show somebody: updates switched off, a backup in
	// progress, a binary this process may not replace.
	//
	// Distinct from the button not being offered at all. A machine with no
	// update source says so in its facts and shows no button, because a
	// button guaranteed to do nothing is worse than none. This field is for a
	// press that happened and could not finish.
	Unavailable string

	// Refused is every version this machine installed, could not keep
	// running, and put back. It is the answer to "why is this machine a
	// version behind" and it belongs beside the button that seems not to be
	// working.
	Refused []string

	// Restarting is set once the exit has been scheduled. The page then says
	// so and reloads itself, because a redirect would be served by a process
	// that is about to stop existing.
	Restarting bool

	// RestartIn is how long the service manager is expected to take. Shown as
	// a sentence and used as the reload interval.
	RestartIn time.Duration

	// Problem is a refusal in the person's own words: a backup in progress,
	// or a daemon nothing would start again.
	Problem string
}

// RestartSeconds is the reload interval, with a margin.
//
// A margin, because a page that reloads exactly when the service manager is
// due to start would show an error most times it ran the race, and "it is
// broken" is a worse thing to be told than "wait another moment".
func (v programView) RestartSeconds() int {
	if v.RestartIn <= 0 {
		return 0
	}

	return int((v.RestartIn + 10*time.Second).Seconds())
}

// RestartWait is the same figure as a phrase.
//
// Its own arithmetic and not planbus.Span, which counts in days because it is
// written for the age of a bucket. Asked for thirty seconds it answers "0
// days", which is how the first version of this page told somebody their
// daemon would be back in about none of them.
func (v programView) RestartWait() string {
	switch secs := int(v.RestartIn.Seconds()); {
	case secs <= 0:
		return "in a moment"

	case secs < 90:
		return fmt.Sprintf("in about %d seconds", secs)

	// "Within", not "in", from here up: a scheduled task retrying on a
	// five-minute interval may start the replacement at any point inside it,
	// and promising the far end of a range as if it were the near one is how
	// a page that meant to reassure ends up looking wrong.
	case secs < 120:
		return "within about two minutes"

	default:
		return fmt.Sprintf("within about %d minutes", (secs+59)/60)
	}
}

// CanCheck reports whether checking is possible on this machine at all.
//
// False means no update source: either this build has none or the config
// switched it off. The facts above the buttons say which, so the button is
// omitted rather than offered as something that can only ever apologise.
func (v programView) CanCheck() bool { return v.Source != "" }

// program builds the section, with nothing having happened yet.
func (s *Server) program() programView {
	v := programView{Version: s.cfg.Version}

	if s.cfg.UpdateSource != nil {
		v.Source, v.Refused = s.cfg.UpdateSource()
	}

	return v
}

// checkForUpdate installs a newer build if there is one.
//
// It downloads as soon as it sees one rather than reporting that one exists:
// somebody who pressed this wants the new version on the disk, and a two-step
// "there is an update" / "now fetch it" spends a click on a question nobody
// asked. What it does not do is start running it. See the head of this file.
func (s *Server) checkForUpdate(w http.ResponseWriter, r *http.Request) {
	view := s.program()

	if s.cfg.CheckForUpdate == nil {
		view.Unavailable = "this build cannot check for updates"

		s.renderProgram(w, r, view)

		return
	}

	// A deadline of its own, and a generous one: this downloads a release,
	// verifies it and runs the new binary once before trusting it. The
	// request context would take all of that away if somebody closed the tab
	// mid-download, leaving a part-written file for the next attempt.
	ctx, cancel := context.WithTimeout(context.Background(), updateDeadline)
	defer cancel()

	out, err := s.cfg.CheckForUpdate(ctx)
	if err != nil {
		s.fail(w, r, "checking for a newer version", err)

		return
	}

	switch {
	case out.Unavailable != "":
		view.Unavailable = out.Unavailable
	case out.Installed != "":
		view.Installed = out.Installed
	default:
		view.Newest = true
	}

	// Re-read, because a refusal list can have grown: a version that was
	// installed, would not stay running and was put back is added here.
	if s.cfg.UpdateSource != nil {
		_, view.Refused = s.cfg.UpdateSource()
	}

	s.renderProgram(w, r, view)
}

// updateDeadline bounds a manual check. Long enough for a release to come
// down a domestic line, short enough that a hung mirror does not hold a
// request open until somebody gives up on the page.
const updateDeadline = 10 * time.Minute

// restart schedules the exit that the service manager turns into a start.
//
// Scheduled rather than immediate, because this response has to reach the
// browser first. The seam returns once the exit is booked and the process
// stays alive long enough to finish writing the page below — which is the
// page that tells somebody their daemon is coming back, and is therefore the
// last one worth losing to a race.
func (s *Server) restart(w http.ResponseWriter, r *http.Request) {
	view := s.program()

	if s.cfg.Restart == nil {
		view.Problem = "this build cannot restart itself"

		s.renderProgram(w, r, view)

		return
	}

	in, err := s.cfg.Restart(r.Context())
	if err != nil {
		// Rendered as a refusal rather than as a failure. Every reason this
		// says no is a true fact about the machine — a backup is running,
		// nothing would start it again — and a person who is told "internal
		// error" learns none of them.
		view.Problem = err.Error()

		s.renderProgram(w, r, view)

		return
	}

	view.Restarting = true
	view.RestartIn = in

	s.renderProgram(w, r, view)
}

// renderProgram redraws the settings page with the program section speaking.
func (s *Server) renderProgram(w http.ResponseWriter, r *http.Request, view programView) {
	plan, err := s.cfg.Plan.Get(r.Context())
	if err != nil && !errors.Is(err, planbus.ErrNoPlan) {
		s.fail(w, r, "reading the backup plan", err)

		return
	}

	if errors.Is(err, planbus.ErrNoPlan) {
		plan.Schedule = planbus.DefaultSchedule()
	}

	s.renderSettingsWith(w, r, plan, false, "", view)
}
