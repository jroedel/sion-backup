package statusapp_test

import (
	"context"
	"errors"
	"net/http"
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

	h := harnessWithDisclosures(t, stubSource{}, nil, func(cfg *statusapp.Config) {
		cfg.IssueCard = func(context.Context) (statusapp.RestoreCard, error) {
			calls++

			return card, err
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

// TestAPrintedCardThatTheServerDoesNotKnowAboutSaysSo. The card in somebody's
// hand is complete; what failed is the fleet's record that one was printed,
// and the difference matters because the banner will keep asking.
func TestAPrintedCardThatTheServerDoesNotKnowAboutSaysSo(t *testing.T) {
	card := sampleCard()
	card.Unrecorded = "This computer could not tell the server that a card was printed."

	h, _ := issuing(t, card, nil)

	body := h.post(t, "/card", "").Body.String()

	if !strings.Contains(body, "the server was not told") {
		t.Error("the page does not say the fleet was not told")
	}

	// And still prints the card, which is the whole point of it not being an
	// error.
	if !strings.Contains(body, "restic-password-not-real") {
		t.Error("a card the server was not told about was not shown")
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
