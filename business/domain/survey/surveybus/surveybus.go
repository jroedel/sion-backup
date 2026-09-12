// Package surveybus is what this machine could back up, how big it is, and how
// fast it can upload.
//
// It exists for the page somebody sees once, immediately after their computer
// is enrolled, where they choose what is backed up and say yes to it. That
// page has to answer three questions before the answer is worth anything:
//
//	what would be backed up      the folders, named, not a pattern language
//	how much is it               measured on this machine, not guessed
//	how long will it take        measured against this bucket, not assumed
//
// None of those is a fact this program can look up. All three are measured,
// here, while somebody watches — which is why this domain is mostly about
// running slow work in the background and being able to say how far it has got.
//
// # Why the measuring is a state machine and not a function call
//
// Because the page has no JavaScript. The Content-Security-Policy the status
// server sets forbids scripts entirely (see foundation/web, and the reasoning
// in app/domain/statusapp), so there is no way to start a scan and fill in the
// number when it arrives. What there is instead is a page that refreshes
// itself, and a domain that can be asked "how far have you got" on every
// refresh and answer immediately. A walk of a home directory takes seconds to
// minutes; a request that waited for one would time out and take the person's
// choice with it.
//
// Nothing here is persisted. A measurement is worth what it was worth on the
// day, the scan is cheap to redo, and a daemon that has just restarted should
// count again rather than show a figure from a home directory that has since
// changed.
package surveybus

import (
	"context"
	"log/slog"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/jroedel/sion-backup/foundation/dirsize"
	"github.com/jroedel/sion-backup/foundation/s3probe"
)

// State is how far a measurement has got.
type State string

const (
	// StateUnmeasured means nobody has asked yet.
	//
	// It is the ZERO VALUE, and that is load-bearing rather than tidy. Every
	// Sizing and every Upload in this package begins life as a zero struct —
	// a map lookup that missed, a field nothing has written yet — and if
	// "unmeasured" were a word instead of the empty string, all of those would
	// be in a fourth state that no branch handles. That bug existed here: the
	// setup page's automatic speed test never ran, because the check for "has
	// this been measured" compared a freshly zeroed State against the word and
	// found them different.
	StateUnmeasured State = ""

	// StateMeasuring means it is happening now, and the figures in the result
	// are a running total rather than an answer.
	StateMeasuring State = "measuring"

	// StateMeasured means the figures are final.
	StateMeasured State = "measured"

	// StateFailed means it was tried and did not work, and Problem says why.
	StateFailed State = "failed"
)

// String names the zero state, so that a log line says "unmeasured" rather
// than leaving an empty value for somebody to interpret.
func (s State) String() string {
	if s == StateUnmeasured {
		return "unmeasured"
	}

	return string(s)
}

// Sizing is what one backup style would come to.
type Sizing struct {
	State State

	// Result is the walk so far, or the finished one. Result.Partial marks a
	// walk that hit its own limits, which every caller renders as "more than".
	Result dirsize.Result

	// Problem is why it failed, in a sentence somebody can read.
	Problem string

	// shape is the excludes and size limit this figure was measured under.
	// A different shape means the answer is stale — turning "leave out the
	// usual junk" on changes it — and the next request re-measures.
	shape string
}

// Measuring reports whether the figure is still moving.
func (s Sizing) Measuring() bool { return s.State == StateMeasuring }

// Known reports whether there is a figure worth showing, final or not.
func (s Sizing) Known() bool { return s.State == StateMeasuring || s.State == StateMeasured }

// Upload is what the speed probe found.
type Upload struct {
	State   State
	Result  s3probe.Result
	Problem string
}

// Measuring reports whether a probe is in flight.
func (u Upload) Measuring() bool { return u.State == StateMeasuring }

// Known reports whether there is a rate to estimate from.
func (u Upload) Known() bool { return u.State == StateMeasured && u.Result.BytesPerSecond > 0 }

// Estimate is how long it would take to upload n bytes at the measured rate,
// or zero when nothing has been measured.
//
// It is an upper bound and the page says so. restic deduplicates and
// compresses before anything leaves the machine, so the bytes actually sent are
// fewer than the bytes on disk — by a third or better on a folder of
// documents, by almost nothing on a folder of photographs. Modelling that
// would mean guessing at somebody's files; overestimating and saying so is the
// honest way to be wrong.
func (u Upload) Estimate(n int64) time.Duration {
	if !u.Known() {
		return 0
	}

	return u.Result.Seconds(n)
}

// Prober measures the upload rate to this machine's own bucket.
//
// A function rather than the probe package itself, because probing needs the
// S3 credentials and those are fetched from Eumaeus per use by a domain this
// one must not import. The composition root supplies the closure; see the
// daemon.
type Prober func(context.Context) (s3probe.Result, error)

