package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/jroedel/sion-backup/business/domain/credential/sources/eumaeuscreds"
	"github.com/jroedel/sion-backup/business/domain/plan/planbus"
	"github.com/jroedel/sion-backup/foundation/eumaeusapi"
	"github.com/jroedel/sion-backup/foundation/restic"
	"github.com/jroedel/sion-backup/foundation/token"
)

// enrollUsage is a var rather than a const so the default server appears in it
// once, from the one place that defines it.
var enrollUsage = fmt.Sprintf(`sion-backup enroll — make this machine ready to back up

Usage:
  sion-backup enroll --code K4TP-9QX2

Get the code from Eumaeus first: sign in, choose "Enrol a computer", pick who
owns it and which bucket it writes to. The code is good for fifteen minutes and
can be used once.

What this does:

  1. Exchanges the code for a machine token, and writes that token — the ONLY
     secret this program keeps on disk — to the data directory.
  2. Proves the bucket opens, while you are still standing at the machine.
  3. Prints the owner's restore card, which lets them recover their own files
     with nothing but a downloaded restic binary. Print it and put it somewhere
     safe; it is not shown again.
  4. Starts the background service and opens the page where the person using
     this computer chooses what is backed up.

Nothing is backed up until that page is answered, and that is deliberate. A
first backup is the largest thing this program ever does — tens of gigabytes,
hours of somebody's uplink — and it should not begin against a list of folders
an administrator guessed at, without the person whose computer it is having
seen it.

The S3 keys and the repository password are NOT stored here. Every backup
fetches them from Eumaeus, uses them, and discards them.

Flags:
  --code      the enrollment code from Eumaeus (required)
  --server    a different Eumaeus URL (default %q)
  --force     replace the token on a machine that is already enrolled
  --no-start  do not start the service afterwards
  --no-open   do not open a browser afterwards
`, DefaultEumaeusURL)

func enrollCmd(args []string) error {
	fs := flag.NewFlagSet("enroll", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, enrollUsage) }

	code := fs.String("code", "", "enrollment code from Eumaeus")
	server := fs.String("server", "", "Eumaeus base URL")
	force := fs.Bool("force", false, "replace an existing enrollment")
	noStart := fs.Bool("no-start", false, "do not start the service afterwards")
	noOpen := fs.Bool("no-open", false, "do not open a browser afterwards")
	verbose := fs.Bool("v", false, "verbose logging")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if *code == "" {
		fmt.Fprint(os.Stderr, enrollUsage)

		return errors.New("an enrollment code is required")
	}

	ctx, cancel := signalContext()
	defer cancel()

	d, err := wire(ctx, *verbose)
	if err != nil {
		return err
	}
	defer d.close()

	if _, err := d.enroll(ctx, enrollment{code: *code, server: *server, force: *force}); err != nil {
		return err
	}

	handoff(ctx, d, !*noStart, !*noOpen)

	return nil
}

// enrollment is one claim, as asked for on the command line.
type enrollment struct {
	code   string
	server string
	force  bool

	// legacy is the old backup this machine is being migrated off, when
	// `adopt-enroll` found one. Nil for an ordinary enrollment, which is what
	// every machine with nothing on it sends.
	legacy *eumaeuscreds.LegacyInstall
}

