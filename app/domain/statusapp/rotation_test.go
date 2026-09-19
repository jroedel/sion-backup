package statusapp_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/sion-backup/app/domain/statusapp"
	"github.com/jroedel/sion-backup/business/domain/machine/machinebus"
	"github.com/jroedel/sion-backup/business/domain/plan/planbus"
)

// eumaeus records the four calls a machine makes about its own repository, and
// answers with whatever the test has set.
type eumaeus struct {
	state machinebus.State

	requested []machinebus.Measured
	accepted  []string

	requestErr error
	cutoverErr error
}

func (e *eumaeus) State(context.Context) (machinebus.State, error) { return e.state, nil }

func (e *eumaeus) RequestRotation(_ context.Context, m machinebus.Measured) error {
	if e.requestErr != nil {
		return e.requestErr
	}

	e.requested = append(e.requested, m)

	return nil
}

func (e *eumaeus) Cutover(_ context.Context, url string) error {
	if e.cutoverErr != nil {
		return e.cutoverErr
	}

	e.accepted = append(e.accepted, url)

	return nil
}

func (e *eumaeus) ReleaseOldBucket(_ context.Context, url string) (machinebus.Released, error) {
	return machinebus.Released{URL: url}, nil
}

func (e *eumaeus) CardIssued(context.Context, string, time.Time) error { return nil }

// rotating builds a machine wired to the stub server above, with a plan and
// whatever state the server is pretending to hold.
func rotating(t *testing.T, srv *eumaeus) (*harness, context.Context) {
	t.Helper()

	h := harnessWithDisclosures(t, stubSource{}, nil, func(c *statusapp.Config) {
		c.Machine = machinebus.NewBusiness(srv)
		c.Metered = func(context.Context) (bool, string) { return false, "" }
	})

	ctx := context.Background()

	if err := h.plan.Put(ctx, samplePlan(), time.Now()); err != nil {
		t.Fatal(err)
	}

	return h, ctx
}

// poll writes what the daemon's poller would have written.
func poll(t *testing.T, h *harness, ctx context.Context, state planbus.MachineState) {
	t.Helper()

	state.AskedAt = time.Now()

	if err := h.plan.RecordMachineState(ctx, state); err != nil {
		t.Fatal(err)
	}
}

func onBucket(age time.Duration) planbus.Bucket {
	return planbus.Bucket{
		URL:       samplePlan().Repository,
		CreatedAt: time.Now().Add(-age),
		State:     planbus.StateActive,
	}
}

// TestAskingForAFreshBucketSendsTheFiguresWithIt.
//
// The ask is a work item for a person, and the figures travel with it so that
// whoever reads the queue sees what the owner was shown when they clicked.
func TestAskingForAFreshBucketSendsTheFiguresWithIt(t *testing.T) {
	const gib = 1 << 30

	srv := &eumaeus{}
	h, ctx := rotating(t, srv)

	plan := samplePlan()
	if err := h.plan.RecordMeasurement(ctx, plan.Repository, time.Now(),
		time.Now().AddDate(0, 0, -400),
		planbus.Size{Now: 340 * gib, Fresh: 150 * gib, Snapshots: 400}); err != nil {
		t.Fatal(err)
	}

	rec := h.post(t, "/rotation/request", "")
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}

	if len(srv.requested) != 1 {
		t.Fatalf("the server was asked %d times, want once", len(srv.requested))
	}

	got := srv.requested[0]

	if got.RepositoryURL != plan.Repository {
		t.Errorf("the request named %q", got.RepositoryURL)
	}

	if got.ReclaimableBytes != 190*gib {
		t.Errorf("ReclaimableBytes = %d, want %d", got.ReclaimableBytes, 190*gib)
	}

	if got.FreshBytes != 150*gib {
		t.Errorf("FreshBytes = %d, want %d", got.FreshBytes, 150*gib)
	}

	// And the page says what did and did not happen, because "asked" and
	// "a bucket now exists" are very different things to be told.
	body := rec.Body.String()

	for _, want := range []string{"Asked", "Nothing is created", "nothing starts uploading"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not say %q", want)
		}
	}
}

