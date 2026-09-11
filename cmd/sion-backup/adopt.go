package main

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jroedel/sion-backup/business/domain/credential/sources/eumaeuscreds"
	"github.com/jroedel/sion-backup/business/domain/legacy/legacybus"
	"github.com/jroedel/sion-backup/business/domain/legacy/sources/legacyscan"
	"github.com/jroedel/sion-backup/business/domain/plan/planbus"
	"github.com/jroedel/sion-backup/foundation/restic"
)

// adoptUsage is a var for the same reason enrollUsage is: the default server
// is defined in one place and appears here from it.
var adoptUsage = fmt.Sprintf(`sion-backup adopt-enroll — take over the backup already running here

Usage:
  sion-backup adopt-enroll [--code K4TP-9QX2] [--yes]

`+"`recon`"+` reports what is on this machine. This is the command that acts on it.
It takes the plan out of the legacy install — what it backs up, what it leaves
out, how it is tuned — writes it as this machine's own, and prints the two
Eumaeus commands that ADOPT the bucket with two years of history in it rather
than replacing it, filled in and wrapped in ssh so they can be run from here.

Run it as root, or with sudo, when you can. These installs commonly live in
/home/restic, which an ordinary account cannot read, and a migration that could
not see the backup already running here is how a machine ends up with a second
bucket and a full re-upload.

It takes two visits, because the step in the middle is not ours:

  1. Here, with no code. Reads the legacy install, writes the plan, opens the
     legacy repository to count what is in it, and prints the two Eumaeus
     commands to run, filled in.
  2. In Eumaeus: adopt the bucket, then issue a code. Both are printed as ssh
     commands against the fleet's server, so they can be run from right here.
  3. Here again, with --code. Claims the code, and then CHECKS that the bucket
     handed back is the legacy one and not a fresh one — which is the mistake
     this command exists to catch, while somebody is still standing here.

Nothing is disabled and nothing of the old install is changed. It keeps running
on its own schedule until somebody turns it off as the LAST step, after the new
install has taken one verified backup. Two backup systems for one night is
untidy; none is worse.

No credential is printed, written down, or sent anywhere. The repository
password stays in the legacy script, and whoever runs the adopt command types it
at that command's prompt, from there. --measure opens the legacy repository to
count its snapshots, which needs those credentials: they are read into memory
for that one command and go no further.

The Eumaeus commands are printed as ssh lines against the fleet's server, so
they can be run from here without a second terminal, and with the sudo prefix
its CLI needs to open the fleet's store rather than root's empty one. --ssh
names a different target, user included; --ssh "" prints them bare, for
somebody already on the server.

Flags:
  --code        the enrollment code, once the bucket has been adopted
  --yes         do not ask before writing the plan
  --legacy-dir  also look here for the old install (comma-separated)
  --measure     open the legacy repository and count its snapshots (default true)
  --ssh         run the Eumaeus commands through this target (default:
                root@ the server's own host; "" to print them bare)
  --server      a different Eumaeus URL (default %q)
  --force       enrol again on a machine that already has a token
`, DefaultEumaeusURL)

