// Package backupbus runs one backup and decides what its outcome was.
//
// # A finished backup is not a good backup
//
// This package exists because "restic exited" and "your files are safe" are
// different statements, and the scripts this replaces conflated them. Three
// things have to be true, and each is checked separately:
//
//  1. restic exited zero. Exit 3 means the snapshot was written and some files
//     could not be read — a real snapshot with holes in it, which the old
//     scripts recorded as success.
//  2. A file written moments before the run comes back out of the repository
//     with the same contents. That is the only check that exercises the whole
//     path: credentials, network, bucket, encryption, and the restore side
//     that nobody tests until the day they need it.
//  3. Nothing about the run was degraded — VSS refused, say.
//
// The outcome records which of those failed, because they call for different
// actions. A machine reporting Unverified is more alarming than one reporting
// Incomplete: incomplete says a named file was locked, unverified says the
// repository may not be readable at all.
//
// # The verification file is deliberately backed up
//
// [Runner.Run] writes a nonce into the verify directory and adds that
// directory to the targets. The legacy scripts wrote their nonce somewhere
// that happened to sit under the backup root, which meant the verification
// silently stopped verifying anything the day somebody narrowed the target
// list. Adding the directory explicitly makes that impossible.
//
// The nonce is random per run and never reused, so a stale restore — the
// previous night's file still sitting in the scratch directory — cannot pass
// for a fresh one. The scratch directory is cleared before and after anyway;
// the nonce is what makes that belt-and-braces rather than load-bearing.
package backupbus

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jroedel/sion-backup/foundation/paths"
	"github.com/jroedel/sion-backup/foundation/restic"
)

// Outcome is how a run ended.
type Outcome string

// The outcomes, worst last. Ordering matters for the status page, which shows
// the worst outcome in a window rather than the most recent one.
const (
	// OutcomeSuccess is a complete snapshot that was verified by restoring
	// from it.
	OutcomeSuccess Outcome = "success"

	// OutcomeDegraded is a verified snapshot taken in a weakened way — VSS was
	// refused, so files that were open may be missing or torn.
	OutcomeDegraded Outcome = "degraded"

	// OutcomeIncomplete is a real, restorable snapshot with named files
	// missing from it.
	OutcomeIncomplete Outcome = "incomplete"

	// OutcomeUnverified is a snapshot restic says it wrote, which could not be
	// read back. Something between here and the bucket is wrong, and the
	// backups may not be restorable at all.
	OutcomeUnverified Outcome = "unverified"

	// OutcomeFailed is no snapshot.
	OutcomeFailed Outcome = "failed"

	// OutcomeCancelled is a run somebody stopped on purpose.
	//
	// Kept apart from OutcomeFailed because the two need different words on a
	// page. A failure is something to look into; a cancellation is somebody
	// noticing that the run is uploading forty gigabytes of video they never
	// wanted, stopping it, and editing the exclude list. Calling that a
	// failure teaches the reader that red means nothing.
	OutcomeCancelled Outcome = "cancelled"
)

// Good reports whether an outcome means the files are safe.
func (o Outcome) Good() bool { return o == OutcomeSuccess }

// Reported is the outcome as the fleet dashboard is told it.
//
// The dashboard's five values are documented in docs/eumaeus-api.md and the
// question they answer is "is this machine protected". After a cancelled run
// the answer is the same as after a failed one -- there is no new snapshot,
// and the staleness clock must not reset -- so cancelled is reported as
// failed, and the reason travels in the message. A sixth value would need the
// server to learn it to mean anything, and it would mean exactly what failed
// already means.
func (o Outcome) Reported() Outcome {
	if o == OutcomeCancelled {
		return OutcomeFailed
	}

	return o
}

