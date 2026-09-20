package statusapp_test

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/sion-backup/app/domain/statusapp"
	"github.com/jroedel/sion-backup/business/domain/credential/credentialbus"
	"github.com/jroedel/sion-backup/business/domain/plan/planbus"
)

// sampleCard is a card with nothing real in it. The three credentials are
// distinct nonsense so that a test can tell which one leaked.
func sampleCard() statusapp.RestoreCard {
	return statusapp.RestoreCard{
		NodeID:           "office-laptop-1",
		OwnerEmail:       "owner@example.org",
		RepositoryURL:    "s3:https://s3.example.com/example-node-bucket",
		RepositoryBucket: "example-node-bucket",
		ResticPassword:   "restic-password-not-real",
		RestoreKeyID:     "restore-key-id-not-real",
		RestoreKeySecret: "restore-key-secret-not-real",
	}
}

// issuing is a machine whose card can be printed, and a count of how many
// times printing it was actually asked for.
func issuing(t *testing.T, card statusapp.RestoreCard, err error) (*harness, *int) {
	t.Helper()

	calls := 0

	var h *harness

	h = harnessWithDisclosures(t, stubSource{}, nil, func(cfg *statusapp.Config) {
		cfg.IssueCard = func(context.Context) (statusapp.RestoreCard, error) {
			calls++

			return card, err
		}

		// Through the harness rather than captured here, so a test can
		// replace it after the server has been built.
		cfg.CardPrinted = func(ctx context.Context, repository string) error {
			if h.cardPrinted == nil {
				return nil
			}

			return h.cardPrinted(ctx, repository)
		}
	})

	if err := h.plan.Put(context.Background(), samplePlan(), time.Now()); err != nil {
		t.Fatal(err)
	}

	return h, &calls
}

// TestTheCardPageOffersWithoutRevealing is the property the whole two-step
// exists for. Landing on the page -- from the banner, a bookmark, a browser
// restoring yesterday's tabs -- must not fetch anybody's credentials and must
// not put a password on screen.
func TestTheCardPageOffersWithoutRevealing(t *testing.T) {
	h, calls := issuing(t, sampleCard(), nil)

	rec := h.get(t, "/card")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}

	body := rec.Body.String()

	for _, secret := range []string{
		"restic-password-not-real",
		"restore-key-id-not-real",
		"restore-key-secret-not-real",
	} {
		if strings.Contains(body, secret) {
			t.Errorf("a GET of the card page revealed %q", secret)
		}
	}

	if *calls != 0 {
		t.Errorf("a GET of the card page fetched credentials %d times", *calls)
	}

	if !strings.Contains(body, "Show the card") {
		t.Error("the card page does not offer the card")
	}
}

// TestPressingTheButtonShowsTheCard is the card itself: the four values
// somebody types into restic, and the two labels that tell them which
// computer and which bucket they are looking at.
func TestPressingTheButtonShowsTheCard(t *testing.T) {
	h, calls := issuing(t, sampleCard(), nil)

	rec := h.post(t, "/card", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}

	body := rec.Body.String()

	for _, want := range []string{
		"RESTIC_REPOSITORY",
		"s3:https://s3.example.com/example-node-bucket",
		"AWS_ACCESS_KEY_ID",
		"restore-key-id-not-real",
		"AWS_SECRET_ACCESS_KEY",
		"restore-key-secret-not-real",
		"RESTIC_PASSWORD",
		"restic-password-not-real",
		"office-laptop-1",
		"owner@example.org",
		"example-node-bucket",
		"restic restore latest --target ./restored",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the printed card does not carry %q", want)
		}
	}

	if *calls != 1 {
		t.Errorf("the button fetched credentials %d times, want 1", *calls)
	}
}

// TestTheCardIsNotCachedAnywhere guards the other half of "shown once". A
// response carrying a restic password must not be stored by anything between
// this process and the screen.
func TestTheCardIsNotCachedAnywhere(t *testing.T) {
	h, _ := issuing(t, sampleCard(), nil)

	rec := h.post(t, "/card", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}

	cache := rec.Header().Get("Cache-Control")
	if !strings.Contains(cache, "no-store") {
		t.Errorf("the card was served with Cache-Control %q, want no-store", cache)
	}
}

// TestACardThatCannotBeBuiltSaysWhy. Every way this fails is a true fact
// about the machine, and the page says it in the sentence the composition
// root wrote rather than showing a blank card or an error page.
func TestACardThatCannotBeBuiltSaysWhy(t *testing.T) {
	h, _ := issuing(t, statusapp.RestoreCard{},
		errors.New("Eumaeus did not return a read-only key pair for this machine"))

	rec := h.post(t, "/card", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}

	body := rec.Body.String()

	if !strings.Contains(body, "read-only key pair") {
		t.Error("the page does not say why there is no card")
	}

	if !strings.Contains(body, "There is no card to print") {
		t.Error("the page does not say that there is no card")
	}
}

// TestShowingACardDoesNotClaimItWasPrinted is the point of the second
// button. `card_issued_at` is read by people deciding whether an owner can
// restore without us, and a render is not a sheet of paper.
func TestShowingACardDoesNotClaimItWasPrinted(t *testing.T) {
	h, _ := issuing(t, sampleCard(), nil)

	recorded := 0

	h.cardPrinted = func(context.Context, string) error {
		recorded++

		return nil
	}

	body := h.post(t, "/card", "").Body.String()

	if recorded != 0 {
		t.Errorf("showing a card told the fleet %d times that one was printed", recorded)
	}

	if !strings.Contains(body, "/card/printed") {
		t.Error("the card page does not offer to record that it printed")
	}
}