func adoptEnrollCmd(args []string) error {
	fs := flag.NewFlagSet("adopt-enroll", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, adoptUsage) }

	code := fs.String("code", "", "enrollment code from Eumaeus")
	yes := fs.Bool("yes", false, "do not ask before writing the plan")
	legacyDir := fs.String("legacy-dir", "", "also look here for the old install (comma-separated)")
	measure := fs.Bool("measure", true, "open the legacy repository and count its snapshots")
	server := fs.String("server", "", "Eumaeus base URL")
	sshHost := fs.String("ssh", "", "run the Eumaeus commands through this host (default: the server's own)")
	force := fs.Bool("force", false, "enrol again on a machine that already has a token")
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

	// Asked at the top rather than where it is needed. Everything below this
	// line is somebody standing at a desk they had to walk to.
	if err := d.refuseSecondEnrollment(*force); err != nil {
		return err
	}

	// legacyscan directly rather than gather(): recon's report also reaches
	// the server and probes two restic binaries, and none of that is shown
	// here. On a machine that cannot reach the network it is forty-five
	// seconds of silence before the first line of output.
	found := legacyscan.Find(ctx, splitList(*legacyDir)...)

	a, err := assemble(Recon{Legacy: found.Install, Blocked: found.Blocked, Notes: found.Notes})
	if err != nil {
		return err
	}

	a.ssh = sshTarget(*sshHost, cmp.Or(*server, d.cfg.EumaeusURL()))

	a.describe()
	a.plannedHere(d, *measure)

	if *code == "" {
		fmt.Print(adoptFirst)
	}

	if !*yes {
		question := "Write this plan and stop, so the bucket can be adopted in Eumaeus?"
		if *code != "" {
			question = "Write this plan and claim the code?"
		}

		switch ok, err := confirm(question); {
		case err != nil:
			return err

		case !ok:
			fmt.Println("\nNothing was written. The legacy backup is untouched.")

			return nil
		}
	}

	if err := d.seedAdoptedPlan(ctx, a); err != nil {
		return err
	}

	// Before the claim, deliberately, and not only on the visit that has no
	// code. The count and the horizon travel with the claim, and so does the
	// repository URL that lets Eumaeus refuse a code pointing at the wrong
	// bucket while it is still unspent — which is the whole reason the check
	// moved to the server. A listing is seconds against a fifteen-minute
	// code.
	if *measure {
		a.measure(ctx, d)
	}

	if *code == "" {
		a.serverSteps()

		return nil
	}

	enrolled, err := d.enroll(ctx, enrollment{
		code:   *code,
		server: *server,
		force:  *force,
		legacy: a.legacy(),
	})
	if err != nil {
		return a.explainRefusal(err)
	}

	a.verify(ctx, d, enrolled)

	return nil
}

// legacy is what the claim tells Eumaeus about the backup already here.
//
// The repository URL goes whether or not it could be opened, because it is
// read out of the script and costs nothing; the count and the horizon go only
// when restic actually answered. A zero count sent as a fact would be a claim
// that the old repository is empty.
func (a adoption) legacy() *eumaeuscreds.LegacyInstall {
	out := eumaeuscreds.LegacyInstall{RepositoryURL: a.install.RepositoryURL}

	if a.snapshots > 0 {
		out.Snapshots = a.snapshots
		out.OldestSnapshot = a.oldest
	}

	return &out
}

// explainRefusal adds what the server's sentence cannot say for itself.
//
// Eumaeus writes one good sentence naming both repositories, and it is
// returned as it stands. What it does not say — because it is a fact about
// this command rather than about the request — is that the visit is not
// wasted: the code is still good, and the same one works once the right
// bucket is adopted.
func (a adoption) explainRefusal(err error) error {
	if !eumaeuscreds.RepositoryMismatch(err) {
		return err
	}

	// Wrapped rather than printed, so that the server's sentence comes first
	// and this follows it on one stream. A note explaining a refusal, printed
	// before the refusal, reads as a warning about something that has not
	// happened yet.
	return fmt.Errorf("%w\n\n"+
		"The code was NOT used. Have the bucket this machine is already writing to\n"+
		"adopted — the commands are above, or run this command again without --code\n"+
		"to print them — and then present the SAME code here.\n\n"+
		"The plan written above stays. Nothing else on this machine changed, and the\n"+
		"legacy backup is still running", err)
}

// adoptFirst is what has to be true before a code is worth asking for. Printed
// before the question, so that somebody who was about to say yes on a machine
// nobody has adopted yet can see the order of things.
const adoptFirst = `
what happens after that
  The two Eumaeus commands to run are printed once the plan is written, with
  this machine's bucket and node ID already filled in and wrapped in ssh so
  they can be run from here. The second issues a code. Then:

      sion-backup adopt-enroll --code <the code>

  A code is good for fifteen minutes and can be used once, so ask for it when
  you are ready to be standing here.
`

// adoption is one machine's migration, assembled from what is already on it.
//
// It holds no credential. The one place this command needs the legacy secrets
// — counting what is in the old repository — reads them, uses them for a
// single restic invocation and lets them go; see [adoption.measure].
type adoption struct {
	install *legacybus.Install

	// bucket and prefix are the repository URL taken apart, because Eumaeus
	// adopts a bucket by name and cannot be handed a restic URL.
	bucket string
	prefix string

	// plan is what this machine will back up once it is enrolled. Assembled
	// from the legacy install and nothing else — this is the whole point of
	// the command.
	plan planbus.Plan

	// fromFile is how many of the plan's excludes came out of the legacy
	// exclude file, so the operator can see their list arrived.
	fromFile int

	// notes are the things that could not be carried over and want a person.
	notes []string

	// snapshots and oldest are what the legacy repository actually holds,
	// when it was opened. Zero means nobody looked, which is not the same as
	// an empty repository and is never reported as one.
	snapshots int
	oldest    time.Time

	// ssh is the host to run the Eumaeus commands on, or empty to print them
	// bare. See [sshTarget].
	ssh string
}