// Run is one backup attempt, as recorded.
type Run struct {
	ID     int64
	NodeID string

	// RunUUID is how the fleet dashboard names this run: a UUIDv7 minted
	// before the backup starts, sent on both the started and the finished
	// event, and stored here so a run reported days later — a laptop that
	// backed up on a plane — still reports the identity it announced.
	//
	// ID cannot do that job. It does not exist yet when the start is
	// announced, and it counts from 1 again on a reimaged machine.
	RunUUID string

	Repository string

	// Seeding marks the first backup into a newly provisioned repository: the
	// one that uploads everything. Not "full vs incremental", which is a
	// distinction restic does not have.
	//
	// # Nothing on the server reads it, and that is settled rather than
	// pending
	//
	// This used to say the cutover guard waited on it. It never did. The
	// permission that lets a client create a repository is Eumaeus's own
	// `expectEmpty`, which waits on a reported snapshot in its runs table; the
	// carve-out that stops a seeding machine being called overdue is its
	// `seedingSince`, which waits on its own `began_cutover_at` column.
	//
	// This field is stored, served back and read by nothing there, on purpose:
	// a flag a machine sets about itself must not be able to switch off
	// alerting about that machine having gone quiet.
	//
	// So it is a fact for whoever reads a run, and the local answer to "is
	// this the run that fills a new bucket?" — which this program does need,
	// because both events one run produces have to say the same thing about
	// it. It is emphatically not a permission: see credentialbus.Set for the
	// one field in that API which is.
	Seeding bool

	StartedAt  time.Time
	FinishedAt time.Time
	Outcome    Outcome

	// Message is the one sentence to show a person. It never contains a
	// credential: restic errors carry the command line, and no secret is ever
	// in one — see foundation/restic.
	Message string

	SnapshotID          string
	FilesNew            int64
	FilesChanged        int64
	TotalFilesProcessed int64
	TotalBytesProcessed int64
	DataAdded           int64

	// UnreadableFiles are the paths restic could not read, capped. This is the
	// list somebody actually needs: "which files are not in my backup".
	UnreadableFiles []string

	// Verified records that a file written just before the run came back out
	// of the repository byte for byte.
	Verified bool

	// VSSFellBack records that a filesystem snapshot was asked for and
	// refused.
	VSSFellBack bool

	// ReportedAt is when the fleet dashboard was told. Nil means it has not
	// been, which is normal on a laptop that was offline; see fleetbus.
	ReportedAt *time.Time
}

// Duration is how long the run took.
func (r Run) Duration() time.Duration {
	if r.FinishedAt.IsZero() {
		return 0
	}

	return r.FinishedAt.Sub(r.StartedAt)
}

// Storer is the persistence this domain needs.
type Storer interface {
	Create(ctx context.Context, r Run) (int64, error)
	SeenRepository(ctx context.Context, repository string) (bool, error)
	Finish(ctx context.Context, r Run) error
	Recent(ctx context.Context, limit int) ([]Run, error)
	Last(ctx context.Context) (Run, error)
	LastFinished(ctx context.Context) (Run, error)
	Unreported(ctx context.Context, limit int) ([]Run, error)
	MarkReported(ctx context.Context, id int64, at time.Time) error
}

// Unfinished reports a run that has started and not yet ended.
//
// A row is written when a run begins, with OutcomeFailed already in it, so
// that a machine losing power mid-backup leaves a failure rather than a row
// that reads as success. The cost is that every run looks like a failed one
// while it is happening, and the status page said exactly that for hours of a
// first full upload: "failed, 0 files, 0 B, verified no", directly underneath
// a banner saying a backup was in progress.
//
// backupdb.Store.Unreported has always known this -- it sends only finished
// runs, because reporting one in progress would put a failure on the fleet
// dashboard for a backup running perfectly well. This is the same rule, for
// the page.
func (r Run) Unfinished() bool { return r.FinishedAt.IsZero() }

// Stopped reports a run somebody cancelled.
//
// Its figures are absent rather than zero: restic prints its totals when it
// finishes, and a run that was stopped never got there, so it uploaded an
// unknown amount rather than none.
func (r Run) Stopped() bool { return r.Outcome == OutcomeCancelled }

