package main

import (
	"cmp"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jroedel/sion-backup/business/domain/legacy/legacybus"
	"github.com/jroedel/sion-backup/business/domain/legacy/sources/legacyscan"
	"github.com/jroedel/sion-backup/foundation/paths"
	"github.com/jroedel/sion-backup/foundation/restic"
	"github.com/jroedel/sion-backup/foundation/token"
)

const reconUsage = `sion-backup recon — what is already on this machine

Usage:
  sion-backup recon [--json]

Reads and reports; changes nothing. Run it before installing on a machine
that has been backing up for two years with the old scripts, which is most
of them.

It reports the machine, whether the server is reachable, what restic is
here, whether sion-backup is already installed — and the legacy install, if
there is one: where it lives, which bucket it has been writing to, what it
backs up, and what starts it.

It never prints a credential. The legacy scripts carry the S3 keys and the
repository password in plain text; this says which file they are in and
leaves them there.

Run it as root, or with sudo, when you can. The old installs commonly live
in /home/restic, which an ordinary account cannot look inside — and a report
that cannot see the backup already running here is worse than no report. It
says so plainly when that happens rather than reporting "none found".

Flags:
  --json         machine-readable, for the installers
  --legacy-dir   also look here for the old install (comma-separated). These
                 installs were done by hand, so if it is somewhere the notes
                 never mentioned, this is how to point at it.
`

// Recon is everything this command found.
type Recon struct {
	Machine  MachineInfo        `json:"machine"`
	Server   ServerInfo         `json:"server"`
	Restic   ResticInfo         `json:"restic"`
	Existing ExistingInstall    `json:"sion_backup"`
	Legacy   *legacybus.Install `json:"legacy,omitempty"`

	// Blocked are directories this account could not look inside. Reported
	// as a field of its own rather than only as prose, because an installer
	// reading --json has to be able to refuse to treat this machine as new.
	Blocked []string `json:"blocked,omitempty"`

	Notes []string `json:"notes,omitempty"`
	Plan  []string `json:"plan,omitempty"`
}

// MachineInfo is what this computer is.
type MachineInfo struct {
	Hostname string `json:"hostname"`
	OS       string `json:"os"`
	Account  string `json:"account"`
	DataDir  string `json:"data_dir"`
}

// ServerInfo is whether Eumaeus can be reached from here, which is the
// question a machine that cannot back up usually turns out to answer no.
type ServerInfo struct {
	URL       string `json:"url"`
	Reachable bool   `json:"reachable"`
	Detail    string `json:"detail"`

	// ClockSkew is this machine's clock against the server's, from the Date
	// header. A wrong clock makes a backup look older or newer than it is
	// and is invisible until somebody compares two dashboards.
	ClockSkew string `json:"clock_skew,omitempty"`
}

// ResticInfo is the binary that does the work: the one this machine will
// use, and the one it happens to have.
//
// Those are two different questions now that the fleet installs its own
// restic, and recon is the command that must not conflate them. A machine
// with a snap-installed 0.14 on PATH has restic and does not have the fleet's
// restic, and an installer that reads "found" and moves on is an installer
// that discovers the difference later.
type ResticInfo struct {
	// Pinned is the version this fleet runs, from the binary reading this.
	Pinned string `json:"pinned"`

	// Path is where the binary this machine will run is, or will be.
	Path string `json:"path,omitempty"`

	// Managed is false when the config file names a restic, in which case
	// nothing here installs or upgrades it.
	Managed bool `json:"managed"`

	// Version is what is at Path today. Empty means nothing is there yet,
	// which is the ordinary state of a machine before it enrolls.
	Version string `json:"version,omitempty"`

	// Problem is why the binary at Path could not be asked, when there is
	// something there and it did not answer.
	Problem string `json:"problem,omitempty"`

	// OnPath is an unrelated restic found on PATH, and OnPathVersion what it
	// reports. Recorded because it is what the old scripts used and what a
	// person at the machine will see when they type "restic version" — and
	// because it is not what backups will run.
	OnPath        string `json:"on_path,omitempty"`
	OnPathVersion string `json:"on_path_version,omitempty"`
}

