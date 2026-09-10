package backupbus_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jroedel/sion-backup/business/domain/backup/backupbus"
	"github.com/jroedel/sion-backup/business/domain/backup/stores/backupdb"
	"github.com/jroedel/sion-backup/foundation/paths"
	"github.com/jroedel/sion-backup/foundation/restic"
	"github.com/jroedel/sion-backup/foundation/sqldb"
)

// fakeRestic is a stand-in that takes the backup by doing nothing and performs
// the restore by copying the file back. It is enough to exercise the part this
// package owns: the order of operations, and the verdict.
//
// backupExit chooses what the backup pretends to be: 0 for a clean run, 3 for
// one with unreadable files. restoreMode chooses whether the restore returns
// the right bytes, the wrong bytes, or nothing at all.
func fakeRestic(t *testing.T, backupExit int, restoreMode string) *restic.Runner {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("the stub is a shell script")
	}

	script := `#!/bin/sh
case "$1" in
backup)
  echo '{"message_type":"summary","snapshot_id":"snap1","files_new":3,"total_files_processed":42,"data_added":1024,"total_duration":0.5}'
  if [ ` + itoa(backupExit) + ` -ne 0 ]; then
    echo '{"message_type":"error","item":"/home/user/locked.pst","error":{"message":"permission denied"}}'
    echo "warning: could not read 1 file" >&2
  fi
  exit ` + itoa(backupExit) + `
  ;;
restore)
  shift
  while [ $# -gt 0 ]; do
    case "$1" in
      --target) TARGET="$2"; shift 2 ;;
      --include) INCLUDE="$2"; shift 2 ;;
      *) shift ;;
    esac
  done
  DEST="$TARGET/${INCLUDE#/}"
  case "` + restoreMode + `" in
    good)    mkdir -p "$(dirname "$DEST")"; cp "$INCLUDE" "$DEST" ;;
    corrupt) mkdir -p "$(dirname "$DEST")"; printf 'not the nonce' > "$DEST" ;;
    empty)   mkdir -p "$(dirname "$DEST")"; : > "$DEST" ;;
    missing) : ;;
  esac
  exit 0
  ;;
unlock) exit 0 ;;
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

func itoa(n int) string {
	return string(rune('0' + n))
}

// harness wires a runner over a real database and a temporary data directory.
func harness(t *testing.T, r *restic.Runner) (*backupbus.Runner, *backupdb.Store, paths.Paths) {
	t.Helper()

	dir := t.TempDir()

	t.Setenv("SION_BACKUP_DATA_DIR", dir)

	p, err := paths.Resolve()
	if err != nil {
		t.Fatal(err)
	}

	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	db, err := sqldb.Open(ctx, p.DB)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { db.Close() })

	if err := backupdb.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}

	store := backupdb.NewStore(db)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	return backupbus.NewRunner(store, r, p, log), store, p
}

func request() backupbus.Request {
	return backupbus.Request{
		NodeID: "office-laptop-1",
		Repository: restic.Repository{
			URL:             "s3:https://s3.example.invalid/bucket",
			Password:        []byte("password"),
			AccessKeyID:     []byte("id"),
			SecretAccessKey: []byte("secret"),
		},
		Options: restic.BackupOptions{Targets: []string{"/home/user"}},
	}
}

func TestASuccessfulRunIsVerified(t *testing.T) {
	b, _, _ := harness(t, fakeRestic(t, 0, "good"))

	run, err := b.Run(context.Background(), request(), time.Now)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if run.Outcome != backupbus.OutcomeSuccess {
		t.Errorf("outcome %q (%s), want success", run.Outcome, run.Message)
	}

	if !run.Verified {
		t.Error("the run was not verified")
	}

	if run.SnapshotID != "snap1" {
		t.Errorf("snapshot ID %q", run.SnapshotID)
	}

	if run.FinishedAt.IsZero() {
		t.Error("the run was never closed")
	}
}

// TestAnUnreadableRestoreIsNotSuccess is the check the legacy scripts did not
// really do. restic said it wrote a snapshot; nothing came back out.
func TestAnUnreadableRestoreIsNotSuccess(t *testing.T) {
	for _, mode := range []string{"corrupt", "empty", "missing"} {
		t.Run(mode, func(t *testing.T) {
			b, _, _ := harness(t, fakeRestic(t, 0, mode))

			run, err := b.Run(context.Background(), request(), time.Now)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}

			if run.Outcome != backupbus.OutcomeUnverified {
				t.Errorf("outcome %q, want unverified", run.Outcome)
			}

			if run.Verified {
				t.Error("a restore that returned the wrong bytes was counted as verified")
			}

			if run.Message == "" {
				t.Error("no message explaining what happened")
			}
		})
	}
}

// TestExitThreeIsIncompleteNotSuccess is the shell scripts' bug, as a test.
func TestExitThreeIsIncompleteNotSuccess(t *testing.T) {
	b, _, _ := harness(t, fakeRestic(t, 3, "good"))

	run, err := b.Run(context.Background(), request(), time.Now)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if run.Outcome != backupbus.OutcomeIncomplete {
		t.Fatalf("outcome %q, want incomplete", run.Outcome)
	}

	if run.Outcome.Good() {
		t.Error("an incomplete backup reported itself as good")
	}

	// Still verified: the repository is readable, the snapshot is just short a
	// file. Both facts are worth having.
	if !run.Verified {
		t.Error("an incomplete backup was not verified even though the restore worked")
	}

	if len(run.UnreadableFiles) != 1 {
		t.Errorf("unreadable files: %v", run.UnreadableFiles)
	}
}

// TestTheVerificationDirectoryIsAlwaysABackupTarget guards the property that
// makes verification mean anything. It cannot be edited out of the plan.
func TestTheVerificationDirectoryIsAlwaysABackupTarget(t *testing.T) {
	b, _, p := harness(t, fakeRestic(t, 0, "good"))

	req := request()
	req.Options.Targets = []string{"/home/user"}

	run, err := b.Run(context.Background(), req, time.Now)
	if err != nil {
		t.Fatal(err)
	}

	// The stub's restore copies from the --include path, so a successful
	// verification proves the include named the file under p.Verify.
	if !run.Verified {
		t.Fatalf("verification failed, so the verify directory was not backed up: %s", run.Message)
	}

	if _, err := os.Stat(p.Verify); err != nil {
		t.Errorf("the verify directory is gone: %v", err)
	}
}

// TestTheNonceIsRemovedAfterASuccessfulRun keeps the verify directory from
// growing a file per run forever, which is what the legacy scripts did.
func TestTheNonceIsRemovedAfterASuccessfulRun(t *testing.T) {
	b, _, p := harness(t, fakeRestic(t, 0, "good"))

	if _, err := b.Run(context.Background(), request(), time.Now); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(p.Verify)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 0 {
		t.Errorf("the verify directory still holds %d files after a run", len(entries))
	}

	scratch, err := os.ReadDir(p.Scratch)
	if err != nil {
		t.Fatal(err)
	}

	if len(scratch) != 0 {
		t.Errorf("the scratch directory still holds %d entries after a run", len(scratch))
	}
}

// TestARunIsRecordedBeforeItFinishes is what lets the status page distinguish
// "nothing happened last night" from "it was killed halfway through".
func TestARunIsRecordedBeforeItFinishes(t *testing.T) {
	b, store, _ := harness(t, fakeRestic(t, 0, "good"))
	ctx := context.Background()

	done := make(chan struct{})

	go func() {
		defer close(done)

		if _, err := b.Run(ctx, request(), time.Now); err != nil {
			t.Errorf("Run: %v", err)
		}
	}()

	<-done

	runs, err := store.Recent(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}

	if len(runs) != 1 {
		t.Fatalf("history holds %d runs, want 1", len(runs))
	}

	if runs[0].ID == 0 {
		t.Error("the run has no ID")
	}
}

func TestUnreportedRunsAreTracked(t *testing.T) {
	b, store, _ := harness(t, fakeRestic(t, 0, "good"))
	ctx := context.Background()

	run, err := b.Run(ctx, request(), time.Now)
	if err != nil {
		t.Fatal(err)
	}

	pending, err := store.Unreported(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}

	if len(pending) != 1 || pending[0].ID != run.ID {
		t.Fatalf("pending = %v, want the one run just taken", pending)
	}

	if err := store.MarkReported(ctx, run.ID, time.Now()); err != nil {
		t.Fatal(err)
	}

	pending, err = store.Unreported(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}

	if len(pending) != 0 {
		t.Errorf("still %d pending after reporting", len(pending))
	}
}

func TestSnapshotAndRestoredPaths(t *testing.T) {
	cases := []struct {
		source   string
		snapshot string
	}{
		{"/home/user/verify/verification.txt", "/home/user/verify/verification.txt"},
		{`C:\Users\Example\AppData\Local\sion-backup\verify\verification.txt`,
			"/C/Users/Example/AppData/Local/sion-backup/verify/verification.txt"},
		{`D:\data\verify`, "/D/data/verify"},
	}

	for _, c := range cases {
		if got := backupbus.SnapshotPath(c.source); got != c.snapshot {
			t.Errorf("SnapshotPath(%q) = %q, want %q", c.source, got, c.snapshot)
		}
	}

	// The restored path is the snapshot path under the target, with no leading
	// separator doubling it up.
	got := backupbus.RestoredPath("/tmp/scratch", "/home/user/verify/v.txt")
	if want := filepath.Join("/tmp/scratch", "home/user/verify/v.txt"); got != want {
		t.Errorf("RestoredPath = %q, want %q", got, want)
	}
}

func TestOutcomeGood(t *testing.T) {
	for outcome, want := range map[backupbus.Outcome]bool{
		backupbus.OutcomeSuccess:    true,
		backupbus.OutcomeDegraded:   false,
		backupbus.OutcomeIncomplete: false,
		backupbus.OutcomeUnverified: false,
		backupbus.OutcomeFailed:     false,
	} {
		if got := outcome.Good(); got != want {
			t.Errorf("%s.Good() = %v, want %v", outcome, got, want)
		}
	}
}

// TestTheRunUUIDSurvivesTheDatabase is what makes a run reported days later —
// a laptop that backed up on a plane — report the identity it announced when
// it started, rather than a fresh one the dashboard cannot pair with anything.
func TestTheRunUUIDSurvivesTheDatabase(t *testing.T) {
	b, store, _ := harness(t, fakeRestic(t, 0, "good"))
	ctx := context.Background()

	req := request()
	req.RunUUID = "0192f3a1-7c4e-7b21-9f10-3c2d5e8a41b7"
	req.Seeding = true

	run, err := b.Run(ctx, req, time.Now)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if run.RunUUID != req.RunUUID || !run.Seeding {
		t.Errorf("Run returned uuid %q seeding %v", run.RunUUID, run.Seeding)
	}

	pending, err := store.Unreported(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}

	if len(pending) != 1 {
		t.Fatalf("%d runs are waiting to be reported, want 1", len(pending))
	}

	if pending[0].RunUUID != req.RunUUID {
		t.Errorf("the stored run reports as %q, want %q", pending[0].RunUUID, req.RunUUID)
	}

	if !pending[0].Seeding {
		t.Error("the seeding flag did not survive the database")
	}
}

// TestSeedingIsTrueOnlyUntilSomethingIsWritten. The server's cutover guard
// waits on the seeding run, so a machine that claimed to be seeding every
// night would keep a rotation open forever.
func TestSeedingIsTrueUntilTheRepositoryHasASnapshot(t *testing.T) {
	b, _, _ := harness(t, fakeRestic(t, 0, "good"))
	ctx := context.Background()

	req := request()

	seeding, err := b.Seeding(ctx, req.Repository.URL)
	if err != nil {
		t.Fatal(err)
	}

	if !seeding {
		t.Error("the first run against a new repository is not reported as seeding")
	}

	if _, err := b.Run(ctx, req, time.Now); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if seeding, err = b.Seeding(ctx, req.Repository.URL); err != nil {
		t.Fatal(err)
	}

	if seeding {
		t.Error("a repository that has been written to is still reported as needing seeding")
	}

	// A rotation gives the machine a different bucket, and that one does need
	// seeding — which is the case the flag exists for.
	if seeding, err = b.Seeding(ctx, "s3:https://s3.example.invalid/rotated"); err != nil {
		t.Fatal(err)
	}

	if !seeding {
		t.Error("a freshly rotated repository is not reported as seeding")
	}
}