// StaleAfter is how long a machine may go without a good backup before it
// stops being described as healthy.
//
// Two days rather than one. A daily schedule plus a weekend, a public holiday,
// or a day working from a café means one missed night is normal, and a page
// that cries wolf on a Monday morning is a page people stop reading — which is
// the actual failure this whole program is designed against.
//
// It lives here rather than on the page because doctor answers the same
// question at a terminal, and the two disagreeing about whether this computer
// is backed up would be worse than either answer.
const StaleAfter = 48 * time.Hour

// ErrNoRuns reports a machine that has never backed up.
var ErrNoRuns = errors.New("backupbus: this machine has no run history")

// ErrAlreadyRunning reports a second run asked for while one is in progress.
//
// Two restics against one repository do not corrupt anything — restic locks —
// but the second one waits on the lock for as long as the first one takes,
// which on a first full backup is hours, and the status page would show two
// runs where the person asked for one.
var ErrAlreadyRunning = errors.New("backupbus: a backup is already running")

// ErrNotRunning reports a cancellation asked for with nothing to cancel.
//
// Ordinary rather than exceptional: the page showing a Cancel button and the
// press arriving are seconds apart, and a backup that finished in between is
// the button doing its job slightly too late.
var ErrNotRunning = errors.New("backupbus: no backup is running")

// ErrCancelled is what context.Cause reports for a run somebody stopped.
var ErrCancelled = errors.New("backupbus: the backup was cancelled")

// cancellation is the cause a cancelled run's context carries: ErrCancelled
// for code to match on, and a human reason for the row somebody reads.
type cancellation struct{ reason string }

func (c *cancellation) Error() string { return c.reason }

func (c *cancellation) Is(target error) bool { return target == ErrCancelled }

// Request is one run's inputs.
//
// The credentials arrive already resolved, inside Repository. This domain
// never fetches them itself: keeping the secret-handling in one place — the
// composition root, which fetches them from Eumaeus and wipes them — means
// there is one lifetime to reason about rather than one per domain.
type Request struct {
	NodeID string

	// RunUUID names this run to the fleet dashboard. Supplied by the caller
	// because the "started" event goes out before Run is called and has to
	// carry the same value; see cmd/sion-backup/events.go.
	//
	// An empty one is not refused. A run that cannot be reported is still a
	// backup worth taking, and the reporting side is built to discard an event
	// the server rejects rather than to retry it forever — see fleetbus.
	RunUUID string

	Seeding bool

	Repository restic.Repository
	Options    restic.BackupOptions
}

// Runner takes backups.
type Runner struct {
	store  Storer
	restic *restic.Runner
	paths  paths.Paths
	log    *slog.Logger

	// running is the single-flight guard. A mutex rather than a channel
	// because the answer to "is one already going" has to be immediate: the
	// status page asks on every render.
	running atomic.Bool

	// progress is the latest status line, for the status page. Stored rather
	// than streamed, because the page polls and restic emits several lines a
	// second — writing each one anywhere durable would be pure waste.
	progress atomic.Pointer[Progress]

	// cancel stops the backup that is running, and is nil when none is. Held
	// as a pointer so that storing nil is how "nothing to cancel" is said,
	// rather than a second flag that could disagree with this one.
	cancel atomic.Pointer[context.CancelCauseFunc]

	// finishing serialises the two writes that end a run, so a shutdown racing
	// a completion cannot interleave them.
	finishing sync.Mutex
}

// Progress is what a running backup is currently doing.
type Progress struct {
	Started     time.Time
	PercentDone float64
	FilesDone   int64
	TotalFiles  int64
	BytesDone   int64
	TotalBytes  int64
	CurrentFile string
}

// NewRunner constructs the domain.
func NewRunner(store Storer, r *restic.Runner, p paths.Paths, log *slog.Logger) *Runner {
	return &Runner{store: store, restic: r, paths: p, log: log}
}