// enroll claims a code and makes this machine ready to back up.
//
// Split out of [enrollCmd] so that `adopt-enroll` can do the same thing as its
// last step rather than telling the operator to run a second command. What the
// two need is identical: a machine whose bucket Eumaeus adopted is enrolled
// exactly like one whose bucket it provisioned — the difference is entirely in
// what happened on the server beforehand, and in the checking afterwards.
//
// It returns the enrollment so that a caller can check the bucket it was given
// is the one it expected. Nothing in it is written down here beyond the token.
func (d *deps) enroll(ctx context.Context, o enrollment) (eumaeuscreds.Enrollment, error) {
	if err := d.refuseSecondEnrollment(o.force); err != nil {
		return eumaeuscreds.Enrollment{}, err
	}

	// --server, then the config file, then the fleet's own server. The last of
	// those is why enrolling a fresh machine needs nothing but the code.
	base := o.server
	if base == "" {
		base = d.cfg.EumaeusURL()
	}

	// Before the code is spent, deliberately. An enrollment code is good for
	// fifteen minutes and can be used once, so a machine that claimed one and
	// then failed to download restic would have burned it — and step 2 below
	// proves the bucket opens, which needs restic anyway.
	if err := d.fetchRestic(ctx); err != nil {
		return eumaeuscreds.Enrollment{}, err
	}

	fmt.Printf("Enrolling against %s\n", base)

	// Anonymous: the whole point of the claim is that there is no token yet.
	anon, err := eumaeusapi.New(eumaeusapi.Config{
		BaseURL:   base,
		UserAgent: "sion-backup/" + version,
	})
	if err != nil {
		return eumaeuscreds.Enrollment{}, err
	}

	enrolled, err := eumaeuscreds.Claim(ctx, anon, o.code, eumaeuscreds.Machine{
		Hostname:     hostname(),
		OS:           osName(),
		LocalAccount: localAccount(),
		Agent:        version,
		Legacy:       o.legacy,
	})
	if err != nil {
		return eumaeuscreds.Enrollment{}, err
	}

	if err := token.Save(d.paths.Token, enrolled.MachineToken); err != nil {
		return eumaeuscreds.Enrollment{}, err
	}

	fmt.Printf("Enrolled %s for %s.\n", enrolled.NodeID, enrolled.OwnerName)

	// The plan is written before the repository is checked, so that a machine
	// whose bucket is briefly unreachable is still enrolled and will simply
	// try again tonight. A failed check below is reported, not rolled back.
	if err := d.storePlan(ctx, enrolled); err != nil {
		return enrolled, err
	}

	if err := checkRepository(ctx, d.restic, enrolled); err != nil {
		fmt.Fprintf(os.Stderr, "\nWARNING: %v\n", err)
		fmt.Fprintln(os.Stderr, "The machine is enrolled; fix this before relying on it.")
	} else {
		fmt.Printf("The repository at %s opened with these credentials.\n", enrolled.RepositoryURL)
	}

	printRestoreCard(enrolled)

	return enrolled, nil
}

// refuseSecondEnrollment stops a machine that already has a token claiming a
// second one by accident.
//
// Checked before anything else and separately from [deps.enroll], because
// `adopt-enroll` has to ask the same question at the top of its own run: it
// reads the machine, writes a plan and opens the legacy repository before it
// gets anywhere near a code, and finding out at the end that none of it could
// be used would be a wasted visit to somebody's desk.
func (d *deps) refuseSecondEnrollment(force bool) error {
	if d.machineToken == "" || force {
		return nil
	}

	return fmt.Errorf("this machine is already enrolled.\n\n"+
		"Its token is in %s. Use --force to replace it — the old token stays "+
		"valid until it is revoked in Eumaeus", d.paths.Token)
}

// storePlan records what the server said, filling in local defaults for the
// parts the owner controls.
func (d *deps) storePlan(ctx context.Context, e eumaeuscreds.Enrollment) error {
	plan, err := d.plan.Get(ctx)
	if err != nil && !errors.Is(err, planbus.ErrNoPlan) {
		return err
	}

	plan.NodeID = e.NodeID
	plan.Repository = e.RepositoryURL

	if len(plan.Schedule.Times) == 0 {
		plan.Schedule = planbus.DefaultSchedule()
	}

	if len(plan.Targets) == 0 {
		// Seeded from config.toml if the installer left one; otherwise the
		// person is sent to the status page, which is where targets belong.
		if seed, err := d.cfg.Plan(); err == nil && len(seed.Targets) > 0 {
			plan.Targets = seed.Targets
			plan.Excludes = seed.Excludes
		}
	}

	if len(plan.Targets) == 0 {
		// Not a problem to report: it is the ordinary state of a machine that
		// has just been enrolled, and the page the handoff opens is where it
		// is answered. Saying so here would read as a fault.
		return nil
	}

	return d.plan.Put(ctx, plan, time.Now())
}

