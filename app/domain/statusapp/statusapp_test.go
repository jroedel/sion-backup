package statusapp_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/sion-backup/app/domain/statusapp"
	"github.com/jroedel/sion-backup/app/sdk/loopback"
	"github.com/jroedel/sion-backup/business/domain/backup/backupbus"
	"github.com/jroedel/sion-backup/business/domain/backup/stores/backupdb"
	"github.com/jroedel/sion-backup/business/domain/credential/credentialbus"
	"github.com/jroedel/sion-backup/business/domain/plan/planbus"
	"github.com/jroedel/sion-backup/business/domain/plan/stores/plandb"
	"github.com/jroedel/sion-backup/business/domain/survey/surveybus"
	"github.com/jroedel/sion-backup/foundation/paths"
	"github.com/jroedel/sion-backup/foundation/sqldb"
)

// stubSource stands in for Eumaeus. The status page never fetches credentials
// — it only asks whether a source exists — so this need not return any.
type stubSource struct{}

func (stubSource) Fetch(context.Context) (credentialbus.Set, error) {
	return credentialbus.Set{}, errors.New("the status page must never fetch credentials")
}

type harness struct {
	server  http.Handler
	guard   *loopback.Guard
	plan    *planbus.Business
	backups *backupbus.Runner
	survey  *surveybus.Business
	offered surveybus.Choice
	runs    *backupdb.Store
	started int
}

func newHarness(t *testing.T) *harness { return harnessWith(t, stubSource{}) }

// notEnrolled is a machine with no source to fetch credentials from, which is
// how credentialbus renders one that has never been enrolled.
func notEnrolled(t *testing.T) *harness { return harnessWith(t, nil) }

func harnessWith(t *testing.T, source credentialbus.Source) *harness {
	t.Helper()

	dir := t.TempDir()
	t.Setenv("SION_BACKUP_DATA_DIR", dir)

	p, err := paths.Resolve()
	if err != nil {
		t.Fatal(err)
	}

	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	db, err := sqldb.Open(ctx, p.DB)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { db.Close() })

	for _, migrate := range []func(context.Context, *sqldb.DB) error{plandb.Migrate, backupdb.Migrate} {
		if err := migrate(ctx, db); err != nil {
			t.Fatal(err)
		}
	}

	guard, err := loopback.New("127.0.0.1:7391")
	if err != nil {
		t.Fatal(err)
	}

	runs := backupdb.NewStore(db)

	h := &harness{
		guard:   guard,
		plan:    planbus.NewBusiness(plandb.NewStore(db)),
		runs:    runs,
		backups: backupbus.NewRunner(runs, nil, p, slog.New(slog.NewTextHandler(io.Discard, nil))),
	}

	// A survey with no prober and one made-up choice.
	//
	// No prober because a test must never send megabytes anywhere; the page
	// renders without one, saying the speed has not been measured. A made-up
	// choice because the real [surveybus.Choices] walks this machine's own home
	// directory, and a page test that does that is a page test whose runtime
	// depends on whose machine it is — it timed out on a macOS CI runner,
	// whose /Users/runner holds Xcode and several toolchains.
	h.offered = surveybus.Choice{
		Style: surveybus.StylePersonal,
		Title: "My documents, desktop and pictures",
		Roots: []string{t.TempDir()},
	}

	h.survey = surveybus.NewBusiness(slog.New(slog.NewTextHandler(io.Discard, nil)), nil,
		[]surveybus.Choice{h.offered})

	app, err := statusapp.New(statusapp.Config{
		Plan:        h.plan,
		Backups:     h.backups,
		Credentials: credentialbus.NewBusiness(source),
		Survey:      h.survey,
		Background:  ctx,
		StartRun:    func(context.Context) error { h.started++; return nil },
		Guard:       guard,
		Paths:       p,
		Version:     "test",
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}

	h.server = guard.Wrap(app)

	return h
}

func (h *harness) get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7391"+path, nil)
	req.Host = "127.0.0.1:7391"

	rec := httptest.NewRecorder()
	h.server.ServeHTTP(rec, req)

	return rec
}

func (h *harness) post(t *testing.T, path, body string) *httptest.ResponseRecorder {
	t.Helper()

	form := "form_token=" + h.guard.Token()
	if body != "" {
		form += "&" + body
	}

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7391"+path, strings.NewReader(form))
	req.Host = "127.0.0.1:7391"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")

	rec := httptest.NewRecorder()
	h.server.ServeHTTP(rec, req)

	return rec
}