// How Eumaeus's admin commands have to be invoked on the server.
//
// Three facts, none of them ours, all three load-bearing:
//
//   - Operator access is as root, by key. Eumaeus's own scripts all use
//     `ssh root@$host`, and its cloud-init sets PermitRootLogin
//     prohibit-password. The `deploy` user exists and is the wrong one: its
//     only privilege is one install script, and it is deliberately in no
//     group that can read the fleet store.
//
//   - The CLI opens a LOCAL store, as the service account, with the data
//     directory named. Run without `-u eumaeus` and EUMAEUS_DATA_DIR, it
//     opens root's own store, which is empty — and an empty store does not
//     refuse, it answers every question wrongly and plausibly. Eumaeus's
//     scripts say exactly that, in a comment, which is how we know it is a
//     mistake somebody has already made.
//
//   - `backup adopt` reads the repository password from /dev/tty rather than
//     from stdin, on purpose, so it cannot be piped and needs a real
//     terminal. Hence `ssh -t` for that one.
//
// Read out of the eumaeus repository rather than measured against the server.
// If the deployment moves, this goes stale quietly — which is the cost of
// printing a command somebody pastes, and worth it against the cost of
// printing one that silently addresses an empty database.
const (
	// eumaeusSSHUser is who to log in as. --ssh overrides the whole target.
	eumaeusSSHUser = "root"

	// eumaeusRun is the prefix every admin subcommand needs, ssh or no ssh.
	eumaeusRun = "sudo -u eumaeus EUMAEUS_DATA_DIR=/var/lib/eumaeus eumaeus"
)

// sshTarget is where the Eumaeus commands should be run.
//
// They are printed to be run somewhere else, and "somewhere else" is over SSH
// for everybody who has ever run them. Printing them bare means whoever is at
// the machine retypes them into a second terminal, which is the same
// transcription this command exists to remove — so the server's own host, out
// of the URL this machine already talks to, is the default.
//
// --ssh names a different target, user included; --ssh "" prints the commands
// bare, which is what a loopback test server wants and what somebody already
// sitting on the server wants. The sudo prefix is not part of this: it is
// needed whether or not there is an ssh in front of it.
func sshTarget(override, serverURL string) string {
	if override != "" {
		return override
	}

	u, err := url.Parse(serverURL)
	if err != nil {
		return ""
	}

	host := u.Hostname()

	// A loopback server is either a test or the machine you are already on.
	// Neither wants an ssh line in front of the command.
	switch host {
	case "", "localhost", "127.0.0.1", "::1":
		return ""
	}

	return eumaeusSSHUser + "@" + host
}

