package diagfile_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/sion-backup/business/domain/diag/diagbus"
	"github.com/jroedel/sion-backup/business/domain/diag/stores/diagfile"
)

func store(t *testing.T) (*diagfile.Store, string) {
	t.Helper()

	dir := filepath.Join(t.TempDir(), "diagnostics")

	s, err := diagfile.NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	return s, dir
}

func report(when time.Time, step string) diagbus.Report {
	return diagbus.Report{
		Kind:       diagbus.KindInstallFailed,
		OccurredAt: when,
		Step:       step,
		Detail:     "something went wrong",
	}
}

// TestReportsComeBackOldestFirst, because a server reading them in the order
// they happened can tell which failure caused the others.
func TestReportsComeBackOldestFirst(t *testing.T) {
	s, _ := store(t)
	now := time.Now()

	for i, step := range []string{"third", "first", "second"} {
		// Deliberately out of order in time, in order of writing.
		when := now.Add(time.Duration(map[int]int{0: 2, 1: 0, 2: 1}[i]) * time.Minute)

		if err := s.Put(report(when, step)); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}

	got, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(got) != 3 {
		t.Fatalf("%d reports, want 3", len(got))
	}

	for i, want := range []string{"first", "second", "third"} {
		if got[i].Report.Step != want {
			t.Errorf("position %d is %q, want %q", i, got[i].Report.Step, want)
		}
	}

	if err := s.Remove(got[0].ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if got, _ = s.List(); len(got) != 2 || got[0].Report.Step != "second" {
		t.Errorf("after removing the oldest: %d left, first is %q", len(got), got[0].Report.Step)
	}
}

// TestTheDirectoryIsBounded. This lives in a user profile that gets copied
// around, and a machine in a crash loop would otherwise fill it.
func TestTheDirectoryIsBounded(t *testing.T) {
	s, dir := store(t)
	now := time.Now()

	for i := range diagfile.Keep + 10 {
		if err := s.Put(report(now.Add(time.Duration(i)*time.Second), "step")); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != diagfile.Keep {
		t.Errorf("%d files on disk, want %d", len(entries), diagfile.Keep)
	}

	// The newest survive: what is wrong now beats what was wrong first.
	got, err := s.List()
	if err != nil {
		t.Fatal(err)
	}

	oldest := got[0].Report.OccurredAt
	if !oldest.After(now.Add(5 * time.Second)) {
		t.Errorf("the oldest kept report is from %v; the early ones should have gone", oldest)
	}
}

// TestAnUnreadableFileDoesNotBlockTheQueue. A file half-written by a machine
// that lost power mid-crash-report must not stop every later report from
// being sent, forever.
func TestAnUnreadableFileDoesNotBlockTheQueue(t *testing.T) {
	s, dir := store(t)

	if err := os.WriteFile(filepath.Join(dir, "20260101T000000.000-deadbeef.json"),
		[]byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := s.Put(report(time.Now(), "good")); err != nil {
		t.Fatal(err)
	}

	got, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(got) != 1 || got[0].Report.Step != "good" {
		t.Fatalf("got %d reports, want just the readable one", len(got))
	}

	// And it is gone, rather than being re-read every hour forever.
	if _, err := os.Stat(filepath.Join(dir, "20260101T000000.000-deadbeef.json")); !os.IsNotExist(err) {
		t.Error("the unparseable file is still there")
	}
}

// TestReportsAgeOut. A server that never grows the endpoint should not leave
// a machine carrying a year of crash reports it will never deliver.
func TestReportsAgeOut(t *testing.T) {
	s, _ := store(t)

	if err := s.Put(report(time.Now().Add(-diagfile.MaxAge-time.Hour), "ancient")); err != nil {
		t.Fatal(err)
	}

	if err := s.Put(report(time.Now(), "recent")); err != nil {
		t.Fatal(err)
	}

	got, err := s.List()
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 1 || got[0].Report.Step != "recent" {
		t.Errorf("got %d reports, want only the recent one", len(got))
	}
}

// TestRemoveOnlyTouchesThisDirectory. IDs come back from List and go straight
// into Remove; one carrying a separator would be a path this store had no
// business deleting.
func TestRemoveOnlyTouchesThisDirectory(t *testing.T) {
	s, _ := store(t)

	for _, id := range []string{"../outside.json", "sub/dir.json"} {
		if err := s.Remove(id); err == nil || !strings.Contains(err.Error(), "not a report") {
			t.Errorf("Remove(%q) = %v, want a refusal", id, err)
		}
	}
}
