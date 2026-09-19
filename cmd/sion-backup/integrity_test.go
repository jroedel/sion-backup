package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"github.com/jroedel/sion-backup/business/domain/credential/credentialbus"
	"github.com/jroedel/sion-backup/business/domain/diag/diagbus"
	"github.com/jroedel/sion-backup/business/domain/plan/planbus"
	"github.com/jroedel/sion-backup/business/domain/plan/stores/plandb"
	"github.com/jroedel/sion-backup/foundation/restic"
	"github.com/jroedel/sion-backup/foundation/sqldb"
)

// lockingRestic is a restic whose check always refuses to take the lock, and
// whose unlock either works or does not.
//
// Exit 11 and not a message, because the whole point is that the CODE is what
// carries "locked" and the code is what restic.Error.Retryable reads. A stub
// that printed the word "locked" and exited 1 would pass a test that the real
// thing fails.
func lockingRestic(t *testing.T, unlockWorks bool, checkAfterUnlock int) *restic.Runner {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("the stub is a shell script")
	}

	marker := filepath.Join(t.TempDir(), "unlocked")

	unlockExit := "1"
	if unlockWorks {
		unlockExit = "0"
	}

	script := `#!/bin/sh
case "$1" in
check)
  if [ -f ` + marker + ` ]; then
    exit ` + strconv.Itoa(checkAfterUnlock) + `
  fi
  echo "unable to create lock in backend: repository is already locked" >&2
  exit 11
  ;;
unlock)
  if [ ` + unlockExit + ` -eq 0 ]; then : > ` + marker + `; fi
  exit ` + unlockExit + `
  ;;
stats)
  echo '{"total_size":1024,"total_file_count":1,"snapshots_count":1}'
  exit 0
  ;;
*) exit 0 ;;
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

// reportQueue keeps what was filed, so a test can ask what the fleet would
// have been told.
type reportQueue struct{ put []diagbus.Report }

func (q *reportQueue) Put(r diagbus.Report) error      { q.put = append(q.put, r); return nil }
func (q *reportQueue) List() ([]diagbus.Queued, error) { return nil, nil }
func (q *reportQueue) Remove(string) error             { return nil }

// damaged reports whether a repository-damage alarm was raised.
func (q *reportQueue) damaged() (bool, string) {
	for _, r := range q.put {
		if r.Kind == diagbus.KindRepositoryDamaged {
			return true, r.Detail
		}
	}

	return false, ""
}

// checkHarness wires just enough of a machine to run one repository check.
func checkHarness(t *testing.T, r *restic.Runner) (*deps, planbus.Plan, *reportQueue) {
	t.Helper()

	ctx := context.Background()

	db, err := sqldb.Open(ctx, filepath.Join(t.TempDir(), "sion.db"))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { db.Close() })

	if err := plandb.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}

	q := &reportQueue{}

	d := &deps{
		log:    quietLog(),
		db:     db,
		plan:   planbus.NewBusiness(plandb.NewStore(db)),
		diag:   diagbus.NewBusiness(q, nil, "sion-backup/test", quietLog()),
		restic: r,
	}

	return d, planbus.Plan{Repository: "s3:https://s3.example.invalid/bucket"}, q
}

// credentials are enough to get past the runner's own refusals, which check
// for an empty password before they check anything else.
func credentials() credentialbus.Set {
	return credentialbus.Set{
		Credentials: credentialbus.Credentials{
			AccessKeyID:     []byte("AKIAEXAMPLE"),
			SecretAccessKey: []byte("secret"),
			ResticPassword:  []byte("the-repository-password"),
		},
	}
}

// TestALockedRepositoryIsNotADamagedOne is the false alarm this branch exists
// to prevent, and it is worth the file on its own.
//
// A laptop suspended mid-check leaves a lock behind. Recorded as a failed
// check it becomes, once the rotation policy is reading it, the loudest
// sentence this program says: your repository may not give the files back,
// rotate now. For a bucket holding two years of history that is days of
// somebody's uplink spent on a stale file — and it is what a real machine in
// this fleet was reporting to its owner.
func TestALockedRepositoryIsNotADamagedOne(t *testing.T) {
	ctx := context.Background()

	d, plan, q := checkHarness(t, lockingRestic(t, true, 0))

	d.checkRepository(ctx, plan, credentials())

	got, err := d.plan.Integrity(ctx, plan.Repository)
	if err != nil {
		t.Fatal(err)
	}

	if got.Failed() {
		t.Errorf("a lock that was cleared was recorded as a failed check: %+v", got)
	}

	if !got.OK {
		t.Errorf("the check passed after the lock was cleared, but OK = false: %+v", got)
	}

	if raised, detail := q.damaged(); raised {
		t.Fatalf("a lock was reported to the fleet as repository damage: %s", detail)
	}
}

// TestALockThatCannotBeClearedIsASkipAndNotAVerdict covers the other half.
//
// Skipped rather than failed, because nothing has been learned about the
// repository — which is exactly the state a metered connection leaves, and is
// recorded the same way. Integrity.Failed is what the rotation policy reads,
// and "we could not look" must never answer the question "is it damaged".
func TestALockThatCannotBeClearedIsASkipAndNotAVerdict(t *testing.T) {
	ctx := context.Background()

	d, plan, q := checkHarness(t, lockingRestic(t, false, 0))

	d.checkRepository(ctx, plan, credentials())

	got, err := d.plan.Integrity(ctx, plan.Repository)
	if err != nil {
		t.Fatal(err)
	}

	if got.Failed() {
		t.Errorf("an uncleared lock was recorded as a failed check: %+v", got)
	}

	if got.SkippedReason == "" {
		t.Errorf("an uncleared lock left no reason for the skip: %+v", got)
	}

	if raised, detail := q.damaged(); raised {
		t.Fatalf("a lock was reported to the fleet as repository damage: %s", detail)
	}
}

// TestRealDamageIsStillReported is the assertion that keeps the two above
// honest. A branch that made every check failure quiet would pass both of them
// and would have removed the only alarm in this program that says the backups
// already taken may not be worth anything.
func TestRealDamageIsStillReported(t *testing.T) {
	ctx := context.Background()

	// The lock clears, and the check that follows fails for a different
	// reason: exit 1, which is restic for "the command failed".
	d, plan, q := checkHarness(t, lockingRestic(t, true, 1))

	d.checkRepository(ctx, plan, credentials())

	got, err := d.plan.Integrity(ctx, plan.Repository)
	if err != nil {
		t.Fatal(err)
	}

	if !got.Failed() {
		t.Errorf("a damaged repository was not recorded as a failed check: %+v", got)
	}

	if raised, _ := q.damaged(); !raised {
		t.Error("a damaged repository was not reported to the fleet")
	}
}
