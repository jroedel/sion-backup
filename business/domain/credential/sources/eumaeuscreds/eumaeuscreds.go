// Package eumaeuscreds fetches a machine's credentials from Eumaeus.
//
//	GET /api/backup/v1/machines/me/credentials
//	Authorization: Bearer <machine token>
//
// Called once per backup. Nothing is cached — see business/domain/credential
// for why, and foundation/token for what remains on the disk instead.
//
// # What the server owes this call
//
// An atomic answer: the credentials and the repository they open, together. A
// response that gave one without the other would let a machine polling across
// a bucket rotation pair the new password with the old bucket.
//
// And never a partial record. A 200 carrying an empty `restic_password` would
// make a machine believe it is enrolled against a repository it cannot open,
// and the failure would surface as an unverified backup at 1am rather than as
// an error here.
package eumaeuscreds

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/jroedel/sion-backup/business/domain/credential/credentialbus"
	"github.com/jroedel/sion-backup/foundation/eumaeusapi"
)

// path is relative to eumaeusapi.APIPrefix.
const path = "/machines/me/credentials"

// payload is the wire form.
//
// Strings, because JSON has no other option — which means these three secrets
// exist as strings inside this process for as long as the garbage collector
// keeps them, and cannot be wiped. That is a real if minor cost of using an
// HTTP API, and it is confined to this file: everything above holds []byte.
type payload struct {
	Version    int `json:"credentials_version"`
	Repository struct {
		URL string `json:"url"`
	} `json:"repository"`
	Credentials struct {
		ResticPassword string `json:"restic_password"`
		Machine        struct {
			AccessKeyID     string `json:"access_key_id"`
			SecretAccessKey string `json:"secret_access_key"`
		} `json:"machine"`
	} `json:"credentials"`
}

// Source reads credentials from Eumaeus.
type Source struct {
	client *eumaeusapi.Client
}

// NewSource constructs one over an API client.
func NewSource(client *eumaeusapi.Client) *Source {
	return &Source{client: client}
}

// Fetch returns the credentials for the repository this machine should be
// using now.
func (s *Source) Fetch(ctx context.Context) (credentialbus.Set, error) {
	var got payload

	err := s.client.Do(ctx, http.MethodGet, path, nil, &got)

	switch {
	case errors.Is(err, eumaeusapi.ErrUnauthorised):
		// Distinguished so the daemon stops retrying and the status page can
		// say something true. A revoked token will still be revoked in an hour.
		//
		// 401 alone. This used to accept a 403 here as well, on the argument
		// that a machine which may not read its own credentials cannot back up
		// whichever status says so. That argument was sound and the branch was
		// still dead: Eumaeus answers 403 in exactly one place in the whole
		// API — the rotation request, when fresh buckets are switched off —
		// and never on this endpoint, which they hold with a test
		// (jroedel/eumaeus#144). A branch that cannot run is a branch nobody
		// can check, and it was copied into two more sources before anybody
		// asked whether the status it guards against ever arrives.
		return credentialbus.Set{}, &credentialbus.Unauthorised{Err: err}

	case errors.Is(err, eumaeusapi.ErrNotFound):
		return credentialbus.Set{}, fmt.Errorf(
			"eumaeuscreds: Eumaeus has no record of this machine: %w", credentialbus.ErrNotEnrolled)

	case err != nil:
		return credentialbus.Set{}, fmt.Errorf("eumaeuscreds: fetching credentials: %w", err)
	}

	return credentialbus.Set{
		RepositoryURL: got.Repository.URL,
		Version:       got.Version,
		Credentials: credentialbus.Credentials{
			AccessKeyID:     []byte(got.Credentials.Machine.AccessKeyID),
			SecretAccessKey: []byte(got.Credentials.Machine.SecretAccessKey),
			ResticPassword:  []byte(got.Credentials.ResticPassword),
		},
	}, nil
}

var _ credentialbus.Source = (*Source)(nil)
