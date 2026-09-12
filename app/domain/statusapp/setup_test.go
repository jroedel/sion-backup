package statusapp_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
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

	rec := h.post(t, "/setup", "action=confirm&style=custom&targets="+url.QueryEscape(h.dir)+""+
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

	if len(plan.Targets) != 1 || plan.Targets[0] != h.dir {
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

	rec := h.post(t, "/setup", "action=save&style=custom&targets="+url.QueryEscape(h.dir)+"&schedule=daily&daily_time=07:30")
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

	h.post(t, "/setup", "action=save&style=custom&targets="+url.QueryEscape(h.dir)+"&schedule=daily&daily_time=13:00&junk=on")

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

	h.post(t, "/setup", "action=save&style=custom&targets="+url.QueryEscape(h.dir)+"&schedule=daily&daily_time=13:00")

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

	h.post(t, "/setup", "action=save&style=custom&targets="+url.QueryEscape(h.dir)+"&schedule=hourly"+
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
	h.post(t, "/setup", "action=save&style=custom&targets="+url.QueryEscape(h.dir)+"&schedule=hourly&skip_larger_than_gb=")

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

// checkedStyle reports which "what should be backed up" radio the page has
// selected, which is the only part of this that somebody actually sees.
func checkedStyle(t *testing.T, body string) string {
	t.Helper()

	for _, style := range []string{"personal", "home", "custom"} {
		if strings.Contains(body, `name="style" value="`+style+`" checked`) {
			return style
		}
	}

	return ""
}

// TestAnUpgradedMachineIsShownTheFoldersItIsActuallyBackingUp.
//
// The bug this is for shipped, and it was found by upgrading a machine and
// looking at the page. Every plan written before this page existed has folders
// and no style recorded beside them, and the migration marks those confirmed
// so they keep backing up. The page read "confirmed" as "somebody has answered
// this before", skipped filling the form in from the plan, and selected its
// own first option — while the machine's real folder list sat unselected in
// the box underneath.
//
// Nothing warned about it. The first press of "Start backing up" would have
// swapped a list somebody had been backing up for months for a default.
func TestAnUpgradedMachineIsShownTheFoldersItIsActuallyBackingUp(t *testing.T) {
	h := newHarness(t)

	// Exactly what plandb's migration leaves: targets, no style, and confirmed
	// as of whenever the plan was last written.
	plan := samplePlan()
	plan.Style = ""
	plan.Targets = []string{"/srv/work", "/home/user/Photos"}

	if err := h.plan.Put(context.Background(), plan, time.Now()); err != nil {
		t.Fatal(err)
	}

	body := h.get(t, "/setup").Body.String()

	if got := checkedStyle(t, body); got != "custom" {
		t.Errorf("the page has %q selected for a machine backing up its own list of "+
			"folders, want custom — one save here replaces that list", got)
	}

	for _, dir := range plan.Targets {
		if !strings.Contains(body, dir) {
			t.Errorf("the page does not show %q, which this machine backs up", dir)
		}
	}
}

// TestAnUpgradedMachineOnAnOfferedListIsShownThatOption is the other half: a
// stored list that happens to be one of the choices on the page is that
// choice, not a custom one. Otherwise somebody who picked "my documents" is
// shown their own folders typed out in the box below, which reads like the
// setting has been lost.
func TestAnUpgradedMachineOnAnOfferedListIsShownThatOption(t *testing.T) {
	h := newHarness(t)

	plan := samplePlan()
	plan.Style = ""
	plan.Targets = h.offered.Roots

	if err := h.plan.Put(context.Background(), plan, time.Now()); err != nil {
		t.Fatal(err)
	}

	if got := checkedStyle(t, h.get(t, "/setup").Body.String()); got != "personal" {
		t.Errorf("style %q selected, want personal", got)
	}
}

// TestExclusionsSomebodyAlreadyHasAreShownAndKept.
//
// There is no exclusions box on this page — the Settings page owns that — and
// a page that carries them along in silence is indistinguishable from one that
// has dropped them. Somebody who cannot see their exclusions assumes they are
// gone, and the sensible thing to do about that is to stop using the page.
func TestExclusionsSomebodyAlreadyHasAreShownAndKept(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	plan := samplePlan()
	plan.Style = ""
	plan.Excludes = []string{"*.iso", "/srv/scratch"}

	if err := h.plan.Put(ctx, plan, time.Now()); err != nil {
		t.Fatal(err)
	}

	body := h.get(t, "/setup").Body.String()

	for _, pattern := range plan.Excludes {
		if !strings.Contains(body, pattern) {
			t.Errorf("the page does not mention %q, which this machine is leaving out", pattern)
		}
	}

	// And a save through the page keeps them, junk or no junk.
	h.post(t, "/setup", "form_token="+h.guard.Token()+
		"&style=custom&targets="+url.QueryEscape(h.dir)+"&junk=on&schedule=daily&daily_time=13:00")

	saved, err := h.plan.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}

	for _, pattern := range plan.Excludes {
		if !contains(saved.Excludes, pattern) {
			t.Errorf("saving the setup page dropped the exclusion %q", pattern)
		}
	}
}

// TestTheJunkListIsNotAddedToAPlanNobodyAskedToChange.
//
// A machine that has been backing up its whole home directory for a year did
// not ask, on the day it was upgraded, to start leaving parts of it out. The
// checkbox is a default offered to a machine nobody has answered for.
func TestTheJunkListIsNotAddedToAPlanNobodyAskedToChange(t *testing.T) {
	h := newHarness(t)

	plan := samplePlan()
	plan.Style = ""
	plan.Excludes = nil

	if err := h.plan.Put(context.Background(), plan, time.Now()); err != nil {
		t.Fatal(err)
	}

	if strings.Contains(h.get(t, "/setup").Body.String(), `name="junk" checked`) {
		t.Error("the junk list is ticked for a machine that is already backing up " +
			"without it, so the next save quietly starts leaving things out")
	}
}

// TestAnExclusionThatLooksLikeJunkIsNotDeletedForLookingLikeIt.
//
// The checkbox is all-or-nothing: it reports itself ticked only when the whole
// junk list is present. Taking it off, though, removed any pattern on that
// list — so a plan carrying one of them and nothing else showed an unticked
// box, and the first save through this page deleted the pattern. Nothing on
// the screen changed. Somebody's exclusion was simply gone.
func TestAnExclusionThatLooksLikeJunkIsNotDeletedForLookingLikeIt(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	plan := samplePlan()
	plan.Style = ""
	plan.Excludes = []string{"*.iso"} // written by hand, and also on the junk list

	if err := h.plan.Put(ctx, plan, time.Now()); err != nil {
		t.Fatal(err)
	}

	// Saved without touching the checkbox, which is how it renders: unticked,
	// because one pattern out of thirty is not the junk list.
	body := h.get(t, "/setup").Body.String()
	if strings.Contains(body, `name="junk" checked`) {
		t.Fatal("one junk pattern reported as the whole set; this test no longer " +
			"describes the situation it was written for")
	}

	if !strings.Contains(body, "*.iso") {
		t.Error("the page does not show the exclusion this machine has")
	}

	h.post(t, "/setup", "form_token="+h.guard.Token()+
		"&style=custom&targets="+url.QueryEscape(h.dir)+"&schedule=daily&daily_time=13:00")

	saved, err := h.plan.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if !contains(saved.Excludes, "*.iso") {
		t.Errorf("saving deleted the exclusion; the plan now excludes %v", saved.Excludes)
	}
}

// TestAMachineWithARepositoryButNoEnrolmentIsNotOfferedTheForm.
//
// The two can come apart: a repository URL written into config.toml by hand
// and no token to fetch a credential with. Such a machine cannot back up —
// credentialbus refuses before restic is started — but the page checked only
// for the repository, so it offered the whole form. It walked the home
// directory, put "this machine is not enrolled" in the small print under a
// failed speed test, and left a button reading "Start backing up" at the
// bottom of it.
func TestAMachineWithARepositoryButNoEnrolmentIsNotOfferedTheForm(t *testing.T) {
	h := notEnrolled(t)

	if err := h.plan.Put(context.Background(), unconfirmed(), time.Now()); err != nil {
		t.Fatal(err)
	}

	body := h.get(t, "/setup").Body.String()

	if !strings.Contains(body, "has not been enrolled") {
		t.Errorf("the page does not say the machine is not enrolled:\n%s", body)
	}

	if strings.Contains(body, "Start backing up") {
		t.Error("the page offered to start backing up a machine that cannot fetch " +
			"a credential")
	}
}

// TestAFolderThatIsNotThereIsRefusedWithoutLosingTheForm.
//
// The whole reason this check is on the page and not only in doctor: somebody
// adding /opt/projects to their list is standing right there, and a typo is
// free to fix now and expensive to find in March.
func TestAFolderThatIsNotThereIsRefusedWithoutLosingTheForm(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if err := h.plan.Put(ctx, unconfirmed(), time.Now()); err != nil {
		t.Fatal(err)
	}

	typo := filepath.Join(h.dir, "projcts")

	rec := h.post(t, "/setup", "action=confirm&style=custom&targets="+
		url.QueryEscape(h.dir+"\n"+typo)+"&schedule=daily&daily_time=13:00")

	body := rec.Body.String()

	if !strings.Contains(body, typo) {
		t.Errorf("the page does not say which folder is wrong:\n%s", body)
	}

	if !strings.Contains(body, "is not on this computer") {
		t.Error("the page does not say what is wrong with it")
	}

	// And the good one is still in the box. Losing a folder list to a
	// validation error is how somebody decides the page is not worth using.
	if !strings.Contains(body, h.dir) {
		t.Error("the rest of the typed list was lost")
	}

	plan, err := h.plan.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if plan.Confirmed() {
		t.Error("a plan naming a folder that is not there was confirmed")
	}

	if h.started != 0 {
		t.Error("a backup was started for a plan that was refused")
	}
}

// TestTheResultingFolderListIsShown.
//
// The three radio buttons describe an intention; this is the consequence.
// "Everything in my user folder" and "/home/jeff, minus /home/jeff/Downloads"
// are the same sentence only to somebody who already knows.
func TestTheResultingFolderListIsShown(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	root := t.TempDir()
	inside := filepath.Join(root, "Downloads")
	elsewhere := "/somewhere/else/entirely"

	plan := unconfirmed()
	plan.Style = "custom"
	plan.Targets = []string{root}
	plan.Excludes = []string{inside, elsewhere, "*.iso"}

	if err := h.plan.Put(ctx, plan, time.Now()); err != nil {
		t.Fatal(err)
	}

	body := h.get(t, "/setup").Body.String()

	for _, want := range []string{"What this comes to", "Backing up", "Leaving out", root, inside, "*.iso"} {
		if !strings.Contains(body, want) {
			t.Errorf("the resulting list does not show %q", want)
		}
	}

	// An exclusion that cannot match anything in the selection is kept, and
	// kept apart: listing it beside the ones that apply is three lines of
	// irrelevance, and dropping it reads as having thrown it away. That the
	// narrowing itself is right is asserted in surveybus, where the rule is.
	if !strings.Contains(body, "Also kept") {
		t.Error("an exclusion that cannot apply here was dropped from the page entirely")
	}

	if !strings.Contains(body, elsewhere) {
		t.Errorf("the page does not mention %q at all, so it reads as having lost it", elsewhere)
	}
}

// TestAnOfferedFolderThatIsMissingIsReportedRatherThanRefused.
//
// A machine with no ~/Videos is not somebody's mistake to fix. Refusing to
// save over it would leave them with a page that cannot be used and no way to
// tell why; saying so beside the folder is the whole of what needs doing.
func TestAnOfferedFolderThatIsMissingIsReportedRatherThanRefused(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if err := h.plan.Put(ctx, unconfirmed(), time.Now()); err != nil {
		t.Fatal(err)
	}

	// The harness offers one real folder; give the plan a style pointing at it
	// plus one that is not there, as PersonalFolders does on a machine with no
	// Videos directory.
	missing := filepath.Join(h.dir, "Videos")

	plan, err := h.plan.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}

	plan.Style = "custom"
	plan.Targets = []string{h.dir, missing}

	if err := h.plan.Put(ctx, plan, time.Now()); err != nil {
		t.Fatal(err)
	}

	body := h.get(t, "/setup").Body.String()

	if !strings.Contains(body, "is not on this computer") {
		t.Error("a folder that is not there was shown without a word about it")
	}

	if rec := h.get(t, "/setup"); rec.Code != http.StatusOK {
		t.Errorf("the page itself failed: %d", rec.Code)
	}
}