// ExistingInstall is this program, if it is already here.
type ExistingInstall struct {
	Installed bool   `json:"installed"`
	Version   string `json:"version,omitempty"`
	Enrolled  bool   `json:"enrolled"`
	NodeID    string `json:"node_id,omitempty"`
}

func reconCmd(args []string) error {
	fs := flag.NewFlagSet("recon", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, reconUsage) }

	asJSON := fs.Bool("json", false, "machine-readable output")
	legacyDir := fs.String("legacy-dir", "",
		"also look here for the old install (comma-separated)")

	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, cancel := signalContext()
	defer cancel()

	r := gather(ctx, splitList(*legacyDir)...)

	if *asJSON {
		out, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			return err
		}

		fmt.Println(string(out))

		return nil
	}

	r.print()

	return nil
}

// gather does the looking.
//
// Deliberately not wire(): recon runs on a machine where the database may not
// exist and restic may not be installed, and refusing to report because one
// of those is true would make it useless exactly when it is needed.
func gather(ctx context.Context, extraLegacyDirs ...string) Recon {
	var r Recon

	host, _ := os.Hostname()

	r.Machine = MachineInfo{
		Hostname: host,
		OS:       runtime.GOOS + "/" + runtime.GOARCH,
		Account:  localAccount(),
	}

	p, err := paths.Resolve()
	if err == nil {
		r.Machine.DataDir = p.DataDir
		r.Existing = existingInstall(p)
	} else {
		r.Notes = append(r.Notes, "could not resolve the data directory: "+err.Error())
	}

	cfg, _, _ := LoadConfig(p.Config)
	r.Server = reachable(ctx, cfg.EumaeusURL())
	r.Restic = resticInfo(ctx, cfg.Server.Restic, p.Bin)

	found := legacyscan.Find(ctx, extraLegacyDirs...)
	r.Legacy = found.Install
	r.Blocked = found.Blocked
	r.Notes = append(r.Notes, found.Notes...)
	r.Plan = plan(r)

	return r
}

// existingInstall reports whether this program is already set up here.
func existingInstall(p paths.Paths) ExistingInstall {
	out := ExistingInstall{Version: version}

	if _, err := os.Stat(p.DB); err == nil {
		out.Installed = true
	}

	if tok, err := token.Load(p.Token); err == nil && tok != "" {
		out.Installed = true
		out.Enrolled = true
	}

	return out
}

// reachable asks the server whether it is there, and what time it thinks it
// is.
func reachable(ctx context.Context, url string) ServerInfo {
	info := ServerInfo{URL: url}

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(url, "/")+"/healthz", nil)
	if err != nil {
		info.Detail = err.Error()

		return info
	}

	req.Header.Set("User-Agent", "sion-backup/"+version)

	started := time.Now()

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		info.Detail = err.Error()

		return info
	}
	defer resp.Body.Close()

	info.Reachable = resp.StatusCode == http.StatusOK
	info.Detail = fmt.Sprintf("%s in %s", resp.Status, time.Since(started).Round(time.Millisecond))

	// The server's clock against this machine's. A laptop whose clock is
	// hours out reports backups at the wrong time and can look healthy when
	// it is not -- the server alerts on when it received an event, but the
	// snapshot timestamps in the repository are this machine's.
	if served, err := http.ParseTime(resp.Header.Get("Date")); err == nil {
		skew := time.Since(served).Round(time.Second)
		if skew < 0 {
			skew = -skew
		}

		switch {
		case skew < 30*time.Second:
			info.ClockSkew = "in step (" + skew.String() + ")"
		default:
			info.ClockSkew = "OUT BY " + skew.String()
		}
	}

	return info
}

// resticInfo reports the binary this machine will run, and separately
// whatever restic is on PATH.
//
// It installs nothing. recon changes nothing on the machine, and a 20 MB
// download is not a thing a report should do — it says what enrolling will
// fetch, which is what the person reading it needs to know.
func resticInfo(ctx context.Context, configured, binDir string) ResticInfo {
	ctx, cancel := context.WithTimeout(ctx, resticProbeTimeout)
	defer cancel()

	out := ResticInfo{Pinned: restic.PinnedVersion}
	out.OnPath, out.OnPathVersion = onPath(ctx)

	r, err := restic.Resolve(configured, binDir)
	if err != nil {
		out.Problem = err.Error()

		return out
	}

	out.Path = r.Bin()
	out.Managed = r.Managed()

	switch v, err := r.InstalledVersion(ctx); {
	case err == nil:
		out.Version = v

	case out.Managed && !fileExists(r.Bin()):
		// Not a problem, and saying so in the Problem field would put a
		// stat error in front of somebody for whom the answer is "nothing
		// has installed it yet, and enrolling will".

	default:
		out.Problem = err.Error()
	}

	return out
}

