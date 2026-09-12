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

// TestARefusedTokenIsDeEnrolment, on either status.
func TestARefusedTokenIsDeEnrolment(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		s := source(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		})

		if _, err := s.State(context.Background()); !errors.Is(err, machinebus.ErrNotEnrolled) {
			t.Errorf("%d: got %v, want ErrNotEnrolled", status, err)
		}
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
