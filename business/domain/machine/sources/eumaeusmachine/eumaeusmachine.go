// Package eumaeusmachine reads this machine's own state from Eumaeus.
//
// # The endpoint
//
//	GET /api/backup/v1/machines/me
//	Authorization: Bearer <machine token>
//
//	{ "node_id": "macbook-air", "repository": { "url": "s3:https://…" }, … }
//
// The machine is identified by its token and never by a field in the request,
// which is what makes "me" safe to say.
//
// Only a few fields are decoded. The response carries the alerting state, the
// owner, the card, the agent version and more, and every one of those is
// somebody else's job today — see machinebus.State for why the client does not
// mirror the server's model wholesale.
package eumaeusmachine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jroedel/sion-backup/business/domain/machine/machinebus"
	"github.com/jroedel/sion-backup/foundation/eumaeusapi"
)

// path is relative to eumaeusapi.APIPrefix.
const path = "/machines/me"

// payload is the wire form, cut down to what is used.
type payload struct {
	NodeID     string `json:"node_id"`
	Repository struct {
		URL       string    `json:"url"`
		CreatedAt time.Time `json:"created_at"`
	} `json:"repository"`
}

// Source reads machine state from Eumaeus.
type Source struct {
	client *eumaeusapi.Client
}

// NewSource constructs one over an API client.
func NewSource(client *eumaeusapi.Client) *Source {
	return &Source{client: client}
}

// State returns what the server says about this machine.
func (s *Source) State(ctx context.Context) (machinebus.State, error) {
	var got payload

	err := s.client.Do(ctx, http.MethodGet, path, nil, &got)

	switch {
	case errors.Is(err, eumaeusapi.ErrUnauthorised), errors.Is(err, eumaeusapi.ErrForbidden):
		// The reading eumaeuscreds gives the same pair on the credentials
		// endpoint: a machine that may not read its own record is a machine
		// that is no longer in the fleet, whichever status the server chose.
		return machinebus.State{}, fmt.Errorf(
			"eumaeusmachine: %w: %w", machinebus.ErrNotEnrolled, err)

	case errors.Is(err, eumaeusapi.ErrNotFound):
		// Ambiguous on this endpoint in a way it is not elsewhere: a Eumaeus
		// too old to serve it and a Eumaeus that has forgotten this machine
		// both answer 404. Read as the server's shortcoming rather than as
		// de-enrolment, because the caller is a repair that must not run and
		// the cost of being wrong in the other direction — a machine deciding
		// it has been retired because its server is a version behind — is far
		// worse than a repair that does not happen.
		return machinebus.State{}, fmt.Errorf(
			"eumaeusmachine: this Eumaeus does not report machine state, or does not "+
				"know this machine: %w", machinebus.ErrUnsupported)

	case err != nil:
		return machinebus.State{}, fmt.Errorf("eumaeusmachine: reading machine state: %w", err)
	}

	return machinebus.State{
		NodeID:              got.NodeID,
		RepositoryURL:       got.Repository.URL,
		RepositoryCreatedAt: got.Repository.CreatedAt,
	}, nil
}

var _ machinebus.Source = (*Source)(nil)
