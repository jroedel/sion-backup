// This file is the fleet's answer to "which restic is on this machine".
//
// # Why the program installs its own
//
// The alternative was: whatever restic the machine happens to have. That was
// tried, and the fleet it produced was a snap 0.14 on one laptop, a Homebrew
// build on another, and nothing at all on a third — three different sets of
// exit codes, three different bug reports, and a `classify` function in
// restic.go whose only job is to guess from stderr text what a modern restic
// would have said in an exit status.
//
// So this program installs exactly [PinnedVersion], from restic's own release,
// verified against a hash compiled into this binary, into the per-user data
// directory. Nothing on PATH is consulted and nothing needs an administrator:
// the account that can write the machine token can write this too. The pin
// travels inside the binary, so a machine that self-updates picks up a new
// restic on the same channel and by the same mechanism — there is no second
// rollout to run and no machine that quietly missed it.
//
// # What is verified before it is ever executed
//
//  1. The download hashes to what [PinFor] says, which is a line copied from
//     upstream's signed SHA256SUMS. A mismatch leaves nothing behind.
//  2. The extracted binary runs and reports the pinned version. That catches
//     a truncated archive and a build for the wrong architecture, and it
//     happens after the hash rather than before.
//
// Only then is it moved into place, and the previous one is kept beside it
// until the next install.
//
// The same weakness the self-updater admits to applies here: the asset and
// the hash both originate at GitHub, so the hash proves the download arrived
// intact, not that upstream was not compromised. The difference is that this
// hash was copied by a person from a signed file at pin time and shipped
// inside our binary, so a later substitution upstream is caught.
package restic

import (
	"archive/zip"
	"compress/bzip2"
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
	"strings"
	"time"
)

// downloadTimeout bounds the whole fetch. Generous, because some of these
// machines are on hotel wifi and a restic archive is around 20 MB.
const downloadTimeout = 10 * time.Minute

// maxAsset caps what will be read from the network, and maxBinary caps what
// will be written from an archive. Both are far above restic's real size and
// exist so that a wrong URL or a crafted archive cannot fill somebody's disk
// — on a laptop, a full disk stops the backups it was meant to protect.
const (
	maxAsset  = 128 << 20
	maxBinary = 256 << 20
)

// smokeTestTimeout bounds the "does it actually run" check.
const smokeTestTimeout = 30 * time.Second

// Action is what EnsurePinned did.
type Action string

// The outcomes, all of which are worth a different sentence in front of a
// person: "installed" on a new machine is ordinary, and "replaced" is the one
// somebody wants to see in a log the morning after a version bump.
const (
	ActionAlready   Action = "already"   // the pinned version was already here
	ActionInstalled Action = "installed" // there was none, and now there is
	ActionReplaced  Action = "replaced"  // a different version was here
	ActionOverride  Action = "override"  // an operator named a binary; not ours to manage
)

// Outcome is what EnsurePinned did, in a form both a log line and a status
// page can render.
type Outcome struct {
	Action Action

	// From is the version that was here before, empty if there was none or if
	// the binary was too broken to ask.
	From string

	// To is the version now in place.
	To string

	// Path is the binary this machine will run.
	Path string
}

// String is the sentence for a human.
func (o Outcome) String() string {
	switch o.Action {
	case ActionAlready:
		return fmt.Sprintf("restic %s at %s", o.To, o.Path)
	case ActionInstalled:
		return fmt.Sprintf("installed restic %s at %s", o.To, o.Path)
	case ActionReplaced:
		from := o.From
		if from == "" {
			from = "an unusable binary"
		}

		return fmt.Sprintf("replaced %s with restic %s at %s", from, o.To, o.Path)
	default:
		return fmt.Sprintf("using the restic configured for this machine: %s", o.Path)
	}
}

// Resolve picks the binary this machine will run. It touches neither the
// network nor, in the managed case, the disk.
//
// Two states and no more, which is the point of managing it:
//
//   - override empty — the ordinary case. The binary is the fleet's own copy
//     in binDir, whether or not it is there yet; [Runner.EnsurePinned] is what
//     puts it there, and every command that needs restic calls that first.
//   - override set — an operator named a binary in the config file. It is
//     used as given and its version is not managed, because somebody who
//     names a path has taken that on. recon and doctor both say so.
//
// The managed path is returned without checking that the file exists, so that
// wiring a fresh machine cannot fail on a binary the next step is about to
// download.
func Resolve(override, binDir string) (*Runner, error) {
	if override != "" {
		resolved, err := exec.LookPath(override)
		if err != nil {
			return nil, fmt.Errorf("restic: the configured restic %q cannot be run: %w", override, err)
		}

		return &Runner{bin: resolved}, nil
	}

	if binDir == "" {
		return nil, errors.New("restic: no directory to keep the managed binary in")
	}

	pin, err := pinHere()
	if err != nil {
		return nil, err
	}

	return &Runner{bin: filepath.Join(binDir, pin.Binary), managed: true}, nil
}

