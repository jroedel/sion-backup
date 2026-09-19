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

// The four calls a machine makes about its own repository.
//
// All are relative to eumaeusapi.APIPrefix.
const (
	rotationRequestPath = "/machines/me/rotation-request"
	cutoverPath         = "/machines/me/cutover"
	oldBucketPath       = "/machines/me/old-bucket"
	cardIssuedPath      = "/machines/me/card-issued"
)

// notEnrolled turns a 401 from any of these into the one terminal answer.
//
// 401 and nothing else, on every one of them. There is no 403 left in this API
// to fold in — the rotation request was the last route that answered one, and
// the fleet-wide switch behind it is withdrawn. The client keeps
// eumaeusapi.ErrForbidden and its handling for a 403 from an older deployment
// or a proxy; it is not something these four can produce.
func notEnrolled(doing string, err error) error {
	return fmt.Errorf("eumaeusmachine: %s: %w: %w", doing, machinebus.ErrNotEnrolled, err)
}

// RequestRotation asks an administrator for a fresh bucket.
//
// 202 is the success. The figures travel with it so that whoever reads the
// queue sees what the owner was shown when they clicked.
func (s *Source) RequestRotation(ctx context.Context, m machinebus.Measured) error {
	body := struct {
		RepositoryURL    string    `json:"repository_url"`
		ReclaimableBytes int64     `json:"reclaimable_bytes"`
		FreshBytes       int64     `json:"fresh_bytes"`
		MeasuredAt       time.Time `json:"measured_at"`
	}{
		RepositoryURL:    m.RepositoryURL,
		ReclaimableBytes: m.ReclaimableBytes,
		FreshBytes:       m.FreshBytes,
		MeasuredAt:       m.MeasuredAt,
	}

	err := s.client.Do(ctx, http.MethodPost, rotationRequestPath, body, nil)

	switch {
	case errors.Is(err, eumaeusapi.ErrUnauthorised):
		return notEnrolled("asking for a fresh bucket", err)

	case errors.Is(err, eumaeusapi.ErrConflict):
		// Three causes, one answer to the owner: an administrator has already
		// been asked, or has already acted. Which of the three it was is on
		// the fleet pages and is not this machine's business — but the third,
		// a bucket already waiting to be accepted, is why this is not an
		// error the page swallows: what is outstanding then is this machine's
		// own decision.
		return machinebus.ErrAlreadyAsked

	case errors.Is(err, eumaeusapi.ErrNotFound):
		return fmt.Errorf("eumaeusmachine: %w", machinebus.ErrNoRepository)

	case err != nil:
		return fmt.Errorf("eumaeusmachine: asking for a fresh bucket: %w", err)
	}

	return nil
}

// Cutover accepts the standing offer.
//
// The URL is named back and the server refuses any other with a 422 whose
// sentence names both. That sentence is returned as it stands — it is written
// for the person who just clicked, and the difference is usually one word of a
// bucket name.
func (s *Source) Cutover(ctx context.Context, repositoryURL string) error {
	body := struct {
		RepositoryURL string `json:"repository_url"`
	}{RepositoryURL: repositoryURL}

	err := s.client.Do(ctx, http.MethodPost, cutoverPath, body, nil)

	switch {
	case errors.Is(err, eumaeusapi.ErrUnauthorised):
		return notEnrolled("accepting the offered bucket", err)

	case errors.Is(err, eumaeusapi.ErrConflict):
		// Nothing offered. 409 rather than 404 so that it can be told apart
		// from the route not being served, which is the distinction this
		// client has got wrong in both directions before.
		return machinebus.ErrNothingOffered

	case errors.Is(err, eumaeusapi.ErrNotFound):
		return fmt.Errorf("eumaeusmachine: %w", machinebus.ErrNoRepository)

	case err != nil:
		// A 422 arrives here as an eumaeusapi.BadRequest carrying the
		// server's own sentence, and is wrapped rather than replaced for that
		// reason.
		return fmt.Errorf("eumaeusmachine: accepting the offered bucket: %w", err)
	}

	return nil
}

// ReleaseOldBucket says this machine has finished with the bucket it moved off.
//
// repositoryURL is the bucket it is on NOW. See machinebus.Business.
func (s *Source) ReleaseOldBucket(ctx context.Context, repositoryURL string) (machinebus.Released, error) {
	body := struct {
		RepositoryURL string `json:"repository_url"`
	}{RepositoryURL: repositoryURL}

	var got struct {
		Bucket       string    `json:"bucket"`
		URL          string    `json:"url"`
		ReleaseAsked time.Time `json:"release_asked_at"`
	}

	err := s.client.Do(ctx, http.MethodPost, oldBucketPath, body, &got)

	switch {
	case errors.Is(err, eumaeusapi.ErrUnauthorised):
		return machinebus.Released{}, notEnrolled("letting go of the old bucket", err)

	case errors.Is(err, eumaeusapi.ErrConflict):
		// Not cutting over, or nothing verified has landed yet. Both are
		// ordinary and both clear by themselves, which is why the daemon
		// retries this after every verified run rather than giving up on it.
		return machinebus.Released{}, machinebus.ErrNotCuttingOver

	case errors.Is(err, eumaeusapi.ErrNotFound):
		return machinebus.Released{}, fmt.Errorf("eumaeusmachine: %w", machinebus.ErrNoRepository)

	case err != nil:
		return machinebus.Released{}, fmt.Errorf(
			"eumaeusmachine: letting go of the old bucket: %w", err)
	}

	return machinebus.Released{
		Bucket:  got.Bucket,
		URL:     got.URL,
		AskedAt: got.ReleaseAsked,
	}, nil
}

// CardIssued records that the owner's restore card was rendered.
//
// 204 is the success, so nothing is decoded. A 404 means the card does not
// name this machine's current repository — either it has none, or the card is
// for a superseded bucket — and is passed up rather than swallowed, because
// whoever just printed a page is entitled to be told it does not describe
// where this machine backs up now.
func (s *Source) CardIssued(ctx context.Context, repositoryURL string, printedAt time.Time) error {
	body := struct {
		RepositoryURL string    `json:"repository_url"`
		PrintedAt     time.Time `json:"printed_at"`
	}{RepositoryURL: repositoryURL, PrintedAt: printedAt}

	err := s.client.Do(ctx, http.MethodPost, cardIssuedPath, body, nil)

	switch {
	case errors.Is(err, eumaeusapi.ErrUnauthorised):
		return notEnrolled("recording the printed card", err)

	case errors.Is(err, eumaeusapi.ErrNotFound):
		return fmt.Errorf("eumaeusmachine: the card does not name this machine's "+
			"current repository, so it cannot mark it as covered: %w", machinebus.ErrNoRepository)

	case err != nil:
		return fmt.Errorf("eumaeusmachine: recording the printed card: %w", err)
	}

	return nil
}
