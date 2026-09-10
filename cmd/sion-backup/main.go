// Command sion-backup keeps a work computer backed up to an S3 bucket with
// restic, and says so.
//
// It is the composition root: it resolves paths, opens the one database, reads
// the machine token, wires each Business domain to its store, and dispatches a
// subcommand. Nothing below this file decides where anything lives.
//
// # The shape of a deployment
//
// One binary, installed per machine by the person administering the fleet,
// running as a service under the user's own account. It schedules its own
// runs, serves a status page on loopback, and reports each run to Eumaeus.
// restic sits beside it and does the actual work.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/jroedel/sion-backup/business/domain/backup/backupbus"
	"github.com/jroedel/sion-backup/business/domain/backup/stores/backupdb"
	"github.com/jroedel/sion-backup/business/domain/credential/credentialbus"
	"github.com/jroedel/sion-backup/business/domain/credential/sources/eumaeuscreds"
	"github.com/jroedel/sion-backup/business/domain/diag/diagbus"
	"github.com/jroedel/sion-backup/business/domain/fleet/fleetbus"
	"github.com/jroedel/sion-backup/business/domain/fleet/sources/eumaeusfleet"
	"github.com/jroedel/sion-backup/business/domain/plan/planbus"
	"github.com/jroedel/sion-backup/business/domain/plan/stores/plandb"
	"github.com/jroedel/sion-backup/foundation/eumaeusapi"
	"github.com/jroedel/sion-backup/foundation/paths"
	"github.com/jroedel/sion-backup/foundation/restic"
	"github.com/jroedel/sion-backup/foundation/sqldb"
	"github.com/jroedel/sion-backup/foundation/token"
)

// version is stamped at build time:
//
//	go build -ldflags "-X main.version=$(git describe --tags --always --dirty)"
//
// It appears in the status page's footer and in every event sent to the fleet
// dashboard, which is how "which machines are still on the old build" becomes
// answerable without visiting them.
var version = "dev"

const usage = `sion-backup — keeps this computer backed up, and says so.

Usage:
  sion-backup <command> [flags]

Commands:
  daemon     Run the scheduler and the status page (what the service runs)
  run        Take one backup now, in the foreground
  status     Print the last few runs
  enroll     Fetch this machine's credentials from Eumaeus and store them
  doctor     Check everything a backup needs, and say what is wrong
  report     Report a failed install or a crash (for the installers)
  update     Replace this binary with the newest release
  paths      Print where this program keeps its files
  version    Print the build

Run "sion-backup <command> -h" for a command's flags.

The status page is at http://127.0.0.1:7391/ while the daemon is running.
`

func main() {
	// Records the crash before letting it be one. See capturePanic.
	defer capturePanic()

	if err := run(); err != nil {
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}

		// Not a failure: the daemon replaced itself and stopped so that the
		// service manager would start the new binary.
		if errors.Is(err, errUpdated) {
			os.Exit(updateExitCode)
		}

		fmt.Fprintf(os.Stderr, "sion-backup: %v\n", err)
		os.Exit(1)
	}
}

// errUsage reports a misuse that has already printed its own guidance.
var errUsage = errors.New("see usage above")

// errUpdated reports a daemon that stopped because it replaced itself. See
// updateExitCode.
var errUpdated = errors.New("a newer version was installed")

func run() error {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)

		return errUsage
	}

	cmd, args := os.Args[1], os.Args[2:]

	switch cmd {
	case "daemon", "serve":
		return daemonCmd(args)
	case "run":
		return runCmd(args)
	case "status":
		return statusCmd(args)
	case "enroll":
		return enrollCmd(args)
	case "doctor":
		return doctorCmd(args)
	case "report":
		return reportCmd(args)
	case "update":
		return updateCmd(args)
	case "paths":
		return pathsCmd()
	case "version":
		fmt.Printf("sion-backup %s (%s/%s, %s)\n", version, runtime.GOOS, runtime.GOARCH, runtime.Version())

		return nil
	case "-h", "--help", "help":
		fmt.Print(usage)

		return nil
	default:
		fmt.Fprint(os.Stderr, usage)

		return fmt.Errorf("unknown command %q", cmd)
	}
}

// deps is everything a subcommand might need, wired once.
type deps struct {
	cfg      Config
	cfgFound bool
	paths    paths.Paths
	log      *slog.Logger

	db *sqldb.DB

	// machineToken is the one secret this program reads from disk. Empty on a
	// machine that has not been enrolled, which is a state several commands
	// have to render rather than fail on.
	machineToken string

	// updated is signalled when self-update has replaced this binary, so the
	// daemon can exit into the new one. Nil outside the daemon, where there
	// is nothing to restart.
	updated chan struct{}

	plan    *planbus.Business
	backups *backupbus.Runner
	creds   *credentialbus.Business
	fleet   *fleetbus.Business
	diag    *diagbus.Business
	restic  *restic.Runner
}