// Managed reports whether this program installs and upgrades this binary
// itself. False for an operator-configured path.
func (r *Runner) Managed() bool { return r.managed }

// EnsurePinned makes this machine's restic be [PinnedVersion], downloading it
// if it is missing, the wrong version, or unrunnable.
//
// Called before a backup and at enrollment rather than at startup: a download
// belongs at a moment when the machine was going to use the network anyway,
// not in front of `sion-backup status`. It is a no-op on the second call, and
// the version check is a subprocess rather than a hash of the file on disk,
// because what matters is what the binary says it is when run.
//
// A machine with no network gets an error here and no backup, which is the
// honest outcome: without restic there is nothing to fall back to.
func (r *Runner) EnsurePinned(ctx context.Context, client *http.Client, log *slog.Logger) (Outcome, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	if !r.managed {
		return Outcome{Action: ActionOverride, Path: r.bin}, nil
	}

	pin, err := pinHere()
	if err != nil {
		return Outcome{}, err
	}

	// An error here is "not installed" or "will not run", and both lead to
	// the same place: install it. The version it reported, if any, is kept
	// for the sentence at the end.
	current, _ := r.InstalledVersion(ctx)

	if current == pin.Version {
		return Outcome{Action: ActionAlready, From: current, To: current, Path: r.bin}, nil
	}

	action := ActionInstalled
	if current != "" || exists(r.bin) {
		action = ActionReplaced
	}

	log.Info("installing the pinned restic",
		"version", pin.Version, "have", current, "path", r.bin, "url", pin.URL)

	if err := install(ctx, r.bin, pin, client); err != nil {
		return Outcome{}, err
	}

	// Asked again, of the binary now in place, rather than assumed from the
	// pin: install() smoke-tested it, and this is the answer that goes into
	// the run history.
	installed, err := r.InstalledVersion(ctx)
	if err != nil {
		return Outcome{}, err
	}

	return Outcome{Action: action, From: current, To: installed, Path: r.bin}, nil
}

// InstalledVersion is the version number this binary reports, "0.19.1".
//
// Empty with an error when there is nothing there, which is an ordinary state
// on a machine that has not enrolled yet and not one worth a special type.
func (r *Runner) InstalledVersion(ctx context.Context) (string, error) {
	full, err := r.Version(ctx)
	if err != nil {
		return "", err
	}

	number := VersionNumber(full)
	if number == "" {
		return "", fmt.Errorf("restic: %s reported %q, which does not name a version", r.bin, full)
	}

	return number, nil
}

// VersionNumber pulls "0.19.1" out of restic's version line, which reads
// "restic 0.19.1 compiled with go1.24.1 on linux/amd64".
//
// Exported because recon reports the version of a binary it found rather than
// one it manages, and it should render it the same way.
func VersionNumber(line string) string {
	fields := strings.Fields(line)
	if len(fields) < 2 || fields[0] != "restic" {
		return ""
	}

	// Only a clean release number, so that a build reporting "0.19.1-dev" is
	// never mistaken for the release it was built from — this fleet runs the
	// pinned version exactly, and "nearly" is a different binary.
	number := strings.TrimPrefix(fields[1], "v")

	if _, ok := parseVersion(number); !ok {
		return ""
	}

	return number
}

// parseVersion reads "0.19.1" into its three numbers.
func parseVersion(v string) ([3]int, bool) {
	var out [3]int

	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".")
	if len(parts) != 3 {
		return out, false
	}

	for i, p := range parts {
		n := 0

		if p == "" {
			return out, false
		}

		for _, c := range p {
			if c < '0' || c > '9' {
				return out, false
			}

			n = n*10 + int(c-'0')
		}

		out[i] = n
	}

	return out, true
}

// install downloads, verifies, extracts, tests and moves one restic into
// place. It leaves nothing behind on any failing path: a half-written binary
// where the next step expects a good one is worse than none at all, because
// the next step will run it.
func install(ctx context.Context, dest string, pin Pin, client *http.Client) error {
	dir := filepath.Dir(dest)

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("restic: creating %s: %w", dir, err)
	}

	// Staged beside the destination rather than in the system temp directory,
	// so the final move is a rename within one filesystem and therefore
	// atomic — /tmp is very often a different mount.
	work, err := os.MkdirTemp(dir, ".restic-install-")
	if err != nil {
		return fmt.Errorf("restic: making room to download into: %w", err)
	}
	defer os.RemoveAll(work)

	archive := filepath.Join(work, pin.Asset)

	sum, err := download(ctx, pin.URL, archive, client)
	if err != nil {
		return err
	}

	if sum != pin.SHA256 {
		return fmt.Errorf("restic: REFUSING %s — it hashes to %s and the pin says %s. "+
			"Do not work around this: either the pin is stale (bump it from a signed "+
			"upstream SHA256SUMS) or this download was tampered with", pin.Asset, sum, pin.SHA256)
	}

	staged := filepath.Join(work, pin.Binary)

	if err := extract(archive, staged, pin); err != nil {
		return err
	}

	if err := smokeTest(ctx, staged, pin.Version); err != nil {
		return err
	}

	return swap(dest, staged)
}

