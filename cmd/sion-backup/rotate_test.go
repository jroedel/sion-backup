package main

import (
	"context"
	"testing"
	"time"

	"github.com/jroedel/sion-backup/business/domain/credential/credentialbus"
	"github.com/jroedel/sion-backup/business/domain/machine/machinebus"
	"github.com/jroedel/sion-backup/business/domain/plan/planbus"
)

// TestNothingIsCreatedUnlessTheServerSaidSo is the most important test in this
// file, and it is written so that a regression cannot pass it.
//
// The runner is nil. A build that reached restic on this path at all — to ask
// whether the repository exists, to create it, to do anything — would panic
// rather than quietly do the wrong thing. That is the property: absence of
// `expect_empty` is byte for byte what this program did before the field
// existed, which is nothing.
func TestNothingIsCreatedUnlessTheServerSaidSo(t *testing.T) {
	d := &deps{log: quietLog()}

	plan := planbus.Plan{Repository: "s3:https://s3.example.invalid/bucket"}

	for _, set := range []credentialbus.Set{
		{},
		{RepositoryState: machinebus.StateActive},

		// The trap this exists for. A cutting-over bucket can be one that was
		// adopted with ten years of somebody's snapshots in it, so the state
		// is descriptive and never the permission.
		{RepositoryState: machinebus.StateCuttingOver},
	} {
		if err := d.openRepository(context.Background(), plan, set); err != nil {
			t.Fatalf("openRepository = %v, want nothing done", err)
		}
	}
}

// TestTheServersAnswerIsStoredWhole.
func TestTheServersAnswerIsStoredWhole(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)

	got := machineStateFrom(machinebus.State{
		NodeID:              "office-laptop-1",
		OwnerName:           "Fr N",
		OwnerEmail:          "n@example.org",
		RepositoryURL:       "s3:https://s3.example/b",
		RepositoryState:     machinebus.StateCuttingOver,
		RepositoryAdopted:   true,
		RepositoryCreatedAt: now.AddDate(-7, 0, 0),
		Card:                machinebus.Card{State: machinebus.CardNever},
		Offer: &machinebus.Offer{
			URL:       "s3:https://s3.example/next",
			Bucket:    "next",
			Region:    "us-central-1",
			OfferedAt: now.AddDate(0, 0, -9),
			Adopted:   true,
		},
	}, now)

	switch {
	case got.AskedAt != now:
		t.Errorf("AskedAt = %v", got.AskedAt)
	case !got.Bucket.CuttingOver():
		t.Error("the cutting-over state was lost")
	case !got.Bucket.Adopted:
		t.Error("the adopted flag was lost")
	case got.Bucket.Age(now) < 6*365*24*time.Hour:
		t.Errorf("Age = %v, want about seven years", got.Bucket.Age(now))
	case got.Offer == nil || !got.Offer.Adopted:
		t.Errorf("Offer = %+v", got.Offer)
	case !got.Card.Owed():
		t.Error("a bucket with no card is not owed one")
	}

	// And no offer stays no offer, rather than becoming a bucket with an empty
	// URL that something could accept.
	if quiet := machineStateFrom(machinebus.State{}, now); quiet.Offer != nil {
		t.Errorf("an answer with no offer produced one: %+v", quiet.Offer)
	}
}

// TestAFailedPollKeepsTheLastAnswerAndSaysWhy.
//
// Two facts rather than one. A page showing a three-week-old answer with no
// indication that asking has been failing for three weeks reads as "nobody has
// provisioned anything" when it means "this computer cannot ask".
func TestAFailedPollKeepsTheLastAnswerAndSaysWhy(t *testing.T) {
	srv := &server{state: known()}
	d, ctx := machineFor(t, srv)

	d.pollState(ctx)

	stored, err := d.plan.MachineState(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if !stored.Known() || stored.Bucket.URL != known().RepositoryURL {
		t.Fatalf("stored = %+v", stored)
	}

	first := stored.AskedAt

	srv.err = context.DeadlineExceeded
	d.pollState(ctx)

	after, err := d.plan.MachineState(ctx)
	if err != nil {
		t.Fatal(err)
	}

	switch {
	case after.Bucket.URL != known().RepositoryURL:
		t.Error("a failed poll threw away the last answer")
	case after.AskedAt != first:
		t.Error("a failed poll pretended the answer had been refreshed")
	case after.Error == "":
		t.Error("a failed poll left no trace of why")
	}
}

// TestTheLastGoodAnswerIsNotShadowedByAnOldError.
func TestTheLastGoodAnswerIsNotShadowedByAnOldError(t *testing.T) {
	srv := &server{state: known(), err: context.DeadlineExceeded}
	d, ctx := machineFor(t, srv)

	d.pollState(ctx)
	srv.err = nil
	d.pollState(ctx)

	stored, err := d.plan.MachineState(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if stored.Error != "" {
		t.Errorf("a successful poll left the old error behind: %q", stored.Error)
	}
}