// Running reports whether a backup is in progress, and what it is doing.
func (b *Runner) Running() (Progress, bool) {
	if !b.running.Load() {
		return Progress{}, false
	}

	p := b.progress.Load()
	if p == nil {
		return Progress{}, true
	}

	return *p, true
}

// Cancel stops the running backup, or reports ErrNotRunning.
//
// The reason is written on the row, because "cancelled" on its own invites
// the question this program exists to answer without being asked.
//
// It returns as soon as restic has been asked to stop, not once it has: a
// restic told to stop is finishing the pack it is uploading and removing its
// lock, which takes a moment, and holding a click on a web page open for it
// would only make somebody press the button again.
func (b *Runner) Cancel(reason string) error {
	cancel := b.cancel.Load()
	if cancel == nil {
		return ErrNotRunning
	}

	(*cancel)(&cancellation{reason: reason})

	return nil
}

// Recent returns the last n runs, newest first.
func (b *Runner) Recent(ctx context.Context, n int) ([]Run, error) {
	return b.store.Recent(ctx, n)
}

// Last returns the most recent run, or ErrNoRuns.
func (b *Runner) Last(ctx context.Context) (Run, error) {
	return b.store.Last(ctx)
}

// LastFinished returns the most recent run that has an outcome, or ErrNoRuns.
//
// What "the last run" means to somebody reading a page: the last one there is
// anything to say about. A run in progress is described by the progress bar
// above it, and describing it twice -- once as a live percentage and once as
// a failure with nothing uploaded -- is how a healthy twelve-hour seed came
// to look like a disaster.
func (b *Runner) LastFinished(ctx context.Context) (Run, error) {
	return b.store.LastFinished(ctx)
}

// RecordFailedStart writes a run that never began.
//
// Everything before [Runner.Run] can fail -- a plan with no folders in it, a
// restic that could not be installed, credentials the server would not hand
// over, a bucket that will not open -- and all of it used to be a line in a
// log. The page said nothing at all: no row, no outcome, no change from the
// press of a button, which is indistinguishable from a button that does not
// work.
//
// Recorded as one instant rather than a duration, because nothing ran.
func (b *Runner) RecordFailedStart(ctx context.Context, req Request, reason string,
	now func() time.Time) error {
	at := now()

	run := Run{
		NodeID:     req.NodeID,
		RunUUID:    req.RunUUID,
		Repository: req.Repository.URL,
		Seeding:    req.Seeding,
		StartedAt:  at,
		FinishedAt: at,
		Outcome:    OutcomeFailed,
		Message:    reason,
	}

	id, err := b.store.Create(ctx, run)
	if err != nil {
		return fmt.Errorf("backupbus: recording a backup that could not start: %w", err)
	}

	run.ID = id

	if err := b.store.Finish(ctx, run); err != nil {
		return fmt.Errorf("backupbus: finishing the record of a backup that could not start: %w", err)
	}

	return nil
}

// Unreported returns runs the fleet dashboard has not been told about.
func (b *Runner) Unreported(ctx context.Context, limit int) ([]Run, error) {
	return b.store.Unreported(ctx, limit)
}

// Seeding reports whether the next run against this repository will be its
// first — the one that uploads everything.
//
// Asked before a run starts, because both events it produces have to say the
// same thing about it.
func (b *Runner) Seeding(ctx context.Context, repository string) (bool, error) {
	seen, err := b.store.SeenRepository(ctx, repository)
	if err != nil {
		return false, err
	}

	return !seen, nil
}

// MarkReported records that the fleet dashboard has been told about a run.
func (b *Runner) MarkReported(ctx context.Context, id int64, at time.Time) error {
	return b.store.MarkReported(ctx, id, at)
}

