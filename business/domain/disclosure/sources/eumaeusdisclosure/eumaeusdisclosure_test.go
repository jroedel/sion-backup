package eumaeusdisclosure_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jroedel/sion-backup/business/domain/disclosure/disclosurebus"
	"github.com/jroedel/sion-backup/business/domain/disclosure/sources/eumaeusdisclosure"
	"github.com/jroedel/sion-backup/foundation/eumaeusapi"
)

func source(t *testing.T, h http.HandlerFunc) *eumaeusdisclosure.Source {
	t.Helper()

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	c, err := eumaeusapi.New(eumaeusapi.Config{BaseURL: srv.URL, Token: "mt_1"})
	if err != nil {
		t.Fatal(err)
	}

	return eumaeusdisclosure.NewSource(c)
}

const oneOfEach = `{
  "count": 412,
  "head": "9f2c1a7e",
  "entries": [
    {"seq": 412, "at": "2026-09-09T01:14:02+02:00", "kind": "fetched",
     "actor": "sion-backup/0.5.3", "from_ip": "203.0.113.9",
     "credentials_version": 3, "by_machine": true,
     "describe": "This computer collected the password to run a backup.",
     "prev_hash": "1a2b", "hash": "9f2c1a7e"},
    {"seq": 411, "at": "2026-09-08T14:02:00+02:00", "kind": "revealed",
     "actor": "Fr Jeff Roedel", "from_ip": "",
     "credentials_version": 3, "by_machine": false,
     "describe": "A person read the password.",
     "prev_hash": "", "hash": "1a2b"}
  ]
}`

// TestTheWholeChainLengthSurvivesACappedList.
//
// count is the length of the chain and not the length of entries, and the
// difference is what the page shows as "the 200 most recent of 412". Reading
// it off len(entries) would be silently right on every small machine and
// wrong on every machine that has been running a year.
func TestTheWholeChainLengthSurvivesACappedList(t *testing.T) {
	s := source(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(oneOfEach))
	})

	log, err := s.List(context.Background(), 200)
	if err != nil {
		t.Fatal(err)
	}

	if log.Count != 412 {
		t.Errorf("chain length %d, want 412", log.Count)
	}

	if len(log.Entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(log.Entries))
	}

	if log.Entries[0].Seq != 412 {
		t.Error("the entries did not arrive newest first")
	}
}

// TestAPersonIsDistinguishedFromTheMachine, from the server's own field.
func TestAPersonIsDistinguishedFromTheMachine(t *testing.T) {
	s := source(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(oneOfEach))
	})

	log, err := s.List(context.Background(), 200)
	if err != nil {
		t.Fatal(err)
	}

	person := log.Entries[1]

	switch {
	case person.ByMachine:
		t.Error("the entry a person made is marked as the machine's")
	case person.Kind != disclosurebus.KindRevealed:
		t.Errorf("kind %q, want %q", person.Kind, disclosurebus.KindRevealed)
	case person.Actor != "Fr Jeff Roedel":
		t.Errorf("actor %q, want the person's name", person.Actor)
	case person.Describe == "":
		t.Error("the sentence written on the server did not survive the trip")
	}
}

// TestTheLimitIsAskedFor, and clamped to what the server will accept.
//
// The server answers 400 above its maximum, and a 400 here would reach a
// person as a page saying something went wrong with their backups — which it
// did not.
func TestTheLimitIsAskedFor(t *testing.T) {
	for _, tc := range []struct{ ask, want string }{
		{"200", "200"},
		{"9000", "500"},
		{"0", "1"},
	} {
		var got string

		s := source(t, func(w http.ResponseWriter, r *http.Request) {
			got = r.URL.Query().Get("limit")
			w.Write([]byte(`{"count":0,"head":"","entries":[]}`))
		})

		limit := 0
		switch tc.ask {
		case "200":
			limit = 200
		case "9000":
			limit = 9000
		}

		if _, err := s.List(context.Background(), limit); err != nil {
			t.Fatal(err)
		}

		if got != tc.want {
			t.Errorf("asked for limit=%s, want %s", got, tc.want)
		}
	}
}

// TestAServerWithoutTheEndpointIsNotAnError.
//
// A fleet mid-upgrade has machines talking to a Eumaeus that has not deployed
// this yet. That is a fact about the server, and showing its owner a failure
// they cannot act on — on a page about whether their data is safe — would be
// the wrong end of the trade entirely.
func TestAServerWithoutTheEndpointIsNotAnError(t *testing.T) {
	s := source(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	_, err := s.List(context.Background(), 200)

	if !errors.Is(err, disclosurebus.ErrUnsupported) {
		t.Errorf("got %v, want ErrUnsupported", err)
	}
}

// TestATokenRefusedIsDeEnrolment, on either status.
//
// The same reading eumaeuscreds gives them on the credentials endpoint: a
// machine that may not read its own record is a machine that is no longer in
// the fleet, whichever of the two the server chose.
func TestATokenRefusedIsDeEnrolment(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		s := source(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		})

		_, err := s.List(context.Background(), 200)

		if !errors.Is(err, disclosurebus.ErrNotEnrolled) {
			t.Errorf("%d: got %v, want ErrNotEnrolled", status, err)
		}
	}
}

// TestAChainedSinceDateIsCarried, because where it is present the page has to
// make the smaller claim.
func TestAChainedSinceDateIsCarried(t *testing.T) {
	s := source(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"count":2,"head":"9f2c","chained_since":"2026-08-01T00:00:00Z",
		  "entries":[{"seq":2,"at":"2026-09-09T01:14:02Z","kind":"fetched","actor":"a",
		  "from_ip":"","credentials_version":1,"by_machine":true,"describe":"d",
		  "prev_hash":"1a","hash":"9f2c"}]}`))
	})

	log, err := s.List(context.Background(), 200)
	if err != nil {
		t.Fatal(err)
	}

	if log.ChainedSince.IsZero() {
		t.Error("the date the seals were computed was dropped")
	}
}

// TestAnAbsentChainedSinceIsTheStrongerAnswer.
func TestAnAbsentChainedSinceIsTheStrongerAnswer(t *testing.T) {
	s := source(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"count":0,"head":"","entries":[]}`))
	})

	log, err := s.List(context.Background(), 200)
	if err != nil {
		t.Fatal(err)
	}

	if !log.ChainedSince.IsZero() {
		t.Error("an absent chained_since did not come through as absent")
	}
}
