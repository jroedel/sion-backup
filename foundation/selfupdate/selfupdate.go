// Package selfupdate replaces this program with a newer build of itself.
//
// # Why a program updates itself here
//
// The fleet is laptops belonging to people who are not administrators of
// them, in several buildings, with no management agent. There is no
// mechanism that reaches them all — which is the same reason the backups
// themselves are a program on the machine rather than something central. A
// version that has to be installed by hand is a version half the fleet will
// never see, and "which machines are behind" stops being a question anybody
// can act on.
//
// # What is checked before anything is replaced
//
//  1. The new version is genuinely newer, by number. A build whose version
//     this package cannot parse — "dev", a bare commit hash — is never
//     replaced, so a developer's working copy cannot update itself out from
//     under them.
//  2. The download matches the SHA-256 the source named. Refused otherwise,
//     without being run.
//  3. The downloaded binary runs and reports the version it claimed. This is
//     the check that catches a truncated download and a build for the wrong
//     architecture, and it happens after the hash rather than before, because
//     running something that failed verification is exactly the thing this
//     package exists to avoid.
//
// Only then is anything moved, and the old binary is kept beside the new one
// until the next start.
//
// # Who says which version is right
//
// Today: GitHub, from the release the tag built (see [GitHub]). That is a
// weaker guarantee than it looks, and the package comment should say so
// plainly — the binary and the SHA256SUMS that vouches for it come from the
// same place, so the hash proves the download was not corrupted, not that it
// was not replaced.
//
// The intended answer is Eumaeus: it already tells this machine the password
// to its own backups, so it is already trusted more than GitHub is, and a
// hash from it means a compromised release cannot reach the fleet on its own.
// It also gives a staged rollout and a kill switch. [Source] is the seam that
// change goes through — see docs/eumaeus-requests.md §5.2.
package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Release is one build that could be installed.
type Release struct {
	// Version is the tag, "v1.4.0".
	Version string

	// URL is where the binary for this machine's platform is.
	URL string

	// SHA256 is what it must hash to, lower-case hex. A release without one
	// is not installable: see the package comment on what the hash does and
	// does not prove, but a missing hash proves nothing at all.
	SHA256 string
}

// Source is where the answer to "what should this machine be running" comes
// from. GitHub implements it today; Eumaeus is meant to.
type Source interface {
	Latest(ctx context.Context, goos, goarch string) (Release, error)
}

// ErrNotWritable reports a binary this process may not replace — installed to
// /usr/local/bin and running as a user, most likely.
//
// Distinguished because it is a deployment fact rather than a failure: it will
// be true again in an hour, and a machine that reported it every hour would
// bury the reports that matter. Callers log it once and carry on backing up,
// which is the job.
var ErrNotWritable = errors.New("selfupdate: this binary is not writable by this process")

// ErrNoRelease reports a source with nothing installable to offer: no build
// for this platform, or a release with no hash.
var ErrNoRelease = errors.New("selfupdate: no installable release for this platform")

// downloadTimeout bounds the whole fetch. Generous: these are 20 MB binaries
// and some of these machines are on hotel wifi.
const downloadTimeout = 10 * time.Minute

// Config is what an updater needs.
type Config struct {
	// Source is where releases come from. It may be nil, and that is the
	// configuration the daemon uses for its first job at startup: deciding
	// what to do about the update it is already running. That decision needs
	// nothing from the network, and it has to be made before the config file
	// has been read — so requiring a source here would mean the machine could
	// not roll back a version whose owner had since switched updates off.
	//
	// [Updater.Apply] is what needs one, and says so if it is missing.
	Source Source

	// Current is this build's version, as stamped at link time.
	Current string

	// Executable overrides os.Executable, for the tests.
	Executable string

	HTTP *http.Client
	Log  *slog.Logger
}

// Updater replaces this program.
type Updater struct {
	source  Source
	current string
	exe     string
	http    *http.Client
	log     *slog.Logger
}

