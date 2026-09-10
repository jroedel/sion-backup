package eumaeusapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jroedel/sion-backup/foundation/eumaeusapi"
)

// TestRequestsCarryTheAPIPrefix is the regression this package was written
// without. Callers name endpoints the way the specification does — "/runs" —
// and every one of them reached an unversioned path on the server, which on a
// real installation is a redirect to a login page rather than a clean 404.
func TestRequestsCarryTheAPIPrefix(t *testing.T) {
	var got string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Path

		if h := r.Header.Get("Authorization"); h != "Bearer mt_1" {
			t.Errorf("Authorization: got %q", h)
		}

		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	client, err := eumaeusapi.New(eumaeusapi.Config{BaseURL: srv.URL, Token: "mt_1"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := client.Do(context.Background(), http.MethodPost, "/runs", struct{}{}, nil); err != nil {
		t.Fatalf("Do: %v", err)
	}

	if want := eumaeusapi.APIPrefix + "/runs"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestABaseURLThatIncludesThePrefixIsAccepted covers the paste from
// openapi.yaml, whose servers entry carries the full base path. Doubling it
// would 404 every call, and the failure would look like a server that has not
// implemented the API.
func TestABaseURLThatIncludesThePrefixIsAccepted(t *testing.T) {
	var got string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Path

		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	for _, base := range []string{
		srv.URL + eumaeusapi.APIPrefix,
		srv.URL + eumaeusapi.APIPrefix + "/",
		srv.URL + "/",
	} {
		client, err := eumaeusapi.New(eumaeusapi.Config{BaseURL: base})
		if err != nil {
			t.Fatalf("New(%q): %v", base, err)
		}

		if err := client.Do(context.Background(), http.MethodGet, "/machines/me", nil, nil); err != nil {
			t.Fatalf("Do(%q): %v", base, err)
		}

		if want := eumaeusapi.APIPrefix + "/machines/me"; got != want {
			t.Errorf("base %q: got %q, want %q", base, got, want)
		}
	}
}

// TestPlainHTTPIsRefused pins the one rule that cannot be relaxed quietly:
// this client carries a token that unlocks somebody's backups.
func TestPlainHTTPIsRefused(t *testing.T) {
	_, err := eumaeusapi.New(eumaeusapi.Config{BaseURL: "http://eumaeus.example.org"})

	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Errorf("got %v, want a refusal mentioning https", err)
	}
}

// TestABadRequestIsDistinguished. A 400 is the one failure that is this
// program's own fault, and the caller has to be able to tell it from a server
// having a bad day — one is dropped, the other is retried.
func TestABadRequestIsDistinguished(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"that enrollment code has expired","field":"code"}`))
	}))
	defer srv.Close()

	client, err := eumaeusapi.New(eumaeusapi.Config{BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}

	err = client.Do(context.Background(), http.MethodPost, "/runs", struct{}{}, nil)

	if !errors.Is(err, eumaeusapi.ErrBadRequest) {
		t.Fatalf("got %v, want ErrBadRequest", err)
	}

	// The server's sentence is kept: it names the field, and that is the whole
	// value of a 400 to whoever reads the log.
	if !strings.Contains(err.Error(), "that enrollment code has expired") {
		t.Errorf("the explanation was lost: %v", err)
	}
}

// TestAForbiddenIsNotAnUnauthorised is the regression this split was made
// for. Both used to answer ErrUnauthorised, which this client treats as
// terminal de-enrolment — so the first 403 the server ever sends for an
// ordinary reason ("fresh buckets are not on offer for this fleet") would
// have told an owner their machine had been cut off.
func TestAForbiddenIsNotAnUnauthorised(t *testing.T) {
	for _, tt := range []struct {
		status int
		want   error
		wrong  error
	}{
		{http.StatusUnauthorized, eumaeusapi.ErrUnauthorised, eumaeusapi.ErrForbidden},
		{http.StatusForbidden, eumaeusapi.ErrForbidden, eumaeusapi.ErrUnauthorised},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tt.status)
		}))

		client, err := eumaeusapi.New(eumaeusapi.Config{BaseURL: srv.URL, Token: "mt_1"})
		if err != nil {
			t.Fatal(err)
		}

		err = client.Do(context.Background(), http.MethodPost,
			"/machines/me/rotation-request", struct{}{}, nil)

		if !errors.Is(err, tt.want) {
			t.Errorf("%d: got %v, want %v", tt.status, err, tt.want)
		}

		if errors.Is(err, tt.wrong) {
			t.Errorf("%d: %v also matched %v; the two must stay apart", tt.status, err, tt.wrong)
		}

		srv.Close()
	}
}

// TestABadRequestCarriesTheServersSentence covers the other half of the
// bargain in §3.1 of eumaeus's reply: the server took its package name off
// the front of that sentence, and it is worth nothing if this client hands
// the whole JSON body to somebody standing at a machine.
func TestABadRequestCarriesTheServersSentence(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"a claim must say what the machine is called","field":"hostname"}`))
	}))
	defer srv.Close()

	client, err := eumaeusapi.New(eumaeusapi.Config{BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}

	err = client.Do(context.Background(), http.MethodPost, "/enrollments/claim", struct{}{}, nil)

	var bad *eumaeusapi.BadRequest
	if !errors.As(err, &bad) {
		t.Fatalf("got %v, want a *BadRequest", err)
	}

	if bad.Message != "a claim must say what the machine is called" {
		t.Errorf("message: got %q", bad.Message)
	}

	if bad.Field != "hostname" {
		t.Errorf("field: got %q", bad.Field)
	}

	// What the installer reads. Not a JSON fragment, and not behind a prefix
	// naming a package of ours or of theirs.
	if got, want := err.Error(),
		"a claim must say what the machine is called (hostname)"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	if !errors.Is(err, eumaeusapi.ErrBadRequest) {
		t.Error("the class was lost; callers that only test errors.Is would stop matching")
	}
}

// TestABadRequestThatIsNotTheAgreedShapeKeepsWhatArrived. A proxy in front of
// Eumaeus answers HTML, and a client that decoded only the documented shape
// would report an empty error and leave whoever is debugging it with nothing.
func TestABadRequestThatIsNotTheAgreedShapeKeepsWhatArrived(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("<html><body>400 Bad Request</body></html>\n"))
	}))
	defer srv.Close()

	client, err := eumaeusapi.New(eumaeusapi.Config{BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}

	err = client.Do(context.Background(), http.MethodPost, "/runs", struct{}{}, nil)

	if !errors.Is(err, eumaeusapi.ErrBadRequest) {
		t.Fatalf("got %v, want ErrBadRequest", err)
	}

	for _, want := range []string{"400 Bad Request", "/runs"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q is missing from %v", want, err)
		}
	}
}
