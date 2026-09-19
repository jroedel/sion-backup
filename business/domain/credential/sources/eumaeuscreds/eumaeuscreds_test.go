package eumaeuscreds_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// TestATokenRefusedOnThisEndpointIsDeEnrolment.
//
// 401 and only 401. This used to accept a 403 here too, on the argument that a
// machine which may not read its own credentials cannot back up whichever
// status says so — sound, and describing something that never happens.
// There is now no 403 anywhere in this API at all — the rotation request was
// the last route that answered one, and the fleet-wide switch it refused on
// has been withdrawn. Eumaeus sweeps every route with a test to keep it that
// way (jroedel/eumaeus#174).
//
// The pair is worth remembering rather than just deleting, because it was
// copied into two later sources before anybody asked whether the status it
// guarded against ever arrives. A branch that cannot run is a branch nobody
// can check.
func TestATokenRefusedOnThisEndpointIsDeEnrolment(t *testing.T) {
	source := eumaeuscreds.NewSource(client(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))

	_, err := source.Fetch(context.Background())

	var revoked *credentialbus.Unauthorised
	if !errors.As(err, &revoked) {
		t.Errorf("got %v, want credentialbus.Unauthorised", err)
	}
}

// TestA403IsNotReadAsDeEnrolment.
//
// It does not arrive on this endpoint — it does not arrive anywhere in that
// API any more — and if it ever did, from an older deployment or a proxy, it
// would mean something else entirely. Reporting a machine as cut off because
// a fleet setting refused something is the bug this pair was untangled to
// prevent, and the untangling is only finished if the reading cannot come
// back here by habit.
func TestA403IsNotReadAsDeEnrolment(t *testing.T) {
	source := eumaeuscreds.NewSource(client(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))

	_, err := source.Fetch(context.Background())

	var revoked *credentialbus.Unauthorised
	if errors.As(err, &revoked) {
		t.Error("a 403 is reported as de-enrolment")
	}

	if err == nil {
		t.Error("a 403 was not reported at all")
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

// TestTheClaimTellsTheServerWhichBucketTheMachineWritesTo.
//
// The block is what lets Eumaeus refuse a code that would enrol this machine
// against a bucket it has never written to, while the code is still unspent.
// A claim that carried the plan and not this would leave that check to the
// client, which can only make it after the code has been consumed.
func TestTheClaimTellsTheServerWhichBucketTheMachineWritesTo(t *testing.T) {
	var got map[string]any

	c := client(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)

		w.Write([]byte(`{"machine_token":"mt_1","node_id":"dell3-backup",
			"repository":{"url":"s3:https://s3.example.com/bucket123","adopted":true},
			"credentials":{"restic_password":"pw"}}`))
	})

	oldest := time.Date(2019, 3, 1, 22, 4, 0, 0, time.UTC)

	enrolled, err := eumaeuscreds.Claim(t.Context(), c, "K4TP-9QX2", eumaeuscreds.Machine{
		Hostname: "desktop",
		Legacy: &eumaeuscreds.LegacyInstall{
			RepositoryURL:  "s3:https://s3.example.com/bucket123",
			Snapshots:      1412,
			OldestSnapshot: oldest,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	legacy, ok := got["legacy"].(map[string]any)
	if !ok {
		t.Fatalf("the claim carried no legacy block: %v", got)
	}

	if legacy["repository_url"] != "s3:https://s3.example.com/bucket123" {
		t.Errorf("repository_url = %v", legacy["repository_url"])
	}

	if legacy["snapshots"] != float64(1412) {
		t.Errorf("snapshots = %v", legacy["snapshots"])
	}

	if legacy["oldest_snapshot"] != "2019-03-01T22:04:00Z" {
		t.Errorf("oldest_snapshot = %v", legacy["oldest_snapshot"])
	}

	if !enrolled.RepositoryAdopted {
		t.Error("the adopted flag did not survive the claim")
	}
}

// TestAClaimWithNothingToSayCarriesNoLegacyBlock. Every machine in the field
// sends a claim without one, and a block full of zeroes would read on the
// server as a machine writing nowhere with an empty repository.
func TestAClaimWithNothingToSayCarriesNoLegacyBlock(t *testing.T) {
	var got map[string]any

	c := client(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)

		w.Write([]byte(`{"machine_token":"mt_1","node_id":"n",
			"repository":{"url":"s3:https://s3.example.com/b"},
			"credentials":{"restic_password":"pw"}}`))
	})

	if _, err := eumaeuscreds.Claim(t.Context(), c, "K4TP-9QX2",
		eumaeuscreds.Machine{Hostname: "desktop"}); err != nil {
		t.Fatal(err)
	}

	if _, present := got["legacy"]; present {
		t.Errorf("a claim with no legacy install still sent one: %v", got["legacy"])
	}
}

// TestARefusedRepositoryIsToldApartFromEveryOtherRefusal.
//
// The code is NOT consumed by this one, which is the fact the person standing
// at the machine needs: the bucket can be adopted properly and the same code
// presented again. A client that let it read as an ordinary failure would send
// them for a second code, or let them conclude the machine is fine to enrol
// against the new bucket.
func TestARefusedRepositoryIsToldApartFromEveryOtherRefusal(t *testing.T) {
	const sentence = "this machine backs up to s3:…/bucket123, and that code enrols it " +
		"against s3:…/desktop-2026-09. The code has not been used"

	c := client(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		w.Write([]byte(`{"error":"` + sentence + `","field":"legacy.repository_url"}`))
	})

	_, err := eumaeuscreds.Claim(t.Context(), c, "K4TP-9QX2", eumaeuscreds.Machine{
		Legacy: &eumaeuscreds.LegacyInstall{RepositoryURL: "s3:…/bucket123"},
	})

	if !eumaeuscreds.RepositoryMismatch(err) {
		t.Fatalf("RepositoryMismatch = false for %v", err)
	}

	// The sentence names both repositories and is meant to be read as it
	// stands, with no prefix of ours in front of it.
	if !strings.HasPrefix(err.Error(), "this machine backs up to") {
		t.Errorf("the server's sentence was wrapped: %q", err.Error())
	}
}

