package eumaeuscreds_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jroedel/sion-backup/business/domain/credential/credentialbus"
	"github.com/jroedel/sion-backup/business/domain/credential/sources/eumaeuscreds"
	"github.com/jroedel/sion-backup/foundation/eumaeusapi"
)

func client(t *testing.T, h http.HandlerFunc) *eumaeusapi.Client {
	t.Helper()

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	c, err := eumaeusapi.New(eumaeusapi.Config{BaseURL: srv.URL, Token: "mt_1"})
	if err != nil {
		t.Fatal(err)
	}

	return c
}

// TestATokenRefusedOnThisEndpointIsDeEnrolment, whichever of the two statuses
// says so.
//
// eumaeusapi now keeps 401 and 403 apart, because everywhere else they mean
// opposite things. Here they do not: a machine that may not read its own
// credentials cannot back up, and telling its owner the server is having
// trouble would be a lie that repeated hourly. So this is the one place that
// deliberately puts them back together, and it is worth a test saying so —
// the next person to see the pair here will think it is the old bug.
func TestATokenRefusedOnThisEndpointIsDeEnrolment(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		source := eumaeuscreds.NewSource(client(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		}))

		_, err := source.Fetch(context.Background())

		var revoked *credentialbus.Unauthorised
		if !errors.As(err, &revoked) {
			t.Errorf("%d: got %v, want credentialbus.Unauthorised", status, err)
		}
	}
}

// TestAMachineEumaeusHasNoRecordOfIsNotEnrolled keeps the 404 an ordinary
// answer rather than a failure — it is what a machine whose record was
// removed sees, and it must not read as a broken network.
func TestAMachineEumaeusHasNoRecordOfIsNotEnrolled(t *testing.T) {
	source := eumaeuscreds.NewSource(client(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))

	if _, err := source.Fetch(context.Background()); !errors.Is(err, credentialbus.ErrNotEnrolled) {
		t.Errorf("got %v, want ErrNotEnrolled", err)
	}
}

// TestARefusedClaimReadsAsOneSentence is what somebody standing at a machine
// sees when the claim is refused. `main` prints "sion-backup: %v", so this
// string is the whole of their evening's diagnosis.
//
// It used to be the server's sentence at the end of a JSON fragment behind
// two package prefixes. Eumaeus removed the one that was theirs; this pins
// that the client does not add ours back.
func TestARefusedClaimReadsAsOneSentence(t *testing.T) {
	c := client(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"a claim must say what the machine is called","field":"hostname"}`))
	})

	_, err := eumaeuscreds.Claim(context.Background(), c, "K4TP-9QX2", eumaeuscreds.Machine{})
	if err == nil {
		t.Fatal("a refused claim returned no error")
	}

	if got, want := err.Error(),
		"a claim must say what the machine is called (hostname)"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	if strings.Contains(err.Error(), "eumaeus") || strings.Contains(err.Error(), "{") {
		t.Errorf("a package name or a JSON body reached the user: %q", err)
	}
}

// TestAnExpiredCodeAndAUsedCodeAreDifferentSentences. The two are the whole
// of what an installer needs to know: fetch another code, or find out who
// already used this one.
func TestAnExpiredCodeAndAUsedCodeAreDifferentSentences(t *testing.T) {
	for _, tt := range []struct {
		status int
		want   error
	}{
		{http.StatusNotFound, eumaeuscreds.ErrCodeUnknown},
		{http.StatusConflict, eumaeuscreds.ErrCodeUsed},
	} {
		c := client(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tt.status)
		})

		_, err := eumaeuscreds.Claim(context.Background(), c, "K4TP-9QX2", eumaeuscreds.Machine{})
		if !errors.Is(err, tt.want) {
			t.Errorf("%d: got %v, want %v", tt.status, err, tt.want)
		}
	}
}