// assemble turns a recon report into a migration, or explains why it cannot.
func assemble(r Recon) (adoption, error) {
	switch {
	case r.Legacy == nil && len(r.Blocked) > 0:
		return adoption{}, fmt.Errorf("no legacy install was found, but this account "+
			"could not read %s — and a home directory belonging to another account is "+
			"exactly where these installs live.\n\n"+
			"Re-run as %s. Migrating on the strength of a report that was not allowed "+
			"to look is how a machine ends up with two backups and one bucket nobody "+
			"reads again", strings.Join(r.Blocked, ", "), elevated("adopt-enroll"))

	case r.Legacy == nil:
		return adoption{}, errors.New("no legacy install was found on this machine, so " +
			"there is nothing to adopt.\n\n" +
			"If that is right, this is a new machine: provision a bucket in Eumaeus, " +
			"issue a code, and run \"sion-backup enroll --code ...\". If it is not, say " +
			"where the old install is with --legacy-dir")
	}

	l := r.Legacy

	// The scan's own notes first. "There is more than one legacy install
	// here" is the one sentence in this whole command that most wants
	// reading before a bucket is chosen.
	a := adoption{install: l, notes: r.Notes}
	a.bucket, a.prefix = bucketFromURL(l.RepositoryURL)

	a.plan = planbus.Plan{
		NodeID:     l.NodeID,
		Repository: l.RepositoryURL,
		Targets:    l.Targets,

		// The excludes written on the command line, before the ones in the
		// file: on Linux these are /dev, /proc and /sys, and they are the
		// ones a migration loses without noticing.
		Excludes: l.Excludes,

		PackSizeMiB:     l.PackSizeMiB,
		ReadConcurrency: l.ReadConcurrency,

		// The legacy schedule is reported, not copied. See [adoption.notes].
		Schedule: planbus.DefaultSchedule(),

		// Platform answers rather than legacy ones: --use-fs-snapshot is
		// carried over because losing VSS is a silent regression, and the
		// other two are this program's own behaviour and were not settings
		// the old scripts had.
		UseFSSnapshot:    l.UsesFSSnapshot,
		AllowVSSFallback: true,
		OneFileSystem:    true,
	}

	switch patterns, err := legacyscan.ExcludePatterns(l); {
	case err != nil:
		a.notes = append(a.notes, "the exclude list at "+l.ExcludeFile+" could not be read ("+
			err.Error()+"), so the plan carries only the excludes written in the script. "+
			"Re-run as "+elevated("adopt-enroll")+", or copy them in by hand on the "+
			"status page — the first backup will otherwise include things somebody "+
			"chose to leave out")

	default:
		a.fromFile = len(patterns)
		a.plan.Excludes = append(a.plan.Excludes, patterns...)
	}

	if a.plan.NodeID == "" {
		a.plan.NodeID = hostname()
		a.notes = append(a.notes, "the legacy script does not name a node ID, so this "+
			"machine's hostname is used below. Decide on one before adopting: it is what "+
			"the dashboard calls this machine, and Eumaeus keeps whatever is given")
	}

	if l.Schedule != "" {
		a.notes = append(a.notes, "the legacy schedule ("+l.Schedule+") is NOT copied. "+
			"This program schedules its own runs and catches up a slot a sleeping laptop "+
			"missed, which cron cannot; the new plan runs at 13:00 local, and the status "+
			"page is where to change that")
	}

	if len(a.plan.Targets) == 0 {
		a.notes = append(a.notes, "what the legacy script backs up could not be read out "+
			"of it. Open "+l.Script+" and set the folders on the status page once the "+
			"service is running — nothing is backed up until you do")
	}

	return a, nil
}

// describe prints what is being taken over.
func (a adoption) describe() {
	l := a.install

	fmt.Printf("taking over\n")
	fmt.Printf("  legacy install %s %s in %s\n", l.Layout, l.Version, l.Dir)
	fmt.Printf("  repository     %s\n", l.RepositoryURL)

	if a.bucket != "" {
		if a.prefix != "" {
			fmt.Printf("  bucket         %s, under the prefix %s\n", a.bucket, a.prefix)
		} else {
			fmt.Printf("  bucket         %s\n", a.bucket)
		}
	}

	if l.NodeID != "" {
		fmt.Printf("  node id        %s\n", l.NodeID)
	}

	if l.Account != "" {
		fmt.Printf("  account        %s\n", l.Account)
	}

	if l.Schedule != "" {
		fmt.Printf("  schedule       %s\n", l.Schedule)
	}

	where := l.Script
	if l.PasswordFile != "" {
		where += " and " + l.PasswordFile
	}

	if l.HasCredentials {
		fmt.Printf("  credentials    in %s — not printed, not copied\n", where)
	} else {
		fmt.Printf("  credentials    NONE FOUND in %s. Adopting needs the repository\n", where)
		fmt.Printf("                 password; find it before going on\n")
	}
}

