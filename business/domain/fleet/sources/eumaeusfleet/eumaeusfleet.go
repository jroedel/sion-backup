// Package eumaeusfleet reports runs to a Eumaeus installation.
//
// # The endpoint
//
//	POST /api/backup/v1/runs
//	Authorization: Bearer <machine token>
//	Content-Type: application/json
//
//	{ "node_id": "...", "run_id": 41, "phase": "finished", ... }
//
// One endpoint for both phases rather than two, because the server's job is to
// hold the latest state of each machine and a phase field says which kind of
// update this is. A separate /start endpoint would double the surface for no
// gain.
//
// # Idempotency
//
// (node_id, run_id, phase) identifies an event. A machine that sends a
// finished event, fails to record that it did, and sends it again on the next
// flush must not produce two rows on the dashboard — see fleetbus.Flush, which
// deliberately accepts that risk on the client side because the server can
// resolve it and the client cannot.
package eumaeusfleet

import (
	"context"
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
func (r *Reporter) Report(ctx context.Context, e fleetbus.Event) error {
	if err := r.client.Do(ctx, http.MethodPost, path, e, nil); err != nil {
		return fmt.Errorf("eumaeusfleet: reporting run %d: %w", e.RunID, err)
	}

	return nil
}

var _ fleetbus.Reporter = (*Reporter)(nil)