// checkRepository proves the credentials work while somebody is watching.
//
// This is the whole reason enrollment is a command rather than a form
// submission. A bucket in the wrong region, a policy that denies writes, a
// clock so far out that S3 rejects the signature — every one of those is
// silent until the first scheduled run, which is after the administrator has
// left the building.
func checkRepository(ctx context.Context, r *restic.Runner, e eumaeuscreds.Enrollment) error {
	repo := restic.Repository{
		URL:             e.RepositoryURL,
		Password:        []byte(e.ResticPassword),
		AccessKeyID:     []byte(e.MachineKeyID),
		SecretAccessKey: []byte(e.MachineKeySecret),
	}

	exists, err := r.Exists(ctx, repo)
	if err != nil {
		return fmt.Errorf("could not reach the repository: %w", err)
	}

	if !exists {
		// Never initialised from here. Eumaeus provisions the bucket and
		// creates the repository; a client that could create one would create
		// a second, empty one the first time a URL was mistyped, and report
		// success while doing it.
		return fmt.Errorf("there is no restic repository at %s yet — "+
			"Eumaeus provisions it, so this needs fixing on the server", e.RepositoryURL)
	}

	return nil
}

// printRestoreCard writes the page the owner keeps.
//
// It names no software of ours on purpose. What this program provides is
// scheduling, verification and somebody noticing when it stops; none of that
// is load-bearing at the moment of restore. The data is in a standard restic
// repository and the person whose files they are holds read-only credentials
// to it. If this organisation and its server both vanish, they still get their
// files back.
func printRestoreCard(e eumaeuscreds.Enrollment) {
	setEnv, exportEnv := "$env:", "export "

	line := strings.Repeat("─", 72)

	fmt.Printf(`
%s
  RESTORING YOUR OWN FILES — %s
%s

  You do not need Eumaeus, the office network, or anybody's help. These
  steps work from any computer, anywhere, for as long as the bucket exists.

  1. Download restic — free, one file, no installer:
       https://github.com/restic/restic/releases
     Any version 0.14 or newer will read this backup.

  2. Open a terminal where you saved it and set four values.

     Windows (PowerShell):
       %[4]sRESTIC_REPOSITORY="%[5]s"
       %[4]sAWS_ACCESS_KEY_ID="%[6]s"
       %[4]sAWS_SECRET_ACCESS_KEY="%[7]s"
       %[4]sRESTIC_PASSWORD="%[8]s"

     macOS / Linux — the same four, with %[9]sNAME="value"

  3. Check it works now, before you need it:
       restic snapshots

  4. Get everything back:
       restic restore latest --target ./restored

     Or one folder:
       restic restore latest --target ./restored --include "/path/to/folder"

  These credentials can READ your backup and nothing else. They cannot
  change or delete it, and they open no other computer's backup.

  Keep this page somewhere safe — it is enough to read every file on this
  computer. Destroy it when you are given a new one.

  Issued %[10]s  ·  owner %[11]s  ·  bucket %[12]s
%s
`, line, e.NodeID, line,
		setEnv, e.RepositoryURL, e.RestoreKeyID, e.RestoreKeySecret, e.ResticPassword,
		exportEnv,
		time.Now().Format("2 January 2006"), e.OwnerEmail, e.RepositoryBucket,
		line)
}

// localAccount names the OS account the daemon will run as.
//
// Reported at enrollment because a machine whose account changes is a machine
// whose scheduled task no longer runs, and "it just stopped" is much easier to
// diagnose when the dashboard can show which account it was set up under.
func localAccount() string {
	for _, key := range []string{"USERNAME", "USER", "LOGNAME"} {
		if v := os.Getenv(key); v != "" {
			if domain := os.Getenv("USERDOMAIN"); domain != "" && runtime.GOOS == "windows" {
				return domain + "\\" + v
			}

			return v
		}
	}

	return "unknown"
}

// unused keeps the context import honest if the checks above are ever trimmed.
var _ = context.Background