// Business is the survey.
type Business struct {
	log     *slog.Logger
	probe   Prober
	choices []Choice

	mu     sync.Mutex
	sizes  map[Style]Sizing
	upload Upload

	// sizing and probing guard against a second run of work that is already
	// happening. The page refreshes itself every few seconds while a scan is
	// running, and without these each refresh would start another walk of the
	// same home directory.
	sizing  bool
	probing bool
}

// NewBusiness constructs the survey for this machine.
//
// The choices are worked out once and handed in, rather than read off the
// filesystem here: [Choices] is a handful of stat calls against a real home
// directory, and a domain that performs them in its constructor cannot be
// tested without one. The composition root calls Choices; see the daemon.
func NewBusiness(log *slog.Logger, probe Prober, choices []Choice) *Business {
	return &Business{
		log:     log,
		probe:   probe,
		choices: choices,
		sizes:   map[Style]Sizing{},
	}
}

// StyleOf reads a stored list of folders back as the choice that would have
// produced it, so a form can put its radio button where the plan actually is.
//
// The same job [planbus.Schedule.Preset] does for times, and it exists for the
// same reason. A plan written before this page existed — which is every plan
// in the fleet upgrading to it — has folders and no style recorded beside
// them. Without this the page offers a default while somebody's real list sits
// unselected in the box below it, and the first save replaces the one with the
// other without ever saying so.
//
// An empty list is not a style: that caller has a machine with no plan yet,
// and what to offer it is the caller's decision.
func (b *Business) StyleOf(targets []string) Style {
	if len(targets) == 0 {
		return ""
	}

	for _, c := range b.choices {
		if sameRoots(c.Roots, targets) {
			return c.Style
		}
	}

	return StyleCustom
}

// sameRoots compares two folder lists as sets of cleaned paths.
//
// Order and a trailing separator are not differences anybody means: a list
// that has been through a text box is the same list whether or not somebody
// reordered it while they were in there.
func sameRoots(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	have := make(map[string]int, len(a))
	for _, s := range a {
		have[filepath.Clean(s)]++
	}

	for _, s := range b {
		key := filepath.Clean(s)

		have[key]--
		if have[key] < 0 {
			return false
		}
	}

	return true
}

// Choices are the backup styles this machine can offer, in the order they
// should be shown.
func (b *Business) Choices() []Choice { return b.choices }

// Sizing reports what is known about one style.
func (b *Business) Sizing(s Style) Sizing {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.sizes[s]
}

// Upload reports what is known about the connection.
func (b *Business) Upload() Upload {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.upload
}

// Measure makes sure every offered style has a size measured under these
// excludes, starting the work if it is not already running or already done.
//
// Called on every render of the setup page, deliberately. It is cheap when
// there is nothing to do, it is what restarts the walk when somebody changes
// the excludes, and it means the page does not need a separate "start
// measuring" button that somebody would have to be told to press.
//
// The context is the daemon's and not the request's: a walk must not be
// abandoned because the page that started it was refreshed.
func (b *Business) Measure(ctx context.Context, custom, excludes []string, largerThanGB int) {
	opts := dirsize.Options{Excludes: excludes}
	if largerThanGB > 0 {
		opts.LargerThan = int64(largerThanGB) << 30
	}

	wanted := order(b.choices, custom)

	b.mu.Lock()

	if b.sizing {
		b.mu.Unlock()

		return
	}

	var todo []Choice

	for _, c := range wanted {
		if got, ok := b.sizes[c.Style]; !ok || got.shape != shapeOf(c, excludes, largerThanGB) ||
			got.State == StateFailed {

			todo = append(todo, c)
		}
	}

	if len(todo) == 0 {
		b.mu.Unlock()

		return
	}

	b.sizing = true

	for _, c := range todo {
		b.sizes[c.Style] = Sizing{State: StateMeasuring, shape: shapeOf(c, excludes, largerThanGB)}
	}

	b.mu.Unlock()

	go b.measure(ctx, todo, opts, excludes, largerThanGB)
}

