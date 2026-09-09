// Package paths resolves every filesystem location sion-backup reads or writes.
//
// It exists to enforce one invariant: **no default path ever resolves inside
// the source tree.** This repository is public. A machine's node name, the
// directories somebody chose to back up, the exclude list that names their
// employer's share drive, the run history that says which laptop was offline
// for three weeks — none of that is a secret in the cryptographic sense, and
// all of it is somebody's business rather than the internet's.
//
// That is a stronger guarantee than a .gitignore can give. An ignore rule is
// advisory: `git add -f`, a tool writing through a symlink, or a rename
// defeats it silently. A path that was never inside the tree cannot be
// committed by accident, because there is nothing there to stage.
// TestNoDefaultPathInsideRepo is the guard on it.
//
// # Per-platform locations
//
// The fleet is Windows, macOS and Linux, so "the XDG directory" is not an
// answer on two of the three. Each platform gets the location its own users
// and its own backup tooling already expect:
//
//	Windows  %LOCALAPPDATA%\sion-backup
//	macOS    ~/Library/Application Support/sion-backup
//	Linux    $XDG_DATA_HOME/sion-backup, or ~/.local/share/sion-backup
//
// LocalAppData rather than AppData on Windows, deliberately: on a domain
// machine AppData\Roaming is synchronised to the profile server at logout,
// which would copy the run history and the sealed credential blob to a file
// share, and would then hand a second machine logging in as the same user a
// DPAPI blob it cannot decrypt.
//
// This is a foundation leaf: it knows about directories, not about backups.
package paths

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// appName is the per-user subdirectory under each platform's root.
const appName = "sion-backup"

// Paths is the resolved set of locations.
type Paths struct {
	// DataDir holds everything below. One directory, so there is never a
	// second configuration somewhere else quietly not being read.
	DataDir string

	// DB is the SQLite file: the backup plan, and the run history behind the
	// status page.
	DB string

	// Config is the seed file an installer drops next to the binary's data
	// directory. It is read once, into the database, and is not a second
	// source of truth afterwards — see business/domain/plan.
	Config string

	// Token is the machine token: the one secret this program keeps on disk.
	//
	// A file rather than a directory because there is exactly one, and there
	// is exactly one because the S3 keys and the repository password are
	// fetched from Eumaeus per run and never written down. See
	// foundation/token, which is candid about what that does and does not buy.
	Token string

	// Verify holds the verification nonce written before each run.
	//
	// It is a directory rather than a file in the data directory because it is
	// added to the backup targets: proving a restore works means restoring
	// something that was definitely backed up, and the only way to be sure of
	// that is to put it there deliberately. The legacy scripts wrote their
	// nonce into a path that happened to be under the backup root and would
	// have silently stopped verifying anything the day somebody narrowed the
	// targets.
	Verify string

	// Scratch is where the restore-verification round trip lands. Emptied at
	// the end of every run, and never anywhere near the directories being
	// backed up: restoring into a backup target would put a copy of the
	// verification file into the next snapshot, forever.
	Scratch string

	// Log is the daemon's log file. The daemon has no terminal to write to,
	// and "is it running properly" is the question this program exists to
	// answer, so the log is a file rather than a stream nobody kept.
	Log string
}

// Resolve works out where everything lives.
//
// Environment overrides exist for one reason: the tests, and an operator
// running two configurations on one machine while migrating. They are read
// before the platform default so that a set variable always wins.
func Resolve() (Paths, error) {
	dataDir := os.Getenv("SION_BACKUP_DATA_DIR")
	if dataDir == "" {
		root, err := platformRoot()
		if err != nil {
			return Paths{}, err
		}

		dataDir = filepath.Join(root, appName)
	}

	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return Paths{}, fmt.Errorf("paths: resolving %s: %w", dataDir, err)
	}

	return Paths{
		DataDir: abs,
		DB:      cmp.Or(os.Getenv("SION_BACKUP_DB"), filepath.Join(abs, "sion-backup.db")),
		Config:  cmp.Or(os.Getenv("SION_BACKUP_CONFIG"), filepath.Join(abs, "config.toml")),
		Token:   filepath.Join(abs, "machine-token"),
		Verify:  filepath.Join(abs, "verify"),
		Scratch: filepath.Join(abs, "scratch"),
		Log:     cmp.Or(os.Getenv("SION_BACKUP_LOG"), filepath.Join(abs, "sion-backup.log")),
	}, nil
}

// platformRoot is the per-user application directory this platform uses.
func platformRoot() (string, error) {
	switch runtime.GOOS {
	case "windows":
		// os.UserConfigDir answers %AppData% (Roaming) on Windows, which is
		// the wrong half of the profile — see the package comment.
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			return local, nil
		}

		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("paths: LOCALAPPDATA is unset and there is no home directory: %w", err)
		}

		return filepath.Join(home, "AppData", "Local"), nil

	case "darwin":
		// os.UserConfigDir is ~/Library/Application Support here, which is
		// where a Mac keeps exactly this kind of state.
		dir, err := os.UserConfigDir()
		if err != nil {
			return "", fmt.Errorf("paths: locating the application support directory: %w", err)
		}

		return dir, nil

	default:
		if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
			return xdg, nil
		}

		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("paths: XDG_DATA_HOME is unset and there is no home directory: %w", err)
		}

		return filepath.Join(home, ".local", "share"), nil
	}
}

// EnsureDirs creates what does not exist yet, owner-only.
//
// 0700 rather than 0755 because the run history says what a person's machine
// holds and when it was last online, and the machine token sits beside it. On Windows the mode is largely ignored and the directory
// inherits the profile's ACL, which is already owner-only.
func (p Paths) EnsureDirs() error {
	for _, dir := range []string{p.DataDir, p.Verify, p.Scratch} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("paths: creating %s: %w", dir, err)
		}
	}

	return nil
}

// ClearScratch empties the restore-verification directory.
//
// Called at the end of a run, and again at the start of the next one — the end
// of a run is not a reliable moment on a laptop that gets its lid closed.
func (p Paths) ClearScratch() error {
	if err := os.RemoveAll(p.Scratch); err != nil {
		return fmt.Errorf("paths: clearing %s: %w", p.Scratch, err)
	}

	if err := os.MkdirAll(p.Scratch, 0o700); err != nil {
		return fmt.Errorf("paths: recreating %s: %w", p.Scratch, err)
	}

	return nil
}