// Run takes one backup, verifies it, and records what happened.
//
// It returns the recorded Run and an error only when something stopped the run
// being recorded at all. A failed backup is a returned Run with
// OutcomeFailed and a nil error: the failure is the result, not an exception,
// and a caller that treated it as one would lose the record of it.
func (b *Runner) Run(ctx context.Context, req Request, now func() time.Time) (Run, error) {
	if !b.running.CompareAndSwap(false, true) {
		return Run{}, ErrAlreadyRunning
	}
	defer b.running.Store(false)
	defer b.progress.Store(nil)

	// The run's own context, so Cancel stops this backup and nothing else.
	// Derived from the caller's, which is the daemon's, so a shutdown still
	// stops it — and WithCancelCause rather than WithCancel so that the two
	// can be told apart afterwards: one is somebody pressing a button, the
	// other is the machine going down, and they are not the same row.
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	b.cancel.Store(&cancel)
	defer b.cancel.Store(nil)

	started := now()
	b.progress.Store(&Progress{Started: started})

	run := Run{
		NodeID:     req.NodeID,
		RunUUID:    req.RunUUID,
		Repository: req.Repository.URL,
		Seeding:    req.Seeding,
		StartedAt:  started,
		Outcome:    OutcomeFailed,
	}

	id, err := b.store.Create(ctx, run)
	if err != nil {
		return Run{}, fmt.Errorf("backupbus: recording the start of a run: %w", err)
	}

	run.ID = id

	// From here on every path finishes the run rather than returning early
	// with it unrecorded. A run row left open forever is exactly what the
	// status page cannot interpret.
	b.execute(runCtx, &run, req, now)

	b.finishing.Lock()
	defer b.finishing.Unlock()

	run.FinishedAt = now()

	// WithoutCancel, and deliberately. This is the caller's context -- the
	// daemon's -- and a machine shutting down cancels it while this run is
	// still closing. The write that records the outcome would fail, leaving a
	// row with no finished_at, which the page reads as a backup that is still
	// running and will go on reading that way forever. The run is over either
	// way; what is left is saying so.
	if err := b.store.Finish(context.WithoutCancel(ctx), run); err != nil {
		return run, fmt.Errorf("backupbus: recording the end of run %d: %w", run.ID, err)
	}

	return run, nil
}

