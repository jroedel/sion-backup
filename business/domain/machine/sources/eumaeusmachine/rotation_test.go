package eumaeusmachine_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jroedel/sion-backup/business/domain/machine/machinebus"
	"github.com/jroedel/sion-backup/business/domain/machine/sources/eumaeusmachine"
	"github.com/jroedel/sion-backup/foundation/eumaeusapi"
)

// TestAnOfferIsDecodedAndItsAbsenceIsTheAnswer.
//
// Almost every machine almost always has no offer, and the absence has to
// survive decoding: an offer read into a value type arrives as a bucket with
// an empty URL, which is one careless test away from being accepted.
func TestAnOfferIsDecodedAndItsAbsenceIsTheAnswer(t *testing.T) {
	withOffer := source(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{
		  "node_id": "macbook-air",
		  "repository": {
		    "url": "s3:https://s3.example/macbook-air-backup",
		    "state": "active",
		    "created_at": "2024-03-14T09:00:00Z"
		  },
		  "card": {"state": "issued", "issued_at": "2024-03-14T09:12:00Z"},
		  "offer": {
		    "url": "s3:https://s3.example/macbook-air-2027",
		    "provider": "wasabi",
		    "region": "us-central-1",
		    "bucket": "macbook-air-2027",
		    "offered_at": "2026-09-13T09:00:00Z"
		  }
		}`))
	})

	state, err := withOffer.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if state.Offer == nil {
		t.Fatal("the offer was not decoded")
	}

	switch {
	case state.Offer.Bucket != "macbook-air-2027":
		t.Errorf("bucket %q", state.Offer.Bucket)
	case state.Offer.Region != "us-central-1":
		t.Errorf("region %q", state.Offer.Region)
	case state.Offer.OfferedAt.IsZero():
		t.Error("the offer has no date, so nothing can say how long it has been waiting")
	case state.Offer.Adopted:
		// `adopted` is omitted rather than sent false, and an ordinary empty
		// bucket must not read as one holding ten years of snapshots.
		t.Error("an offer with no adopted key decoded as adopted")
	}

	if state.RepositoryState != machinebus.StateActive || state.CuttingOver() {
		t.Errorf("repository state %q", state.RepositoryState)
	}

	if state.Card.State != machinebus.CardIssued || state.Card.IssuedAt.IsZero() {
		t.Errorf("card = %+v", state.Card)
	}

	noOffer := source(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{
		  "node_id": "macbook-air",
		  "repository": {"url": "s3:https://s3.example/b", "state": "active",
		    "created_at": "2024-03-14T09:00:00Z"},
		  "card": {"state": "never", "issued_at": null}
		}`))
	})

	quiet, err := noOffer.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if quiet.Offer != nil {
		t.Errorf("a response with no offer produced one: %+v", quiet.Offer)
	}

	// issued_at is null alongside "never" and alongside "superseded". The
	// state is the signal; a client reading the date as "has a card" would get
	// both of those backwards.
	if !quiet.Card.Owed() || !quiet.Card.IssuedAt.IsZero() {
		t.Errorf("card = %+v, want owed with no date", quiet.Card)
	}
}

// TestASupersededCardIsTheOnlyOneThatSaysDestroy.
//
// "never" is what a machine reads immediately after a cutover, while the old
// bucket's keys still work and its card is the only way into the only bucket
// holding any history. Telling an owner to shred that would be the worst
// advice this program could give.
func TestASupersededCardIsTheOnlyOneThatSaysDestroy(t *testing.T) {
	s := source(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{
		  "node_id": "macbook-air",
		  "repository": {"url": "s3:https://s3.example/b", "state": "cutting-over",
		    "created_at": "2024-03-14T09:00:00Z"},
		  "card": {"state": "never", "issued_at": null}
		}`))
	})

	state, err := s.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if !state.CuttingOver() {
		t.Error("a cutting-over repository did not decode as one")
	}

	if state.Card.DestroyTheOld() {
		t.Fatal("a machine mid-cutover was told to destroy the card that still works")
	}

	if !state.Card.Owed() {
		t.Error("a new bucket with no card is not owed one")
	}
}

// TestAnAdoptedOfferSaysSo. "Move onto the archive we already have" reads very
// differently from "move onto empty storage", and the seeding run is a
// different length in each case.
func TestAnAdoptedOfferSaysSo(t *testing.T) {
	s := source(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{
		  "node_id": "macbook-air",
		  "repository": {"url": "s3:https://s3.example/b", "state": "active",
		    "created_at": "2024-03-14T09:00:00Z", "adopted": true},
		  "card": {"state": "issued"},
		  "offer": {"url": "s3:https://s3.example/archive", "provider": "wasabi",
		    "region": "eu-central-1", "bucket": "archive",
		    "offered_at": "2026-09-13T09:00:00Z", "adopted": true}
		}`))
	})

	state, err := s.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if !state.RepositoryAdopted {
		t.Error("an adopted repository did not decode as one")
	}

	if state.Offer == nil || !state.Offer.Adopted {
		t.Error("an adopted offer did not decode as one")
	}
}

// posted captures one request body, so the four calls can be checked for what
// they actually send.
type posted struct {
	method string
	path   string
	body   map[string]any
}

func recording(t *testing.T, status int, reply string) (*eumaeusapi.Client, *posted) {
	t.Helper()

	got := &posted{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method, got.path = r.Method, r.URL.Path

		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got.body)

		w.WriteHeader(status)

		if reply != "" {
			w.Write([]byte(reply))
		}
	}))
	t.Cleanup(srv.Close)

	c, err := eumaeusapi.New(eumaeusapi.Config{BaseURL: srv.URL, Token: "mt_1"})
	if err != nil {
		t.Fatal(err)
	}

	return c, got
}

