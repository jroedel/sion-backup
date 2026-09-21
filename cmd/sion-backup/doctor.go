package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jroedel/sion-backup/business/domain/backup/backupbus"
	"github.com/jroedel/sion-backup/business/domain/credential/credentialbus"
	"github.com/jroedel/sion-backup/business/domain/plan/planbus"
	"github.com/jroedel/sion-backup/foundation/restic"
)

// doctorCmd checks everything a backup needs and says what is wrong.
//
// It exists because the alternative is a support call that begins "it says it
// failed" and takes an hour. Every check names what it looked at and, when it
// fails, what to do — a check that only says "FAIL" has moved the problem
// rather than solved it.
//
// It exits non-zero if anything failed, so it can be the thing a monitoring
// script runs.
func doctorCmd(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	verbose := fs.Bool("v", false, "verbose logging")

	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, cancel := signalContext()
	defer cancel()

	d, err := wire(ctx, *verbose)
	if err != nil {
		// Wiring failing is itself the diagnosis. It no longer covers the
		// commonest fault — a missing restic is a thing this program installs
		// rather than reports — so what is left here is a data directory that
		// cannot be written or a database that will not open.
		fmt.Printf("FAIL  starting up: %v\n", err)
		os.Exit(1)
	}
	defer d.close()

	c := &checker{}

	c.check("paths", func() (string, error) {
		return d.paths.DataDir, nil
	})

	c.check("config file", func() (string, error) {
		if !d.cfgFound {
			return "none at " + d.paths.Config + " (fine once enrolled)", nil
		}

		return d.paths.Config, nil
	})

	// One version across the fleet, or a named exception. Anything else is a
	// machine whose snapshots were written by a restic nobody chose, which is
	// how "it works here and not there" starts.
	c.check("restic", func() (string, error) {
		version, err := d.restic.InstalledVersion(ctx)

		switch {
		case err != nil && d.restic.Managed():
			return "", fmt.Errorf("no working restic at %s: %w\n"+
				"      run \"sion-backup restic\" to install the pinned %s "+
				"(the next backup would do it anyway)",
				d.restic.Bin(), err, restic.PinnedVersion)

		case err != nil:
			return "", fmt.Errorf("the config file names %s and it does not run: %w",
				d.restic.Bin(), err)

		case !d.restic.Managed():
			// Not a failure. Somebody named a path, which is allowed and is
			// how a machine runs a build we do not ship; it is reported so
			// that a version this fleet has never tested is never a surprise.
			return version + " at " + d.restic.Bin() +
				" (named in the config file, so its version is not managed here)", nil

		case version != restic.PinnedVersion:
			return "", fmt.Errorf("this machine has restic %s and the fleet runs %s; "+
				"the next backup replaces it, or run \"sion-backup restic\" now",
				version, restic.PinnedVersion)
		}

		return version + " at " + d.restic.Bin(), nil
	})

	c.check("enrollment", func() (string, error) {
		if d.machineToken == "" {
			// Where the code comes from and how long it lasts, because this
			// line is often the first thing anybody reads on a machine that
			// will not back up, and "run enroll --code <code>" answers none
			// of the questions somebody without a code then has.
			return "", fmt.Errorf("no machine token in %s; get a code from Eumaeus — good "+
				"for fifteen minutes, and once — and run \"sion-backup enroll --code <code>\"",
				d.paths.Token)
		}

		if !d.creds.Enrolled() {
			return "", fmt.Errorf("a token is present but no Eumaeus server is configured in %s",
				d.paths.Config)
		}

		return "machine token present; credentials are fetched per run and not stored", nil
	})

	// Reported rather than judged. What a deployment implements is not this
	// machine's business to have an opinion about — a fleet mid-upgrade is an
	// ordinary state — but it is the first thing somebody wants when a feature
	// is missing and nobody can say whether the client or the server is
	// behind. Asking is also the only way to know: a 404 from this API means
	// either "no such path here" or something documented about this machine,
	// and they are identical on the wire (jroedel/eumaeus#144).
	if d.machine.Available() {
		c.check("server", func() (string, error) {
			state, err := d.machine.State(ctx)
			if err != nil {
				return "", err
			}

			if !state.KnowsWhatItServes() {
				return "reachable; it is older than the block that says which " +
					"endpoints it implements, so nothing can be assumed about them", nil
			}

			serves := "serves " + strconv.Itoa(len(state.Routes)) + " endpoints"

			if state.Serves(http.MethodGet, "/machines/me/disclosures") {
				return serves + ", including the record of who has opened this " +
					"machine's password", nil
			}

			return serves + ", not including the record of who has opened this " +
				"machine's password", nil
		})
	}

	plan, planErr := d.plan.Get(ctx)

	c.check("backup plan", func() (string, error) {
		if errors.Is(planErr, planbus.ErrNoPlan) {
			// Two different machines land here and they need different
			// answers. One has never been enrolled. The other has a token,
			// and telling that one to enrol sends it at a command that will
			// refuse — which is the dead end that made this distinction worth
			// drawing: before v0.6.0, enrolment could store the token and
			// drop the plan, leaving a machine that said it was not enrolled
			// and could not be enrolled again.
			if d.machineToken != "" {
				return "", fmt.Errorf("this machine has a token but no plan. Starting "+
					"the daemon recovers it from Eumaeus; if that has already been "+
					"tried, %s/setup and the log say why", d.statusPage())
			}

			return "", errors.New("this machine has no plan; run \"sion-backup enroll\"")
		}

		if planErr != nil {
			return "", planErr
		}

		if plan.Paused {
			return "", errors.New("backups are PAUSED; turn them back on at the status page")
		}

		// A failure rather than a note, because from here it is
		// indistinguishable from a machine that has simply stopped: the
		// scheduler will never start a run, and nothing else on this page
		// would say why. It is the ordinary state of a machine enrolled ten
		// minutes ago, and a bad one for a machine enrolled last month.
		if !plan.Confirmed() {
			return "", fmt.Errorf("nobody at this machine has chosen what to back up "+
				"yet, so nothing is scheduled. Open %s/setup", d.statusPage())
		}

		return fmt.Sprintf("%s → %s, %d target(s), at %v",
			plan.NodeID, plan.Repository, len(plan.Targets), plan.Schedule.Times), nil
	})

	// Before everything that reaches the network, and outside the block that
	// needs a plan: a machine that cannot resolve names cannot enrol either,
	// and the checks that follow all fail in ways that point somewhere else.
	// "There is no repository at that URL" is what a machine with a broken
	// resolver says, and it sends somebody to the bucket.
	c.check("name resolution", func() (string, error) {
		var hosts []string

		if u, err := url.Parse(d.cfg.EumaeusURL()); err == nil && u.Hostname() != "" {
			hosts = append(hosts, u.Hostname())
		}

		if planErr == nil {
			hosts = append(hosts, restic.HostOf(plan.Repository))
		}

		return resolves(ctx, hosts)
	})

	if planErr == nil {
		c.check("targets exist", func() (string, error) {
			var missing []string

			for _, t := range plan.Targets {
				if _, err := os.Stat(t); err != nil {
					missing = append(missing, t)
				}
			}

			if len(missing) > 0 {
				// A real failure and not a warning. A target that has been
				// renamed is the classic silent gap: restic reports it and
				// carries on, the run says success, and the folder somebody
				// cares about has not been backed up for months.
				return "", fmt.Errorf("these targets do not exist: %s", strings.Join(missing, ", "))
			}

			return fmt.Sprintf("%d target(s) present", len(plan.Targets)), nil
		})

		c.check("repository", func() (string, error) {
			set, err := d.creds.ForRun(ctx)
			if err != nil {
				return "", err
			}
			defer set.Wipe()

			repo := set.Credentials.Repository(
				set.RepositoryURL, plan.PackSizeMiB, plan.ReadConcurrency)

			exists, err := d.restic.Exists(ctx, repo)
			if err != nil {
				return "", err
			}

			if !exists {
				return "", fmt.Errorf("there is no repository at %s", set.RepositoryURL)
			}

			snaps, err := d.restic.Snapshots(ctx, repo)
			if err != nil {
				return "opened, but the snapshot list could not be read: " + err.Error(), nil
			}

			return fmt.Sprintf("opened; %d snapshot(s)", len(snaps)), nil
		})
	}

	c.check("last run", func() (string, error) {
		last, err := d.backups.Last(ctx)

		switch {
		case errors.Is(err, backupbus.ErrNoRuns):
			return "", errors.New("nothing has run yet")
		case err != nil:
			return "", err
		}

		age := time.Since(last.StartedAt).Round(time.Minute)

		// A run in progress carries OutcomeFailed as its placeholder until it
		// finishes, so reading the outcome of the newest row reports a
		// healthy backup as a failed one -- for as long as it takes, which on
		// a first full upload is most of a day.
		if last.Unfinished() {
			return fmt.Sprintf("a backup has been running for %s", age), nil
		}

		// A run somebody stopped is not a fault to report, for the same
		// reason the status page shows it amber: they meant it, and a machine
		// that prints a failure for every deliberate act teaches its owner to
		// ignore the ones that are not. It stops being deliberate and starts
		// being a machine that is not backed up at the same age the page
		// says so.
		if last.Outcome == backupbus.OutcomeCancelled && age < backupbus.StaleAfter {
			return fmt.Sprintf("stopped %s ago: %s", age, last.Message), nil
		}

		if !last.Outcome.Good() {
			return "", fmt.Errorf("%s, %s ago: %s", last.Outcome, age, last.Message)
		}

		return fmt.Sprintf("%s, %s ago, verified=%v", last.Outcome, age, last.Verified), nil
	})

	c.check("diagnostics", func() (string, error) {
		waiting := d.diag.Waiting()
		if waiting == 0 {
			return "nothing waiting to be reported", nil
		}

		// Not a failure. Today it is the expected state: Eumaeus does not
		// serve the endpoint yet, so reports queue and age out. It is worth a
		// line because a machine with reports waiting is a machine something
		// happened to.
		return fmt.Sprintf("%d report(s) waiting in %s; they go with the next run",
			waiting, d.paths.Diag), nil
	})

	c.check("repository check", func() (string, error) {
		if planErr != nil || plan.Repository == "" {
			return "", errSkipCheck
		}

		i, err := d.plan.Integrity(ctx, plan.Repository)
		if err != nil {
			return "", err
		}

		// A damaged repository is the one failure here that means the backups
		// already taken may not come back, so it is a failure rather than a
		// line of detail.
		if !i.CheckedAt.IsZero() && !i.OK && i.SkippedReason == "" {
			return "", fmt.Errorf("%s", i.Describe(time.Now()))
		}

		return i.Describe(time.Now()), nil
	})

	c.check("self-update", func() (string, error) {
		u := supervisor(d.log)
		if u == nil {
			return "", errors.New("cannot tell: this binary's own path could not be resolved")
		}

		// The list of versions this machine gave up on is the answer to "why
		// is this machine a version behind", and the only place a person can
		// read it. A machine that cannot say so is a machine somebody has to
		// guess about.
		if refused := u.Refused(); len(refused) > 0 {
			return "", fmt.Errorf("gave up on %s: %s would not stay running here. "+
				"It will not be installed again; \"sion-backup update --forget\" "+
				"clears that if the version was fine and the machine was not",
				strings.Join(refused, ", "), refused[len(refused)-1])
		}

		if !d.cfg.Update.On() {
			return "off in this machine's config.toml", nil
		}

		// Whether this machine can replace its own binary at all. Checked
		// rather than assumed, because on Windows and macOS the answer is
		// usually no — the binary is in a directory an administrator owns and
		// the service runs as the user — and the fleet dashboard cannot tell
		// that apart from a machine that stopped checking in.
		if err := u.Writable(); err != nil {
			return "", fmt.Errorf("this machine cannot update itself: %w. "+
				"It will keep backing up on %s until somebody installs a new "+
				"one by hand", err, version)
		}

		return fmt.Sprintf("on; this binary is replaceable by this process (%s)", version), nil
	})

	c.check("eumaeus", func() (string, error) {
		if !d.creds.Enrolled() {
			return "", errors.New("this machine has no token: run \"sion-backup enroll\". " +
				"Nothing can back up until it does, because the credentials " +
				"live on the server and nowhere else")
		}

		// A real round trip, because "the URL parses" is not the question. It
		// also exercises exactly the call every backup makes first.
		set, err := d.creds.ForRun(ctx)

		var revoked *credentialbus.Unauthorised

		switch {
		case err == nil:
			defer set.Wipe()

			return fmt.Sprintf("%s answered; credentials v%d for %s",
				d.cfg.EumaeusURL(), set.Version, set.RepositoryURL), nil

		case errors.As(err, &revoked):
			return "", errors.New("this machine has been de-enrolled; " +
				"issue a new code in Eumaeus and run \"sion-backup enroll\"")

		default:
			return "", err
		}
	})

	// Windows only; snapshotCheck skips itself elsewhere.
	c.check("shadow copies", func() (string, error) {
		last, err := d.backups.Last(ctx)
		if err != nil {
			// Including ErrNoRuns: a machine checked before its first backup
			// is the machine most worth telling, because whoever installed it
			// is still standing there.
			return snapshotCheck(false, false)
		}

		return snapshotCheck(last.VSSFellBack, true)
	})

	c.check("status page port", func() (string, error) {
		addr := d.cfg.Addr()

		if err := refuseNonLoopback(addr); err != nil {
			return "", err
		}

		ln, err := net.Listen("tcp", addr)
		if err != nil {
			// Almost always the daemon itself, which is the healthy case.
			return addr + " is in use, most likely by the running daemon", nil
		}

		ln.Close()

		return addr + " is free (the daemon is not running)", nil
	})

	fmt.Printf("\n%d checks, %d failed\n", c.total, c.failed)

	if c.failed > 0 {
		os.Exit(1)
	}

	return nil
}

