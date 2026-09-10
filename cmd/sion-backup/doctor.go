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
		// Wiring failing is itself the diagnosis, and the most common one: no
		// restic on the machine.
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

	c.check("restic", func() (string, error) {
		version, err := d.restic.Version(ctx)
		if err != nil {
			return "", err
		}

		return strings.TrimSpace(version) + " at " + d.restic.Bin(), nil
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
	if err != nil {
		c.failed++

		fmt.Printf("FAIL  %-18s %s\n", name, err)

		return
	}

	fmt.Printf("ok    %-18s %s\n", name, detail)
}