// TestTheOtherReasonForA422IsNotAMismatch. §4 answers 422 for a machine whose
// repository has not been provisioned yet, and that one is not fixed by
// adopting a bucket.
func TestTheOtherReasonForA422IsNotAMismatch(t *testing.T) {
	c := client(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		w.Write([]byte(`{"error":"that machine has no repository provisioned yet"}`))
	})

	_, err := eumaeuscreds.Claim(t.Context(), c, "K4TP-9QX2", eumaeuscreds.Machine{})

	if eumaeuscreds.RepositoryMismatch(err) {
		t.Errorf("RepositoryMismatch = true for %v", err)
	}
}

// TestExpectEmptyIsOffUnlessTheServerSaysOtherwise.
//
// The only field in this API that permits a client to create a repository, and
// the polarity is the contract: it is omitted when false, so absence has to
// mean exactly what it meant before the field existed. A client that read a
// missing key as anything but "create nothing" would, against a server that
// predates the field, silently write an empty repository over a real one.
func TestExpectEmptyIsOffUnlessTheServerSaysOtherwise(t *testing.T) {
	cases := []struct {
		name  string
		repo  string
		want  bool
		state string
	}{
		{
			name: "a server that predates the field",
			repo: `{"url": "s3:https://s3.example/b"}`,
		},
		{
			name:  "an ordinary run",
			repo:  `{"url": "s3:https://s3.example/b", "state": "active"}`,
			state: "active",
		},
		{
			name:  "cutting over, but something has already landed",
			repo:  `{"url": "s3:https://s3.example/b", "state": "cutting-over"}`,
			state: "cutting-over",
		},
		{
			name:  "the one moment a client may create a repository",
			repo:  `{"url": "s3:https://s3.example/b", "state": "cutting-over", "expect_empty": true}`,
			state: "cutting-over",
			want:  true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := eumaeuscreds.NewSource(client(t, func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{
				  "credentials_version": 3,
				  "repository": %s,
				  "credentials": {
				    "restic_password": "p",
				    "machine": {"access_key_id": "A", "secret_access_key": "S"},
				    "restore": {"access_key_id": "R", "secret_access_key": "T"}
				  }
				}`, c.repo)
			}))

			set, err := s.Fetch(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer set.Wipe()

			if set.ExpectEmpty != c.want {
				t.Errorf("ExpectEmpty = %v, want %v", set.ExpectEmpty, c.want)
			}

			if set.RepositoryState != c.state {
				t.Errorf("RepositoryState = %q, want %q", set.RepositoryState, c.state)
			}
		})
	}
}

// TestTheRestoreKeyComesBackForTheCard.
//
// The card carries the read-only pair and never the machine's own, so that a
// page found in a filing cabinet exposes somebody's data and cannot destroy
// it. Nothing but `sion-backup card` reads it, and it is wiped with the rest.
func TestTheRestoreKeyComesBackForTheCard(t *testing.T) {
	s := eumaeuscreds.NewSource(client(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{
		  "credentials_version": 3,
		  "repository": {"url": "s3:https://s3.example/b", "state": "active"},
		  "credentials": {
		    "restic_password": "p",
		    "machine": {"access_key_id": "A", "secret_access_key": "S"},
		    "restore": {"access_key_id": "R", "secret_access_key": "T"}
		  }
		}`))
	}))

	set, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if !set.Restore.Complete() {
		t.Fatal("the read-only pair did not come back")
	}

	if string(set.Restore.AccessKeyID) != "R" {
		t.Errorf("the card would carry %q", set.Restore.AccessKeyID)
	}

	// And wiping the set wipes it, like every other secret here.
	set.Wipe()

	if set.Restore.Complete() && string(set.Restore.AccessKeyID) == "R" {
		t.Error("the read-only key survived the wipe")
	}
}
