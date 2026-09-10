// Package diagbus collects the reports a machine sends about itself failing.
//
// Not runs — those are fleetbus, and they are the ordinary business of a
// working machine. These are the three failures that leave nothing behind on
// the dashboard because the program was not working when they happened:
//
//  1. an install that did not finish;
//  2. a panic;
//  3. a self-update that could not be applied.
//
// # Why this exists at all
//
// Machines are installed by hand, by whoever is standing in front of them,
// often in the evening and usually not by a programmer. The failure that
// matters most is the one at nine o'clock on a colleague's laptop, and the
// person in front of it is not going to copy a stack trace into an email. So
// the program copies it instead.
//
// The second reason is the same failure twice. An install that fell over on
// one machine will fall over on the next one, and the only thing that changes
// that is somebody seeing the first one.
//
// # What is never in a report
//
// No credentials, and no path from the user's file tree. A report carries this
// program's own error text and the file and function names of its own source,
// which are compiled in and are not anybody's private business. [Scrub] is the
// backstop for the case where an error string quotes a path anyway — it
// rewrites a home directory to ~ rather than dropping the line, because a
// message with the shape of the path left in it is still worth reading.
//
// The reports are held on disk until the server accepts them, and the disk is
// a directory of files rather than a table, because the report worth having
// most is the one from an install that fell over before there was a database.
package diagbus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// Kind is what happened.
//
// A short closed set, because the server groups by it. A kind it does not
// recognise is a fourth kind and not an error — see docs/eumaeus-requests.md
// §5.1.
type Kind string

const (
	// KindInstallFailed is an install or upgrade that did not finish. It is
	// the one that usually arrives with no node ID, because the machine never
	// got as far as being enrolled.
	KindInstallFailed Kind = "install-failed"

	// KindPanic is this program crashing.
	KindPanic Kind = "panic"

	// KindUpdateFailed is a self-update that could not be downloaded,
	// verified or swapped in. Distinguished from a panic because the machine
	// is still running afterwards — on the old version, which is the thing
	// somebody needs to know.
	KindUpdateFailed Kind = "update-failed"

	// KindUpdateRolledBack is a version that installed, would not stay
	// running, and was replaced with the one before it.
	//
	// The most valuable report this program sends. Every other kind describes
	// one machine's bad evening; this one says a release is bad, and it says
	// so from a machine that is running again and can therefore be believed.
	// One of these is a reason to look; two from different machines is a
	// reason to un-publish the release before the rest of the fleet takes it.
	KindUpdateRolledBack Kind = "update-rolled-back"

	// KindRepositoryDamaged is a repository that failed its own integrity
	// check.
	//
	// The gravest report this program sends, and the only one that is about
	// backups already taken rather than a backup that did not happen. Every
	// other kind means somebody's files are not being protected from now on;
	// this one means the copies already made may not come back. It is also the
	// only failure nobody else can see: a damaged pack reads as a healthy
	// repository to the snapshot list, the index and the dashboard, right up
	// until somebody needs the file inside it.
	KindRepositoryDamaged Kind = "repository-damaged"
)

// maxDetail bounds the free text.
//
// A panic on a machine with a deep stack can run to tens of kilobytes, and
// nothing after the first few frames tells anybody anything they did not
// already know from the first few frames.
const maxDetail = 16 << 10

// Report is one thing worth telling somebody about.
type Report struct {
	Kind       Kind      `json:"kind"`
	OccurredAt time.Time `json:"occurred_at"`
	Agent      string    `json:"agent"`
	OS         string    `json:"os"`

	// InstallID ties the several reports one install can produce together,
	// and lets the server drop a duplicate from a retried send.
	InstallID string `json:"install_id,omitempty"`

	// NodeID is absent when the machine never got far enough to have one.
	NodeID string `json:"node_id,omitempty"`

	// Step is which part of the install failed: a short name from the
	// installer, not free text.
	Step string `json:"step,omitempty"`

	// PriorVersion is what was on the machine before, including the legacy
	// scripts — "legacy-windows-1.3". It is how a migration that goes wrong
	// on one version and not another becomes visible.
	PriorVersion string `json:"prior_version,omitempty"`

	Detail string `json:"detail"`
}

// Sink is somewhere reports go. Implemented by the Eumaeus source.
type Sink interface {
	Send(ctx context.Context, r Report) error
}

// Queue is the disk the reports wait on.
//
// Separate from Sink because the two fail independently and at different
// times: a report is written by a program that is about to die, and sent by
// one that is running normally, possibly days later.
type Queue interface {
	Put(r Report) error
	List() ([]Queued, error)
	Remove(id string) error
}

// Queued is a report and the handle to delete it by.
type Queued struct {
	ID     string
	Report Report
}

// Business records and forwards reports.
type Business struct {
	queue Queue
	sink  Sink
	log   *slog.Logger

	agent string
}

