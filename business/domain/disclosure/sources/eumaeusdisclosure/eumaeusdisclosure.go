// Package eumaeusdisclosure reads a machine's own credential-disclosure trail
// from Eumaeus.
//
// # The endpoint
//
//	GET /api/backup/v1/machines/me/disclosures?limit=200
//	Authorization: Bearer <machine token>
//
//	{ "count": 412, "head": "9f2c...", "entries": [ ... ] }
//
// Served to the machine because it is meant for the machine's owner: they have
// no account on the server, and the status page this machine serves on
// loopback is the only surface they have. See disclosurebus for what is done
// with it and, more importantly, for what it is not claimed to prove.
//
// # Unknown kinds are passed through
//
// Every entry carries a sentence written on the server, so a kind added after
// this build still reads correctly here. Nothing in this file filters on kind,
// and nothing above it drops a row it does not recognise: an old client that
// hid unfamiliar rows would hide the newest way of getting at a password.
package eumaeusdisclosure

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/jroedel/sion-backup/business/domain/disclosure/disclosurebus"
	"github.com/jroedel/sion-backup/foundation/eumaeusapi"
)

// path is relative to eumaeusapi.APIPrefix.
const path = "/machines/me/disclosures"

// maxLimit is what the server will accept. Asking for more is a 400, so it is
// clamped here rather than discovered in front of somebody.
const maxLimit = 500

// payload is the wire form.
type payload struct {
	Count   int64  `json:"count"`
	Head    string `json:"head"`
	Entries []struct {
		Seq                int64     `json:"seq"`
		At                 time.Time `json:"at"`
		Kind               string    `json:"kind"`
		Actor              string    `json:"actor"`
		FromIP             string    `json:"from_ip"`
		CredentialsVersion int       `json:"credentials_version"`
		ByMachine          bool      `json:"by_machine"`
		Describe           string    `json:"describe"`
		Detail             string    `json:"detail"`
		PrevHash           string    `json:"prev_hash"`
		Hash               string    `json:"hash"`
	} `json:"entries"`

	// ChainedSince is absent on a chain that covers every entry, which is the
	// ordinary case and the stronger guarantee.
	ChainedSince time.Time `json:"chained_since"`
}

// Source reads the trail from Eumaeus.
type Source struct {
	client *eumaeusapi.Client
}

// NewSource constructs one over an API client.
func NewSource(client *eumaeusapi.Client) *Source {
	return &Source{client: client}
}

// List returns the trail, newest first.
func (s *Source) List(ctx context.Context, limit int) (disclosurebus.Log, error) {
	if limit < 1 {
		limit = 1
	}

	if limit > maxLimit {
		limit = maxLimit
	}

	q := url.Values{"limit": {strconv.Itoa(limit)}}

	var got payload

	err := s.client.Do(ctx, http.MethodGet, path+"?"+q.Encode(), nil, &got)

	switch {
	case errors.Is(err, eumaeusapi.ErrUnauthorised), errors.Is(err, eumaeusapi.ErrForbidden):
		// The same meaning it has on the credentials endpoint: this machine is
		// no longer part of the fleet. Distinguished so the page says that
		// rather than reporting a network fault.
		return disclosurebus.Log{}, fmt.Errorf(
			"eumaeusdisclosure: %w: %w", disclosurebus.ErrNotEnrolled, err)

	case errors.Is(err, eumaeusapi.ErrNotFound):
		// An older Eumaeus that has not deployed this endpoint yet. Named, so
		// the page can stay quiet about a feature the server does not have
		// instead of showing an error to somebody who cannot act on it.
		return disclosurebus.Log{}, fmt.Errorf(
			"eumaeusdisclosure: this Eumaeus does not serve a disclosure trail: %w",
			disclosurebus.ErrUnsupported)

	case err != nil:
		return disclosurebus.Log{}, fmt.Errorf("eumaeusdisclosure: fetching the trail: %w", err)
	}

	log := disclosurebus.Log{
		Count:        got.Count,
		Head:         got.Head,
		ChainedSince: got.ChainedSince,
		Entries:      make([]disclosurebus.Entry, 0, len(got.Entries)),
	}

	for _, e := range got.Entries {
		log.Entries = append(log.Entries, disclosurebus.Entry{
			Seq:                e.Seq,
			At:                 e.At,
			Kind:               disclosurebus.Kind(e.Kind),
			Actor:              e.Actor,
			FromIP:             e.FromIP,
			CredentialsVersion: e.CredentialsVersion,
			ByMachine:          e.ByMachine,
			Describe:           e.Describe,
			Detail:             e.Detail,
			PrevHash:           e.PrevHash,
			Hash:               e.Hash,
		})
	}

	return log, nil
}

var _ disclosurebus.Source = (*Source)(nil)
