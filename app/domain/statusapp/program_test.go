package statusapp_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/sion-backup/app/domain/statusapp"
)

// programHarness is a machine whose two program buttons are wired to whatever
// a test wants them to do.
func programHarness(t *testing.T,
	check func(context.Context) (statusapp.UpdateOutcome, error),
	restart func(context.Context) (time.Duration, error)) *harness {

	t.Helper()

	return harnessWithDisclosures(t, stubSource{}, nil, func(c *statusapp.Config) {
		c.UpdateSource = func() (string, []string) {
			return "github.com/jroedel/sion-backup", nil
		}

		c.CheckForUpdate = check
		c.Restart = restart
	})
}

// TestAnInstalledVersionIsNotTheRunningOne is the distinction the whole
// section exists to make.
//
// A program cannot replace itself while it is running, so "installed" and
// "running" are two different versions for as long as it takes somebody to
// press the second button. A page that showed only one of them would be
// telling half the truth at the one moment both halves matter.
func TestAnInstalledVersionIsNotTheRunningOne(t *testing.T) {
	h := programHarness(t,
		func(context.Context) (statusapp.UpdateOutcome, error) {
			return statusapp.UpdateOutcome{Installed: "v9.9.9"}, nil
		},
		func(context.Context) (time.Duration, error) { return 30 * time.Second, nil })

	body := h.post(t, "/settings/update", "").Body.String()

	for _, want := range []string{"v9.9.9", "was installed", "Restart"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not say %q", want)
		}
	}

	// The running version, still, and said so. Reading only "v9.9.9 was
	// installed" somebody would reasonably conclude they are running it.
	if !strings.Contains(body, "still running") {
		t.Error("the page does not say which version is still running")
	}
}

// TestNothingToInstallIsAnAnswer covers the common press.
//
// Most of the time there is no newer version, and a button that does nothing
// visible when nothing needed doing is a button people stop trusting.
func TestNothingToInstallIsAnAnswer(t *testing.T) {
	h := programHarness(t,
		func(context.Context) (statusapp.UpdateOutcome, error) {
			return statusapp.UpdateOutcome{}, nil
		},
		func(context.Context) (time.Duration, error) { return 0, nil })

	body := h.post(t, "/settings/update", "").Body.String()

	if !strings.Contains(body, "newest release") {
		t.Errorf("a check that found nothing said nothing: %s", excerpt(body))
	}
}

// TestUpdatesBeingOffIsNotAFailure keeps a chosen configuration out of the
// error path.
//
// "Updates are switched off" is a deployment somebody meant. Rendering it
// through the same route as a corrupt download would have the page shout at a
// person about a setting they chose on purpose.
func TestUpdatesBeingOffIsNotAFailure(t *testing.T) {
	h := programHarness(t,
		func(context.Context) (statusapp.UpdateOutcome, error) {
			return statusapp.UpdateOutcome{
				Unavailable: "Updates are switched off for this machine in its config.toml.",
			}, nil
		},
		func(context.Context) (time.Duration, error) { return 0, nil })

	rec := h.post(t, "/settings/update", "")

	if rec.Code >= 500 {
		t.Fatalf("status = %d, want a page rather than a failure", rec.Code)
	}

	if !strings.Contains(rec.Body.String(), "switched off") {
		t.Errorf("the page does not say why nothing happened: %s", excerpt(rec.Body.String()))
	}
}

// TestARestartIsRefusedWhileABackupIsRunning is the refusal that matters.
//
// The restart is an exit, and an exit part-way through a backup abandons it:
// the next run starts from the beginning, and on a domestic uplink that is
// somebody's evening. The refusal has to reach the page as a sentence rather
// than as a failure, because it is a true and temporary fact about the
// machine and the answer is "press it again in a minute".
func TestARestartIsRefusedWhileABackupIsRunning(t *testing.T) {
	refusal := errors.New("a backup is running. Restarting now would abandon it part-way")

	h := programHarness(t,
		func(context.Context) (statusapp.UpdateOutcome, error) {
			return statusapp.UpdateOutcome{}, nil
		},
		func(context.Context) (time.Duration, error) { return 0, refusal })

	rec := h.post(t, "/settings/restart", "")

	if rec.Code >= 500 {
		t.Fatalf("status = %d, want a page rather than a failure", rec.Code)
	}

	body := rec.Body.String()

	if !strings.Contains(body, "Not restarted") || !strings.Contains(body, "a backup is running") {
		t.Errorf("the refusal is not on the page: %s", excerpt(body))
	}
}