// measure walks each style in turn.
//
// In turn rather than at once: walking three trees in parallel on one disk is
// slower than walking them one after another. The order is the one Measure
// assembled — whatever is chosen first, then the offered choices cheapest
// first — so the page has the figure somebody is looking at within a second or
// two and fills the others in behind it.
func (b *Business) measure(ctx context.Context, todo []Choice, opts dirsize.Options,
	excludes []string, largerThanGB int) {
	defer func() {
		b.mu.Lock()
		b.sizing = false
		b.mu.Unlock()
	}()

	for _, c := range todo {
		if ctx.Err() != nil {
			return
		}

		started := time.Now()
		shape := shapeOf(c, excludes, largerThanGB)

		result := dirsize.Walk(ctx, c.Roots, opts, func(sofar dirsize.Result) {
			b.mu.Lock()
			b.sizes[c.Style] = Sizing{State: StateMeasuring, Result: sofar, shape: shape}
			b.mu.Unlock()
		})

		b.mu.Lock()
		b.sizes[c.Style] = Sizing{State: StateMeasured, Result: result, shape: shape}
		b.mu.Unlock()

		b.log.Debug("measured a backup style",
			"style", c.Style, "bytes", result.Bytes, "files", result.Files,
			"unreadable", result.Unreadable, "took", time.Since(started).Round(time.Millisecond))
	}
}

// Probe measures the upload rate, unless one is already being measured.
//
// Started rather than waited on, for the same reason the walks are: this takes
// ten seconds or so against a real bucket and the page has to answer now.
func (b *Business) Probe(ctx context.Context) {
	b.mu.Lock()

	if b.probing || b.probe == nil {
		b.mu.Unlock()

		return
	}

	b.probing = true
	b.upload = Upload{State: StateMeasuring}

	b.mu.Unlock()

	go func() {
		result, err := b.probe(ctx)

		b.mu.Lock()
		defer b.mu.Unlock()

		b.probing = false

		if err != nil {
			b.upload = Upload{State: StateFailed, Problem: err.Error()}
			b.log.Warn("could not measure the upload speed", "err", err)

			return
		}

		b.upload = Upload{State: StateMeasured, Result: result}

		b.log.Info("measured the upload speed",
			"bytes_per_second", int64(result.BytesPerSecond),
			"sent", result.Sent, "took", result.Took.Round(time.Millisecond))
	}()
}

// ProbeOnce measures the upload rate only if it has never been measured.
//
// What the setup page calls on an ordinary render, so that the figure appears
// without anybody asking for it — while the "test it again" button calls
// [Business.Probe], which always measures. The difference matters because a
// probe costs real bytes on somebody's connection, and a page that refreshes
// itself every three seconds while a folder is being counted must not start a
// speed test on each of those refreshes.
func (b *Business) ProbeOnce(ctx context.Context) {
	b.mu.Lock()
	measured := b.upload.State != StateUnmeasured
	b.mu.Unlock()

	if measured {
		return
	}

	b.Probe(ctx)
}

// shapeOf identifies everything a size was measured under: the folders, the
// excludes and the limit.
//
// Per choice rather than one shape for the page, because the folders are part
// of it and only one choice's folders can change: somebody editing the list
// they wrote themselves must not invalidate the two measured answers beside
// it and make the whole page start counting again.
func shapeOf(c Choice, excludes []string, largerThanGB int) string {
	out := make([]byte, 0, 128)

	for _, list := range [][]string{c.Roots, excludes} {
		for _, item := range list {
			out = append(out, item...)
			out = append(out, 0)
		}

		out = append(out, '|')
	}

	return string(out) + strconv.Itoa(largerThanGB)
}

// Remeasure forgets the sizes so the next [Business.Measure] counts again.
//
// Nothing else invalidates on time. A walk is only redone when the settings
// behind it change, which means a figure can be an hour old and describe a
// folder somebody has since emptied — so there is a button, and this is what
// it calls. It does not stop a walk that is already running: that walk is
// measuring the same thing and will finish sooner than a new one.
func (b *Business) Remeasure() {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.sizing {
		return
	}

	clear(b.sizes)
}

// order is the sequence the walks happen in.
//
// The list somebody wrote themselves is measured like the offered choices are,
// and BEFORE them.
//
// It has to be measured at all because on a machine that adopt-enroll set up,
// that list is the only answer on the page, and showing every other option's
// size while leaving the chosen one blank would be exactly backwards. It has to
// be measured first for the same reason: a custom list exists only when
// somebody already has one — out of config.toml, out of a legacy script, or
// typed — and in every one of those cases it is the choice the radio is on.
// Walking the whole home directory before it means the one figure they are
// waiting for is the last to arrive, which on a large home directory is
// minutes.
//
// A free function over its inputs so the ordering can be asserted without
// walking a filesystem, which is the only way to assert it at all: a test that
// waits for two small temporary directories to be measured cannot tell which
// of them was measured first.
func order(choices []Choice, custom []string) []Choice {
	if len(custom) == 0 {
		return choices
	}

	return append([]Choice{{Style: StyleCustom, Roots: custom}}, choices...)
}
