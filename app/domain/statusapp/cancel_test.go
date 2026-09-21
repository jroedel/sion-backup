package statusapp_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/sion-backup/business/domain/backup/backupbus"
	"github.com/jroedel/sion-backup/foundation/restic"
)

// endlessRestic is a backup that runs until it is asked to stop, so that a
// page test can see the page a person sees while one is uploading.
func endlessRestic(t *testing.T, dir string) *restic.Runner {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("the stub is a shell script")
	}

	script := `#!/bin/sh
case "$1" in
backup)
  echo '{"message_type":"status","percent_done":0.4,"files_done":2,"total_files":5}'
  sleep 30 &
  SLEEPER=$!
  trap 'kill $SLEEPER 2>/dev/null; exit 130' INT
  touch "` + filepath.Join(dir, "started") + `"
  wait $SLEEPER
  exit 0
  ;;
unlock) exit 0 ;;
*) exit 1 ;;
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

// runningRequest is a backup of the stub's own directory against a repository
// that is never reached, because the stub never gets as far as the network.
func runningRequest(dir string) backupbus.Request {
	return backupbus.Request{
		NodeID: "office-laptop-1",
		Repository: restic.Repository{
			URL:             "s3:https://s3.example.invalid/bucket",
			Password:        []byte("pw"),
			AccessKeyID:     []byte("id"),
			SecretAccessKey: []byte("secret"),
		},
		Options: restic.BackupOptions{Targets: []string{dir}},
	}
}

func waitForFile(t *testing.T, path string) {
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

// TestTheCancelButtonStopsTheBackup is the whole feature from the outside:
// somebody watching a seed climb through folders they never meant to include
// presses a button, and the upload stops.
func TestTheCancelButtonStopsTheBackup(t *testing.T) {
	dir := t.TempDir()
	h := harnessDriving(t, endlessRestic(t, dir), stubSource{}, nil)
	ctx := context.Background()

	if err := h.plan.Put(ctx, samplePlan(), time.Now()); err != nil {
		t.Fatal(err)
	}

	stopped := make(chan backupbus.Run, 1)

	go func() {
		run, err := h.backups.Run(ctx, runningRequest(dir), time.Now)
		if err != nil {
			t.Error("the run returned an error:", err)
		}

		stopped <- run
	}()

	waitForFile(t, filepath.Join(dir, "started"))

	// The button is on the page, next to the progress it is about.
	body := h.get(t, "/").Body.String()

	if !strings.Contains(body, `action="/run/cancel"`) {
		t.Fatal("there is no way to stop a backup from the page while one is running")
	}

	if !strings.Contains(body, "Cancel backup") {
		t.Error("the button does not say what it does")
	}

	rec := h.post(t, "/run/cancel", "")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}

	if got := rec.Header().Get("Location"); got != "/?cancelling" {
		t.Fatalf("redirected to %q, want /?cancelling", got)
	}

	select {
	case run := <-stopped:
		if run.Outcome != backupbus.OutcomeCancelled {
			t.Errorf("outcome = %q, want %q", run.Outcome, backupbus.OutcomeCancelled)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the backup did not stop")
	}
}

// TestTheCancelButtonIsNotThereWithNothingToCancel. A button that does
// nothing teaches people that buttons do nothing.
func TestTheCancelButtonIsNotThereWithNothingToCancel(t *testing.T) {
	h := newHarness(t)

	if err := h.plan.Put(context.Background(), samplePlan(), time.Now()); err != nil {
		t.Fatal(err)
	}

	if body := h.get(t, "/").Body.String(); strings.Contains(body, `action="/run/cancel"`) {
		t.Error("the page offers to stop a backup that is not running")
	}
}

// TestCancellingAFinishedBackupSaysSo is the race the page has to survive:
// the button was rendered a few seconds ago and the backup has finished in
// between.
func TestCancellingAFinishedBackupSaysSo(t *testing.T) {
	h := newHarness(t)

	if err := h.plan.Put(context.Background(), samplePlan(), time.Now()); err != nil {
		t.Fatal(err)
	}

	rec := h.post(t, "/run/cancel", "")
	if got := rec.Header().Get("Location"); got != "/?notrunning" {
		t.Fatalf("redirected to %q, want /?notrunning", got)
	}

	if body := h.get(t, "/?notrunning").Body.String(); !strings.Contains(body, "nothing was stopped") {
		t.Error("the page does not say why nothing happened")
	}
}

// TestAStoppedBackupIsNotCalledAFailure. The page has one job when somebody
// has just stopped a backup on purpose: not to alarm them about it.
func TestAStoppedBackupIsNotCalledAFailure(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if err := h.plan.Put(ctx, samplePlan(), time.Now()); err != nil {
		t.Fatal(err)
	}

	h.record(t, backupbus.Run{
		NodeID:     "office-laptop-1",
		Repository: samplePlan().Repository,
		StartedAt:  time.Now().Add(-20 * time.Minute),
		FinishedAt: time.Now().Add(-19 * time.Minute),
		Outcome:    backupbus.OutcomeCancelled,
		Message:    "stopped from the status page",
	})

	body := h.get(t, "/").Body.String()

	if strings.Contains(body, "The last backup failed") {
		t.Error("a backup somebody stopped is described as a failure")
	}

	if !strings.Contains(body, "The last backup was stopped") {
		t.Error("the page does not say the last backup was stopped")
	}

	// And says the thing somebody stopping a seed most needs to know.
	if !strings.Contains(body, "carries on from where") {
		t.Error("the page does not say that the next backup carries on")
	}
}