// record writes a finished run straight into the history, so the page's
// history branches can be rendered without a restic binary.
func (h *harness) record(t *testing.T, r backupbus.Run) {
	t.Helper()

	ctx := context.Background()

	id, err := h.runs.Create(ctx, r)
	if err != nil {
		t.Fatal(err)
	}

	r.ID = id

	if err := h.runs.Finish(ctx, r); err != nil {
		t.Fatal(err)
	}
}

func samplePlan() planbus.Plan {
	return planbus.Plan{
		NodeID:     "office-laptop-1",
		Repository: "s3:https://s3.us-central-1.wasabisys.com/example-node-bucket",
		Targets:    []string{"/home/user"},
		Excludes:   []string{"*.iso"},
		Schedule:   planbus.DefaultSchedule(),

		// Confirmed, because every test using this describes a machine that
		// is already backing up. The setup page's own tests are the ones that
		// start from a plan nobody has answered for yet.
		ConfirmedAt: time.Now().Add(-24 * time.Hour),
	}
}

// TestAnUnconfiguredMachineSaysSo is the first thing an administrator sees
// after installing the binary, and it must not be a stack trace.
func TestAnUnconfiguredMachineSaysSo(t *testing.T) {
	h := newHarness(t)

	rec := h.get(t, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}

	body := rec.Body.String()

	for _, want := range []string{"not backing up", "enroll"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not mention %q", want)
		}
	}
}

// TestTheStatusPageRenders covers every branch of the template that a
// configured machine with history takes. Template errors are runtime errors,
// so this is the only thing standing between a typo and a blank page.
func TestTheStatusPageRenders(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if err := h.plan.Put(ctx, samplePlan(), time.Now()); err != nil {
		t.Fatal(err)
	}

	// A finished run with an unreadable file, so the outcome pill, the summary
	// list and the "not in the backup" section all render.
	h.record(t, backupbus.Run{
		NodeID:              "office-laptop-1",
		Repository:          samplePlan().Repository,
		StartedAt:           time.Now().Add(-20 * time.Minute),
		FinishedAt:          time.Now().Add(-8 * time.Minute),
		Outcome:             backupbus.OutcomeIncomplete,
		Message:             "1 file could not be read",
		SnapshotID:          "a1b2c3d4",
		TotalFilesProcessed: 4211,
		DataAdded:           88 << 20,
		UnreadableFiles:     []string{"/home/user/locked.pst permission denied"},
		Verified:            true,
	})

	rec := h.get(t, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}

	body := rec.Body.String()

	for _, want := range []string{
		"office-laptop-1",
		"example-node-bucket",
		"/home/user",
		"nothing stored here",
		"Back up now",
		"a1b2c3d4",
		"locked.pst",
		"Not in the backup",
		h.guard.Token(),
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not show %q", want)
		}
	}
}

func TestTheSettingsPageRendersAndSaves(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if err := h.plan.Put(ctx, samplePlan(), time.Now()); err != nil {
		t.Fatal(err)
	}

	rec := h.get(t, "/settings")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}

	if !strings.Contains(rec.Body.String(), "/home/user") {
		t.Error("the form does not show the current targets")
	}

	rec = h.post(t, "/settings",
		"targets=%2Fhome%2Fuser%0A%2Fsrv%2Fshared&excludes=*.iso&times=13%3A00%2C+22%3A00&paused=on")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}

	plan, err := h.plan.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(plan.Targets) != 2 || plan.Targets[1] != "/srv/shared" {
		t.Errorf("targets = %v", plan.Targets)
	}

	if len(plan.Schedule.Times) != 2 || plan.Schedule.Times[1] != "22:00" {
		t.Errorf("times = %v", plan.Schedule.Times)
	}

	if !plan.Paused {
		t.Error("the pause checkbox was not applied")
	}
}

// TestSettingsCannotChangeTheNodeOrRepository is the property the form's own
// text promises. Changing either from a web page would detach a machine from
// its own backup history.
func TestSettingsCannotChangeTheNodeOrRepository(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if err := h.plan.Put(ctx, samplePlan(), time.Now()); err != nil {
		t.Fatal(err)
	}

	rec := h.post(t, "/settings",
		"targets=%2Fhome%2Fuser&times=13%3A00&node_id=attacker&repository=s3%3Ahttps%3A%2F%2Fevil.example%2Fbucket")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}

	plan, err := h.plan.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if plan.NodeID != "office-laptop-1" {
		t.Errorf("the node ID was changed to %q", plan.NodeID)
	}

	if !strings.Contains(plan.Repository, "wasabisys") {
		t.Errorf("the repository was changed to %q", plan.Repository)
	}
}