// resolveGrace bounds each lookup. Generous against a slow resolver, short
// enough that a dead one does not make this command look hung: the failure
// being diagnosed is one where every lookup takes the full timeout.
const resolveGrace = 5 * time.Second

// resolves looks each host up the way the rest of the program will.
//
// net.DefaultResolver on purpose, rather than asking a specific nameserver:
// the question is not whether some resolver somewhere can answer, it is
// whether this computer's own configuration produces an answer -- which is
// exactly what restic gets, being a Go program reading the same file.
func resolves(ctx context.Context, hosts []string) (string, error) {
	var found []string

	for _, h := range hosts {
		if h == "" {
			continue
		}

		lookup, cancel := context.WithTimeout(ctx, resolveGrace)
		addrs, err := net.DefaultResolver.LookupHost(lookup, h)

		cancel()

		if err != nil {
			// Which host, and no guess about why. A name that does not
			// resolve is usually a resolver that is not answering, but it is
			// sometimes a typo in a config file, and this check cannot tell
			// them apart.
			return "", fmt.Errorf("this computer cannot look up %s: %w; "+
				"nothing will reach that host until name resolution works", h, err)
		}

		found = append(found, h+" → "+addrs[0])
	}

	if len(found) == 0 {
		return "", errSkipCheck
	}

	return strings.Join(found, ", "), nil
}

// errSkipCheck reports a check that does not apply to this machine, as
// opposed to one that passed or failed. Returning it prints nothing and
// counts as nothing: a Linux machine should not be told that a Windows
// feature is fine, and it should not be told that it is missing either.
var errSkipCheck = errors.New("this check does not apply on this platform")

// checker runs and prints the checks.
type checker struct {
	total  int
	failed int
}

// check runs one, printing a line either way.
//
// The detail is printed on success as well as on failure, deliberately. Half
// the value of this command is the administrator reading "s3:https://..." and
// realising it is last year's bucket.
func (c *checker) check(name string, fn func() (string, error)) {
	c.total++

	detail, err := fn()

	if errors.Is(err, errSkipCheck) {
		c.total--

		return
	}

	if err != nil {
		c.failed++

		fmt.Printf("FAIL  %-18s %s\n", name, err)

		return
	}

	fmt.Printf("ok    %-18s %s\n", name, detail)
}