// plannedHere prints what this command writes on this machine.
//
// measuring says whether the legacy repository is going to be opened, which is
// the one other thing written here: restic itself, if this machine has not got
// the fleet's copy yet. A list of what will be touched that quietly omitted a
// 20 MB download would be the wrong kind of list.
func (a adoption) plannedHere(d *deps, measuring bool) {
	fmt.Printf("\nwhat gets written here\n")
	fmt.Printf("  the plan       %s\n", d.paths.DB)
	fmt.Printf("  node           %s\n", a.plan.NodeID)
	fmt.Printf("  targets        %s\n", orNone(strings.Join(a.plan.Targets, " ")))
	fmt.Printf("  excludes       %s\n", a.excludeSummary())

	if a.plan.PackSizeMiB > 0 || a.plan.ReadConcurrency > 0 {
		fmt.Printf("  tuning         pack %d MiB, read concurrency %d — as the legacy\n",
			a.plan.PackSizeMiB, a.plan.ReadConcurrency)
		fmt.Printf("                 script had them\n")
	}

	if a.plan.UseFSSnapshot {
		fmt.Printf("  vss            yes, as the legacy script had it\n")
	}

	fmt.Printf("  schedule       %s local, jitter %d minutes\n",
		strings.Join(a.plan.Schedule.Times, ", "), a.plan.Schedule.JitterMinutes)

	if measuring && d.restic.Managed() {
		fmt.Printf("  restic         %s, if it is not there yet — downloaded from\n", d.restic.Bin())
		fmt.Printf("                 restic's own release and verified against the hash in\n")
		fmt.Printf("                 this binary. Enrolling would fetch it anyway\n")
	}

	fmt.Printf("\nwhat is left alone\n")
	fmt.Printf("  The legacy install, its schedule, its binaries and its credentials.\n")
	fmt.Printf("  Nothing of it is changed, moved or disabled by this command.\n")

	for _, n := range a.notes {
		fmt.Printf("\n  NOTE %s\n", wrap(n, 7))
	}
}

// excludeSummary says where the plan's excludes came from, because "20" on its
// own does not tell an operator whether their list arrived.
func (a adoption) excludeSummary() string {
	inline := len(a.plan.Excludes) - a.fromFile

	switch {
	case len(a.plan.Excludes) == 0:
		return "none"

	case a.fromFile == 0:
		return fmt.Sprintf("%d, from the script", inline)

	case inline == 0:
		return fmt.Sprintf("%d, from %s", a.fromFile, a.install.ExcludeFile)

	default:
		return fmt.Sprintf("%d: %d from the script, %d from %s",
			len(a.plan.Excludes), inline, a.fromFile, a.install.ExcludeFile)
	}
}

// seedAdoptedPlan writes the assembled plan, and only on a machine that has
// none.
//
// [planbus.Business.Seed] rather than Put, deliberately. A machine that has
// already been set up has a plan somebody may have edited on the status page,
// and silently replacing it with one read out of a script that is on its way
// to being deleted is exactly the maddening bug planbus is shaped to avoid.
// Re-running this command is a normal thing to do — the first run may have
// stopped at "adopt it in Eumaeus" — so the second one says what it skipped
// rather than failing.
func (d *deps) seedAdoptedPlan(ctx context.Context, a adoption) error {
	if len(a.plan.Targets) == 0 {
		fmt.Printf("\nNo plan was written: nothing was found for it to back up. Everything\n")
		fmt.Printf("below still applies — the bucket is worth adopting whatever this\n")
		fmt.Printf("machine turns out to back up.\n")

		return nil
	}

	seeded, err := d.plan.Seed(ctx, a.plan, time.Now())
	if err != nil {
		return err
	}

	if !seeded {
		fmt.Printf("\nThis machine already has a backup plan, so the legacy one was NOT\n")
		fmt.Printf("written over it. Compare them on the status page if that is a\n")
		fmt.Printf("surprise; everything below still applies.\n")

		return nil
	}

	fmt.Printf("\nThe plan is written. Nothing runs from it until this machine is\n")
	fmt.Printf("enrolled and the service is installed.\n")

	return nil
}

// legacyProbeTimeout bounds the one command run against the old repository.
//
// Generous, because it is a listing over somebody's office connection against
// a bucket with years in it, and the number it produces is the difference
// between an owner's page that says "backups since March 2019" and one that
// says the history starts today.
const legacyProbeTimeout = 10 * time.Minute

