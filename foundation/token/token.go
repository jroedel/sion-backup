// Package token stores the one secret this program keeps on disk.
//
// # Why there is exactly one, and what it can do
//
// Nothing else is cached here. The S3 access keys and the restic repository
// password are fetched from Eumaeus at the start of every run, used, and
// wiped — see business/domain/credential. A machine needs the network to reach
// the bucket anyway, so "back up while offline" was never a real capability,
// and paying for it by leaving three credentials on a laptop was a bad trade.
//
// What remains is a machine token: a bearer credential that lets this machine,
// and only this machine, ask Eumaeus for its own credentials.
//
// # Be precise about what that buys
//
// It is not "no secrets on the client". Something has to authenticate an
// unattended daemon, and whatever that is sits on the disk. Anybody who takes
// this file can fetch the same credentials the machine can.
//
// The gain is the three properties the cached credentials did not have:
//
//   - Revocable. Revoking the token cuts the machine off immediately. Cutting
//     off a laptop that held cached credentials meant rotating the S3 keys
//     and — for the repository password, which cannot be rotated at all —
//     rotating the entire bucket.
//   - Audited. Every fetch is a logged request against one machine's identity,
//     so a stolen laptop being used is visible from the server.
//   - Narrow. It authorises "fetch my credentials", not "read the repository".
//     The restic password never touches this disk.
//
// # Why a plain file and not the platform keyring
//
// This package replaces a 900-line foundation/secrets that wrapped DPAPI, the
// macOS Keychain and the Secret Service, with a file fallback for the headless
// Linux case where none of them answers. That complexity bought something real
// when three irreplaceable secrets lived here. It buys much less for one
// revocable one, and it was the least testable and most platform-specific code
// in the repository.
//
// Sealing this file with DPAPI or the Keychain remains a small, contained
// addition if it is ever wanted. It is deliberately not load-bearing.
package token

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ErrNotEnrolled reports that no token is stored — this machine has never been
// enrolled, or has been de-enrolled.
var ErrNotEnrolled = errors.New("token: this machine is not enrolled")

// Load reads the machine token.
func Load(path string) (string, error) {
	raw, err := os.ReadFile(path)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", ErrNotEnrolled
	case err != nil:
		return "", fmt.Errorf("token: reading %s: %w", path, err)
	}

	// Only the trailing newline. A token is opaque, and an administrator may
	// well have written the file by hand with an editor that adds one.
	value := strings.TrimRight(string(raw), "\r\n")
	if value == "" {
		return "", ErrNotEnrolled
	}

	return value, nil
}

// Save writes the token 0600, atomically.
//
// The rename is what makes it atomic: a crash mid-write leaves either the old
// token or the new one, never half of one. A half-written token does not fail
// in any legible way — it fails to authenticate, which reads exactly like
// revocation and would send somebody looking in entirely the wrong place.
func Save(path, value string) error {
	if value == "" {
		return errors.New("token: refusing to store an empty token")
	}

	dir := filepath.Dir(path)

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("token: creating %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".token-*")
	if err != nil {
		return fmt.Errorf("token: creating a temporary file in %s: %w", dir, err)
	}

	tmpName := tmp.Name()

	// Removed on every failure path below; a no-op after a successful rename.
	defer os.Remove(tmpName)

	if err := tmp.Chmod(0o600); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		tmp.Close()

		return fmt.Errorf("token: setting the mode on %s: %w", tmpName, err)
	}

	if _, err := tmp.WriteString(value + "\n"); err != nil {
		tmp.Close()

		return fmt.Errorf("token: writing %s: %w", tmpName, err)
	}

	// Sync before rename. Without it a power loss can leave a renamed file
	// with no contents, which here means a machine that believes it is
	// enrolled and cannot authenticate.
	if err := tmp.Sync(); err != nil {
		tmp.Close()

		return fmt.Errorf("token: flushing %s: %w", tmpName, err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("token: closing %s: %w", tmpName, err)
	}

	// Windows will not rename onto an existing file, so the old one goes
	// first. Losing the token is recoverable: the machine is re-enrolled.
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("token: replacing %s: %w", path, err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("token: installing %s: %w", path, err)
	}

	return nil
}

// Delete removes the token. Removing one that is not there is not an error:
// de-enrolling a machine twice should not fail the second time.
func Delete(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("token: removing %s: %w", path, err)
	}

	return nil
}

// Wipe overwrites a secret's backing array.
//
// A smaller guarantee than it looks, and worth stating so nobody relies on
// more: the garbage collector may have copied the slice and the operating
// system may have paged it out. What it reliably prevents is the long tail — a
// credential still legible in a heap dump or a core file an hour after the run
// that used it has finished.
func Wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
