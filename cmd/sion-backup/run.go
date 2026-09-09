package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/jroedel/sion-backup/business/domain/backup/backupbus"
	"github.com/jroedel/sion-backup/business/domain/plan/planbus"
)

// runCmd takes one backup in the foreground.
//
// It exists for the administrator standing at a machine during an install, and
// for a support call. Its exit status is the thing to script against:
//
//	0  the backup succeeded and was verified
//	1  it failed, or could not be read back
//	3  a snapshot was written with files missing
//
// Those mirror restic's own 0/1/3, deliberately, so a wrapper script that
// already understands restic understands this.
func runCmd(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	verbose := fs.Bool("v", false, "verbose logging")

	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, cancel := signalContext()
	defer cancel()

	d, err := wire(ctx, *verbose)
	if err != nil {
		return err
	}
	defer d.close()

	if err := d.seedPlan(ctx); err != nil {
		return err
	}

	plan, err := d.plan.Get(ctx)
	if errors.Is(err, planbus.ErrNoPlan) {
		return errors.New("this machine has no backup plan yet; run \"sion-backup enroll\" first")
	} else if err != nil {
		return err
	}

	fmt.Printf("backing up %d target(s) to %s as %s\n",
		len(plan.Targets), plan.Repository, plan.NodeID)

	// Progress on a terminal, at a human pace. restic emits several status
	// lines a second and printing all of them turns a backup log into
	// something nobody can read.
	done := make(chan struct{})
	defer close(done)

	go printProgress(d, done)

	if err := d.backup(ctx, plan); err != nil {
		return err
	}

	last, err := d.backups.Last(ctx)
	if err != nil {
		return err
	}

	fmt.Printf("\n%s: %s\n", last.Outcome, last.Message)

	if len(last.UnreadableFiles) > 0 {
		fmt.Println("\nnot in the backup:")

		for _, f := range last.UnreadableFiles {
			fmt.Println("  " + f)
		}
	}

	switch last.Outcome {
	case backupbus.OutcomeSuccess, backupbus.OutcomeDegraded:
		return nil
	case backupbus.OutcomeIncomplete:
		os.Exit(3)
	}

	return errors.New("the backup did not succeed")
}

// progressEvery is how often the foreground run prints a line.
const progressEvery = 5 * time.Second

func printProgress(d *deps, done <-chan struct{}) {
	ticker := time.NewTicker(progressEvery)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			p, running := d.backups.Running()
			if !running {
				continue
			}

			fmt.Printf("\r  %3.0f%%  %d/%d files  %s        ",
				p.PercentDone*100, p.FilesDone, p.TotalFiles, humanBytes(p.BytesDone))
		}
	}
}

// statusCmd prints the recent history, for a support call where nobody can see
// the status page.
func statusCmd(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	limit := fs.Int("n", 10, "how many runs to show")

	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx := context.Background()

	d, err := wire(ctx, false)
	if err != nil {
		return err
	}
	defer d.close()

	plan, err := d.plan.Get(ctx)
	if errors.Is(err, planbus.ErrNoPlan) {
		fmt.Println("This machine has no backup plan yet. Run \"sion-backup enroll\".")

		return nil
	} else if err != nil {
		return err
	}

	fmt.Printf("node        %s\n", plan.NodeID)
	fmt.Printf("repository  %s\n", plan.Repository)
	fmt.Printf("schedule    %v%s\n", plan.Schedule.Times, pausedNote(plan.Paused))
	fmt.Printf("next run    %s\n", plan.Schedule.Next(plan.NodeID, time.Now()).Format(time.RFC1123))
	fmt.Printf("credentials %s\n", enrolledWord(d.creds.Enrolled()))

	if p, running := d.backups.Running(); running {
		fmt.Printf("\nA backup is running: %.0f%%, %d/%d files.\n",
			p.PercentDone*100, p.FilesDone, p.TotalFiles)
	}

	runs, err := d.backups.Recent(ctx, *limit)
	if err != nil {
		return err
	}

	if len(runs) == 0 {
		fmt.Println("\nNo runs yet.")

		return nil
	}

	fmt.Println()

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)

	fmt.Fprintln(w, "STARTED\tOUTCOME\tVERIFIED\tFILES\tADDED\tTOOK\tREPORTED")

	for _, r := range runs {
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\t%s\n",
			r.StartedAt.Local().Format("2006-01-02 15:04"),
			r.Outcome,
			yesNo(r.Verified),
			r.TotalFilesProcessed,
			humanBytes(r.DataAdded),
			r.Duration().Round(time.Second),
			yesNo(r.ReportedAt != nil),
		)
	}

	return w.Flush()
}

// enrolledWord describes credential handling in one word for the terminal.
func enrolledWord(enrolled bool) string {
	if enrolled {
		return "fetched from Eumaeus per run; nothing stored here"
	}

	return "NOT ENROLLED"
}

func pausedNote(paused bool) string {
	if paused {
		return "  (PAUSED)"
	}

	return ""
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}

	return "no"
}

// humanBytes renders a byte count for the terminal.
func humanBytes(n int64) string {
	const unit = 1024

	if n < unit {
		return fmt.Sprintf("%d B", n)
	}

	div, exp := int64(unit), 0

	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}

	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
