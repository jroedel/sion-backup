// Package eumaeusapi is the HTTP client for the Eumaeus server.
//
// It knows about requests, timeouts and authentication. It knows nothing about
// backups: the endpoints and their payloads belong to the domains that use
// them, in business/domain/*/sources. This is the transport, and it is here so
// that the credential escrow and the fleet reporter cannot drift into two
// different opinions about how long to wait or how to present a token.
//
// # The server side
//
// Eumaeus serves these endpoints under APIPrefix; docs/eumaeus-api.md is the
// contract, and its machine-readable form is openapi.yaml in the eumaeus
// repository, beside the handlers. Not all six exist yet on either side, and
// that is fine: an endpoint neither has implemented answers 404, which
// `sion-backup doctor` reports as clearly as a wrong token.
package eumaeusapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrNotFound is a 404, which several callers treat as an ordinary answer
// rather than a failure — "this machine has no escrow entry yet" arrives that
// way.
var ErrNotFound = errors.New("eumaeusapi: not found")

// ErrUnauthorised is a 401: the token is unknown, malformed, revoked or
// expired. Distinguished because it is the one failure retrying will never
// fix, and because for this client it means something specific — the machine
// has been de-enrolled and should say so rather than showing a network error
// forever.
//
// A 401 and nothing else. It used to be a 403 as well; see ErrForbidden.
var ErrUnauthorised = errors.New("eumaeusapi: the token was refused")

// ErrForbidden is a 403: the token was accepted, and what it asked for was
// refused anyway.
//
// The two statuses look alike and mean opposite things to a machine. A 401
// says this machine is finished and should stop. A 403 says the fleet is not
// set up for what was just asked — Eumaeus answers one when fresh buckets are
// not on offer, which is the ordinary state of a fleet where nobody has
// turned them on. Folded together, as they were until now, the rotation
// button would have told an owner their machine had been de-enrolled because
// an administrator had never run `eumaeus backup settings -fresh-buckets=on`.
//
// So nothing may treat this as terminal for the machine's identity. Whether
// it is terminal for the request is the endpoint's business and is left to
// the domain that knows: eumaeuscreds does treat it as de-enrolment, because
// a token that may not read its own credentials is finished whichever status
// says so.
var ErrForbidden = errors.New("eumaeusapi: the server refused this request")

// ErrBadRequest is a 400: the server could not parse what was sent, and says
// so about a request that will never become well-formed. Distinguished because
// it is the other failure retrying cannot fix — but unlike ErrUnauthorised it
// is this program's fault, and the caller's job is to stop resending and make
// enough noise that somebody fixes the client.
//
// Every 400 arrives as a [BadRequest] wrapping this, so a caller that wants
// the class asks errors.Is and one that wants the sentence asks errors.As.
var ErrBadRequest = errors.New("eumaeusapi: the request was rejected as malformed")

// ErrConflict is a 409: the request collided with existing state. The only
// place it means anything specific is an enrollment code that has already been
// claimed, where re-issuing a token would turn a replayed code into a second
// machine.
var ErrConflict = errors.New("eumaeusapi: already claimed")

// ErrUnprocessable is a 422: the request was well-formed and the server will
// not act on it, because something on its own side has to change first.
//
// Kept apart from ErrBadRequest because the two say opposite things about
// whose fault it is and what to do next. A 400 is this client sending
// nonsense; nobody at the machine can help and the answer is to stop and make
// noise. A 422 is the server saying the request is fine and the situation is
// not — the machine's bucket has not been provisioned, or the code enrols it
// against a repository it is not the one writing to — and the person standing
// at the machine is exactly who can fix it, often without the code expiring.
//
// Every 422 arrives as a [BadRequest] too, for its sentence.
var ErrUnprocessable = errors.New("eumaeusapi: the server will not act on this yet")