// TestAnInvalidScheduleIsRefusedWithoutLosingTheForm. Losing a carefully
// assembled exclude list to a typo in the times box is how somebody stops
// maintaining it.
func TestAnInvalidScheduleIsRefusedWithoutLosingTheForm(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if err := h.plan.Put(ctx, samplePlan(), time.Now()); err != nil {
		t.Fatal(err)
	}

	rec := h.post(t, "/settings", "targets=%2Fhome%2Fuser&excludes=*.iso%0A*.dmg&times=1pm")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want the form re-rendered", rec.Code)
	}

	body := rec.Body.String()

	if !strings.Contains(body, "Not saved") {
		t.Error("the page does not say it refused")
	}

	if !strings.Contains(body, "*.dmg") {
		t.Error("the excludes the person typed were lost")
	}

	plan, err := h.plan.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(plan.Schedule.Times) != 1 || plan.Schedule.Times[0] != "13:00" {
		t.Errorf("the stored schedule was damaged: %v", plan.Schedule.Times)
	}
}

func TestBackUpNowStartsARun(t *testing.T) {
	h := newHarness(t)

	if err := h.plan.Put(context.Background(), samplePlan(), time.Now()); err != nil {
		t.Fatal(err)
	}

	rec := h.post(t, "/run", "")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}

	if h.started != 1 {
		t.Errorf("StartRun was called %d times, want 1", h.started)
	}
}

// TestTheRotationCardAppearsOnlyWhenItIsWorthIt. The card asks somebody for
// two days of their bandwidth and all of their history, so showing it on a
// three-week-old repository would be worse than not having it.
func TestTheRotationCardAppearsOnlyWhenItIsWorthIt(t *testing.T) {
	const gib = 1 << 30

	cases := []struct {
		name string
		m    planbus.Measurement
		want bool
	}{
		{
			name: "old and bloated",
			m: planbus.Measurement{
				Since:      time.Now().AddDate(0, 0, -200),
				MeasuredAt: time.Now(),
				Now:        340 * gib, Fresh: 150 * gib, Snapshots: 200,
			},
			want: true,
		},
		{
			name: "young",
			m: planbus.Measurement{
				Since:      time.Now().AddDate(0, 0, -20),
				MeasuredAt: time.Now(),
				Now:        340 * gib, Fresh: 150 * gib,
			},
		},
		{
			name: "never measured",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			ctx := context.Background()

			plan := samplePlan()
			if err := h.plan.Put(ctx, plan, time.Now()); err != nil {
				t.Fatal(err)
			}

			if !c.m.MeasuredAt.IsZero() {
				if err := h.plan.RecordMeasurement(ctx, plan.Repository, c.m.MeasuredAt, c.m.Since,
					planbus.Size{Now: c.m.Now, Fresh: c.m.Fresh, Snapshots: c.m.Snapshots}); err != nil {
					t.Fatal(err)
				}
			}

			body := h.get(t, "/").Body.String()

			if got := strings.Contains(body, "A fresh start would free"); got != c.want {
				t.Errorf("card shown = %v, want %v", got, c.want)
			}

			if !c.want {
				return
			}

			// Both halves of the trade, in front of the person being asked.
			for _, want := range []string{"190.0 GiB", "Would be lost", "Would need uploading"} {
				if !strings.Contains(body, want) {
					t.Errorf("the card does not show %q", want)
				}
			}

			// No price is configured in this harness, so no money is quoted.
			if strings.Contains(body, "a month") {
				t.Error("a saving was quoted with no storage price configured")
			}
		})
	}
}

func TestHealthz(t *testing.T) {
	h := newHarness(t)

	rec := h.get(t, "/healthz")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}

	if strings.TrimSpace(rec.Body.String()) != "ok" {
		t.Errorf("body = %q", rec.Body.String())
	}
}

// TestTheStylesheetIsServed. The Content-Security-Policy permits exactly one
// stylesheet and forbids inline styles, so a 404 here is an unstyled page.
func TestTheStylesheetIsServed(t *testing.T) {
	h := newHarness(t)

	rec := h.get(t, "/style.css")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}

	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Errorf("content type %q", ct)
	}
}