// New validates the configuration and resolves this binary's own path.
func New(cfg Config) (*Updater, error) {
	exe := cfg.Executable

	if exe == "" {
		found, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("selfupdate: locating this binary: %w", err)
		}

		// Resolved, because a symlinked /usr/local/bin/sion-backup should be
		// replaced where it actually lives rather than turned into a file.
		if resolved, err := filepath.EvalSymlinks(found); err == nil {
			found = resolved
		}

		exe = found
	}

	client := cfg.HTTP
	if client == nil {
		client = &http.Client{Timeout: downloadTimeout}
	}

	log := cfg.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	return &Updater{
		source:  cfg.Source,
		current: cfg.Current,
		exe:     exe,
		http:    client,
		log:     log,
	}, nil
}

// Apply installs a newer build if there is one.
//
// It reports the release it installed and true, or a zero release and false
// when there was nothing to do — which is the ordinary answer and not an
// error. The caller decides what to do next; nothing here restarts anything.
func (u *Updater) Apply(ctx context.Context) (Release, bool, error) {
	if u.source == nil {
		return Release{}, false, errors.New("selfupdate: no source configured")
	}

	latest, err := u.source.Latest(ctx, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return Release{}, false, err
	}

	if latest.SHA256 == "" || latest.URL == "" {
		return Release{}, false, ErrNoRelease
	}

	if !Newer(u.current, latest.Version) {
		return Release{}, false, nil
	}

	// A version this machine already gave up on. Reported as nothing to do
	// rather than as an error, deliberately: it will still be /latest in an
	// hour, and a machine that filed a report every hour about a decision it
	// made itself would bury the reports that matter. What makes it visible is
	// the rollback's own report, sent once, plus [Updater.Refused] — which
	// doctor prints and the status page shows.
	if u.refuses(latest.Version) {
		u.log.Info("not installing a version this machine gave up on",
			"version", latest.Version)

		return Release{}, false, nil
	}

	if err := u.Writable(); err != nil {
		return Release{}, false, err
	}

	// Beside the binary rather than in the system temp directory, because the
	// swap at the end has to be a rename and a rename has to stay on one
	// filesystem.
	staged := u.exe + ".new"

	if err := u.download(ctx, latest, staged); err != nil {
		_ = os.Remove(staged)

		return Release{}, false, err
	}

	defer os.Remove(staged)

	if err := smokeTest(ctx, staged, latest.Version); err != nil {
		return Release{}, false, err
	}

	if err := swap(u.exe, staged); err != nil {
		return Release{}, false, err
	}

	// Before the caller is told anything, because the caller's next move is to
	// exit into the new binary and the new binary reads this on the way up.
	// A failure is logged rather than returned: the swap has happened, and
	// unwinding a working update because a bookkeeping file could not be
	// written would be the wrong trade. The cost is a version nobody is
	// watching, which is where this package was before probation.
	if err := u.begin(latest.Version, u.current); err != nil {
		u.log.Warn("installed a new version but could not put it on probation",
			"err", err, "consequence", "it will not be rolled back if it fails to start")
	}

	u.log.Info("replaced this binary with a newer build",
		"was", u.current, "now", latest.Version, "path", u.exe,
		"on_probation_for", probationStarts)

	return latest, true, nil
}

// Writable reports whether this process may replace the binary.
//
// Checked by writing beside it rather than by reading permission bits, which
// answer the wrong question on every platform for a different reason: an ACL
// on Windows, a read-only mount on Linux, a signed bundle on macOS.
//
// Exported because it is a question worth asking before anything has gone
// wrong: on Windows and macOS the answer is usually no, and a machine that
// cannot replace its own binary looks exactly like one that stopped checking
// in. doctor asks it so that a person can be told plainly.
func (u *Updater) Writable() error {
	probe := u.exe + ".probe"

	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrNotWritable, filepath.Dir(u.exe), err)
	}

	f.Close()

	return os.Remove(probe)
}

