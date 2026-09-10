package eumaeusfleet_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/sion-backup/business/domain/fleet/fleetbus"
	"github.com/jroedel/sion-backup/business/domain/fleet/sources/eumaeusfleet"
	"github.com/jroedel/sion-backup/foundation/eumaeusapi"
)

func event() fleetbus.Event {
	finished := time.Date(2026, 9, 9, 13, 19, 48, 0, time.UTC)

	return fleetbus.Event{
		NodeID:          "office-laptop-1",
		RunUUID:         "0192f3a1-7c4e-7b21-9f10-3c2d5e8a41b7",
		RepositoryURL:   "s3:https://s3.example.invalid/bucket",
		Seeding:         true,
		Phase:           fleetbus.PhaseFinished,
		StartedAt:       time.Date(2026, 9, 9, 13, 4, 22, 0, time.UTC),
		FinishedAt:      &finished,
		Outcome:         "success",
		Message:         "4211 files, 88.0 MiB added",
		SnapshotID:      "a1b2c3d4",
		FilesProcessed:  4211,
		UnreadableFiles: 0,
		Verified:        true,
		Agent:           "v1.2.0",
		OS:              "linux/amd64",
	}
}

// TestTheWireShapeMatchesTheSpecification pins §7 of docs/eumaeus-api.md
// field by field. It is here because the client and the server are two
// repositories: nothing but a test on each side stops a rename in one of them
// from turning into a silent 400 on thirty machines.
func TestTheWireShapeMatchesTheSpecification(t *testing.T) {
	var (
		body map[string]any
		path string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path

		raw, _ := io.ReadAll(r.Body)

		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("the server could not parse the event: %v", err)
		}

		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	client, err := eumaeusapi.New(eumaeusapi.Config{BaseURL: srv.URL, Token: "mt_1"})
	if err != nil {
		t.Fatal(err)
	}

	if err := eumaeusfleet.NewReporter(client).Report(context.Background(), event()); err != nil {
		t.Fatalf("Report: %v", err)
	}

	if want := eumaeusapi.APIPrefix + "/runs"; path != want {
		t.Errorf("posted to %q, want %q", path, want)
	}

	// The machine is named by its token. A body that could name a machine is a
	// body that could name somebody else's.
	for _, gone := range []string{"node_id", "run_id", "repository"} {
		if _, present := body[gone]; present {
			t.Errorf("the event still carries %q, which the server does not read", gone)
		}
	}

	want := map[string]any{
		"run_uuid":        "0192f3a1-7c4e-7b21-9f10-3c2d5e8a41b7",
		"repository_url":  "s3:https://s3.example.invalid/bucket",
		"seeding":         true,
		"phase":           "finished",
		"outcome":         "success",
		"snapshot_id":     "a1b2c3d4",
		"files_processed": float64(4211),
		"verified":        true,
		"agent":           "v1.2.0",
		"os":              "linux/amd64",
	}

	for k, v := range want {
		if body[k] != v {
			t.Errorf("%s = %v, want %v", k, body[k], v)
		}
	}

	// Both timestamps are RFC 3339 with an offset, which is what the server
	// stores and what lets a fleet in two time zones report in each one's own.
	for _, k := range []string{"started_at", "finished_at"} {
		s, ok := body[k].(string)
		if !ok {
			t.Errorf("%s is missing", k)

			continue
		}

		if _, err := time.Parse(time.RFC3339, s); err != nil {
			t.Errorf("%s = %q, which is not RFC 3339: %v", k, s, err)
		}
	}
}

// TestABadRequestBecomesARejection is the other half of the bargain in §7: the
// server promises 400 only for a body that will never parse, and the client
// promises to stop resending it. fleetbus.Flush acts on ErrRejected, and this
// is where an HTTP status becomes one.
func TestABadRequestBecomesARejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"a run event must carry a run_uuid"}`))
	}))
	defer srv.Close()

	client, err := eumaeusapi.New(eumaeusapi.Config{BaseURL: srv.URL, Token: "mt_1"})
	if err != nil {
		t.Fatal(err)
	}

	err = eumaeusfleet.NewReporter(client).Report(context.Background(), event())

	if !errors.Is(err, fleetbus.ErrRejected) {
		t.Errorf("got %v, want a fleetbus.ErrRejected", err)
	}

	// The server's sentence survives into the log, because "which field" is
	// the whole value of a 400 to whoever has to fix the client.
	if err == nil || !strings.Contains(err.Error(), "must carry a run_uuid") {
		t.Errorf("the server's explanation was lost: %v", err)
	}
}

// TestAServerHavingABadDayIsNotARejection. A 500 is retried; only a 400 is
// dropped.
func TestAServerHavingABadDayIsNotARejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	client, err := eumaeusapi.New(eumaeusapi.Config{BaseURL: srv.URL, Token: "mt_1"})
	if err != nil {
		t.Fatal(err)
	}

	err = eumaeusfleet.NewReporter(client).Report(context.Background(), event())

	if err == nil {
		t.Fatal("a 500 was reported as success")
	}

	if errors.Is(err, fleetbus.ErrRejected) {
		t.Error("a 500 was treated as a permanent rejection; the event would be discarded")
	}
}
