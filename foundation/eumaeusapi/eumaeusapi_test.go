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
