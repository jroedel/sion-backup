package disclosuredb_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/jroedel/sion-backup/business/domain/disclosure/disclosurebus"
	"github.com/jroedel/sion-backup/business/domain/disclosure/stores/disclosuredb"
	"github.com/jroedel/sion-backup/foundation/sqldb"
)

func open(t *testing.T) (*disclosuredb.Store, context.Context) {
	t.Helper()

	ctx := context.Background()

	db, err := sqldb.Open(ctx, filepath.Join(t.TempDir(), "sion.db"))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { db.Close() })

	if err := disclosuredb.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}

	return disclosuredb.NewStore(db), ctx
}

var (
	march = time.Date(2026, 3, 14, 9, 0, 0, 0, time.UTC)
	sept  = time.Date(2026, 9, 12, 9, 0, 0, 0, time.UTC)
)

// TestSeeingTheSameHeadAgainDoesNotMoveTheDateItWasFirstSeen.
//
// The whole value of this table is the age of its oldest row: the page says
// "unaltered since 14 March" and means it. Writing seen_at again on every look
// would quietly reduce that claim to "unaltered since you opened this page",
// and it would do so without any test failing anywhere else.
func TestSeeingTheSameHeadAgainDoesNotMoveTheDateItWasFirstSeen(t *testing.T) {
	store, ctx := open(t)

	head := disclosurebus.Witness{Count: 7, Hash: "9f2c", At: march}

	if err := store.Remember(ctx, head, march); err != nil {
		t.Fatal(err)
	}

	if err := store.Remember(ctx, head, sept); err != nil {
		t.Fatal(err)
	}

	kept, err := store.Kept(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(kept) != 1 {
		t.Fatalf("got %d rows, want the second sighting to write nothing", len(kept))
	}

	if !kept[0].Seen.Equal(march) {
		t.Errorf("first seen %v, want %v — the claim just got six months smaller",
			kept[0].Seen, march)
	}
}

// TestHeadsComeBackOldestFirst, which is the order the check walks them in to
// find the furthest back it can vouch from.
func TestHeadsComeBackOldestFirst(t *testing.T) {
	store, ctx := open(t)

	for _, n := range []int64{9, 2, 30, 5} {
		w := disclosurebus.Witness{Count: n, Hash: "h", At: march}

		if err := store.Remember(ctx, w, march); err != nil {
			t.Fatal(err)
		}
	}

	kept, err := store.Kept(ctx)
	if err != nil {
		t.Fatal(err)
	}

	var got []int64
	for _, k := range kept {
		got = append(got, k.Count)
	}

	want := []int64{2, 5, 9, 30}

	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// TestAnEmptyChainIsKeptToo.
//
// A provisioned machine that has never enrolled has a chain of length zero and
// no head at all. Recording it is what lets a later count of zero be compared
// against something rather than being the first thing this machine has seen.
func TestAnEmptyChainIsKeptToo(t *testing.T) {
	store, ctx := open(t)

	if err := store.Remember(ctx, disclosurebus.Witness{}, march); err != nil {
		t.Fatal(err)
	}

	kept, err := store.Kept(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(kept) != 1 || !kept[0].Zero() {
		t.Fatalf("got %+v, want one row recording an empty chain", kept)
	}
}

// TestMigrateRunsTwice, because every start-up runs it.
func TestMigrateRunsTwice(t *testing.T) {
	ctx := context.Background()

	db, err := sqldb.Open(ctx, filepath.Join(t.TempDir(), "sion.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for range 2 {
		if err := disclosuredb.Migrate(ctx, db); err != nil {
			t.Fatal(err)
		}
	}
}