// download fetches the release and refuses anything that does not match its
// hash.
func (u *Updater) download(ctx context.Context, r Release, to string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.URL, nil)
	if err != nil {
		return fmt.Errorf("selfupdate: %w", err)
	}

	resp, err := u.http.Do(req)
	if err != nil {
		return fmt.Errorf("selfupdate: downloading %s: %w", r.Version, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("selfupdate: downloading %s: %s", r.Version, resp.Status)
	}

	// 0700: it is about to be executed, and until it has been verified nobody
	// else should be able to run it either.
	f, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o700)
	if err != nil {
		return fmt.Errorf("selfupdate: writing the download: %w", err)
	}

	sum := sha256.New()

	if _, err := io.Copy(io.MultiWriter(f, sum), resp.Body); err != nil {
		f.Close()

		return fmt.Errorf("selfupdate: downloading %s: %w", r.Version, err)
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("selfupdate: writing the download: %w", err)
	}

	if got := hex.EncodeToString(sum.Sum(nil)); !strings.EqualFold(got, r.SHA256) {
		return fmt.Errorf("selfupdate: refusing %s: it hashes to %s, and the release says %s",
			r.Version, got, strings.ToLower(r.SHA256))
	}

	return nil
}

// smokeTestTimeout bounds the "does it run" check. A binary for the wrong
// architecture fails immediately; one that hangs is as broken as one that
// crashes.
const smokeTestTimeout = 30 * time.Second

// smokeTest runs the downloaded binary and asks it what it is.
//
// The last thing between a verified download and replacing the program with
// it. It catches the two failures a hash cannot: a build for the wrong
// platform, and one whose version is not what the release said it was.
func smokeTest(ctx context.Context, path, want string) error {
	ctx, cancel := context.WithTimeout(ctx, smokeTestTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, path, "version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("selfupdate: the downloaded binary does not run: %w: %s",
			err, strings.TrimSpace(string(out)))
	}

	if !strings.Contains(string(out), want) {
		return fmt.Errorf("selfupdate: the downloaded binary reports %q, and the release said %s",
			strings.TrimSpace(string(out)), want)
	}

	return nil
}

// swap moves the new binary into place, keeping the old one beside it.
//
// Aside-then-into-place rather than a single rename over the top, for two
// reasons: on Windows the running image can be renamed but not overwritten,
// and everywhere it means the previous version is still on the disk if the
// new one turns out not to start.
func swap(exe, staged string) error {
	old := exe + ".old"

	_ = os.Remove(old)

	if err := os.Rename(exe, old); err != nil {
		return fmt.Errorf("selfupdate: moving the current binary aside: %w", err)
	}

	if err := os.Rename(staged, exe); err != nil {
		// Put it back. A machine with no binary at all is a machine that
		// stops backing up and cannot update itself out of it.
		if back := os.Rename(old, exe); back != nil {
			return fmt.Errorf("selfupdate: installing the new binary failed (%w) "+
				"and the old one could not be restored (%w); it is at %s", err, back, old)
		}

		return fmt.Errorf("selfupdate: installing the new binary: %w", err)
	}

	return nil
}

// AssetName is what a release calls the binary for one platform. It matches
// what the Makefile's release target produces.
func AssetName(goos, goarch string) string {
	name := "sion-backup-" + goos + "-" + goarch

	if goos == "windows" {
		name += ".exe"
	}

	return name
}

// Newer reports whether candidate is a later version than current.
//
// Both are release tags: v1.4.0. Anything else — "dev", a bare commit hash,
// the "-3-gabc1234" git describe adds to a build between tags — makes this
// false, deliberately in both directions:
//
//   - an unparseable current version is a build somebody made by hand, and
//     replacing it with a release would throw away exactly the change they
//     are testing;
//   - an unparseable candidate is a release this build does not understand,
//     and installing something it cannot reason about is not an improvement.
func Newer(current, candidate string) bool {
	c, ok := parse(current)
	if !ok {
		return false
	}

	n, ok := parse(candidate)
	if !ok {
		return false
	}

	for i := range 3 {
		switch {
		case n[i] > c[i]:
			return true
		case n[i] < c[i]:
			return false
		}
	}

	return false
}

// parse reads "v1.4.0" into its three numbers.
func parse(v string) ([3]int, bool) {
	var out [3]int

	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")

	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, false
	}

	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}

		out[i] = n
	}

	return out, true
}