// TestARotationRequestCarriesTheFiguresTheOwnerWasShown.
func TestARotationRequestCarriesTheFiguresTheOwnerWasShown(t *testing.T) {
	c, got := recording(t, http.StatusAccepted, "")

	err := eumaeusmachine.NewSource(c).RequestRotation(context.Background(), machinebus.Measured{
		RepositoryURL:    "s3:https://s3.example/b",
		ReclaimableBytes: 204010946560,
		FreshBytes:       161061273600,
		MeasuredAt:       time.Date(2026, 9, 9, 13, 20, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}

	if got.path != "/api/backup/v1/machines/me/rotation-request" || got.method != http.MethodPost {
		t.Fatalf("%s %s", got.method, got.path)
	}

	for _, key := range []string{"repository_url", "reclaimable_bytes", "fresh_bytes", "measured_at"} {
		if _, ok := got.body[key]; !ok {
			t.Errorf("the request does not carry %q", key)
		}
	}
}

// TestEveryRefusalIsToldApart.
//
// Each of these is an ordinary answer about the fleet's state rather than a
// fault of the machine's, and the page says something different about each.
func TestEveryRefusalIsToldApart(t *testing.T) {
	cases := []struct {
		name   string
		status int
		call   func(*testing.T, *eumaeusapi.Client) error
		want   error
	}{
		{
			name:   "asked twice",
			status: http.StatusConflict,
			call: func(_ *testing.T, c *eumaeusapi.Client) error {
				return eumaeusmachine.NewSource(c).RequestRotation(context.Background(),
					machinebus.Measured{RepositoryURL: "s3:https://s3.example/b"})
			},
			want: machinebus.ErrAlreadyAsked,
		},
		{
			name:   "nothing offered",
			status: http.StatusConflict,
			call: func(_ *testing.T, c *eumaeusapi.Client) error {
				return eumaeusmachine.NewSource(c).Cutover(context.Background(), "s3:https://s3.example/b")
			},
			want: machinebus.ErrNothingOffered,
		},
		{
			name:   "not cutting over, or nothing verified has landed",
			status: http.StatusConflict,
			call: func(_ *testing.T, c *eumaeusapi.Client) error {
				_, err := eumaeusmachine.NewSource(c).ReleaseOldBucket(context.Background(), "s3:https://s3.example/b")

				return err
			},
			want: machinebus.ErrNotCuttingOver,
		},
		{
			name:   "no repository at all",
			status: http.StatusNotFound,
			call: func(_ *testing.T, c *eumaeusapi.Client) error {
				return eumaeusmachine.NewSource(c).Cutover(context.Background(), "s3:https://s3.example/b")
			},
			want: machinebus.ErrNoRepository,
		},
		{
			name:   "the token is finished",
			status: http.StatusUnauthorized,
			call: func(_ *testing.T, c *eumaeusapi.Client) error {
				return eumaeusmachine.NewSource(c).Cutover(context.Background(), "s3:https://s3.example/b")
			},
			want: machinebus.ErrNotEnrolled,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			client, _ := recording(t, c.status, `{"error":"no"}`)

			if err := c.call(t, client); !errors.Is(err, c.want) {
				t.Errorf("err = %v, want %v", err, c.want)
			}
		})
	}
}

// TestReleasingTheOldBucketNamesWhereTheMachineIsNow.
//
// Deliberately that direction: a machine's memory of an earlier bucket is
// local state that a reimage takes with it while the token survives, so naming
// where it is now makes this a coherence check rather than a guess.
func TestReleasingTheOldBucketNamesWhereTheMachineIsNow(t *testing.T) {
	c, got := recording(t, http.StatusOK, `{
	  "bucket": "office-laptop-1-backup",
	  "url": "s3:https://s3.example/office-laptop-1-backup",
	  "release_asked_at": "2026-09-16T08:15:00Z"
	}`)

	released, err := eumaeusmachine.NewSource(c).ReleaseOldBucket(context.Background(), "s3:https://s3.example/new")
	if err != nil {
		t.Fatal(err)
	}

	if got.body["repository_url"] != "s3:https://s3.example/new" {
		t.Errorf("named %v, want the bucket the machine is on now", got.body["repository_url"])
	}

	// The date of the FIRST ask, kept across retries, because "when did this
	// machine say it was done" has to stay answerable for the person acting on
	// it.
	if released.AskedAt.IsZero() || released.Bucket != "office-laptop-1-backup" {
		t.Errorf("released = %+v", released)
	}
}

// TestRecordingAPrintedCardNamesTheRepositoryItOpens, so that a card printed
// for a superseded bucket cannot mark the current one as covered.
func TestRecordingAPrintedCardNamesTheRepositoryItOpens(t *testing.T) {
	c, got := recording(t, http.StatusNoContent, "")

	when := time.Date(2026, 9, 9, 13, 22, 0, 0, time.UTC)

	if err := eumaeusmachine.NewSource(c).CardIssued(context.Background(), "s3:https://s3.example/b", when); err != nil {
		t.Fatal(err)
	}

	if got.path != "/api/backup/v1/machines/me/card-issued" {
		t.Fatalf("path %s", got.path)
	}

	if got.body["repository_url"] != "s3:https://s3.example/b" {
		t.Errorf("repository_url = %v", got.body["repository_url"])
	}

	if _, ok := got.body["printed_at"]; !ok {
		t.Error("no printed_at")
	}
}
