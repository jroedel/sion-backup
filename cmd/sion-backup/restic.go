package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/jroedel/sion-backup/business/domain/diag/diagbus"
	"github.com/jroedel/sion-backup/foundation/restic"
)

var resticUsage = fmt.Sprintf(`sion-backup restic — install or check the restic this machine runs

Usage:
  sion-backup restic [--force]

This fleet runs one version of restic, %s, and installs it itself: into the
data directory, from restic's own release, verified against a hash compiled
into this binary. No administrator, no package manager, and no second visit
to the machine — the account that takes the backup can replace it.

Nothing normally needs to run this. Enrolling installs restic, and every
backup checks it first, so a machine whose restic is removed repairs itself
on the next run. It is here for an install that wants to do the download
while somebody is watching, and for answering "what has this machine got".

If the config file names a restic of its own, that one is used as given and
this command leaves it alone: naming a path is taking the version on.

Flags:
  --force   reinstall even if the pinned version is already here
`, restic.PinnedVersion)

func resticCmd(args []string) error {
	fs := flag.NewFlagSet("restic", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, resticUsage) }

	force := fs.Bool("force", false, "reinstall even if the pinned version is here")
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

	// --force is a removal rather than a flag threaded through the package:
	// EnsurePinned's question is "is the pinned version here", and the way to
	// make the answer no is to take it away. The binary that gets removed is
	// the managed one, never an operator's.
	if *force && d.restic.Managed() {
		if err := os.Remove(d.restic.Bin()); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("could not remove %s: %w", d.restic.Bin(), err)
		}
	}

	out, err := d.restic.EnsurePinned(ctx, nil, d.log)
	if err != nil {
		return err
	}

	fmt.Println(out)

	return nil
}

// fetchRestic is ensureRestic for a command with somebody watching it.
//
// It says, in the command's own voice, that a download is about to happen and
// roughly how long it will be — because the alternative is twenty megabytes of
// silence in the middle of a page of formatted output, and the person at the
// machine cannot tell that from a hang.
//
// This exists because the log used to do it. A foreground command's logger is
// now quiet (see wireDaemon), which removed a genuine piece of information
// along with the noise; this is that information put back where it belongs.
func (d *deps) fetchRestic(ctx context.Context) error {
	// Only when something really is going to be fetched. "Fetching restic"
	// followed instantly by nothing teaches people to ignore the line.
	if d.restic.Managed() {
		if v, err := d.restic.InstalledVersion(ctx); err != nil || v != restic.PinnedVersion {
			fmt.Printf("\nFetching restic %s — about 20 MB, once, verified against the hash\n",
				restic.PinnedVersion)
			fmt.Printf("compiled into this binary. Nothing else on this machine is touched.\n")
		}
	}

	return d.ensureRestic(ctx)
}

// ensureRestic makes sure the pinned restic is on this machine before
// something needs it.
//
// Called at the top of a backup and during enrollment. Not from wire(): a
// download does not belong in front of `sion-backup status`, and a machine
// with no network should still be able to print what it did last week.
//
// A failure here is reported as an install failure rather than only logged.
// It is the one error in this program that produces no run at all — there is
// no backup to record as failed if the thing that takes backups is missing —
// and a machine that stops without saying so is precisely the failure this
// program exists against.
func (d *deps) ensureRestic(ctx context.Context) error {
	out, err := d.restic.EnsurePinned(ctx, nil, d.log)
	if err != nil {
		_ = d.diag.Record(diagbus.Report{
			Kind:   diagbus.KindInstallFailed,
			Step:   "fetch-restic",
			Detail: err.Error(),
		})

		return fmt.Errorf("this machine has no usable restic and one could not be "+
			"installed: %w", err)
	}

	// Only the interesting outcomes. "already 0.19.1" is true before every
	// backup on every machine and would be the most common line in the log
	// while saying nothing.
	if out.Action != restic.ActionAlready {
		d.log.Info("restic", "outcome", out.String(), "action", string(out.Action),
			"from", out.From, "to", out.To)
	}

	return nil
}