// download writes a URL to a file and returns its SHA-256, hashing as it
// goes so the bytes are never read twice.
func download(ctx context.Context, url, dest string, client *http.Client) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("restic: %w", err)
	}

	req.Header.Set("User-Agent", "sion-backup")

	if client == nil {
		client = &http.Client{Timeout: downloadTimeout}
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("restic: downloading %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("restic: downloading %s: %s", url, resp.Status)
	}

	f, err := os.Create(dest)
	if err != nil {
		return "", fmt.Errorf("restic: %w", err)
	}
	defer f.Close()

	h := sha256.New()

	if _, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxAsset)); err != nil {
		return "", fmt.Errorf("restic: downloading %s: %w", url, err)
	}

	if err := f.Close(); err != nil {
		return "", fmt.Errorf("restic: writing %s: %w", dest, err)
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// extract pulls the executable out of the downloaded asset. Upstream ships
// bzip2 for Unix and a zip for Windows.
func extract(archive, dest string, pin Pin) error {
	if strings.HasSuffix(pin.Asset, ".zip") {
		return extractZip(archive, dest)
	}

	f, err := os.Open(archive)
	if err != nil {
		return fmt.Errorf("restic: %w", err)
	}
	defer f.Close()

	return writeBinary(dest, bzip2.NewReader(f))
}

// extractZip takes the one executable out of a Windows release.
func extractZip(archive, dest string) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return fmt.Errorf("restic: reading %s: %w", filepath.Base(archive), err)
	}
	defer zr.Close()

	for _, entry := range zr.File {
		// By suffix rather than by exact name: upstream has named this file
		// both restic.exe and restic_<version>_windows_amd64.exe across
		// releases, and the archive holds exactly one executable either way.
		if !strings.HasSuffix(strings.ToLower(entry.Name), ".exe") {
			continue
		}

		rc, err := entry.Open()
		if err != nil {
			return fmt.Errorf("restic: reading %s from the archive: %w", entry.Name, err)
		}
		defer rc.Close()

		return writeBinary(dest, rc)
	}

	return fmt.Errorf("restic: %s contains no .exe", filepath.Base(archive))
}

// writeBinary puts the decompressed executable on disk, owner-only.
//
// 0700 rather than 0755: only the account that takes the backup runs this,
// and it sits in a directory beside the machine token.
func writeBinary(dest string, src io.Reader) error {
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o700)
	if err != nil {
		return fmt.Errorf("restic: %w", err)
	}
	defer f.Close()

	n, err := io.Copy(f, io.LimitReader(src, maxBinary))
	if err != nil {
		return fmt.Errorf("restic: extracting to %s: %w", dest, err)
	}

	if n == 0 {
		return fmt.Errorf("restic: the archive extracted to nothing")
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("restic: writing %s: %w", dest, err)
	}

	return nil
}

// smokeTest runs the extracted binary and checks it is what it claims.
//
// After the hash, never before: running something that failed verification is
// the thing this file exists to avoid. What it catches is a build for the
// wrong architecture, a truncated decompression, and a machine whose data
// directory is mounted noexec — the last of which is otherwise discovered at
// two in the morning by a backup that does not happen.
func smokeTest(ctx context.Context, path, want string) error {
	ctx, cancel := context.WithTimeout(ctx, smokeTestTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, path, "version")
	cmd.Env = []string{}

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("restic: the downloaded binary does not run (%w): %s",
			err, strings.TrimSpace(string(out)))
	}

	if got := VersionNumber(strings.TrimSpace(string(out))); got != want {
		return fmt.Errorf("restic: the downloaded binary reports %q, and the pin says %s",
			strings.TrimSpace(string(out)), want)
	}

	return nil
}

// swap moves the new binary into place, keeping the old one beside it.
//
// Aside-then-into-place rather than one rename over the top: on Windows a
// running image can be renamed but not overwritten, and everywhere it leaves
// the previous version on disk if the new one turns out not to start. The
// same shape as foundation/selfupdate.swap, for the same reasons.
func swap(dest, staged string) error {
	old := dest + ".old"

	_ = os.Remove(old)

	if exists(dest) {
		if err := os.Rename(dest, old); err != nil {
			return fmt.Errorf("restic: moving the current binary aside: %w", err)
		}
	}

	if err := os.Rename(staged, dest); err != nil {
		if back := os.Rename(old, dest); back != nil && exists(old) {
			return fmt.Errorf("restic: installing the new binary failed (%w) and the "+
				"old one could not be put back (%w); it is at %s", err, back, old)
		}

		return fmt.Errorf("restic: installing the new binary: %w", err)
	}

	// Best effort: a leftover .old costs 20 MB and nothing else, and on
	// Windows it cannot be removed while it is running.
	_ = os.Remove(old)

	return nil
}

// exists reports whether a path is there at all.
func exists(path string) bool {
	_, err := os.Stat(path)

	return err == nil
}