// BadRequest is a refusal with the server's own explanation read out of it: a
// 400, or a 422.
//
// The explanation is the point of the type. Eumaeus answers a refused claim
// with one sentence written for a person and the name of the input that was
// wrong:
//
//	{ "error": "a claim must say what the machine is called", "field": "hostname" }
//
// That sentence is read by whoever is standing at the machine at nine in the
// evening. Until this type existed the client did keep it — as a JSON
// fragment, at the end of a line that began "eumaeuscreds: claiming the
// enrollment code: eumaeusapi: the request was rejected as malformed". Which
// is to say the client put back, twice over, the package prefix Eumaeus had
// just been asked to drop.
type BadRequest struct {
	Method string
	Path   string

	// Status is 400 or 422, and decides which sentinel [BadRequest.Unwrap]
	// answers with. A caller that only wants the sentence never reads it; one
	// deciding whether to give up or to send somebody to fix something does.
	Status int

	// Message is the server's sentence, fit to print as it stands. Empty when
	// the body was not the shape the contract describes.
	Message string

	// Field names the input that was wrong. Optional, and often absent.
	Field string

	// Body is what actually arrived, kept only when no Message could be read
	// out of it — a proxy's HTML error page, or a shape nobody agreed to.
	Body string
}

// Error prefers the server's sentence, because the reader this status usually
// concerns is the one who cannot look anything up.
func (e *BadRequest) Error() string {
	switch {
	case e.Message != "" && e.Field != "":
		return fmt.Sprintf("%s (%s)", e.Message, e.Field)

	case e.Message != "":
		return e.Message

	default:
		return fmt.Sprintf("%s: %s %s: %s", e.Unwrap(), e.Method, e.Path, e.Body)
	}
}

// Unwrap keeps errors.Is true for the class this refusal belongs to, which is
// all the callers that only need the class ever ask.
func (e *BadRequest) Unwrap() error {
	if e.Status == http.StatusUnprocessableEntity {
		return ErrUnprocessable
	}

	return ErrBadRequest
}

// maxDetail bounds how much server text this client will carry around inside
// an error. A 400 body is one sentence; anything near this is a proxy's error
// page, and the whole of one belongs neither in a log file nor on a screen.
const maxDetail = 1 << 10

// newBadRequest reads the contract's error shape out of a body, and keeps the
// body itself when it is not that shape.
func newBadRequest(status int, method, path string, body []byte) *BadRequest {
	e := BadRequest{Method: method, Path: path, Status: status}

	var decoded struct {
		Error string `json:"error"`
		Field string `json:"field"`
	}

	if err := json.Unmarshal(body, &decoded); err == nil && decoded.Error != "" {
		e.Message = clip(decoded.Error)
		e.Field = clip(decoded.Field)

		return &e
	}

	e.Body = clip(string(body))

	return &e
}

// clip trims and shortens server text, cutting on a rune boundary so that a
// truncated sentence is still printable.
func clip(s string) string {
	s = strings.TrimSpace(s)

	if len(s) <= maxDetail {
		return s
	}

	return strings.ToValidUTF8(s[:maxDetail], "") + "…"
}

// APIPrefix is the base path every endpoint hangs off.
//
//	/api/backup/v1
//
// Owned by the client rather than written into each caller's path, and not
// left to the configured URL either: this binary speaks v1 and only v1, so the
// version belongs next to the code that would have to change to speak v2. An
// administrator configures a host, not an API version.
const APIPrefix = "/api/backup/v1"

// defaultTimeout bounds every call.
//
// Generous, because these run on a laptop on hotel wifi and a failure means
// either no enrollment or an unreported run. Bounded, because the daemon must
// not sit on a half-open connection until somebody reboots it.
const defaultTimeout = 30 * time.Second

// Config is what a client needs.
type Config struct {
	// BaseURL is the Eumaeus installation, e.g. "https://eumaeus.example.org".
	//
	// The host, without the API's own base path: APIPrefix is added to every
	// request. A URL that already ends in it is accepted anyway, because
	// openapi.yaml lists the full server URL and pasting that in is the
	// obvious mistake to make.
	BaseURL string

	// Token is the machine token, sent as a bearer credential.
	//
	// Empty is allowed, and means an anonymous client. Exactly one call needs
	// one — the enrollment claim, which exchanges a one-time code for a token
	// and therefore cannot have one yet.
	//
	// The one this program uses should be minted read-only and scoped to the
	// backup vault alone: `eumaeus token mint -label sion-backup -vaults
	// backup`. A token on a work computer is a token that can be taken off a
	// work computer, and it must not be able to read a ledger.
	Token string

	// Timeout overrides defaultTimeout.
	Timeout time.Duration

	// UserAgent identifies the client in the server's logs. A fleet of
	// machines all calling themselves "Go-http-client/1.1" is a fleet nobody
	// can debug from the server side.
	UserAgent string
}