// execute does the work and fills in the outcome. It never returns an error:
// everything it learns goes into the Run.
func (b *Runner) execute(ctx context.Context, run *Run, req Request, now func() time.Time) {
	if err := b.paths.ClearScratch(); err != nil {
		run.Message = "could not prepare the verification directory: " + err.Error()

		return
	}

	nonce, nonceFile, err := b.writeNonce(now())
	if err != nil {
		run.Message = err.Error()

		return
	}

	opts := req.Options

	// The verify directory joins the targets here rather than in the plan, so
	// it cannot be edited out of a plan on the status page.
	opts.Targets = append(append([]string(nil), opts.Targets...), b.paths.Verify)

	summary, backupErr := b.restic.Backup(ctx, req.Repository, opts, func(p restic.Progress) {
		b.progress.Store(&Progress{
			Started:     run.StartedAt,
			PercentDone: p.PercentDone,
			FilesDone:   p.FilesDone,
			TotalFiles:  p.TotalFiles,
			BytesDone:   p.BytesDone,
			TotalBytes:  p.TotalBytes,
			CurrentFile: firstOf(p.CurrentFiles),
		})
	})

	// Before the error and before the summary is read, because a cancelled
	// run must not be described by whatever restic happened to exit with. A
	// restic that has been interrupted may exit 0 after tidying up, or 1, or
	// be killed and exit with a signal; all three mean the same thing here,
	// and only this program knows which of them was asked for.
	if cause := context.Cause(ctx); errors.Is(cause, ErrCancelled) {
		b.cancelled(ctx, run, req, cause)

		return
	}

	run.SnapshotID = summary.SnapshotID
	run.FilesNew = summary.FilesNew
	run.FilesChanged = summary.FilesChanged
	run.TotalFilesProcessed = summary.TotalFilesProcessed
	run.TotalBytesProcessed = summary.TotalBytesProcessed
	run.DataAdded = summary.DataAdded
	run.VSSFellBack = summary.VSSFellBack

	for _, e := range summary.Errors {
		run.UnreadableFiles = append(run.UnreadableFiles, unreadableLine(e.Item, e.Error))
	}

	if backupErr != nil {
		var rerr *restic.Error

		switch {
		case errors.As(backupErr, &rerr) && rerr.Incomplete():
			// A real snapshot with holes. Worth verifying anyway: knowing that
			// the repository is readable is useful even when the snapshot is
			// short of a locked Outlook file.
			run.Outcome = OutcomeIncomplete

			// summary.ErrorCount, not len(summary.Errors): the list is capped
			// and the count is not, and "100 files could not be read" about a
			// machine where four thousand could not is the sort of number
			// somebody acts on and should not have.
			run.Message = fmt.Sprintf("%d files could not be read", summary.ErrorCount)

		case errors.As(backupErr, &rerr) && rerr.Retryable():
			// A stale lock from a laptop suspended mid-backup is the most
			// common way a machine in this fleet quietly stops backing up.
			// Clearing it here means it self-heals overnight instead of
			// waiting for somebody to notice.
			run.Message = "the repository was locked; the lock has been cleared and the next run should succeed"

			if err := b.restic.Unlock(ctx, req.Repository); err != nil {
				run.Message = "the repository is locked and the lock could not be cleared: " + err.Error()
			}

			return

		default:
			run.Message = backupErr.Error()

			return
		}
	} else {
		run.Outcome = OutcomeSuccess
	}

	// Verification. A snapshot that cannot be read back is the alarming case,
	// so it overrides Incomplete rather than being overridden by it.
	if err := b.verify(ctx, req.Repository, nonce, nonceFile); err != nil {
		run.Outcome = OutcomeUnverified
		run.Message = "the backup could not be read back: " + err.Error()

		return
	}

	run.Verified = true

	if run.VSSFellBack && run.Outcome == OutcomeSuccess {
		run.Outcome = OutcomeDegraded
		run.Message = "backed up without a filesystem snapshot, so files that were open may be " +
			"missing or incomplete; run the service as an administrator to restore VSS"
	}

	if run.Message == "" {
		run.Message = fmt.Sprintf("%d files, %s added",
			run.TotalFilesProcessed, humanBytes(run.DataAdded))
	}

	// The nonce has served its purpose. Leaving it would grow the verify
	// directory by a file per run forever, and every one of them would be
	// backed up again the next night.
	if err := b.paths.ClearScratch(); err != nil {
		b.log.Warn("could not clear the scratch directory", "err", err)
	}

	if err := os.Remove(nonceFile); err != nil && !os.IsNotExist(err) {
		b.log.Warn("could not remove the verification file", "path", nonceFile, "err", err)
	}
}

// unlockGrace bounds the tidying-up after a cancelled run. Generous, because
// it is one small request to a bucket and the alternative to waiting is a
// repository nobody can back up to until somebody runs a command.
const unlockGrace = 2 * time.Minute

// cancelled records a run somebody stopped, and leaves the repository usable.
//
// No verification: there is nothing to verify, and asking would fail under a
// context that has just been cancelled — which would put "the backup could not
// be read back", the loudest thing this program says, on a run that was
// stopped on purpose.
func (b *Runner) cancelled(ctx context.Context, run *Run, req Request, cause error) {
	run.Outcome = OutcomeCancelled
	run.Message = cause.Error()

	// restic releases its own lock when it is interrupted, so most of the time
	// this finds nothing to do. It runs anyway for the times it does not: a
	// restic that ignored the interrupt was killed, and a killed one leaves a
	// lock that refuses the next backup. One request to be sure beats a
	// machine that stopped backing up because somebody pressed Cancel.
	//
	// WithoutCancel because the context this is reached through is the
	// cancelled one; a deadline of its own so a bucket that has stopped
	// answering cannot hold a run open.
	clearing, stop := context.WithTimeout(context.WithoutCancel(ctx), unlockGrace)
	defer stop()

	if err := b.restic.Unlock(clearing, req.Repository); err != nil {
		b.log.Warn("could not clear the repository lock after a cancelled backup", "err", err)

		run.Message += "; the repository may still be locked, and the next backup will clear it"
	}
}