// TestAskingTwiceIsNotAFailure. Three server states answer 409 here, and the
// third is the one worth saying out loud: a bucket may already be waiting, in
// which case what is outstanding is this computer's own decision.
func TestAskingTwiceIsNotAFailure(t *testing.T) {
	srv := &eumaeus{requestErr: machinebus.ErrAlreadyAsked}
	h, _ := rotating(t, srv)

	body := h.post(t, "/rotation/request", "").Body.String()

	if strings.Contains(body, "That did not work") {
		t.Error("a second ask is shown as a failure")
	}

	if !strings.Contains(body, "already been asked") {
		t.Error("the page does not say somebody has already been asked")
	}
}

// TestAnOfferIsShownAndNeverTaken.
//
// The whole design in one test: a bucket provisioned for this machine appears,
// with what accepting would cost, and nothing has moved until the button is
// pressed.
func TestAnOfferIsShownAndNeverTaken(t *testing.T) {
	srv := &eumaeus{}
	h, ctx := rotating(t, srv)

	offered := &planbus.Offered{
		URL:       "s3:https://s3.us-central-1.wasabisys.com/example-node-2027",
		Provider:  "wasabi",
		Region:    "us-central-1",
		Bucket:    "example-node-2027",
		OfferedAt: time.Now().AddDate(0, 0, -9),
	}

	poll(t, h, ctx, planbus.MachineState{Bucket: onBucket(400 * 24 * time.Hour), Offer: offered})

	page := h.get(t, "/rotation").Body.String()

	for _, want := range []string{"example-node-2027", "us-central-1", "Start moving"} {
		if !strings.Contains(page, want) {
			t.Errorf("the page does not show %q", want)
		}
	}

	if len(srv.accepted) != 0 {
		t.Fatal("rendering the page accepted the offer")
	}

	// The front page says so too, because somebody has done work and is
	// waiting for an answer.
	if !strings.Contains(h.get(t, "/").Body.String(), "waiting for this computer") {
		t.Error("the front page does not mention the waiting bucket")
	}

	rec := h.post(t, "/rotation/cutover", "repository_url="+offered.URL)
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}

	if len(srv.accepted) != 1 || srv.accepted[0] != offered.URL {
		t.Fatalf("accepted = %q, want the offered URL once", srv.accepted)
	}

	if !strings.Contains(rec.Body.String(), "leave this computer on") {
		t.Error("the page does not say what the person now has to do")
	}
}

// TestACutoverNamesTheBucketFromTheForm.
//
// What the person clicked is what is sent. An offer replaced between the page
// being rendered and the button being pressed is the server's to refuse, and
// it refuses with a sentence naming both buckets; a client that sent its own
// idea of the current offer would move a laptop somewhere nobody was shown.
func TestACutoverNamesTheBucketFromTheForm(t *testing.T) {
	srv := &eumaeus{}
	h, ctx := rotating(t, srv)

	poll(t, h, ctx, planbus.MachineState{
		Bucket: onBucket(400 * 24 * time.Hour),
		Offer:  &planbus.Offered{URL: "s3:https://s3.example.invalid/april-bucket"},
	})

	h.post(t, "/rotation/cutover", "repository_url=s3:https://s3.example.invalid/march-bucket")

	if len(srv.accepted) != 1 || !strings.Contains(srv.accepted[0], "march-bucket") {
		t.Fatalf("accepted = %q, want what the form said", srv.accepted)
	}
}

// TestACutoverWithNoBucketNamedDoesNothing.
func TestACutoverWithNoBucketNamedDoesNothing(t *testing.T) {
	srv := &eumaeus{}
	h, _ := rotating(t, srv)

	if body := h.post(t, "/rotation/cutover", "").Body.String(); !strings.Contains(body, "named no bucket") {
		t.Error("the page does not say the form named nothing")
	}

	if len(srv.accepted) != 0 {
		t.Fatal("a form naming no bucket accepted one anyway")
	}
}

// TestACutoverAlreadyUnderWayIsNotAnOffer.
//
// A machine that is being moved has no button to press; what it has is a
// warning that its backups will be slow for a while and that it should be left
// on. Showing the "accept" form here would invite somebody to start the thing
// that is already happening.
func TestACutoverAlreadyUnderWayIsNotAnOffer(t *testing.T) {
	srv := &eumaeus{}
	h, ctx := rotating(t, srv)

	b := onBucket(10 * 24 * time.Hour)
	b.State = planbus.StateCuttingOver

	poll(t, h, ctx, planbus.MachineState{Bucket: b})

	page := h.get(t, "/rotation").Body.String()

	if !strings.Contains(page, "moving to a new bucket now") {
		t.Error("the page does not say a move is under way")
	}

	if strings.Contains(page, "Start moving to the new bucket") {
		t.Error("the page offers to start a move that is already running")
	}

	if !strings.Contains(h.get(t, "/").Body.String(), "Moving to a new bucket") {
		t.Error("the front page does not say a move is under way")
	}
}