// close releases the database handle.
func (d *deps) close() {
	if d.db != nil {
		d.db.Close()
	}
}

// wire builds everything.
//
// There is no password prompt and there cannot be: this runs as an unattended
// service. Nor is there a keyring to open — the only thing read from disk is
// the machine token, and the credentials it fetches live in memory for the
// length of one backup.
func wire(ctx context.Context, verbose bool) (*deps, error) {
	p, err := paths.Resolve()
	if err != nil {
		return nil, err
	}

	if err := p.EnsureDirs(); err != nil {
		return nil, err
	}

	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	cfg, found, err := LoadConfig(p.Config)
	if err != nil {
		return nil, err
	}

	d := &deps{cfg: cfg, cfgFound: found, paths: p, log: log}

	// Before the database, deliberately: if opening it is what fails, that is
	// a report somebody wants.
	d.diag = diagnostics(log)

	if d.db, err = sqldb.Open(ctx, p.DB); err != nil {
		_ = d.diag.Record(diagbus.Report{
			Kind:   diagbus.KindInstallFailed,
			Step:   "open-database",
			Detail: err.Error(),
		})

		return nil, err
	}

	if err := prepare(ctx, d.db); err != nil {
		d.close()

		return nil, err
	}

	// A missing token is not an error here: `sion-backup enroll` is how one
	// arrives, and doctor and the status page both have something useful to
	// say about a machine that has none.
	switch tok, err := token.Load(p.Token); {
	case err == nil:
		d.machineToken = tok
	case !errors.Is(err, token.ErrNotEnrolled):
		d.close()

		return nil, err
	}

	d.plan = planbus.NewBusiness(plandb.NewStore(d.db))

	// restic is resolved here even though only some commands need it, because
	// "restic is not installed" is a thing to find out at startup rather than
	// three hours into a maintenance window.
	if d.restic, err = restic.New(cfg.Server.Restic); err != nil {
		d.close()

		return nil, err
	}

	d.backups = backupbus.NewRunner(backupdb.NewStore(d.db), d.restic, p, log)

	if err := d.wireEumaeus(); err != nil {
		d.close()

		return nil, err
	}

	return d, nil
}

// wireEumaeus builds the credential source and the fleet reporter.
//
// Both hang off one client and one token. Unlike the previous design, the
// credential source is not optional: with nothing cached on this machine, a
// backup cannot happen without it. A machine that has not been enrolled can
// still serve its status page and report what it has done in the past, which
// is why this returns cleanly rather than refusing to start.
//
// The token is what decides that, not the URL: the server has a default
// (Config.EumaeusURL), so "no URL" is no longer a state a machine can be in.
func (d *deps) wireEumaeus() error {
	if d.machineToken == "" {
		d.fleet = fleetbus.NewBusiness(fleetbus.Nop{}, d.log)
		d.creds = credentialbus.NewBusiness(nil)

		return nil
	}

	client, err := eumaeusapi.New(eumaeusapi.Config{
		BaseURL:   d.cfg.EumaeusURL(),
		Token:     d.machineToken,
		UserAgent: "sion-backup/" + version,
	})
	if err != nil {
		return err
	}

	d.fleet = fleetbus.NewBusiness(eumaeusfleet.NewReporter(client), d.log)
	d.creds = credentialbus.NewBusiness(eumaeuscreds.NewSource(client))

	return nil
}

// migrators is every schema this build owns.
//
// Listed here, in the composition root, because it is the only place allowed
// to know about all of them: a store must not import a sibling store, and
// foundation must not import a store at all.
var migrators = []struct {
	name string
	fn   func(context.Context, *sqldb.DB) error
}{
	{"plan", plandb.Migrate},
	{"backup", backupdb.Migrate},
}

// prepare brings every schema up to date.
func prepare(ctx context.Context, db *sqldb.DB) error {
	for _, m := range migrators {
		if err := m.fn(ctx, db); err != nil {
			return fmt.Errorf("migrating the %s schema: %w", m.name, err)
		}
	}

	return nil
}

// signalContext cancels on interrupt, so a running daemon shuts down cleanly
// rather than leaving a WAL sidecar mid-write.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func pathsCmd() error {
	p, err := paths.Resolve()
	if err != nil {
		return err
	}

	fmt.Printf("data          %s\n", p.DataDir)
	fmt.Printf("database      %s\n", p.DB)
	fmt.Printf("config        %s\n", p.Config)
	fmt.Printf("machine token %s\n", p.Token)
	fmt.Printf("verify        %s\n", p.Verify)
	fmt.Printf("scratch       %s\n", p.Scratch)
	fmt.Printf("log           %s\n", p.Log)

	fmt.Println()
	fmt.Println("None of these are inside the source tree, which is why this repository")
	fmt.Println("is safe to make public. Keep it that way.")

	return nil
}