// nonceBytes is the size of a verification nonce. 16 bytes is far more than
// enough to make a stale file from a previous run impossible to mistake for a
// fresh one.
const nonceBytes = 16

// writeNonce puts a fresh random value in the verify directory.
func (b *Runner) writeNonce(now time.Time) (nonce []byte, path string, err error) {
	if err := os.MkdirAll(b.paths.Verify, 0o700); err != nil {
		return nil, "", fmt.Errorf("preparing %s: %w", b.paths.Verify, err)
	}

	raw := make([]byte, nonceBytes)
	if _, err := rand.Read(raw); err != nil {
		return nil, "", fmt.Errorf("generating a verification nonce: %w", err)
	}

	nonce = []byte(hex.EncodeToString(raw))

	// One fixed filename rather than a timestamped one. The legacy scripts
	// made a new file per run and never deleted them, so the verify directory
	// grew without bound and every old nonce was re-uploaded nightly.
	path = filepath.Join(b.paths.Verify, "verification.txt")

	// The timestamp goes in the contents, where it is useful for debugging and
	// costs nothing, rather than in the name.
	body := append([]byte(now.UTC().Format(time.RFC3339)+" "), nonce...)

	if err := os.WriteFile(path, body, 0o600); err != nil {
		return nil, "", fmt.Errorf("writing %s: %w", path, err)
	}

	return body, path, nil
}

// verify restores the nonce file and compares it byte for byte.
//
// Byte for byte, and not "the file exists". The legacy Windows script checked
// only existence and left a TODO saying so, which means it would have passed
// against a zero-length file — the exact thing a half-broken restore produces.
//
// # The bytes are the verdict, and restic's exit status is not
//
// This used to return as soon as the restore command failed, which sounds
// like the careful order and is the wrong one. The question a verification
// asks is "did the repository give the bytes back". A non-zero exit from
// `restic restore` is evidence about that question and not an answer to it,
// because restore does more than read: it recreates the directory chain under
// --target and restores ownership on every level of it.
//
// That is what broke a real machine. Backing up /home/someone puts the
// root-owned /home in the snapshot, so a restore running as an ordinary user
// tries to chown it inside the scratch directory and cannot. restic reports
// "ignoring error for /home" and then exits 1 with "There were 1 errors" —
// having ignored it. Every run on that machine was marked unverified for two
// years' worth of nights, over a directory nobody was restoring, while the
// file itself came back perfectly every single time.
//
// The cost of that is not cosmetic. `verified` is what Eumaeus's
// VerifiedSnapshotSince reads, and a machine that never verifies can never
// release its old bucket or have it retired — so a rotation would cut over,
// seed, and stall there with two live buckets.
//
// So the restore error is kept and reported, and the bytes decide. Reading
// the file back is a stronger test than the exit status ever was: it cannot
// pass unless restic actually fetched the pack, decrypted it and wrote the
// content out. A restore that failed in a way that mattered fails the read or
// the comparison below.
func (b *Runner) verify(ctx context.Context, repo restic.Repository, want []byte, source string) error {
	restoreErr := b.restic.Restore(ctx, repo, restic.RestoreOptions{
		Snapshot: "latest",
		Target:   b.paths.Scratch,
		Include:  []string{SnapshotPath(source)},
	})

	// Logged rather than swallowed. Something that makes restore unhappy every
	// night is worth somebody seeing, even when the file came back — it is
	// how the ownership problem above would be noticed next time instead of
	// being discovered from a stalled rotation.
	if restoreErr != nil {
		b.log.Warn("restore reported errors during verification",
			"err", restoreErr,
			"note", "the verification file is checked below; this is not itself a verdict")
	}

	got, err := os.ReadFile(RestoredPath(b.paths.Scratch, source))
	if err != nil {
		// Both, when there are both. The missing file is the failure and the
		// restore error is almost always why, and reporting only the first
		// would send somebody looking in the wrong place.
		if restoreErr != nil {
			return fmt.Errorf(
				"the verification file was not in the restore: %w (the restore also failed: %w)",
				err, restoreErr)
		}

		return fmt.Errorf("the verification file was not in the restore: %w", err)
	}

	if string(got) != string(want) {
		return fmt.Errorf("the restored verification file does not match what was backed up "+
			"(%d bytes restored, %d written)", len(got), len(want))
	}

	return nil
}