// resticProbeTimeout bounds the two `restic version` execs recon does.
const resticProbeTimeout = 30 * time.Second

// onPath reports an unmanaged restic on PATH, and what it says it is.
func onPath(ctx context.Context) (path, version string) {
	r, err := restic.New("")
	if err != nil {
		return "", ""
	}

	v, err := r.InstalledVersion(ctx)
	if err != nil {
		// A binary on PATH that will not answer is worth naming anyway: it
		// is what somebody at the machine will find when they look.
		return r.Bin(), ""
	}

	return r.Bin(), v
}

// fileExists is the question "is there something there at all", asked
// separately from "does it run".
func fileExists(path string) bool {
	_, err := os.Stat(path)

	return err == nil
}

// plan turns what was found into what an upgrade would do.
//
// Sentences rather than steps a machine executes: the installer prints these
// and waits for a person, because the decision in the middle of them — adopt
// the existing bucket or start a new one — is not one a script should make on
// somebody's two years of history.
func plan(r Recon) []string {
	var out []string

	// What the new install needs, before anything about the old one. It is
	// the same sentence on every machine, which is the point of managing the
	// version: nobody has to decide anything about restic.
	if r.Restic.Managed && r.Restic.Version != r.Restic.Pinned {
		out = append(out, fmt.Sprintf(
			"Enrolling will download restic %s, verify it against the hash in this "+
				"binary, and keep it in %s. Nothing else on this machine is touched, "+
				"and no administrator is needed.", r.Restic.Pinned, r.Restic.Path))
	}

	if r.Legacy == nil && len(r.Blocked) > 0 {
		// The one thing this report must never do is call a machine new when
		// it was not allowed to look at the place the old install lives.
		out = append(out, fmt.Sprintf(
			"NOT PROVEN NEW: nothing was found, but this account could not read %s — "+
				"and a home directory belonging to another account is exactly where "+
				"these installs live. Re-run as %s before concluding anything.",
			strings.Join(r.Blocked, ", "), elevated("recon")))

		out = append(out,
			"If it really is new: provision a bucket in Eumaeus, issue a code, and "+
				"run \"sion-backup enroll --code ...\". If it is not, \"sion-backup "+
				"adopt-enroll\" run as "+elevated("adopt-enroll")+" is what takes the old "+
				"install over, and it refuses to migrate a machine it was not allowed "+
				"to look at.")

		return out
	}

	if r.Legacy == nil {
		out = append(out,
			"No legacy install found: this is a new machine as far as backups go.",
			"Provision a bucket in Eumaeus, issue a code, and run \"sion-backup enroll --code ...\".")

		return out
	}

	l := r.Legacy

	out = append(out, fmt.Sprintf(
		"A legacy install is running here (%s %s, in %s). It is still taking backups.",
		l.Layout, l.Version, l.Dir))

	if l.RepositoryURL != "" {
		out = append(out, fmt.Sprintf(
			"It writes to %s. Decide before enrolling: have Eumaeus ADOPT this bucket "+
				"(keeps the history, no re-upload) or provision a new one (starts from "+
				"nothing, and the first backup uploads everything). \"sion-backup "+
				"adopt-enroll\" does the first of those: it assembles the plan from what "+
				"is here and prints the Eumaeus commands to run, filled in.", l.RepositoryURL))
	}

	if l.HasCredentials {
		where := l.Script
		if l.PasswordFile != "" {
			where += " and " + l.PasswordFile
		}

		out = append(out, "The credentials for that bucket are in "+where+
			" — they are needed to adopt it, and they are not printed here.")
	}

	if l.NodeID != "" {
		out = append(out, fmt.Sprintf(
			"It calls this machine %q on the old dashboard. Reusing that node ID keeps "+
				"the name somebody recognises.", l.NodeID))
	}

	if l.ExcludeCount > 0 || len(l.Excludes) > 0 {
		out = append(out, fmt.Sprintf(
			"It excludes %s. \"sion-backup adopt-enroll\" copies both into the new plan; "+
				"by hand, the first backup will otherwise include things somebody chose to "+
				"leave out.", excludeSources(l)))
	}

	if l.UsesFSSnapshot {
		out = append(out, "It uses Volume Shadow Copy. Install with -Elevated, or every "+
			"run here will be recorded as degraded.")
	}

	if l.Schedule != "" {
		out = append(out, "It is started by "+l.Schedule+
			". Disable that as the LAST step, once the new install has taken a verified "+
			"backup — two backup systems for one night is untidy; none is worse.")
	} else {
		out = append(out, "Nothing found that starts it automatically. Look for the "+
			"schedule by hand before assuming there is none.")
	}

	return out
}

