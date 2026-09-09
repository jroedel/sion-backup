package paths

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// clearEnv removes every variable Resolve consults, so a developer's real
// environment cannot make a test pass that would fail on a clean machine.
func clearEnv(t *testing.T) {
	t.Helper()

	for _, k := range []string{
		"XDG_DATA_HOME", "LOCALAPPDATA",
		"SION_BACKUP_DATA_DIR", "SION_BACKUP_DB", "SION_BACKUP_CONFIG", "SION_BACKUP_LOG",
	} {
		t.Setenv(k, "")
	}
}

// TestNoDefaultPathInsideRepo is this package's whole reason to exist: with a
// clean environment nothing resolves into the working tree, so no amount of
// `git add` can stage a node's run history or its sealed credentials.
func TestNoDefaultPathInsideRepo(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // Windows' answer to HOME
	clearEnv(t)

	p, err := Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	repo := filepath.Dir(filepath.Dir(wd)) // .../sion-backup, from foundation/paths

	// Every path the program will write to, not a sample of them. A path
	// missing from this map is a path the repository's central invariant has
	// quietly stopped covering, which is worse than never having had the test.
	for name, got := range map[string]string{
		"DataDir": p.DataDir,
		"DB":      p.DB,
		"Config":  p.Config,
		"Token":   p.Token,
		"Verify":  p.Verify,
		"Scratch": p.Scratch,
		"Log":     p.Log,
	} {
		abs, err := filepath.Abs(got)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}

		if rel, err := filepath.Rel(repo, abs); err == nil && !strings.HasPrefix(rel, "..") {
			t.Errorf("%s resolves inside the repository: %s", name, abs)
		}
	}
}

// TestEveryPathIsUnderTheDataDirectory keeps the "one directory" promise the
// README makes. A path that escapes it is a second place to look when
// something is wrong, and a second place to forget when a machine is retired.
func TestEveryPathIsUnderTheDataDirectory(t *testing.T) {
	dir := t.TempDir()
	clearEnv(t)
	t.Setenv("SION_BACKUP_DATA_DIR", dir)

	p, err := Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	for name, got := range map[string]string{
		"DB": p.DB, "Config": p.Config, "Token": p.Token,
		"Verify": p.Verify, "Scratch": p.Scratch, "Log": p.Log,
	} {
		if rel, err := filepath.Rel(p.DataDir, got); err != nil || strings.HasPrefix(rel, "..") {
			t.Errorf("%s is outside the data directory: %s", name, got)
		}
	}
}

// TestScratchIsNotSharedWithAnythingElse guards the one path whose contents
// are deleted wholesale. ClearScratch calls RemoveAll, so the day Scratch is
// edited to equal DataDir is the day an upgrade erases the run history.
func TestScratchIsNotSharedWithAnythingElse(t *testing.T) {
	dir := t.TempDir()
	clearEnv(t)
	t.Setenv("SION_BACKUP_DATA_DIR", dir)

	p, err := Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	for name, other := range map[string]string{
		"DataDir": p.DataDir, "DB": p.DB, "Config": p.Config,
		"Token": p.Token, "Verify": p.Verify, "Log": p.Log,
	} {
		if p.Scratch == other {
			t.Errorf("Scratch is the same path as %s (%s); ClearScratch would delete it", name, other)
		}

		if rel, err := filepath.Rel(p.Scratch, other); err == nil && !strings.HasPrefix(rel, "..") && rel != "." {
			t.Errorf("%s (%s) is inside Scratch; ClearScratch would delete it", name, other)
		}
	}
}

// TestEnsureDirsIsOwnerOnly checks the mode on the directories holding the run
// history and the sealed credentials. Skipped on Windows, where the mode is
// not the mechanism.
func TestEnsureDirsIsOwnerOnly(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("POSIX modes are not how Windows answers this")
	}

	dir := t.TempDir()
	clearEnv(t)
	t.Setenv("SION_BACKUP_DATA_DIR", filepath.Join(dir, "data"))

	p, err := Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}

	for _, d := range []string{p.DataDir, p.Verify, p.Scratch} {
		info, err := os.Stat(d)
		if err != nil {
			t.Fatalf("stat %s: %v", d, err)
		}

		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Errorf("%s has mode %o, want 700", d, perm)
		}
	}
}

// TestClearScratchLeavesAnEmptyDirectory covers both halves: the contents go,
// and the directory itself comes back — a run that found it missing would
// fail its verification step for the wrong reason.
func TestClearScratchLeavesAnEmptyDirectory(t *testing.T) {
	dir := t.TempDir()
	clearEnv(t)
	t.Setenv("SION_BACKUP_DATA_DIR", dir)

	p, err := Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}

	stale := filepath.Join(p.Scratch, "restored", "nonce.txt")
	if err := os.MkdirAll(filepath.Dir(stale), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(stale, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := p.ClearScratch(); err != nil {
		t.Fatalf("ClearScratch: %v", err)
	}

	entries, err := os.ReadDir(p.Scratch)
	if err != nil {
		t.Fatalf("the scratch directory did not come back: %v", err)
	}

	if len(entries) != 0 {
		t.Errorf("scratch still holds %d entries", len(entries))
	}
}
