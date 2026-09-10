package fleetbus_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/jroedel/sion-backup/business/domain/fleet/fleetbus"
)

// reporter answers with whatever err is set for the event's run.
type reporter struct {
	err  map[string]error
	sent []string
}

func (r *reporter) Report(_ context.Context, e fleetbus.Event) error {
	if err := r.err[e.RunUUID]; err != nil {
		return err
	}

	r.sent = append(r.sent, e.RunUUID)

	return nil
}

func pending(uuids ...string) []fleetbus.Pending {
	var out []fleetbus.Pending

	for i, u := range uuids {
		out = append(out, fleetbus.Pending{ID: int64(i + 1), Event: fleetbus.Event{RunUUID: u}})
	}

	return out
}

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestAnOfflineMachineKeepsItsHistory is the ordinary case: nothing lands,
// nothing is marked, and the whole list is still waiting when the lid opens in
// the office.
func TestAnOfflineMachineKeepsItsHistory(t *testing.T) {
	offline := errors.New("dial tcp: no route to host")
	r := &reporter{err: map[string]error{"b": offline}}

	var marked []int64

	sent, err := fleetbus.NewBusiness(r, quiet()).Flush(context.Background(), pending("a", "b", "c"),
		func(_ context.Context, id int64) error {
			marked = append(marked, id)

			return nil
		})

	if !errors.Is(err, offline) {
		t.Errorf("got %v, want the network error", err)
	}

	if sent != 1 {
		t.Errorf("sent %d, want 1", sent)
	}

	// Stopped at the failure rather than reporting c around b: the dashboard
	// sees a machine's history in order or not at all.
	if len(marked) != 1 || marked[0] != 1 {
		t.Errorf("marked %v, want just run 1", marked)
	}
}

// TestARejectedEventIsDroppedRatherThanRetriedForever is the reason
// ErrRejected exists. An event the server cannot parse will never parse, and
// Flush sends in order and stops at the first failure — so holding on to one
// would block every later run behind it, every hour, until somebody read the
// log.
func TestARejectedEventIsDroppedRatherThanRetriedForever(t *testing.T) {
	rejected := fmt.Errorf("eumaeusfleet: reporting run b: %w: no run_uuid", fleetbus.ErrRejected)
	r := &reporter{err: map[string]error{"b": rejected}}

	var marked []int64

	sent, err := fleetbus.NewBusiness(r, quiet()).Flush(context.Background(), pending("a", "b", "c"),
		func(_ context.Context, id int64) error {
			marked = append(marked, id)

			return nil
		})

	if err != nil {
		t.Fatalf("Flush: %v", err)
	}

	if len(marked) != 3 {
		t.Errorf("marked %v, want all three: a rejected event must not be sent again", marked)
	}

	// The count is what lands on the dashboard, and the rejected one did not.
	if sent != 2 {
		t.Errorf("sent %d, want 2", sent)
	}

	if len(r.sent) != 2 || r.sent[0] != "a" || r.sent[1] != "c" {
		t.Errorf("delivered %v, want a and c", r.sent)
	}
}

// TestNopReportsSuccessfully covers the machine with no server: every call
// site reports unconditionally, and this is what makes that safe.
func TestNopReportsSuccessfully(t *testing.T) {
	if err := fleetbus.NewBusiness(nil, quiet()).Report(context.Background(), fleetbus.Event{}); err != nil {
		t.Errorf("got %v", err)
	}
}
