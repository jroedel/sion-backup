package main

import (
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
	Notes    []string           `json:"notes,omitempty"`
	Plan     []string           `json:"plan,omitempty"`
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

// ResticInfo is the binary that does the work.
type ResticInfo struct {
	Path    string `json:"path,omitempty"`
	Version string `json:"version,omitempty"`
	Problem string `json:"problem,omitempty"`
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

	var extra []string

	for _, dir := range strings.Split(*legacyDir, ",") {
		if dir = strings.TrimSpace(dir); dir != "" {
			extra = append(extra, dir)
		}
	}

	r := gather(ctx, extra...)

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
	r.Restic = resticInfo(cfg.Server.Restic)

	legacy, notes := legacyscan.Find(ctx, extraLegacyDirs...)
	r.Legacy = legacy
	r.Notes = append(r.Notes, notes...)
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

// resticInfo finds restic and asks its version.
func resticInfo(configured string) ResticInfo {
	r, err := restic.New(configured)
	if err != nil {
		return ResticInfo{Problem: err.Error()}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	v, err := r.Version(ctx)
	if err != nil {
		return ResticInfo{Path: r.Bin(), Problem: err.Error()}
	}

	return ResticInfo{Path: r.Bin(), Version: v}
}

// plan turns what was found into what an upgrade would do.
//
// Sentences rather than steps a machine executes: the installer prints these
// and waits for a person, because the decision in the middle of them — adopt
// the existing bucket or start a new one — is not one a script should make on
// somebody's two years of history.
func plan(r Recon) []string {
	var out []string

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
				"nothing, and the first backup uploads everything).", l.RepositoryURL))
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

	if l.ExcludeCount > 0 {
		out = append(out, fmt.Sprintf(
			"Its exclude list has %d entries, in %s. Copy them into the new config before "+
				"the first run, or the first backup will include things somebody chose to "+
				"leave out.", l.ExcludeCount, l.ExcludeFile))
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

	switch {
	case r.Restic.Version != "":
		fmt.Printf("  found          %s (%s)\n", r.Restic.Path, r.Restic.Version)
	case r.Restic.Path != "":
		fmt.Printf("  found          %s, but %s\n", r.Restic.Path, r.Restic.Problem)
	default:
		fmt.Printf("  not found      %s\n", r.Restic.Problem)
	}

	fmt.Printf("\nsion-backup\n")
	fmt.Printf("  installed      %s\n", yesNo(r.Existing.Installed))
	fmt.Printf("  enrolled       %s\n", yesNo(r.Existing.Enrolled))

	if r.Legacy == nil {
		fmt.Printf("\nlegacy install   none found\n")
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

		if l.ExcludeFile != "" {
			fmt.Printf("  excludes       %d entries in %s\n", l.ExcludeCount, l.ExcludeFile)
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
