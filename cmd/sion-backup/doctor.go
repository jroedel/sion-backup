package main

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
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
			return "", fmt.Errorf("no machine token in %s; run \"sion-backup enroll --code <code>\"",
				d.paths.Token)
		}

		if !d.creds.Enrolled() {
			return "", fmt.Errorf("a token is present but no Eumaeus server is configured in %s",
				d.paths.Config)
		}

		return "machine token present; credentials are fetched per run and not stored", nil
	})

	plan, planErr := d.plan.Get(ctx)

	c.check("backup plan", func() (string, error) {
		if errors.Is(planErr, planbus.ErrNoPlan) {
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
