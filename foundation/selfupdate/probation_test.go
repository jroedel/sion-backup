package selfupdate_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/jroedel/sion-backup/foundation/selfupdate"
)

// updater builds one over a fake binary that reports version, with no source:
// the probation path needs none, which is the point of it being usable before
// the config has been read.
func updater(t *testing.T, dir, current string) (*selfupdate.Updater, string) {
	t.Helper()

	exe := filepath.Join(dir, "sion-backup")
	fakeBinary(t, exe, current)

	u, err := selfupdate.New(selfupdate.Config{
		Current:    current,
		Executable: exe,
	})
	if err != nil {
		t.Fatal(err)
	}

	return u, exe
}

// install fakes what a swap leaves behind: the new binary in place, the
// previous one beside it, and a probation file.
func install(t *testing.T, exe, installed, previous string, starts int) {
	t.Helper()

	fakeBinary(t, exe, installed)
	fakeBinary(t, exe+".old", previous)

	// Written by hand rather than through Apply, so that a test can start from
	// the third attempt without running the first two.
	blob := `{"installed":"` + installed + `","previous":"` + previous +
		`","starts":` + strconv.Itoa(starts) + `,"began":"2026-09-10T12:00:00Z"}`

	if err := os.WriteFile(exe+".probation", []byte(blob), 0o600); err != nil {
		t.Fatal(err)
	}
}

func versionOf(t *testing.T, path string) string {
	t.Helper()

	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	return string(blob)
}

// TestAVersionThatWillNotStayRunningIsPutBack is the whole point of the
// package. Everything else here is a way this can go wrong.
func TestAVersionThatWillNotStayRunningIsPutBack(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake binary is a shell script")
	}

	dir := t.TempDir()
	u, exe := updater(t, dir, "v1.1.0")

	install(t, exe, "v1.1.0", "v1.0.0", 0)

	// Three starts are allowed, so the first three report probation and change
	// nothing. A machine that rolled back on the first crash would give up on
	// a version that lost a race with the network at boot.
	for i := 1; i <= 3; i++ {
		got := u.Start(context.Background())

		if got.Outcome != selfupdate.OnProbation {
			t.Fatalf("start %d = %v, want OnProbation", i, got.Outcome)
		}

		if got.Starts != i {
			t.Errorf("start %d counted as %d", i, got.Starts)
		}

		if !strings.Contains(versionOf(t, exe), "v1.1.0") {
			t.Fatalf("start %d replaced the binary before running out of attempts", i)
		}
	}

	got := u.Start(context.Background())

	if got.Outcome != selfupdate.RolledBack {
		t.Fatalf("fourth start = %v, want RolledBack", got.Outcome)
	}

	if got.Version != "v1.0.0" || got.Failed != "v1.1.0" {
		t.Errorf("rolled back to %q having given up on %q", got.Version, got.Failed)
	}

	if !strings.Contains(versionOf(t, exe), "v1.0.0") {
		t.Error("the binary on the disk is not the one that worked")
	}

	// Probation is over either way: a machine that tried to roll back on every
	// restart would be its own kind of broken.
	if _, err := os.Stat(exe + ".probation"); !os.IsNotExist(err) {
		t.Error("probation outlived the rollback")
	}

	// And the failed binary is beside it as .old, to be removed by the next
	// start that is not on probation. Renamed rather than deleted because on
	// Windows it is the image of the process doing the renaming.
	if !strings.Contains(versionOf(t, exe+".old"), "v1.1.0") {
		t.Error("the failed binary was not kept aside as .old")
	}

	// The next start is ordinary, and tidies up.
	if next := u.Start(context.Background()); next.Outcome != selfupdate.Nothing {
		t.Errorf("the start after a rollback = %v, want Nothing", next.Outcome)
	}

	if _, err := os.Stat(exe + ".old"); !os.IsNotExist(err) {
		t.Error("the failed binary was never cleaned up")
	}
}

