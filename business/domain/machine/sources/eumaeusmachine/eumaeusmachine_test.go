package eumaeusmachine_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jroedel/sion-backup/business/domain/machine/machinebus"
	"github.com/jroedel/sion-backup/business/domain/machine/sources/eumaeusmachine"
	"github.com/jroedel/sion-backup/foundation/eumaeusapi"
)

func source(t *testing.T, h http.HandlerFunc) *eumaeusmachine.Source {
	t.Helper()

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	c, err := eumaeusapi.New(eumaeusapi.Config{BaseURL: srv.URL, Token: "mt_1"})
	if err != nil {
		t.Fatal(err)
	}

	return eumaeusmachine.NewSource(c)
}

// TestTheTwoFactsAMachineCannotLearnAnywhereElse.
//
// The node ID and the repository URL. Everything else in the response belongs
// to somebody else today, and this reads past it rather than mirroring it.
func TestTheTwoFactsAMachineCannotLearnAnywhereElse(t *testing.T) {
	s := source(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer mt_1" {
			t.Errorf("authorisation %q, want the machine token", got)
		}

		w.Write([]byte(`{
		  "node_id": "macbook-air",
		  "owner": {"name": "Fr Domingo"},
		  "state": "ok",
		  "warn_after_hours": 48,
		  "repository": {
		    "url": "s3:https://s3.example/macbook-air-backup",
		    "state": "active",
		    "created_at": "2026-09-12T09:00:00Z"
		  },
		  "credentials_version": 1,
		  "card": {"state": "issued"},
		  "server_time": "2026-09-12T12:00:00Z"
		}`))
	})

	state, err := s.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	switch {
	case state.NodeID != "macbook-air":
		t.Errorf("node ID %q", state.NodeID)
	case state.RepositoryURL != "s3:https://s3.example/macbook-air-backup":
		t.Errorf("repository %q", state.RepositoryURL)
	case state.RepositoryCreatedAt.IsZero():
		t.Error("the history horizon was dropped")
	case !state.Usable():
		t.Error("a complete answer is not usable")
	}
}

// TestARefusedTokenIsDeEnrolment, on 401 and on nothing else.
func TestARefusedTokenIsDeEnrolment(t *testing.T) {
	s := source(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})

	if _, err := s.State(context.Background()); !errors.Is(err, machinebus.ErrNotEnrolled) {
		t.Errorf("got %v, want ErrNotEnrolled", err)
	}
}

// TestA403IsNotDeEnrolmentHereEither.
//
// This source copied the 401-or-403 pair out of eumaeuscreds, where it was a
// deliberate exception. It was not an exception worth copying — the status
// does not arrive on this endpoint — and copying it is how a reading spreads
// past the one place somebody thought about it.
func TestA403IsNotDeEnrolmentHereEither(t *testing.T) {
	s := source(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})

	_, err := s.State(context.Background())

	if errors.Is(err, machinebus.ErrNotEnrolled) {
		t.Error("a 403 is reported as de-enrolment")
	}

	if err == nil {
		t.Error("a 403 was not reported at all")
	}
}

// TestA404IsAMachineWithNoRepository, which is what the specification says it
// is and not what a 404 usually means.
//
// "The token is good and the machine has no repository. An enrolled machine
// always has one, so this is a repository removed from underneath a machine
// rather than an ordinary answer." Reading it as a server too old to answer —
// the reflex, and what this client did first — would turn a repository that
// has gone missing into a shrug in a log.
//
// It is not de-enrolment either: retirement and revocation refuse at
// authentication with 401 (jroedel/eumaeus#116).
func TestA404IsAMachineWithNoRepository(t *testing.T) {
	s := source(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error": "this machine has no repository"}`))
	})

	_, err := s.State(context.Background())

	if !errors.Is(err, machinebus.ErrNoRepository) {
		t.Errorf("got %v, want ErrNoRepository", err)
	}

	if errors.Is(err, machinebus.ErrNotEnrolled) {
		t.Error("a 404 was read as de-enrolment")
	}
}

// TestHalfAnAnswerIsRefused.
//
// A state with a node and no repository cannot produce a plan, and writing
// half of one would leave a machine that looks repaired and is not.
func TestHalfAnAnswerIsRefused(t *testing.T) {
	s := source(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"node_id": "macbook-air", "repository": {"url": ""}}`))
	})

	b := machinebus.NewBusiness(s)

	if _, err := b.State(context.Background()); err == nil {
		t.Error("a state naming no repository was accepted")
	}
}

// TestAMachineWithNoTokenHasNobodyToAsk.
func TestAMachineWithNoTokenHasNobodyToAsk(t *testing.T) {
	b := machinebus.NewBusiness(nil)

	if b.Available() {
		t.Error("a business with no source reports itself available")
	}

	if _, err := b.State(context.Background()); !errors.Is(err, machinebus.ErrNotEnrolled) {
		t.Errorf("got %v, want ErrNotEnrolled", err)
	}
}

// TestTheServerSaysWhatItServes.
//
// Asked rather than inferred, which is the whole point: a 404 from this API is
// either "no such path here" or something documented about this machine, and
// the two are identical on the wire down to the error body. See
// jroedel/eumaeus#144, and the bug in this package that prompted it.
func TestTheServerSaysWhatItServes(t *testing.T) {
	s := source(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{
		  "node_id": "macbook-air",
		  "repository": {"url": "s3:https://s3.example/b"},
		  "api": {"serves": [
		    "GET /machines/me",
		    "GET /machines/me/disclosures",
		    "POST /runs"
		  ]}
		}`))
	})

	state, err := s.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if !state.KnowsWhatItServes() {
		t.Fatal("a server that listed its routes is reported as having said nothing")
	}

	if !state.Serves("GET", "/machines/me/disclosures") {
		t.Error("a route the server listed is reported as not served")
	}

	if state.Serves("GET", "/machines/me/card-issued") {
		t.Error("a route the server did not list is reported as served")
	}
}

// TestTheMethodIsPartOfTheQuestion.
//
// The server matches method and path together, so a GET to a POST-only path
// falls through to the catch-all as a 404 rather than a 405. A client matching
// on the path alone would believe it could call it.
func TestTheMethodIsPartOfTheQuestion(t *testing.T) {
	s := source(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"node_id": "n", "repository": {"url": "u"},
		  "api": {"serves": ["POST /runs"]}}`))
	})

	state, err := s.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if !state.Serves("POST", "/runs") {
		t.Error("POST /runs is not recognised")
	}

	if state.Serves("GET", "/runs") {
		t.Error("a GET to a POST-only path is reported as served")
	}
}

// TestSayingNothingIsNotSayingNothingIsServed.
//
// A deployment older than the field omits it, and the honest reading is
// "assume nothing" — fall back to whatever the client did before it could ask.
// Flattening that to an empty list would turn every older server into one that
// implements no endpoints at all, which is the opposite of the truth.
func TestSayingNothingIsNotSayingNothingIsServed(t *testing.T) {
	s := source(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"node_id": "n", "repository": {"url": "u"}}`))
	})

	state, err := s.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if state.KnowsWhatItServes() {
		t.Error("a server that said nothing is reported as having answered")
	}

	if state.Serves("GET", "/machines/me") {
		t.Error("a server that said nothing is reported as serving something")
	}
}
