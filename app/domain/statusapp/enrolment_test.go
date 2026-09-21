package statusapp_test

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestTheSetupPageSaysHowLongACodeLasts.
//
// This page is what somebody is looking at when they decide to go and fetch
// an enrolment code, and a code fetched before the installer has been
// downloaded is a code that expires while it waits. The fifteen minutes used
// to live only in the help of two commands that the person holding the code
// has not necessarily run.
func TestTheSetupPageSaysHowLongACodeLasts(t *testing.T) {
	h := notEnrolled(t)

	body := h.get(t, "/setup").Body.String()

	if !strings.Contains(body, "fifteen minutes") {
		t.Error("the page does not say how long an enrolment code lasts")
	}

	if !strings.Contains(body, "used once") {
		t.Error("the page does not say a code can only be used once")
	}
}

// TestTheStatusPageSaysWhatEnrollingNeeds. The other page a machine with no
// credentials is looked at on.
func TestTheStatusPageSaysWhatEnrollingNeeds(t *testing.T) {
	h := notEnrolled(t)

	// With a plan, so the page renders the block that describes where this
	// machine's credentials come from. Without one it is the setup page's
	// job, which the test above covers.
	if err := h.plan.Put(context.Background(), samplePlan(), time.Now()); err != nil {
		t.Fatal(err)
	}

	body := h.get(t, "/").Body.String()

	if !strings.Contains(body, "fifteen minutes") {
		t.Error("the status page does not say how long an enrolment code lasts")
	}
}