// TestARolledBackVersionIsNotInstalledAgain closes the loop. Without this the
// machine reinstalls the same broken release within the hour, forever.
func TestARolledBackVersionIsNotInstalledAgain(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake binary is a shell script")
	}

	dir := t.TempDir()
	exe := filepath.Join(dir, "sion-backup")

	install(t, exe, "v1.1.0", "v1.0.0", 3)

	next := filepath.Join(dir, "next")
	body := fakeBinary(t, next, "v1.1.0")
	srv := serve(t, body)

	u, err := selfupdate.New(selfupdate.Config{
		Source: source{release: selfupdate.Release{
			Version: "v1.1.0", URL: srv.URL, SHA256: sum(body),
		}},
		Current:    "v1.1.0",
		Executable: exe,
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := u.Start(context.Background()); got.Outcome != selfupdate.RolledBack {
		t.Fatalf("start = %v, want RolledBack", got.Outcome)
	}

	if refused := u.Refused(); len(refused) != 1 || refused[0] != "v1.1.0" {
		t.Fatalf("Refused() = %v, want [v1.1.0]", refused)
	}

	// The machine is on v1.0.0 now, and v1.1.0 is still what the release
	// server offers. It must decline.
	back, err := selfupdate.New(selfupdate.Config{
		Source: source{release: selfupdate.Release{
			Version: "v1.1.0", URL: srv.URL, SHA256: sum(body),
		}},
		Current:    "v1.0.0",
		Executable: exe,
	})
	if err != nil {
		t.Fatal(err)
	}

	release, ok, err := back.Apply(context.Background())

	// Nothing to do, not an error: it will still be the latest release in an
	// hour, and a report every hour about a decision this machine made itself
	// would bury the reports that matter.
	if err != nil {
		t.Fatalf("Apply on a refused version returned an error: %v", err)
	}

	if ok {
		t.Fatalf("reinstalled a version it had given up on: %+v", release)
	}

	if !strings.Contains(versionOf(t, exe), "v1.0.0") {
		t.Error("the binary changed")
	}

	// And --forget is the way out, for a version that was fine and a machine
	// that was not.
	if err := back.Forget(); err != nil {
		t.Fatalf("Forget: %v", err)
	}

	if _, ok, err := back.Apply(context.Background()); err != nil || !ok {
		t.Errorf("after Forget, Apply = %v, %v; wanted the update to proceed", ok, err)
	}
}

// TestAMachineWithNoWayBackIsLeftAlone. A machine running a version that will
// not start is bad. A machine with no working binary at all cannot even be
// updated out of it.
func TestAMachineWithNoWayBackIsLeftAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake binary is a shell script")
	}

	for _, tc := range []struct {
		name string
		set  func(t *testing.T, exe string)
	}{
		{
			name: "the previous binary is gone",
			set:  func(t *testing.T, exe string) { os.Remove(exe + ".old") },
		},
		{
			name: "the previous binary does not run",
			set: func(t *testing.T, exe string) {
				if err := os.WriteFile(exe+".old", []byte("not a program"), 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "the previous binary reports the wrong version",
			set:  func(t *testing.T, exe string) { fakeBinary(t, exe+".old", "v0.9.0") },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			u, exe := updater(t, dir, "v1.1.0")

			install(t, exe, "v1.1.0", "v1.0.0", 3)
			tc.set(t, exe)

			got := u.Start(context.Background())

			if got.Outcome != selfupdate.Stuck {
				t.Fatalf("start = %v, want Stuck", got.Outcome)
			}

			if got.Failed != "v1.1.0" {
				t.Errorf("Failed = %q", got.Failed)
			}

			// Still there. Broken, but there.
			if !strings.Contains(versionOf(t, exe), "v1.1.0") {
				t.Error("the binary was replaced with something that does not work")
			}

			if _, err := os.Stat(exe + ".probation"); !os.IsNotExist(err) {
				t.Error("probation outlived a rollback that could not happen")
			}
		})
	}
}

// TestACorruptProbationFileIsIgnored. A machine that can never start again
// because a bookkeeping file was half-written is a worse failure than the one
// probation exists to prevent.
func TestACorruptProbationFileIsIgnored(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake binary is a shell script")
	}

	dir := t.TempDir()
	u, exe := updater(t, dir, "v1.1.0")

	if err := os.WriteFile(exe+".probation", []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := u.Start(context.Background()); got.Outcome != selfupdate.Nothing {
		t.Errorf("start = %v, want Nothing", got.Outcome)
	}

	if !strings.Contains(versionOf(t, exe), "v1.1.0") {
		t.Error("the binary was changed on the strength of an unreadable file")
	}
}

// TestSettleIsSafeToCallWhenNothingIsOnProbation, because the daemon's timer
// can fire on a start that found nothing, and callers should not have to
// remember which.
func TestSettleIsSafeToCallWhenNothingIsOnProbation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake binary is a shell script")
	}

	dir := t.TempDir()
	u, _ := updater(t, dir, "v1.0.0")

	u.Settle()
	u.Settle()
}