// TestSayingItPrintedIsWhatRecordsIt, and the page that follows says so.
func TestSayingItPrintedIsWhatRecordsIt(t *testing.T) {
	h, _ := issuing(t, sampleCard(), nil)

	var gotRepository string

	recorded := 0

	h.cardPrinted = func(_ context.Context, repository string) error {
		recorded++
		gotRepository = repository

		return nil
	}

	rec := h.post(t, "/card/printed", "repository="+url.QueryEscape(sampleCard().RepositoryURL))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}

	if recorded != 1 {
		t.Errorf("the fleet was told %d times, want once", recorded)
	}

	if gotRepository != sampleCard().RepositoryURL {
		t.Errorf("recorded against %q, want the repository the card named", gotRepository)
	}

	if got := rec.Header().Get("Location"); got != "/card?filed" {
		t.Errorf("redirected to %q", got)
	}

	if !strings.Contains(h.get(t, "/card?filed").Body.String(), "stop asking") {
		t.Error("the page does not confirm that the card was recorded")
	}
}

// TestAFiledCardIsNotAlsoAskedFor: two true sentences about different
// moments, stacked on one page.
//
// "Recorded -- this computer will stop asking" sat directly above "This
// computer's card has not been printed", because the second is read from the
// stored state the daemon's poller wrote and the poller had not run since.
// Whichever one a person believes, the page has told them the other.
func TestAFiledCardIsNotAlsoAskedFor(t *testing.T) {
	h, _ := issuing(t, sampleCard(), nil)

	// A machine the server last said was owed a card, which is every machine
	// at the moment somebody prints one.
	poll(t, h, context.Background(), planbus.MachineState{
		Card: planbus.Card{State: planbus.CardNever},
	})

	rec := h.post(t, "/card/printed", "repository="+url.QueryEscape(sampleCard().RepositoryURL))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}

	body := h.get(t, "/card?filed").Body.String()

	if !strings.Contains(body, "stop asking") {
		t.Error("the page does not confirm the card was recorded")
	}

	if strings.Contains(body, "has not been printed") {
		t.Error("the page confirms the card and asks for it in the same breath")
	}
}

// TestFilingACardAsksTheServerAgain is the fix for the same thing one layer
// down: the stored state is refreshed on the way to that page, so the status
// page stops asking too rather than waiting for the next poll.
func TestFilingACardAsksTheServerAgain(t *testing.T) {
	refreshed := 0

	h := harnessWithDisclosures(t, stubSource{}, nil, func(cfg *statusapp.Config) {
		cfg.IssueCard = func(context.Context) (statusapp.RestoreCard, error) {
			return sampleCard(), nil
		}

		cfg.CardPrinted = func(context.Context, string) error { return nil }
		cfg.RefreshState = func(context.Context) { refreshed++ }
	})

	if err := h.plan.Put(context.Background(), samplePlan(), time.Now()); err != nil {
		t.Fatal(err)
	}

	if rec := h.post(t, "/card/printed", "repository=whatever"); rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d", rec.Code)
	}

	if refreshed != 1 {
		t.Errorf("the server was asked again %d times, want once", refreshed)
	}
}

// TestACardTheFleetWouldNotAcceptSaysWhy. The paper in somebody's hand is
// fine; what failed is the record, and the sentence has to say which -- a
// person who has just filed a card and is told "error" will go and print
// another one.
func TestACardTheFleetWouldNotAcceptSaysWhy(t *testing.T) {
	h, _ := issuing(t, sampleCard(), nil)

	h.cardPrinted = func(context.Context, string) error {
		return errors.New("the card is fine, but this computer could not reach the server")
	}

	rec := h.post(t, "/card/printed", "repository=whatever")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}

	if !strings.Contains(rec.Body.String(), "the card is fine") {
		t.Error("the page does not say what went wrong")
	}
}

// TestAnUnenrolledMachineIsNotOfferedACard. There is nothing to print and
// nothing to fetch, and a button that can only apologise is worse than a
// sentence that explains.
func TestAnUnenrolledMachineIsNotOfferedACard(t *testing.T) {
	h := harnessWithDisclosures(t, nil, nil, func(cfg *statusapp.Config) {
		cfg.Credentials = credentialbus.NewBusiness(nil)
		cfg.IssueCard = func(context.Context) (statusapp.RestoreCard, error) {
			t.Error("an unenrolled machine asked for credentials")

			return statusapp.RestoreCard{}, nil
		}
	})

	body := h.get(t, "/card").Body.String()

	if strings.Contains(body, "Show the card") {
		t.Error("an unenrolled machine was offered a card to print")
	}

	if !strings.Contains(body, "not enrolled") {
		t.Error("the page does not say why there is no card")
	}
}

// TestTheBannerCarriesAButtonToTheCard is the reason this page exists. The
// banner used to end with "run sion-backup card", which is an instruction to
// go and find somebody else for everybody it was written for.
func TestTheBannerCarriesAButtonToTheCard(t *testing.T) {
	for _, path := range []string{"/", "/rotation"} {
		srv := &eumaeus{}
		h, ctx := rotating(t, srv)

		poll(t, h, ctx, planbus.MachineState{
			Bucket: onBucket(30 * 24 * time.Hour),
			Card:   planbus.Card{State: planbus.CardNever},
		})

		body := h.get(t, path).Body.String()

		if !strings.Contains(body, `href="/card"`) {
			t.Errorf("%s tells somebody to print a card without linking to the page", path)
		}

		if strings.Contains(body, "sion-backup card") {
			t.Errorf("%s still sends the owner to a terminal", path)
		}
	}
}
