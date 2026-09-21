package backupbus_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jroedel/sion-backup/business/domain/backup/backupbus"
	"github.com/jroedel/sion-backup/foundation/restic"
)

// slowRestic is a backup that does not end on its own.
//
// It reports that it has started, then waits. The trap is the point: a restic
// that is interrupted tidies up and leaves, and the marker file is how a test
// tells that apart from a restic that was killed where it stood — which is
// the difference between a repository the next backup can use and one it
// cannot.
func slowRestic(t *testing.T, dir string) *restic.Runner {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("the stub is a shell script")
	}

	script := `#!/bin/sh
case "$1" in
backup)
  echo '{"message_type":"status","percent_done":0.25,"files_done":1,"total_files":4}'
  sleep 30 &
  SLEEPER=$!
  # The sleeper is killed with the shell, not left behind: a child holding the
  # output pipe open keeps the parse on the other end of it waiting, which is
  # a stub artefact rather than anything restic does, and it would turn a test
  # about stopping quickly into one that takes half a minute to agree.
  trap 'echo yes > "` + filepath.Join(dir, "interrupted") + `"; kill $SLEEPER 2>/dev/null; exit 130' INT
  touch "` + filepath.Join(dir, "started") + `"
  wait $SLEEPER
  exit 0
  ;;
unlock) echo yes > "` + filepath.Join(dir, "unlocked") + `"; exit 0 ;;
*) echo "unexpected subcommand: $1" >&2; exit 1 ;;
esac
`

	bin := filepath.Join(t.TempDir(), "restic")
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	r, err := restic.New(bin)
	if err != nil {
		t.Fatal(err)
	}

	return r
}

// waitFor polls for a file the stub writes, so the test acts on what restic
// has actually done rather than on a sleep long enough to usually work.
func waitFor(t *testing.T, path string) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)

	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("%s never appeared", filepath.Base(path))
}

func TestCancellingABackupStopsItAndSaysWhy(t *testing.T) {
	dir := t.TempDir()
	b, store, _ := harness(t, slowRestic(t, dir))

	done := make(chan backupbus.Run, 1)

	go func() {
		run, err := b.Run(context.Background(), request(), time.Now)
		if err != nil {
			t.Error("the run returned an error:", err)
		}

		done <- run
	}()

	waitFor(t, filepath.Join(dir, "started"))

	if err := b.Cancel("stopped from the status page"); err != nil {
		t.Fatal("cancelling:", err)
	}

	var run backupbus.Run

	select {
	case run = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the run did not stop")
	}

	if run.Outcome != backupbus.OutcomeCancelled {
		t.Errorf("outcome = %q, want %q", run.Outcome, backupbus.OutcomeCancelled)
	}

	if run.Message != "stopped from the status page" {
		t.Errorf("message = %q, want the reason it was given", run.Message)
	}

	// The lock, cleared. A cancelled backup that leaves one behind is a
	// machine that has quietly stopped backing up.
	waitFor(t, filepath.Join(dir, "unlocked"))

	// Recorded as finished, not left open. A row with no end is shown as a
	// backup still running, forever.
	last, err := store.Last(context.Background())
	if err != nil {
		t.Fatal("reading the history:", err)
	}

	if last.Unfinished() {
		t.Error("the cancelled run was left open in the history")
	}

	if last.Outcome != backupbus.OutcomeCancelled {
		t.Errorf("recorded outcome = %q, want %q", last.Outcome, backupbus.OutcomeCancelled)
	}
}

func TestACancelledResticIsAskedToStopRatherThanKilled(t *testing.T) {
	dir := t.TempDir()
	b, _, _ := harness(t, slowRestic(t, dir))

	go func() {
		if _, err := b.Run(context.Background(), request(), time.Now); err != nil {
			t.Error("the run returned an error:", err)
		}
	}()

	waitFor(t, filepath.Join(dir, "started"))

	if err := b.Cancel("stopped from the status page"); err != nil {
		t.Fatal("cancelling:", err)
	}

	// Written by the stub's signal handler. A killed process never gets to
	// write it -- and a killed restic leaves its lock in the repository.
	waitFor(t, filepath.Join(dir, "interrupted"))
}

func TestCancellingWithNothingRunningSaysSo(t *testing.T) {
	b, _, _ := harness(t, fakeRestic(t, 0, "good"))

	if err := b.Cancel("stopped from the status page"); !errors.Is(err, backupbus.ErrNotRunning) {
		t.Errorf("err = %v, want ErrNotRunning", err)
	}
}

func TestAStoppedBackupIsReportedAsAFailedOne(t *testing.T) {
	// The fleet dashboard has five outcomes and asks one question: is this
	// machine protected. After a cancelled run the answer is the one failed
	// already means, and the reason travels in the message.
	if got := backupbus.OutcomeCancelled.Reported(); got != backupbus.OutcomeFailed {
		t.Errorf("reported as %q, want %q", got, backupbus.OutcomeFailed)
	}

	for _, o := range []backupbus.Outcome{
		backupbus.OutcomeSuccess, backupbus.OutcomeDegraded,
		backupbus.OutcomeIncomplete, backupbus.OutcomeUnverified, backupbus.OutcomeFailed,
	} {
		if got := o.Reported(); got != o {
			t.Errorf("%q was reported as %q; only cancelled is mapped", o, got)
		}
	}
}