// print writes the human form.
func (r Recon) print() {
	fmt.Printf("machine\n")
	fmt.Printf("  hostname       %s\n", r.Machine.Hostname)
	fmt.Printf("  os             %s\n", r.Machine.OS)
	fmt.Printf("  account        %s\n", r.Machine.Account)
	fmt.Printf("  data           %s\n", r.Machine.DataDir)

	fmt.Printf("\nserver\n")
	fmt.Printf("  url            %s\n", r.Server.URL)
	fmt.Printf("  reachable      %s\n", answer(r.Server.Reachable, r.Server.Detail))

	if r.Server.ClockSkew != "" {
		fmt.Printf("  clock          %s\n", r.Server.ClockSkew)
	}

	fmt.Printf("\nrestic\n")
	fmt.Printf("  fleet runs     %s\n", r.Restic.Pinned)

	switch {
	case r.Restic.Path == "":
		fmt.Printf("  PROBLEM        %s\n", r.Restic.Problem)

	case !r.Restic.Managed:
		// The config file named one. Its version is the operator's business,
		// and the report says so rather than grading it.
		fmt.Printf("  configured     %s\n", r.Restic.Path)
		fmt.Printf("  version        %s\n", cmp.Or(r.Restic.Version, "will not run: "+r.Restic.Problem))
		fmt.Printf("  managed        no — named in the config file, so this machine keeps\n")
		fmt.Printf("                 whatever version is at that path\n")

	case r.Restic.Version == r.Restic.Pinned:
		fmt.Printf("  here           %s at %s\n", r.Restic.Version, r.Restic.Path)

	case r.Restic.Version != "":
		fmt.Printf("  here           %s at %s\n", r.Restic.Version, r.Restic.Path)
		fmt.Printf("  WRONG VERSION  the next backup replaces it with %s\n", r.Restic.Pinned)

	case r.Restic.Problem != "":
		fmt.Printf("  here           something at %s that will not run: %s\n",
			r.Restic.Path, r.Restic.Problem)
		fmt.Printf("                 the next backup replaces it\n")

	default:
		fmt.Printf("  here           not yet — enrolling downloads and verifies it into\n")
		fmt.Printf("                 %s\n", r.Restic.Path)
	}

	// The restic somebody at this machine would find by typing "restic
	// version", named because it is almost certainly not the one that will
	// take the backups, and a report that omits it invites the argument.
	if r.Restic.OnPath != "" && r.Restic.OnPath != r.Restic.Path {
		also := r.Restic.OnPath
		if r.Restic.OnPathVersion != "" {
			also += " (" + r.Restic.OnPathVersion + ")"
		}

		used := "not used"
		if !r.Restic.Managed {
			used = "not the configured one"
		}

		fmt.Printf("  also on PATH   %s — %s\n", also, used)
	}

	fmt.Printf("\nsion-backup\n")
	fmt.Printf("  installed      %s\n", yesNo(r.Existing.Installed))
	fmt.Printf("  enrolled       %s\n", yesNo(r.Existing.Enrolled))

	if r.Legacy == nil {
		if len(r.Blocked) > 0 {
			fmt.Printf("\nlegacy install   none found WHERE THIS ACCOUNT CAN LOOK\n")
		} else {
			fmt.Printf("\nlegacy install   none found\n")
		}
	} else {
		l := r.Legacy

		fmt.Printf("\nlegacy install   FOUND (%s %s)\n", l.Layout, l.Version)
		fmt.Printf("  directory      %s\n", l.Dir)
		fmt.Printf("  script         %s\n", filepath.Base(l.Script))

		if len(l.Binaries) > 0 {
			fmt.Printf("  binaries       %s\n", strings.Join(l.Binaries, ", "))
		}

		if l.Account != "" {
			fmt.Printf("  account        %s\n", l.Account)
		}

		if l.Schedule != "" {
			fmt.Printf("  schedule       %s\n", l.Schedule)
		}

		if l.RepositoryURL != "" {
			fmt.Printf("  repository     %s\n", l.RepositoryURL)
		}

		if l.NodeID != "" {
			fmt.Printf("  node id        %s\n", l.NodeID)
		}

		if len(l.Targets) > 0 {
			fmt.Printf("  targets        %s\n", strings.Join(l.Targets, " "))
		}

		if l.ExcludeFile != "" || len(l.Excludes) > 0 {
			fmt.Printf("  excludes       %s\n", excludeSources(l))
		}

		if l.UsesFSSnapshot {
			fmt.Printf("  vss            yes (--use-fs-snapshot)\n")
		}

		if l.PackSizeMiB > 0 || l.ReadConcurrency > 0 {
			fmt.Printf("  tuning         pack %d MiB, read concurrency %d\n",
				l.PackSizeMiB, l.ReadConcurrency)
		}

		if l.HasCredentials {
			fmt.Printf("  credentials    present in the script — not shown here\n")
		}
	}

	if len(r.Blocked) > 0 {
		fmt.Printf("\nCOULD NOT LOOK\n")

		for _, dir := range r.Blocked {
			fmt.Printf("  %s (permission denied)\n", dir)
		}

		fmt.Printf("  Re-run as %s. Until then \"none found\" above means\n", elevated("recon"))
		fmt.Printf("  \"nothing found where this account can see\".\n")
	}

	if len(r.Notes) > 0 {
		fmt.Printf("\nnotes\n")

		for _, n := range r.Notes {
			fmt.Printf("  - %s\n", n)
		}
	}

	fmt.Printf("\nwhat an upgrade means here\n")

	for i, step := range r.Plan {
		fmt.Printf("  %d. %s\n", i+1, wrap(step, 5))
	}

	fmt.Println()
}