// Client talks to one Eumaeus installation.
type Client struct {
	base  *url.URL
	token string
	agent string
	http  *http.Client
}

// New validates the configuration and builds a client.
func New(cfg Config) (*Client, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("eumaeusapi: no server URL configured")
	}

	trimmed := strings.TrimSuffix(strings.TrimSuffix(cfg.BaseURL, "/"), APIPrefix)

	base, err := url.Parse(strings.TrimSuffix(trimmed, "/"))
	if err != nil {
		return nil, fmt.Errorf("eumaeusapi: %q is not a URL: %w", cfg.BaseURL, err)
	}

	// Refused rather than warned about. This carries a token that unlocks
	// somebody's backups, and a plain-HTTP base URL in a config file is a
	// mistake nobody would make on purpose — except against a server on the
	// same machine, which is the one case where there is no wire to sniff.
	if base.Scheme != "https" && !isLoopback(base.Hostname()) {
		return nil, fmt.Errorf("eumaeusapi: refusing to send a token to %s over %s; use https",
			base.Host, base.Scheme)
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	agent := cfg.UserAgent
	if agent == "" {
		agent = "sion-backup"
	}

	return &Client{
		base:  base,
		token: cfg.Token,
		agent: agent,
		http:  &http.Client{Timeout: timeout},
	}, nil
}

func isLoopback(host string) bool {
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true
	default:
		return false
	}
}

// maxResponse bounds what a reply may be.
//
// A client that reads an unbounded body from a server is a client a
// misconfigured proxy can exhaust the memory of. Nothing this API returns is
// remotely near this size.
const maxResponse = 1 << 20

// Do sends a JSON request and decodes a JSON reply.
//
// path is relative to APIPrefix, so callers name the endpoint the
// specification names — "/runs", not "/api/backup/v1/runs".
//
// body may be nil for a GET. out may be nil when the reply is not wanted.
func (c *Client) Do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader

	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("eumaeusapi: encoding the request: %w", err)
		}

		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.base.String()+APIPrefix+path, reader)
	if err != nil {
		return fmt.Errorf("eumaeusapi: building the request: %w", err)
	}

	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.agent)

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("eumaeusapi: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	limited := io.LimitReader(resp.Body, maxResponse)

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return ErrNotFound

	case resp.StatusCode == http.StatusBadRequest,
		resp.StatusCode == http.StatusUnprocessableEntity:
		// The body is kept, and decoded: it carries one sentence meant for a
		// person and the name of the field that was wrong, and that sentence
		// is the whole value of the status to whoever reads it.
		//
		// 422 belongs here rather than in the catch-all below, and did not
		// until the claim grew a second cause for one. It is now the status
		// an adopting client is most likely to meet — the code enrols this
		// machine against a repository it has not been writing to — and the
		// catch-all rendered it as two package prefixes, a status line and a
		// JSON fragment, in front of the one reader who cannot look anything
		// up.
		detail, _ := io.ReadAll(limited)

		return newBadRequest(resp.StatusCode, method, path, detail)

	case resp.StatusCode == http.StatusUnauthorized:
		return ErrUnauthorised

	case resp.StatusCode == http.StatusForbidden:
		return ErrForbidden

	case resp.StatusCode == http.StatusConflict:
		return ErrConflict

	case resp.StatusCode >= 300:
		// The body is included because a 400 from this API says which field
		// was wrong, and without it the administrator is guessing.
		detail, _ := io.ReadAll(limited)

		return fmt.Errorf("eumaeusapi: %s %s: %s: %s",
			method, path, resp.Status, strings.TrimSpace(string(detail)))
	}

	if out == nil {
		// Drained so the connection can be reused rather than dropped.
		_, _ = io.Copy(io.Discard, limited)

		return nil
	}

	if err := json.NewDecoder(limited).Decode(out); err != nil {
		return fmt.Errorf("eumaeusapi: reading the reply to %s %s: %w", method, path, err)
	}

	return nil
}
