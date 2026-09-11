package statusapp_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/sion-backup/business/domain/plan/planbus"
)

// unconfirmed is a machine as enrolment leaves it: credentials, a repository,
// and a plan nobody at the machine has answered for.
func unconfirmed() planbus.Plan {
	p := samplePlan()
	p.ConfirmedAt = time.Time{}

	return p
}

// TestAnEnrolledMachineIsHeldUntilSomebodyAnswers is the whole point of the
// setup page, asserted from the outside: the status page says so in large
// type, and it says it about a machine that is otherwise perfectly healthy.
func TestAnEnrolledMachineIsHeldUntilSomebodyAnswers(t *testing.T) {
	h := newHarness(t)

	if err := h.plan.Put(context.Background(), unconfirmed(), time.Now()); err != nil {
		t.Fatal(err)
	}

	body := h.get(t, "/").Body.String()

	for _, want := range []string{"Waiting for you", "/setup"} {
		if !strings.Contains(body, want) {
			t.Errorf("the status page does not mention %q:\n%s", want, body)
		}
	}

	// And it must not claim a scheduled run that will not happen.
	if strings.Contains(body, "Next scheduled run") {
		t.Error("the status page promised a scheduled run on a machine that is holding")
	}
}

// TestTheSetupPageRenders covers every branch of a template that is only ever
// seen once, by somebody who has never seen this program before. A template
// error here is a blank page at exactly the wrong moment.
func TestTheSetupPageRenders(t *testing.T) {
	h := newHarness(t)

	if err := h.plan.Put(context.Background(), unconfirmed(), time.Now()); err != nil {
		t.Fatal(err)
	}

	rec := h.get(t, "/setup")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}

	body := rec.Body.String()

	for _, want := range []string{
		"What should be backed up",
		"Or choose the folders myself",
		"Every hour",
		"Three times a day",
		"Start backing up",
		"Your connection",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the setup page is missing %q", want)
		}
	}
}

// TestAMachineWithNowhereToWriteSaysSoRatherThanOfferingChoices: the page is
// reachable before enrolment, and offering somebody a folder picker for a
// backup that has no destination would waste their time twice.
func TestAMachineWithNowhereToWriteSaysSoRatherThanOfferingChoices(t *testing.T) {
	h := newHarness(t)

	body := h.get(t, "/setup").Body.String()

	if !strings.Contains(body, "has not been enrolled") {
		t.Errorf("the setup page does not say the machine is not enrolled:\n%s", body)
	}

	if strings.Contains(body, "Start backing up") {
		t.Error("the setup page offered to start backing up a machine with no repository")
	}
}

// TestConfirmingReleasesTheSchedulerAndStartsTheFirstBackup is the button.
func TestConfirmingReleasesTheSchedulerAndStartsTheFirstBackup(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if err := h.plan.Put(ctx, unconfirmed(), time.Now()); err != nil {
		t.Fatal(err)
	}

	rec := h.post(t, "/setup", "action=confirm&style=custom&targets=/home/jeff"+
		"&schedule=thrice-daily&junk=on")

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}

	plan, err := h.plan.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if !plan.Confirmed() {
		t.Fatal("pressing “Start backing up” did not confirm the plan")
	}

	if got := plan.Schedule.Preset(); got != planbus.PresetThriceDaily {
		t.Errorf("schedule preset %q, want three times a day", got)
	}

	if len(plan.Targets) != 1 || plan.Targets[0] != "/home/jeff" {
		t.Errorf("targets %v, want the one that was typed", plan.Targets)
	}

	// The button says "start backing up", so it starts a backup. A machine
	// that then sat idle until one in the afternoon would not have done what
	// the button said.
	if h.started != 1 {
		t.Errorf("%d runs were started, want 1", h.started)
	}
}

// TestSavingWithoutConfirmingKeepsTheMachineHeld is the other button: somebody
// changing an answer to see what it does to the estimate has not said yes.
func TestSavingWithoutConfirmingKeepsTheMachineHeld(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if err := h.plan.Put(ctx, unconfirmed(), time.Now()); err != nil {
		t.Fatal(err)
	}

	rec := h.post(t, "/setup", "action=save&style=custom&targets=/home/jeff&schedule=daily&daily_time=07:30")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}

	plan, err := h.plan.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if plan.Confirmed() {
		t.Error("saving confirmed the plan; only the other button may do that")
	}

	if got := plan.Schedule.DailyTime(); got != "07:30" {
		t.Errorf("the chosen time is %q, want 07:30", got)
	}

	if h.started != 0 {
		t.Error("saving started a backup")
	}
}

// TestTheJunkExcludesGoOnAndComeOffAgain: the checkbox has to be reversible,
// and it must leave alone anything the person wrote themselves.
func TestTheJunkExcludesGoOnAndComeOffAgain(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	plan := unconfirmed()
	plan.Excludes = []string{"/home/jeff/scratch"}

	if err := h.plan.Put(ctx, plan, time.Now()); err != nil {
		t.Fatal(err)
	}

	h.post(t, "/setup", "action=save&style=custom&targets=/home/jeff&schedule=daily&daily_time=13:00&junk=on")

	with, err := h.plan.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(with.Excludes) < 2 {
		t.Fatalf("ticking the box added nothing: %v", with.Excludes)
	}

	if !contains(with.Excludes, "*.iso") || !contains(with.Excludes, "node_modules") {
		t.Errorf("the junk list is not in the excludes: %v", with.Excludes)
	}

	h.post(t, "/setup", "action=save&style=custom&targets=/home/jeff&schedule=daily&daily_time=13:00")

	without, err := h.plan.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if contains(without.Excludes, "*.iso") {
		t.Errorf("unticking the box left the junk list behind: %v", without.Excludes)
	}

	if !contains(without.Excludes, "/home/jeff/scratch") {
		t.Errorf("unticking the box took away a pattern somebody wrote: %v", without.Excludes)
	}
}

