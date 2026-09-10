package diagbus_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/jroedel/sion-backup/business/domain/diag/diagbus"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// queue is an in-memory Queue.
type queue struct {
	items []diagbus.Queued
	n     int
}

func (q *queue) Put(r diagbus.Report) error {
	q.n++
	q.items = append(q.items, diagbus.Queued{ID: fmt.Sprintf("%d", q.n), Report: r})

	return nil
}

// List copies, as a real store does. Handing out the live slice and then
// mutating it in Remove made this fake, not the code under test, skip
// entries -- which is a nice illustration of why the store is a directory
// listing rather than a slice somebody holds a reference to.
func (q *queue) List() ([]diagbus.Queued, error) {
	return append([]diagbus.Queued(nil), q.items...), nil
}

func (q *queue) Remove(id string) error {
	for i, item := range q.items {
		if item.ID == id {
			q.items = append(q.items[:i], q.items[i+1:]...)

			return nil
		}
	}

	return nil
}

// sink answers with whatever is set for a report's step.
type sink struct {
	err  map[string]error
	sent []string
}

func (s *sink) Send(_ context.Context, r diagbus.Report) error {
	if err := s.err[r.Step]; err != nil {
		return err
	}

	s.sent = append(s.sent, r.Step)

	return nil
}

// TestScrubRewritesHomeDirectories. A report carries this program's own error
// text, and an error string quotes whatever path it was given — so the
// backstop has to work on all three platforms, and has to leave the shape of
// the path behind, because "it could not read something under Documents" is
// the half that helps.
func TestScrubRewritesHomeDirectories(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"could not read /home/gonzalo/Documents/tax.pdf", "could not read ~/Documents/tax.pdf"},
		{"could not read /Users/fj/Desktop/notes.txt", "could not read ~/Desktop/notes.txt"},
		{`open C:\Users\Gonzalo\AppData\Local\x: denied`, `open ~\AppData\Local\x: denied`},

		// Left alone: a repository URL is not somebody's home directory, and
		// a redactor that eats them teaches people to stop reading reports.
		{
			"s3:https://s3.us-central-1.wasabisys.com/bucket is unreachable",
			"s3:https://s3.us-central-1.wasabisys.com/bucket is unreachable",
		},
		{"restic exited with status 3", "restic exited with status 3"},
	} {
		if got := diagbus.Scrub(c.in); got != c.want {
			t.Errorf("Scrub(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

// TestRecordFillsInWhatEveryReportNeeds, so no caller has to remember to.
func TestRecordFillsInWhatEveryReportNeeds(t *testing.T) {
	q := &queue{}

	if err := diagbus.NewBusiness(q, nil, "v1.4.0", quiet()).Record(diagbus.Report{
		Kind:   diagbus.KindPanic,
		Detail: "panic: nil map read at /home/someone/x",
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	got := q.items[0].Report

	switch {
	case got.Agent != "v1.4.0":
		t.Errorf("agent = %q", got.Agent)
	case got.OS == "":
		t.Error("no OS recorded")
	case got.OccurredAt.IsZero():
		t.Error("no time recorded")
	case strings.Contains(got.Detail, "/home/someone"):
		t.Errorf("the detail was not scrubbed: %q", got.Detail)
	}
}

// TestRecordSurvivesHavingNowhereToPutIt. Record is called from a panic
// handler and from an installer that is already dealing with a failure. A
// machine so broken it has no data directory must not crash a second time on
// the way out.
func TestRecordSurvivesHavingNowhereToPutIt(t *testing.T) {
	if err := diagbus.NewBusiness(nil, nil, "v1", quiet()).Record(diagbus.Report{
		Kind: diagbus.KindPanic, Detail: "x",
	}); err != nil {
		t.Errorf("got %v, want a quiet nil", err)
	}

	var nilBusiness *diagbus.Business

	if err := nilBusiness.Record(diagbus.Report{Kind: diagbus.KindPanic}); err != nil {
		t.Errorf("got %v", err)
	}
}

// TestARejectedReportIsDroppedAndTheRestGoOn. The queue is capped, so one
// report the server will never accept must not sit at the head of it holding
// everything else back.
func TestARejectedReportIsDroppedAndTheRestGoOn(t *testing.T) {
	q := &queue{}
	s := &sink{err: map[string]error{
		"bad": fmt.Errorf("eumaeusdiag: %w: no kind", diagbus.ErrRejected),
	}}

	b := diagbus.NewBusiness(q, s, "v1", quiet())

	for _, step := range []string{"first", "bad", "last"} {
		if err := b.Record(diagbus.Report{Kind: diagbus.KindInstallFailed, Step: step, Detail: "x"}); err != nil {
			t.Fatal(err)
		}
	}

	sent, err := b.Flush(context.Background())
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}

	if sent != 2 {
		t.Errorf("sent = %d, want 2 (the rejected one does not count as sent)", sent)
	}

	if b.Waiting() != 0 {
		t.Errorf("%d still waiting; a rejected report was kept", b.Waiting())
	}

	if len(s.sent) != 2 || s.sent[0] != "first" || s.sent[1] != "last" {
		t.Errorf("delivered %v", s.sent)
	}
}

// TestAnUnreachableServerKeepsTheQueue, which is the state every machine is in
// today: the endpoint does not exist yet and answers 404.
func TestAnUnreachableServerKeepsTheQueue(t *testing.T) {
	q := &queue{}
	offline := errors.New("eumaeusapi: not found")
	s := &sink{err: map[string]error{"only": offline}}

	b := diagbus.NewBusiness(q, s, "v1", quiet())

	if err := b.Record(diagbus.Report{Kind: diagbus.KindPanic, Step: "only", Detail: "x"}); err != nil {
		t.Fatal(err)
	}

	sent, err := b.Flush(context.Background())

	if !errors.Is(err, offline) {
		t.Errorf("got %v, want the transport error", err)
	}

	if sent != 0 || b.Waiting() != 1 {
		t.Errorf("sent %d, %d waiting; the report should have been kept", sent, b.Waiting())
	}
}

// TestFlushWithNoSinkIsQuiet: an unenrolled machine still records, and must
// not report an error for having nowhere to send yet.
func TestFlushWithNoSinkIsQuiet(t *testing.T) {
	b := diagbus.NewBusiness(&queue{}, nil, "v1", quiet())

	sent, err := b.Flush(context.Background())
	if sent != 0 || err != nil {
		t.Errorf("sent %d, err %v", sent, err)
	}
}
