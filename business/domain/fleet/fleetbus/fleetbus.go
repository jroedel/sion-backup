// Package fleetbus reports what a machine did to whoever is watching the fleet.
//
// # Why reporting is a domain and not two lines of HTTP
//
// The thing an IT administrator needs is not "did last night's backup work" —
// it is "which of my thirty machines has quietly stopped". Those are different
// questions, and only the second one is answered by a machine that reports
// even when it has nothing good to say.
//
// So the contract here is: every finished run is reported, exactly once,
// eventually. Not "when the network is up", because the machines that most
// need backing up are laptops that are frequently not. A run taken on a plane
// is reported when the lid opens in the office, in the order it happened,
// which is why the store carries reported_at and this package has [Flush].
//
// # A reporting failure is never a backup failure
//
// Nothing in this package may cause a run to fail or be retried. The dashboard
// being down is the dashboard's problem; the files are already safe. Every
// call here is best-effort and returns an error the caller is expected to log
// and move past.
package fleetbus

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// Phase distinguishes the two moments a machine speaks up.
type Phase string

const (
	// PhaseStarted is sent when a run begins. It is what makes a machine that
	// dies mid-backup visible: the dashboard has a start with no end.
	PhaseStarted Phase = "started"

	// PhaseFinished carries the outcome.
	PhaseFinished Phase = "finished"
)

// Event is one thing worth telling the fleet dashboard.
//
// It carries no paths from the machine beyond the count of unreadable files,
// and no credentials. That is deliberate: this crosses the network to a server
// whose logs are not this program's to control, and "which directories exist
// on the finance officer's laptop" is not information that needs to be there.
// The status page on the machine itself shows the paths.
type Event struct {
	// NodeID is not sent. The server identifies the machine by its token, and
	// a client that could name a machine in the body could name somebody
	// else's. It is kept here because every log line about a run wants it.
	NodeID string `json:"-"`

	// RunUUID is a UUIDv7 minted before the run began. Both phases of one run
	// carry the same value, and the server is idempotent on it: an
	// acknowledgement lost in transit produces a resend, not a second row on
	// the dashboard.
	RunUUID string `json:"run_uuid"`

	RepositoryURL string `json:"repository_url"`

	// Seeding is the first backup into a newly provisioned repository. It is
	// what the server's cutover guard waits on, and it is why a machine that
	// has been uploading for two days is not overdue.
	Seeding bool `json:"seeding"`

	Phase     Phase     `json:"phase"`
	StartedAt time.Time `json:"started_at"`

	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Outcome    string     `json:"outcome,omitempty"`
	Message    string     `json:"message,omitempty"`
	SnapshotID string     `json:"snapshot_id,omitempty"`

	FilesProcessed  int64 `json:"files_processed,omitempty"`
	BytesProcessed  int64 `json:"bytes_processed,omitempty"`
	DataAdded       int64 `json:"data_added,omitempty"`
	UnreadableFiles int   `json:"unreadable_files,omitempty"`

	Verified bool `json:"verified"`

	// Agent and OS let the dashboard say "this machine is three versions
	// behind", which is the other question a fleet raises.
	Agent string `json:"agent,omitempty"`
	OS    string `json:"os,omitempty"`
}

// Reporter is somewhere events go.
type Reporter interface {
	Report(ctx context.Context, e Event) error
}

// Nop discards events.
//
// It is the reporter a machine gets when no server is configured, and it
// exists so that every call site can be unconditional. The alternative — a nil
// check before each report — is a nil dereference waiting for the one path
// somebody forgets.
type Nop struct{}

// Report does nothing, successfully.
func (Nop) Report(context.Context, Event) error { return nil }

// Pending is one run waiting to be reported, as the caller supplies it.
//
// A minimal struct rather than the backup domain's Run, because a business
// domain importing a sibling business domain is the beginning of a knot: the
// composition root already knows about both and can convert between them.
type Pending struct {
	ID    int64
	Event Event
}

// Business coordinates reporting.
type Business struct {
	reporter Reporter
	log      *slog.Logger
}

// NewBusiness constructs it. A nil reporter becomes Nop.
func NewBusiness(reporter Reporter, log *slog.Logger) *Business {
	if reporter == nil {
		reporter = Nop{}
	}

	return &Business{reporter: reporter, log: log}
}

// Report sends one event, and never returns an error the caller must act on.
//
// The error is returned anyway, for a caller that wants to log it with its own
// context. Nothing upstream should branch on it.
func (b *Business) Report(ctx context.Context, e Event) error {
	if err := b.reporter.Report(ctx, e); err != nil {
		b.log.Warn("the fleet dashboard could not be told about a run",
			"node", e.NodeID, "run", e.RunUUID, "phase", e.Phase, "err", err)

		return err
	}

	return nil
}

// Flush sends everything that has been waiting, oldest first, and calls
// markReported for each one that lands.
//
// It stops at the first failure rather than working through the list. Two
// reasons: the failure is almost always "no network", in which case the rest
// will fail identically and thirty timeouts is thirty times the delay; and the
// dashboard should see a machine's history in order, not with holes where a
// transient error fell.
//
// An event the server rejects outright is the exception: it is marked reported
// and the flush carries on. See ErrRejected.
func (b *Business) Flush(ctx context.Context, pending []Pending,
	markReported func(context.Context, int64) error) (sent int, err error) {

	for i, p := range pending {
		landed := true

		switch err := b.reporter.Report(ctx, p.Event); {
		case errors.Is(err, ErrRejected):
			// Loud, because this is a bug in this program rather than a
			// condition of the world: the server is saying it could not parse
			// what we sent, and no amount of resending will change that. It is
			// still marked reported below — see ErrRejected.
			landed = false

			b.log.Error("the fleet dashboard rejected a run event as malformed; "+
				"dropping it rather than resending it forever",
				"node", p.Event.NodeID, "run", p.Event.RunUUID,
				"phase", p.Event.Phase, "err", err)

		case err != nil:
			b.log.Debug("stopping the flush at the first failure",
				"sent", sent, "remaining", len(pending)-i, "err", err)

			return sent, err
		}

		if err := markReported(ctx, p.ID); err != nil {
			// The event has been sent. Failing to record that means it will be
			// sent again next time, which is a duplicate on the dashboard —
			// unpleasant but harmless, and much better than the alternative of
			// marking it before it lands.
			return sent, err
		}

		if landed {
			sent++
		}
	}

	return sent, nil
}

// ErrNoServer reports that no fleet server is configured. Callers use it to
// tell "not set up" from "set up and broken", which are different things to
// show on a status page.
var ErrNoServer = errors.New("fleetbus: no fleet server is configured")

// ErrRejected reports an event the server will never accept — a malformed
// body, not a server having a bad day.
//
// It exists because the two failures need opposite handling. A network error
// means "try again later", and an event held back is a gap the dashboard will
// fill in when the laptop comes home. A rejection means "this will never
// parse", and an event held back for that reason is retried every hour until
// somebody notices the log, blocking every later event behind it in the
// process — Flush sends in order and stops at the first failure.
//
// So a rejection is logged at error level and the run is marked reported, on
// the grounds that a run nobody can be told about is not worth stopping the
// rest of the history for. The server's side of this bargain is that it
// returns 400 only for a body it genuinely cannot parse, never for an event it
// merely finds unwelcome — see docs/eumaeus-api.md §7.
var ErrRejected = errors.New("fleetbus: the server rejected the event")
