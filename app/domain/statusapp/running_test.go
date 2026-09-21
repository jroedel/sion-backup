package statusapp_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/sion-backup/business/domain/backup/backupbus"
)

// begin writes the row a run writes when it starts and does not finish it:
// OutcomeFailed as a placeholder, no finished_at. What the page sees while a
// backup is actually running.
func (h *harness) begin(t *testing.T, r backupbus.Run) {
	t.Helper()

	r.Outcome = backupbus.OutcomeFailed

	if _, err := h.runs.Create(context.Background(), r); err != nil {
		t.Fatal(err)
	}
}

// TestARunInProgressIsNotShownAsAFailure is what a first full upload looked
// like: a banner saying a backup was running, and underneath it "The last
// run: failed, 0 files, 0 B, verified no" — about the same backup.
func TestARunInProgressIsNotShownAsAFailure(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if err := h.plan.Put(ctx, samplePlan(), time.Now()); err != nil {
		t.Fatal(err)
	}

	h.record(t, backupbus.Run{
		NodeID:              "office-laptop-1",
		Repository:          samplePlan().Repository,
		StartedAt:           time.Now().Add(-3 * time.Hour),
		FinishedAt:          time.Now().Add(-2 * time.Hour),
		Outcome:             backupbus.OutcomeSuccess,
		Message:             "4211 files, 88 MiB added",
		SnapshotID:          "aaaa1111",
		TotalFilesProcessed: 4211,
		Verified:            true,
	})

	h.begin(t, backupbus.Run{
		NodeID:     "office-laptop-1",
		Repository: samplePlan().Repository,
		StartedAt:  time.Now().Add(-90 * time.Minute),
	})

	body := h.get(t, "/").Body.String()

	// The table says what it is.
	if !strings.Contains(body, ">running<") {
		t.Error("the run in progress is not shown as running")
	}

	// And "The last run" describes the last run there is anything to say
	// about, which is the one that finished.
	if !strings.Contains(body, "aaaa1111") {
		t.Error("the last finished run is not the one described")
	}

	if strings.Contains(body, "<strong>no</strong>") {
		t.Error("a run in progress was described as unverified")
	}
}

// TestPressingBackUpNowSaysSomething is the gap the notice exists for.
//
// Starting a run means fetching credentials over the network before anything
// registers as running, so the redirect lands on a page where Running is
// still false — byte-for-byte the page already on the screen. Somebody who
// presses a button and sees nothing change presses it again.
func TestPressingBackUpNowSaysSomething(t *testing.T) {
	h := newHarness(t)

	if err := h.plan.Put(context.Background(), samplePlan(), time.Now()); err != nil {
		t.Fatal(err)
	}

	rec := h.post(t, "/run", "")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}

	if got := rec.Header().Get("Location"); got != "/?started" {
		t.Fatalf("redirected to %q, want /?started", got)
	}

	body := h.get(t, "/?started").Body.String()

	if !strings.Contains(body, "Starting") {
		t.Error("the page does not say a backup is starting")
	}

	// And carries itself across the gap rather than sitting there.
	if !strings.Contains(body, `http-equiv="refresh" content="2"`) {
		t.Error("the page does not refresh itself across the gap before the run registers")
	}
}

// TestAskingForASecondBackupSaysWhyNot. The refusal was a silent redirect to
// a page that already looked like this one.
func TestAskingForASecondBackupSaysWhyNot(t *testing.T) {
	h := newHarness(t)

	if err := h.plan.Put(context.Background(), samplePlan(), time.Now()); err != nil {
		t.Fatal(err)
	}

	body := h.get(t, "/?running").Body.String()

	if !strings.Contains(body, "already running") {
		t.Error("the page does not say a backup is already running")
	}
}
