// Package eumaeusfleet reports runs to a Eumaeus installation.
//
// # The endpoint
//
//	POST /api/backup/v1/runs
//	Authorization: Bearer <machine token>
//	Content-Type: application/json
//
//	{ "run_uuid": "0192f3a1-...", "phase": "finished", ... }
//
// The machine is identified by its token, never by a field in the body: a
// client that could name a machine could name somebody else's.
//
// One endpoint for both phases rather than two, because the server's job is to
// hold the latest state of each machine and a phase field says which kind of
// update this is. A separate /start endpoint would double the surface for no
// gain.
//
// # Idempotency
//
// (machine, run_uuid, phase) identifies an event. A machine that sends a
// finished event, fails to record that it did, and sends it again on the next
// flush must not produce two rows on the dashboard — see fleetbus.Flush, which
// deliberately accepts that risk on the client side because the server can
// resolve it and the client cannot.
package eumaeusfleet

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/jroedel/sion-backup/business/domain/fleet/fleetbus"
	"github.com/jroedel/sion-backup/foundation/eumaeusapi"
)

// path is where run events are posted, relative to eumaeusapi.APIPrefix.
const path = "/runs"

// Reporter sends events to Eumaeus.
type Reporter struct {
	client *eumaeusapi.Client
}

// NewReporter constructs one over an API client.
func NewReporter(client *eumaeusapi.Client) *Reporter {
	return &Reporter{client: client}
}

// Report posts one event.
//
// A 400 is translated rather than passed through, because fleetbus decides
// what to do about it and must not have to know about HTTP to do so. The
// server returns one only for a body it could not parse — never for an event
// it merely finds unwelcome — which is what makes discarding it safe.
func (r *Reporter) Report(ctx context.Context, e fleetbus.Event) error {
	err := r.client.Do(ctx, http.MethodPost, path, e, nil)

	switch {
	case errors.Is(err, eumaeusapi.ErrBadRequest):
		return fmt.Errorf("eumaeusfleet: reporting run %s: %w: %w",
			e.RunUUID, fleetbus.ErrRejected, err)

	case err != nil:
		return fmt.Errorf("eumaeusfleet: reporting run %s: %w", e.RunUUID, err)
	}

	return nil
}

var _ fleetbus.Reporter = (*Reporter)(nil)