// measure opens the legacy repository and counts what is in it.
//
// The numbers go into the `eumaeus backup adopt` command below as
// -snapshots and -history-since. Without them adoption still works and the
// history horizon is today, which is wrong on every machine this command is
// for.
//
// A failure is reported and carried on from. There are several ordinary
// reasons this cannot be done — a scrubbed script, a password file only root
// can read, no network — and none of them is a reason to refuse to migrate a
// machine.
func (a *adoption) measure(ctx context.Context, d *deps) {
	creds, err := legacyscan.Credentials(a.install)

	switch {
	case err != nil:
		fmt.Printf("\nCould not read the legacy credentials (%v), so the repository was\n", err)
		fmt.Printf("not opened. Adopt without -snapshots and -history-since, or re-run\n")
		fmt.Printf("as %s.\n", elevated("adopt-enroll"))

		return

	case !creds.Complete():
		fmt.Printf("\nThe legacy script does not carry a complete set of credentials, so\n")
		fmt.Printf("the repository was not opened. Adopt without -snapshots and\n")
		fmt.Printf("-history-since; the horizon will read as today.\n")

		return
	}

	if err := d.ensureRestic(ctx); err != nil {
		fmt.Printf("\nThe repository was not opened: %v\n", err)

		return
	}

	ctx, cancel := context.WithTimeout(ctx, legacyProbeTimeout)
	defer cancel()

	fmt.Printf("\nOpening the legacy repository to count what is in it. This is a\n")
	fmt.Printf("listing, not a download, but it is a listing over the office\n")
	fmt.Printf("connection — give it a minute.\n")

	snaps, err := d.restic.Snapshots(ctx, restic.Repository{
		URL:             a.install.RepositoryURL,
		Password:        creds.Password,
		AccessKeyID:     creds.AccessKeyID,
		SecretAccessKey: creds.SecretAccessKey,
	})
	if err != nil {
		fmt.Printf("\nThe legacy repository would not open: %v\n", err)
		fmt.Printf("Adopt without -snapshots and -history-since. Worth knowing before\n")
		fmt.Printf("the migration rather than after it: this is the password Eumaeus is\n")
		fmt.Printf("about to be given.\n")

		return
	}

	a.snapshots, a.oldest = count(snaps)

	if a.snapshots == 0 {
		// It opened, and there is nothing in it. Worth stopping over: the
		// legacy install on this machine has been running nightly and
		// writing nowhere, which is the failure this whole program exists
		// against, and it has just been found with somebody standing here.
		fmt.Printf("\nTHE LEGACY REPOSITORY IS EMPTY. It opened with the credentials in\n")
		fmt.Printf("%s, and holds no snapshots at all —\n", a.install.Script)
		fmt.Printf("so whatever this machine has been doing nightly, it has not been\n")
		fmt.Printf("backing up. There is no history to adopt; find out why before\n")
		fmt.Printf("deciding whether to adopt this bucket or provision a new one.\n")

		return
	}

	fmt.Printf("\nIt holds %s, the oldest from %s.\n",
		plural(a.snapshots, "snapshot"), a.oldest.Format("2 January 2006"))
}

// count reduces a snapshot listing to the two facts adoption needs.
func count(snaps []restic.Snapshot) (int, time.Time) {
	var oldest time.Time

	for _, s := range snaps {
		if oldest.IsZero() || s.Time.Before(oldest) {
			oldest = s.Time
		}
	}

	return len(snaps), oldest
}