// NewBusiness constructs it.
//
// A nil sink means "hold everything on disk", which is what a machine has
// while the server does not serve the endpoint. A nil queue means "discard",
// which is what a machine has when even resolving its own data directory
// failed — see cmd/sion-backup/diagnostics.go. Neither is an error, because
// the callers are a panic handler and an installer that is already dealing
// with something going wrong.
func NewBusiness(queue Queue, sink Sink, agent string, log *slog.Logger) *Business {
	return &Business{queue: queue, sink: sink, agent: agent, log: log}
}

// Record writes a report to the queue.
//
// It does not send. The caller is usually a program that is about to exit, and
// an HTTP round trip is the wrong thing to do on the way out of a panic: it
// delays the crash, it can hang, and it can panic again. Sending is [Flush]'s
// job, on the next ordinary run.
//
// The error is returned for logging and nothing more. Failing to record a
// diagnostic must never change what the program does next.
func (b *Business) Record(r Report) error {
	if b == nil || b.queue == nil {
		return nil
	}

	r.Agent = b.agent
	r.OS = runtime.GOOS + "/" + runtime.GOARCH

	if r.OccurredAt.IsZero() {
		r.OccurredAt = time.Now()
	}

	r.Detail = Scrub(r.Detail)

	if len(r.Detail) > maxDetail {
		r.Detail = r.Detail[:maxDetail] + "\n[truncated]"
	}

	if err := b.queue.Put(r); err != nil {
		b.log.Warn("could not record a diagnostic report", "kind", r.Kind, "err", err)

		return err
	}

	return nil
}

// Flush sends what is waiting and deletes what lands.
//
// Best-effort in exactly the way fleetbus is: nothing here may fail a backup
// or delay one. It stops at the first failure, because the usual failure is
// the server not being reachable and the rest will fail identically.
//
// A report the server refuses as malformed is deleted rather than kept. It
// will not become well-formed, and a queue that never drains is a queue that
// eventually holds nothing but its own oldest failure.
func (b *Business) Flush(ctx context.Context) (sent int, err error) {
	if b == nil || b.queue == nil || b.sink == nil {
		return 0, nil
	}

	waiting, err := b.queue.List()
	if err != nil {
		return 0, err
	}

	for _, q := range waiting {
		landed := true

		switch err := b.sink.Send(ctx, q.Report); {
		case err == nil:

		case errors.Is(err, ErrRejected):
			// Dropped below, with the ones that landed, but not counted as
			// one: sent is what reached somebody.
			landed = false

			b.log.Error("the server rejected a diagnostic report; dropping it",
				"kind", q.Report.Kind, "err", err)

		default:
			return sent, err
		}

		if err := b.queue.Remove(q.ID); err != nil {
			b.log.Warn("a diagnostic report was sent but not removed from the queue",
				"id", q.ID, "err", err)

			return sent, err
		}

		if landed {
			sent++
		}
	}

	return sent, nil
}

// Waiting reports how many are held on disk, for the status page and doctor.
func (b *Business) Waiting() int {
	if b == nil || b.queue == nil {
		return 0
	}

	waiting, err := b.queue.List()
	if err != nil {
		return 0
	}

	return len(waiting)
}

// ErrRejected marks a report the server will never accept — a malformed body
// rather than a server having a bad day. The Eumaeus source wraps a 400 in it,
// for the same reason fleetbus does: the domain decides what to do about it
// and should not have to know about HTTP to decide.
var ErrRejected = errors.New("diagbus: the server rejected the report")

// homePath matches an absolute path inside somebody's home directory on any of
// the three platforms.
//
// Deliberately narrow. It is a backstop for an error string that quotes a path
// this program did not mean to send, not a general redactor, and a general
// redactor would turn every message into asterisks and teach everybody to stop
// reading them.
var homePath = regexp.MustCompile(`(?i)(/home/|/Users/|[A-Z]:\\Users\\)([^/\\ "']+)`)

// Scrub rewrites the parts of a message that are somebody's business and
// nobody else's.
//
// A path under a home directory becomes ~, keeping the shape — which is the
// part that helps — and dropping the person. Everything else is left alone: an
// error that has been edited until it is safe is usually an error that no
// longer says anything.
func Scrub(detail string) string {
	scrubbed := homePath.ReplaceAllStringFunc(detail, func(match string) string {
		switch {
		case strings.HasPrefix(match, "/home/"), strings.HasPrefix(match, "/Users/"):
			return "~"
		default:
			// C:\Users\someone
			return "~"
		}
	})

	return strings.TrimSpace(scrubbed)
}

// PanicReport builds the report for a recovered panic.
//
// The stack is the program's own, and the frames name this program's source
// files — which ship in the binary and are on GitHub. There is nothing of the
// machine's owner in it.
func PanicReport(recovered any, stack []byte, nodeID string) Report {
	return Report{
		Kind:       KindPanic,
		OccurredAt: time.Now(),
		NodeID:     nodeID,
		Detail:     fmt.Sprintf("panic: %v\n\n%s", recovered, stack),
	}
}