// TestTheRestartPageComesBackByItself is why this one page reloads.
//
// The process is about to stop, so a redirect would be served by something
// that no longer exists and the browser would show a connection error at the
// exact moment somebody wants reassurance. Instead the response is a full
// page that says what is happening and reloads itself once the service
// manager has had time to start the replacement.
func TestTheRestartPageComesBackByItself(t *testing.T) {
	h := programHarness(t,
		func(context.Context) (statusapp.UpdateOutcome, error) {
			return statusapp.UpdateOutcome{}, nil
		},
		func(context.Context) (time.Duration, error) { return 30 * time.Second, nil })

	body := h.post(t, "/settings/restart", "").Body.String()

	if !strings.Contains(body, "Restarting") {
		t.Fatalf("the page does not say it is restarting: %s", excerpt(body))
	}

	if !strings.Contains(body, "http-equiv=\"refresh\"") {
		t.Error("the page does not reload itself, so it would sit there dead")
	}

	// Later than the service manager's own wait, so the reload does not race
	// the start it is waiting for and report a failure most times it runs.
	if !strings.Contains(body, "content=\"40\"") {
		t.Errorf("the reload does not leave a margin over the 30s restart: %s", excerpt(body))
	}
}

// TestTheButtonsNeedTheSameGuardAsEverythingElse.
//
// Both are unsafe methods on a page with no login. The loopback guard is what
// stops any site in the browser posting to them, and a route added without it
// is a route that can restart somebody's backup daemon from a web page.
func TestTheButtonsNeedTheSameGuardAsEverythingElse(t *testing.T) {
	h := programHarness(t,
		func(context.Context) (statusapp.UpdateOutcome, error) {
			t.Error("the check ran without passing the guard")

			return statusapp.UpdateOutcome{}, nil
		},
		func(context.Context) (time.Duration, error) {
			t.Error("the restart ran without passing the guard")

			return 0, nil
		})

	for _, path := range []string{"/settings/update", "/settings/restart"} {
		if rec := h.postUnguarded(t, path); rec.Code < 400 {
			t.Errorf("POST %s without the guard = %d, want a refusal", path, rec.Code)
		}
	}
}

// postUnguarded posts with no form token, which is what any other page in the
// browser would manage.
func (h *harness) postUnguarded(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7391"+path, strings.NewReader(""))
	req.Host = "127.0.0.1:7391"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()
	h.server.ServeHTTP(rec, req)

	return rec
}

func excerpt(body string) string {
	if len(body) > 400 {
		return body[:400] + "…"
	}

	return body
}

// TestTheWaitIsSaidInUnitsSomebodyIsWaitingIn.
//
// planbus.Span counts in days, because it was written for the age of a
// bucket. Asked about a thirty-second restart it answers "0 days", which is
// what this page told somebody the first time it ran.
func TestTheWaitIsSaidInUnitsSomebodyIsWaitingIn(t *testing.T) {
	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		{30 * time.Second, "in about 30 seconds"},
		{5 * time.Second, "in about 5 seconds"},
		{5 * time.Minute, "within about 5 minutes"},
	} {
		h := programHarness(t,
			func(context.Context) (statusapp.UpdateOutcome, error) {
				return statusapp.UpdateOutcome{}, nil
			},
			func(context.Context) (time.Duration, error) { return tc.in, nil })

		body := h.post(t, "/settings/restart", "").Body.String()

		if !strings.Contains(body, tc.want) {
			t.Errorf("a %s restart does not say %q: %s", tc.in, tc.want, excerpt(body))
		}

		if strings.Contains(body, "0 days") {
			t.Errorf("a %s restart is described in days", tc.in)
		}
	}
}