// TestAnEmptyFolderListIsRefusedWithoutLosingTheForm.
//
// A backup of nothing exits zero and reports success, which is the worst
// outcome this program can produce — so it is refused here as well as in
// planbus, with a sentence rather than a validation error.
func TestAnEmptyFolderListIsRefusedWithoutLosingTheForm(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if err := h.plan.Put(ctx, unconfirmed(), time.Now()); err != nil {
		t.Fatal(err)
	}

	rec := h.post(t, "/setup", "action=confirm&style=custom&targets=&schedule=daily&daily_time=13:00")

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want the page back with the problem on it", rec.Code)
	}

	if !strings.Contains(rec.Body.String(), "nothing to back up") {
		t.Errorf("the page does not say why it refused:\n%s", rec.Body)
	}

	plan, err := h.plan.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if plan.Confirmed() {
		t.Error("a plan with no folders was confirmed")
	}
}

// TestTheSizeLimitAndTheMeteredSwitchAreStored covers the two extra options
// the page offers, both of which reach restic and the scheduler rather than
// stopping at the form.
func TestTheSizeLimitAndTheMeteredSwitchAreStored(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if err := h.plan.Put(ctx, unconfirmed(), time.Now()); err != nil {
		t.Fatal(err)
	}

	h.post(t, "/setup", "action=save&style=custom&targets=/home/jeff&schedule=hourly"+
		"&skip_larger_than_gb=2&skip_on_metered=on")

	plan, err := h.plan.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}

	switch {
	case plan.SkipLargerThanGB != 2:
		t.Errorf("size limit %d, want 2", plan.SkipLargerThanGB)
	case !plan.SkipOnMetered:
		t.Error("the metered switch was not stored")
	case plan.Schedule.Preset() != planbus.PresetHourly:
		t.Errorf("schedule %q, want hourly", plan.Schedule.Preset())
	}

	// And empty means no limit, rather than a parse error that silently keeps
	// the old one.
	h.post(t, "/setup", "action=save&style=custom&targets=/home/jeff&schedule=hourly&skip_larger_than_gb=")

	if plan, _ = h.plan.Get(ctx); plan.SkipLargerThanGB != 0 {
		t.Errorf("an empty size limit left %d behind", plan.SkipLargerThanGB)
	}
}

// TestTheSetupPageCannotBeDrivenFromAnotherSite: it changes what a computer
// backs up, so it is behind the same guard as everything else that does.
func TestTheSetupPageCannotBeDrivenFromAnotherSite(t *testing.T) {
	h := newHarness(t)

	if err := h.plan.Put(context.Background(), unconfirmed(), time.Now()); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"/setup", "/setup/speed", "/setup/measure"} {
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7391"+path,
			strings.NewReader("form_token="+h.guard.Token()+"&style=custom&targets=/etc"))
		req.Host = "127.0.0.1:7391"
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		// The token is right, and it is not enough: a page on another site
		// cannot read it, but a browser sends this header whether or not the
		// page could. See app/sdk/loopback.
		req.Header.Set("Sec-Fetch-Site", "cross-site")

		rec := httptest.NewRecorder()
		h.server.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Errorf("%s from another site: status %d, want 403", path, rec.Code)
		}
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}

	return false
}

// TestChoosingAnOfferedStyleWritesItsFolders is the ordinary path: somebody
// picks a radio button rather than typing a list, and what lands in the plan is
// that choice's folders.
func TestChoosingAnOfferedStyleWritesItsFolders(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if err := h.plan.Put(ctx, unconfirmed(), time.Now()); err != nil {
		t.Fatal(err)
	}

	rec := h.post(t, "/setup", "action=confirm&style="+string(h.offered.Style)+
		"&schedule=daily&daily_time=13:00")

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}

	plan, err := h.plan.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(plan.Targets) != 1 || plan.Targets[0] != h.offered.Roots[0] {
		t.Errorf("targets %v, want the chosen style's folders %v", plan.Targets, h.offered.Roots)
	}

	if plan.Style != string(h.offered.Style) {
		t.Errorf("style %q, want %q", plan.Style, h.offered.Style)
	}
}

// TestAStyleThatIsNotOnThePageIsRefused. The folders come from the server's own
// list of choices, never from the form, so a made-up style must not fall
// through to an empty target list.
func TestAStyleThatIsNotOnThePageIsRefused(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if err := h.plan.Put(ctx, unconfirmed(), time.Now()); err != nil {
		t.Fatal(err)
	}

	rec := h.post(t, "/setup", "action=confirm&style=everything&schedule=daily&daily_time=13:00")

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want the page back with the problem on it", rec.Code)
	}

	plan, err := h.plan.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if plan.Confirmed() {
		t.Error("a style that is not on the page was accepted and confirmed")
	}
}