// serverSteps prints the two commands somebody with an Eumaeus admin terminal
// has to run, filled in from this machine.
//
// Filled in rather than described, because the alternative is an operator
// transcribing a bucket name off a screen into a shell, and a mistyped bucket
// name does not fail — it provisions a second one.
func (a adoption) serverSteps() {
	if a.ssh != "" {
		fmt.Printf("\nwhat has to happen in Eumaeus — run these from here\n\n")
	} else {
		fmt.Printf("\nwhat has to happen in Eumaeus, by somebody with an admin terminal\n\n")
	}

	node := a.plan.NodeID
	bucket := a.bucket

	if bucket == "" {
		bucket = "<BUCKET — could not be read out of " + a.install.RepositoryURL + ">"
	}

	adopt := eumaeusRun + " backup adopt -owner <OWNER EMAIL> -node " + node + " \\\n" +
		"      -bucket " + bucket

	if a.snapshots > 0 {
		adopt += fmt.Sprintf(" \\\n      -history-since %s -snapshots %d",
			a.oldest.Format("2006-01-02"), a.snapshots)
	}

	code := eumaeusRun + " backup code " + node

	switch a.ssh {
	case "":
		fmt.Printf("    %s backup check\n", eumaeusRun)
		fmt.Printf("    %s\n", adopt)
		fmt.Printf("    %s\n", code)

	default:
		// -t on the first two, because `adopt` reads the repository password
		// from /dev/tty rather than from stdin — deliberately, so it cannot
		// be piped — and `check` is where a missing provisioning key is
		// found. Not on the last: it prints a code to be copied, and a
		// pseudo-terminal puts carriage returns through the middle of that.
		//
		// Double quotes around adopt, so that the backslash-newlines inside
		// it are continuations of the line being typed here rather than
		// characters sent to the far end.
		fmt.Printf("    ssh -t %s '%s backup check'\n", a.ssh, eumaeusRun)
		fmt.Printf("    ssh -t %s \"%s\"\n", a.ssh, adopt)
		fmt.Printf("    ssh %s '%s'\n", a.ssh, code)
	}

	fmt.Printf("\n  `backup check` first, because `adopt` needs the Wasabi provisioning\n")
	fmt.Printf("  key and fails without it — better found before the bucket than\n")
	fmt.Printf("  halfway through adopting it.\n")

	fmt.Printf("\n  sudo -u eumaeus and EUMAEUS_DATA_DIR are not decoration. Without\n")
	fmt.Printf("  them the CLI opens root's own store, which is empty — and an empty\n")
	fmt.Printf("  store does not refuse, it answers every question wrongly.\n")

	fmt.Printf("\n  adopt, not provision. `provision` makes a new empty bucket, and the\n")
	fmt.Printf("  first backup from here would then upload everything and leave the\n")
	fmt.Printf("  history attached to nothing.\n")

	fmt.Printf("\n  The repository password is typed at adopt's prompt, never passed as a\n")
	fmt.Printf("  flag — a password in a flag is a password in shell history. It reads\n")
	fmt.Printf("  /dev/tty rather than stdin, which is why that line has ssh -t and why\n")
	fmt.Printf("  it cannot be piped. It is in %s.\n", passwordLocation(a.install))

	if a.snapshots == 0 {
		fmt.Printf("\n  -history-since and -snapshots are left off because this machine did\n")
		fmt.Printf("  not open the repository. Both are optional; without the first, the\n")
		fmt.Printf("  owner's page says the history starts today.\n")
	}

	if a.prefix != "" {
		fmt.Printf("\n  This repository is under the prefix %q inside the bucket. Check\n", a.prefix)
		fmt.Printf("  that adoption points at the same place before issuing a code.\n")
	}

	fmt.Printf("\nthen, back here\n")
	fmt.Printf("    sion-backup adopt-enroll --code <the code>\n")
	fmt.Printf("  which claims it and checks that the bucket handed back is this one.\n")
}

// passwordLocation names the file holding the repository password.
func passwordLocation(l *legacybus.Install) string {
	if l.PasswordFile != "" {
		return l.PasswordFile
	}

	return l.Script + ", as RESTIC_PASSWORD"
}

// verify is the reason this command exists rather than a page of instructions.
//
// The claim has already been made by this point and the token is on disk. What
// is checked here is the thing an administrator can get wrong in a way nothing
// else would notice: `provision` typed where `adopt` was meant, which hands
// back a working, empty bucket. Every subsequent step succeeds. The machine
// backs up, the dashboard goes green, and the two years of history sit in a
// bucket nothing points at until somebody needs a file from 2024.
func (a adoption) verify(ctx context.Context, d *deps, e eumaeuscreds.Enrollment) {
	fmt.Printf("\nadoption\n")

	if e.RepositoryURL != a.install.RepositoryURL {
		fmt.Printf("  THIS IS NOT THE LEGACY REPOSITORY\n")
		fmt.Printf("    legacy    %s\n", a.install.RepositoryURL)
		fmt.Printf("    enrolled  %s\n", e.RepositoryURL)
		fmt.Printf("\n  %s\n", wrap("Eumaeus provisioned a bucket rather than adopting this "+
			"machine's. The first backup will upload everything, and the legacy history "+
			"stays where it is, attached to nothing.", 2))
		fmt.Printf("\n  %s\n", wrap("If that was intended, carry on. If it was not: adopt the "+
			"bucket in Eumaeus, issue a second code, and run this command again with "+
			"--force. Do NOT disable the legacy schedule in the meantime.", 2))

		return
	}

	fmt.Printf("  repository     the legacy one, unchanged\n")

	switch {
	case e.RepositoryAdopted:
		fmt.Printf("  eumaeus says   adopted")

		if e.RepositorySnapshots > 0 {
			fmt.Printf(", %s", plural(e.RepositorySnapshots, "snapshot"))
		}

		if !e.RepositoryCreatedAt.IsZero() {
			fmt.Printf(", history from %s", e.RepositoryCreatedAt.Format("2 January 2006"))
		}

		fmt.Println()

	default:
		// Absence is not denial: the fields are omitted by a server that was
		// not told, and by every adoption made before Eumaeus grew them.
		fmt.Printf("  eumaeus says   nothing either way, which means ask restic\n")
	}

	if e.NodeID != a.plan.NodeID {
		fmt.Printf("  NODE ID        %s here, %s in Eumaeus — the dashboard will call\n",
			a.plan.NodeID, e.NodeID)
		fmt.Printf("                 this machine something its history does not\n")
	}

	// The authority on what a repository contains is the repository. Asked
	// with the credentials Eumaeus just issued, so this proves the new keys
	// open the old history — which is the whole of what adoption promised.
	snaps, err := d.restic.Snapshots(ctx, restic.Repository{
		URL:             e.RepositoryURL,
		Password:        []byte(e.ResticPassword),
		AccessKeyID:     []byte(e.MachineKeyID),
		SecretAccessKey: []byte(e.MachineKeySecret),
	})
	if err != nil {
		fmt.Printf("  restic says    the snapshots could not be listed: %v\n", err)

		return
	}

	n, oldest := count(snaps)

	switch {
	case n == 0:
		fmt.Printf("  restic says    THE REPOSITORY IS EMPTY. The new keys opened it, so\n")
		fmt.Printf("                 this is the wrong bucket or an adoption that did not\n")
		fmt.Printf("                 land. Stop, and do not disable the legacy schedule.\n")

	default:
		fmt.Printf("  restic says    %s here, the oldest from %s\n",
			plural(n, "snapshot"), oldest.Format("2 January 2006"))
		fmt.Printf("                 the keys Eumaeus just issued open the old history\n")

		if e.RepositorySnapshots > n {
			// The repository is the authority on what it contains; the count
			// in Eumaeus was typed by a person at adoption. They disagree
			// when that number was read off the wrong machine's report,
			// which is worth saying while somebody is standing here.
			fmt.Printf("  DISAGREEMENT   Eumaeus was told %d. The repository is the authority\n",
				e.RepositorySnapshots)
			fmt.Printf("                 on what it holds; that number was typed by hand at\n")
			fmt.Printf("                 adoption, and this one was not\n")
		}
	}

	fmt.Print(adoptRemaining)
}