// SnapshotPath converts a local absolute path to the form restic stores and
// matches against.
//
// On Windows restic drops the drive's colon: C:\Users\x becomes /C/Users/x.
// Getting this wrong does not produce an error — `--include` simply matches
// nothing, the restore succeeds with an empty result, and verification fails
// for a reason that has nothing to do with the backup. That is a bad failure
// to debug, which is why this is an exported function with its own tests
// rather than three lines inside verify.
//
// Windows paths are recognised by their shape rather than by runtime.GOOS, so
// the conversion is testable from any machine — and so a backslash in a
// filename on Linux, which is legal there, is left alone rather than being
// turned into a directory separator.
func SnapshotPath(local string) string {
	p := local

	if isWindowsPath(p) {
		p = strings.ReplaceAll(p, `\`, "/")

		// C:/Users/x -> C/Users/x
		if len(p) >= 2 && p[1] == ':' {
			p = p[:1] + p[2:]
		}
	} else {
		p = filepath.ToSlash(p)
	}

	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}

	return p
}

// isWindowsPath reports whether s has a drive letter or is a UNC path.
func isWindowsPath(s string) bool {
	if strings.HasPrefix(s, `\\`) {
		return true
	}

	return len(s) >= 3 && s[1] == ':' && (s[2] == '\\' || s[2] == '/') &&
		(s[0] >= 'A' && s[0] <= 'Z' || s[0] >= 'a' && s[0] <= 'z')
}

// RestoredPath is where restic puts a given source path under a restore
// target: the snapshot path, appended to the target directory.
func RestoredPath(target, source string) string {
	return filepath.Join(target, filepath.FromSlash(strings.TrimPrefix(SnapshotPath(source), "/")))
}

// unreadableLine renders one per-file failure the way the list on the status
// page is read: the path first, then why.
//
// restic names the file twice. The JSON error carries `item`, and its message
// names the same path again inside a syscall sentence, so joining the two
// produced lines like
//
//	/home/user/Pictures/2026/DSC_7447.jpg open /home/user/Pictures/2026/DSC_7447.jpg: permission denied
//
// A real machine here listed 309 of those in one run. The path leads because
// the question somebody brings to that list is "which of my files are not in
// the backup", and the reason follows it.
//
// The syscall verb goes with the duplicate. "open" and "lstat" say how restic
// touched the file rather than why it could not have it, and at 309
// repetitions the shorter sentence is the one that gets read.
func unreadableLine(item, msg string) string {
	item, msg = strings.TrimSpace(item), strings.TrimSpace(msg)

	switch {
	case item == "":
		return msg
	case msg == "":
		return item
	}

	// The last occurrence, because one sentence can name a path twice -- a
	// failed rename reports both ends of it, and the reason follows the second.
	if i := strings.LastIndex(msg, item); i >= 0 {
		reason := strings.TrimLeft(strings.TrimSpace(msg[i+len(item):]), ": ")
		if reason == "" {
			return item
		}

		return item + ": " + reason
	}

	return item + ": " + msg
}

// firstOf returns the first element, or "".
func firstOf(s []string) string {
	if len(s) == 0 {
		return ""
	}

	return s[0]
}

// humanBytes renders a byte count for a status line.
func humanBytes(n int64) string {
	const unit = 1024

	if n < unit {
		return fmt.Sprintf("%d B", n)
	}

	div, exp := int64(unit), 0

	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}

	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
