// Package eumaeusdiag sends diagnostic reports to Eumaeus.
//
//	POST /api/backup/v1/diagnostics
//
// Authenticated when this machine has a token and anonymous when it does not,
// which is the whole point: the reports most worth having come from an install
// that never got as far as being enrolled. See jroedel/eumaeus#113 for the
// endpoint this expects and the rules it is written against.
//
// # The endpoint does not exist yet
//
// Until it does, every send comes back 404 and the reports stay on disk, which
// is the correct behaviour and needs no flag: diagfile caps and ages them out,
// and the day the server grows the endpoint the backlog goes with the next
// run.
package eumaeusdiag

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/jroedel/sion-backup/business/domain/diag/diagbus"
	"github.com/jroedel/sion-backup/foundation/eumaeusapi"
)

// path is where reports are posted, relative to eumaeusapi.APIPrefix.
const path = "/diagnostics"

// Sink posts reports.
type Sink struct {
	client *eumaeusapi.Client
}

// NewSink constructs one over an API client, which may be anonymous.
func NewSink(client *eumaeusapi.Client) *Sink {
	return &Sink{client: client}
}

// Send posts one report.
func (s *Sink) Send(ctx context.Context, r diagbus.Report) error {
	err := s.client.Do(ctx, http.MethodPost, path, r, nil)

	switch {
	case errors.Is(err, eumaeusapi.ErrBadRequest):
		// Permanent. Dropped by diagbus rather than retried forever.
		return fmt.Errorf("eumaeusdiag: %w: %w", diagbus.ErrRejected, err)

	case err != nil:
		return fmt.Errorf("eumaeusdiag: sending a %s report: %w", r.Kind, err)
	}

	return nil
}

var _ diagbus.Sink = (*Sink)(nil)