// excludeSources says where a legacy install's excludes are, in both of the
// places they live.
//
// Two places, because the Linux script keeps the pseudo-filesystems on the
// command line and everything else in a file, and a report that mentioned only
// the file would send somebody to copy half a list.
func excludeSources(l *legacybus.Install) string {
	var parts []string

	if len(l.Excludes) > 0 {
		parts = append(parts, fmt.Sprintf("%d in the script", len(l.Excludes)))
	}

	if l.ExcludeFile != "" {
		parts = append(parts, fmt.Sprintf("%d in %s", l.ExcludeCount, l.ExcludeFile))
	}

	if len(parts) == 0 {
		return "none"
	}

	return strings.Join(parts, ", ")
}

// elevated names what to re-run as, in this platform's words, and names the
// command while it is at it.
//
// The command matters: this sentence is printed by `recon` and by
// `adopt-enroll`, and "re-run as root: sudo sion-backup recon" in the middle of
// a migration sends somebody back to the report they have already read.
func elevated(cmd string) string {
	if runtime.GOOS == "windows" {
		return "Administrator"
	}

	return "root: sudo sion-backup " + cmd
}

// answer renders a yes/no with the reason beside it, which is the form a
// person reading a report wants: "no" on its own sends them looking.
func answer(ok bool, detail string) string {
	word := yesNo(ok)

	if detail == "" {
		return word
	}

	return word + " (" + detail + ")"
}

// wrap folds a long sentence to something readable in a terminal, indenting
// the continuation lines.
func wrap(s string, indent int) string {
	const width = 74

	var (
		out  strings.Builder
		line int
	)

	for i, word := range strings.Fields(s) {
		switch {
		case i == 0:
			out.WriteString(word)
			line = len(word)

		case line+1+len(word) > width:
			out.WriteString("\n" + strings.Repeat(" ", indent) + word)
			line = indent + len(word)

		default:
			out.WriteString(" " + word)
			line += 1 + len(word)
		}
	}

	return out.String()
}
