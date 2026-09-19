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
	NodeID string `json:"node_id"`

	Owner struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	} `json:"owner"`

	Repository struct {
		URL       string    `json:"url"`
		State     string    `json:"state"`
		Adopted   bool      `json:"adopted"`
		CreatedAt time.Time `json:"created_at"`
	} `json:"repository"`

	// Offer is a pointer because absence is the answer for almost every
	// machine almost always, and because the absence has to survive decoding:
	// an offer decoded into a value type would arrive as a bucket with an
	// empty URL, which is one careless test away from being accepted.
	Offer *struct {
		URL       string    `json:"url"`
		Provider  string    `json:"provider"`
		Region    string    `json:"region"`
		Bucket    string    `json:"bucket"`
		OfferedAt time.Time `json:"offered_at"`
		Adopted   bool      `json:"adopted"`
	} `json:"offer"`

	Card struct {
		State string `json:"state"`

		// IssuedAt is null on the wire whenever State is "superseded", so it
		// is a pointer here for honesty rather than for the client's sake —
		// nothing reads it as a presence test. See machinebus.Card.IssuedAt.
		IssuedAt *time.Time `json:"issued_at"`
	} `json:"card"`

	// API is absent from a deployment older than the field, and a nil Serves
	// is carried through as nil rather than flattened to an empty slice:
	// machinebus.State draws the whole distinction between "said nothing" and
	// "said nothing is served" from that.
	API struct {
		Serves []string `json:"serves"`
	} `json:"api"`
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
	case errors.Is(err, eumaeusapi.ErrUnauthorised):
		// 401 and only 401. There is no 403 anywhere in this API to branch on:
		// the rotation request was the last route that answered one, and the
		// fleet-wide switch it refused on has been withdrawn. Eumaeus holds
		// that with a test that sweeps every route (jroedel/eumaeus#174), and
		// this client keeps eumaeusapi.ErrForbidden and the branch on it
		// anyway, for a 403 arriving from an older deployment or a proxy.
		return machinebus.State{}, fmt.Errorf(
			"eumaeusmachine: %w: %w", machinebus.ErrNotEnrolled, err)

	case errors.Is(err, eumaeusapi.ErrNotFound):
		// Documented, and it does NOT mean what a 404 usually means. Per the
		// specification: the token is good and the machine has no repository.
		// An enrolled machine always has one, so this is a repository removed
		// from underneath a machine — something to be fixed on the server, and
		// not something this machine can do anything about but report.
		//
		// It is deliberately not read as "this server is too old to answer".
		// Retirement and revocation refuse at authentication with 401
		// (jroedel/eumaeus#116), so a 404 here is never de-enrolment either.
		return machinebus.State{}, fmt.Errorf(
			"eumaeusmachine: %w", machinebus.ErrNoRepository)

	case err != nil:
		return machinebus.State{}, fmt.Errorf("eumaeusmachine: reading machine state: %w", err)
	}

	state := machinebus.State{
		NodeID:              got.NodeID,
		OwnerName:           got.Owner.Name,
		OwnerEmail:          got.Owner.Email,
		RepositoryURL:       got.Repository.URL,
		RepositoryState:     got.Repository.State,
		RepositoryAdopted:   got.Repository.Adopted,
		RepositoryCreatedAt: got.Repository.CreatedAt,
		Routes:              got.API.Serves,
		Card:                machinebus.Card{State: got.Card.State},
	}

	if got.Card.IssuedAt != nil {
		state.Card.IssuedAt = *got.Card.IssuedAt
	}

	if o := got.Offer; o != nil {
		state.Offer = &machinebus.Offer{
			URL:       o.URL,
			Provider:  o.Provider,
			Region:    o.Region,
			Bucket:    o.Bucket,
			OfferedAt: o.OfferedAt,
			Adopted:   o.Adopted,
		}
	}

	return state, nil
}

var _ machinebus.Source = (*Source)(nil)