// TestTheCardIsAskedForWhenTheServerSaysSo, and the two states say different
// things: one prints a card, the other prints a card and destroys the old.
func TestTheCardIsAskedForWhenTheServerSaysSo(t *testing.T) {
	for _, c := range []struct {
		state string
		want  string
	}{
		{planbus.CardNever, "Nobody has printed"},
		{planbus.CardSuperseded, "destroy the old page"},
	} {
		t.Run(c.state, func(t *testing.T) {
			srv := &eumaeus{}
			h, ctx := rotating(t, srv)

			poll(t, h, ctx, planbus.MachineState{
				Bucket: onBucket(30 * 24 * time.Hour),
				Card:   planbus.Card{State: c.state},
			})

			if !strings.Contains(h.get(t, "/").Body.String(), c.want) {
				t.Errorf("the front page does not say %q", c.want)
			}
		})
	}

	// And says nothing at all when the card is in hand.
	srv := &eumaeus{}
	h, ctx := rotating(t, srv)

	poll(t, h, ctx, planbus.MachineState{
		Bucket: onBucket(30 * 24 * time.Hour),
		Card:   planbus.Card{State: planbus.CardIssued, IssuedAt: time.Now()},
	})

	if strings.Contains(h.get(t, "/").Body.String(), "needs printing") {
		t.Error("a machine whose card is printed is told to print one")
	}
}

// TestTheMeteredWarningIsOnThePageThatAsksForBandwidth.
//
// The one fact the server cannot have and the one that most often decides the
// answer. A person about to commit to days of upload is told, in the place
// where they are being asked.
func TestTheMeteredWarningIsOnThePageThatAsksForBandwidth(t *testing.T) {
	srv := &eumaeus{}

	h := harnessWithDisclosures(t, stubSource{}, nil, func(c *statusapp.Config) {
		c.Machine = machinebus.NewBusiness(srv)
		c.Metered = func(context.Context) (bool, string) { return true, "the operating system" }
	})

	ctx := context.Background()
	if err := h.plan.Put(ctx, samplePlan(), time.Now()); err != nil {
		t.Fatal(err)
	}

	poll(t, h, ctx, planbus.MachineState{Bucket: onBucket(400 * 24 * time.Hour)})

	page := h.get(t, "/rotation").Body.String()

	if !strings.Contains(page, "paying for these bytes right now") {
		t.Error("the page does not warn about the metered connection")
	}
}

// TestAPageThatCannotAskSaysSo.
//
// A three-week-old answer with no indication that asking has been failing for
// three weeks is worse than no answer: it reads as "nobody has provisioned
// anything" when it means "this computer has not been able to ask".
func TestAPageThatCannotAskSaysSo(t *testing.T) {
	srv := &eumaeus{}
	h, ctx := rotating(t, srv)

	poll(t, h, ctx, planbus.MachineState{Bucket: onBucket(30 * 24 * time.Hour)})

	if err := h.plan.RecordStateFailure(ctx, "dial tcp: no route to host"); err != nil {
		t.Fatal(err)
	}

	page := h.get(t, "/rotation").Body.String()

	if !strings.Contains(page, "no route to host") {
		t.Error("the page does not say why the last attempt failed")
	}
}

// TestNoButtonOnAMachineThatIsNotEnrolled. There is nobody to ask.
func TestNoButtonOnAMachineThatIsNotEnrolled(t *testing.T) {
	h := harnessWithDisclosures(t, nil, nil)

	ctx := context.Background()
	if err := h.plan.Put(ctx, samplePlan(), time.Now()); err != nil {
		t.Fatal(err)
	}

	page := h.get(t, "/rotation").Body.String()

	if !strings.Contains(page, "disabled") {
		t.Error("an unenrolled machine is offered a button that cannot work")
	}

	if body := h.post(t, "/rotation/request", "").Body.String(); !strings.Contains(body, "not enrolled") {
		t.Error("asking from an unenrolled machine does not say why it cannot")
	}
}