// adoptRemaining is what is left after a machine is enrolled, in the order it
// has to happen. The last step is last for a reason and says so.
const adoptRemaining = `
what remains
  1. sion-backup run
     The first backup, in the foreground, where you can watch it. Against an
     adopted repository this is an ordinary incremental run, not a re-upload.

  2. Install the service — see the README for this platform.

  3. LAST, and only once step 1 has produced a verified backup: turn off the
     legacy schedule. install.sh --disable-legacy, or install.ps1
     -DisableLegacyTask. Until then both are running, which is untidy and
     harmless; neither running is the failure worth avoiding.
`

// bucketFromURL takes a restic S3 URL apart.
//
// Eumaeus adopts a bucket by name and has no use for a restic URL, so this is
// the translation between the two. A URL with more than one path segment is a
// repository under a prefix, which is reported rather than quietly dropped:
// adopting the bucket and pointing at the wrong place inside it would look
// like success.
func bucketFromURL(raw string) (bucket, prefix string) {
	rest, ok := strings.CutPrefix(raw, "s3:")
	if !ok {
		return "", ""
	}

	for _, scheme := range []string{"https://", "http://"} {
		rest = strings.TrimPrefix(rest, scheme)
	}

	_, path, found := strings.Cut(rest, "/")
	if !found {
		return "", ""
	}

	parts := strings.Split(strings.Trim(path, "/"), "/")
	if parts[0] == "" {
		return "", ""
	}

	return parts[0], strings.Join(parts[1:], "/")
}

// splitList turns a comma-separated flag into the values in it.
func splitList(raw string) []string {
	var out []string

	for _, item := range strings.Split(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}

	return out
}

// plural counts something in a sentence a person reads. "1 snapshots" is the
// sort of thing that makes an operator wonder what else was not looked at.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}

	return fmt.Sprintf("%d %ss", n, noun)
}

// orNone renders an empty list as a word rather than as nothing at all.
func orNone(s string) string {
	if s == "" {
		return "none found"
	}

	return s
}

// confirm asks before something is written.
//
// Silence is a no. This runs on a machine somebody depends on, it may be
// invoked from an installer with no terminal attached, and the safe reading of
// "no answer" is not to proceed — --yes is how a script says yes.
func confirm(question string) (bool, error) {
	fmt.Printf("\n%s [y/N] ", question)

	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}

	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil

	default:
		return false, nil
	}
}
