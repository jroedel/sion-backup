package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/jroedel/sion-backup/app/domain/statusapp"
	"github.com/jroedel/sion-backup/app/sdk/loopback"
	"github.com/jroedel/sion-backup/business/domain/backup/backupbus"
	"github.com/jroedel/sion-backup/business/domain/credential/credentialbus"
	"github.com/jroedel/sion-backup/business/domain/diag/diagbus"
	"github.com/jroedel/sion-backup/business/domain/fleet/fleetbus"
	"github.com/jroedel/sion-backup/business/domain/plan/planbus"
	"github.com/jroedel/sion-backup/business/domain/survey/surveybus"
	"github.com/jroedel/sion-backup/foundation/restic"
	"github.com/jroedel/sion-backup/foundation/selfupdate"
	"github.com/jroedel/sion-backup/foundation/web"
)

// tick is how often the scheduler asks whether a run is due.
//
// A minute. The schedule's resolution is a minute, so anything finer only
// wakes the machine up more often — and on a laptop, waking up is not free.
const tick = time.Minute

// flushEvery is how often unreported runs are pushed to the fleet dashboard.
//
// Five minutes, rather than only after a run, because the case this exists for
// is a machine that backed up while offline: the run finished hours ago and
// the network came back since. Trying more often would be polling a server
// that has nothing to say.
const flushEvery = 5 * time.Minute

// shutdownGrace is how long the HTTP server is given to finish in flight
// requests. Short: they are page renders on loopback.
const shutdownGrace = 5 * time.Second

func daemonCmd(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	addr := fs.String("addr", "", "listen address for the status page (default "+DefaultAddr+")")
	verbose := fs.Bool("v", false, "verbose logging")
	noSchedule := fs.Bool("no-schedule", false, "serve the status page but never start a run")

	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, cancel := signalContext()
	defer cancel()

	// BEFORE wire(), and that ordering is the entire mechanism.
	//
	// The failure this guards against is a new version for which wire() does
	// not work: a schema migration that will not apply, a config field that no
	// longer parses. Such a build passes the hash and the smoke test — which
	// runs `version`, handled before wire() in main's dispatch — and then
	// fails here, every thirty seconds, on a machine that is no longer running
	// the code that could report it or replace it.
	//
	// So this decides what to do about the last update using nothing but the
	// path to this binary and a file beside it.
	startLog := slog.New(slog.NewTextHandler(os.Stderr, nil))

	u, start, wentBack := superviseLastUpdate(ctx, startLog)
	if wentBack {
		// The previous version is on the disk now. Exiting is how it starts:
		// the same path a successful update takes, for the same reason.
		startLog.Warn("went back to the previous version; exiting so the service manager starts it",
			"gave_up_on", start.Failed, "now", start.Version)

		return errUpdated
	}

	d, err := wireDaemon(ctx, *verbose)
	if err != nil {
		return err
	}
	defer d.close()

	d.updated = make(chan struct{}, 1)

	listen := d.cfg.Addr()
	if *addr != "" {
		listen = *addr
	}

	if err := refuseNonLoopback(listen); err != nil {
		return err
	}

	d.listen = listen

	// Seed the plan from config.toml, if this machine has none yet.
	if err := d.seedPlan(ctx); err != nil {
		return err
	}

	guard, err := loopback.New(listen)
	if err != nil {
		return err
	}

	// A local rather than a field on deps: nothing outside this function uses
	// it, and every other command exits long before background work would
	// finish.
	survey := surveybus.NewBusiness(d.log, d.probeUpload, surveybus.Choices())

	app, err := statusapp.New(statusapp.Config{
		Plan:                    d.plan,
		Backups:                 d.backups,
		Credentials:             d.creds,
		Survey:                  survey,
		Background:              ctx,
		MeteredKnown:            meteredKnown(d.cfg),
		StartRun:                d.startRun(ctx),
		Guard:                   guard,
		Paths:                   d.paths,
		Version:                 version,
		StoragePricePerTiBMonth: d.cfg.Storage.PricePerTiBMonth,
		Log:                     d.log,
	})
	if err != nil {
		return err
	}

	handler := web.Logging(d.log, time.Now, web.SecureHeaders(guard.Wrap(app)))

	server := &http.Server{
		Addr:    listen,
		Handler: handler,

		// Modest, because every client is on this machine. The read timeout is
		// the one that matters: without it a half-open connection from a
		// crashed browser holds a goroutine for as long as the daemon lives.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	// Listen before announcing, so "listening on" is true when it is printed
	// and a port collision is reported as itself rather than as a silent
	// no-op.
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("the status page could not listen on %s: %w", listen, err)
	}

	d.log.Info("sion-backup started",
		"version", version, "status_page", "http://"+listen+"/",
		"enrolled", d.creds.Enrolled(), "restic", d.restic.Bin())

	if !d.creds.Enrolled() {
		d.log.Warn("this machine is not enrolled, so no backup can run",
			"fix", "sion-backup enroll --code <code>")
	}

	errs := make(chan error, 1)

	// A version on probation is believed once it has stayed up. Not once it has
	// taken a backup: a laptop can legitimately go a week without one, and
	// three reboots in that week must not be read as a crash loop. A daemon
	// still running after the grace period has opened its database, read its
	// config, resolved restic and served this page.
	//
	// Stopped on the way out, so a daemon that is shut down before the grace
	// period ends leaves the probation in place for the next start to judge.
	if start.Outcome == selfupdate.OnProbation && u != nil {
		d.log.Info("this version is on probation",
			"version", start.Version, "start", start.Starts,
			"settles_in", selfupdate.Grace())

		settle := time.AfterFunc(selfupdate.Grace(), u.Settle)
		defer settle.Stop()
	}

	go supervise(d.log, "status page", func() {
		if err := server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
	})

	if !*noSchedule {
		go supervise(d.log, "scheduler", func() { d.scheduler(ctx) })
	}

	go supervise(d.log, "flusher", func() { d.flusher(ctx) })

	select {
	case err := <-errs:
		return err

	case <-d.updated:
		// A newer binary is on the disk. Exiting is how it starts running:
		// systemd restarts on any exit, and a Windows scheduled task restarts
		// on a failing one, which is what updateExitCode is for.
		d.log.Info("a newer version was installed; exiting so the service manager starts it")

		return errUpdated

	case <-ctx.Done():
		d.log.Info("shutting down")
	}

	// A separate context: the one that just fired is cancelled, and passing it
	// to Shutdown would abandon in-flight requests immediately rather than
	// letting them finish.
	stop, cancelStop := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancelStop()

	return server.Shutdown(stop)
}

// refuseNonLoopback stops the status page being served to the network.
//
// A hard refusal and not a warning. The page has no login — see app/sdk/loopback
// for why that is right on loopback — so binding it to 0.0.0.0 would put an
// unauthenticated configuration UI for somebody's backups on the office LAN.
// There is no flag to override this, because there is no legitimate use for it.
func refuseNonLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%q is not a host:port address: %w", addr, err)
	}

	switch host {
	case "localhost", "127.0.0.1", "::1":
		return nil
	}

	ip := net.ParseIP(host)
	if ip != nil && ip.IsLoopback() {
		return nil
	}

	return fmt.Errorf("refusing to serve the status page on %s: it has no login, "+
		"which is only safe on loopback. Use 127.0.0.1, and reach it over SSH if "+
		"you need it from elsewhere", addr)
}

// seedPlan installs config.toml's plan on a machine that has none.
func (d *deps) seedPlan(ctx context.Context) error {
	if !d.cfgFound {
		return nil
	}

	seed, err := d.cfg.Plan()
	if err != nil {
		return err
	}

	// A config file that carries no plan is perfectly normal — it may hold
	// only the Eumaeus settings — so an incomplete one is skipped rather than
	// rejected.
	if seed.NodeID == "" || seed.Repository == "" || len(seed.Targets) == 0 {
		return nil
	}

	seeded, err := d.plan.Seed(ctx, seed, time.Now())
	if err != nil {
		return err
	}

	if seeded {
		d.log.Info("took the backup plan from the config file",
			"node", seed.NodeID, "targets", len(seed.Targets),
			"note", "it is in the database now, and the file will not be read again")
	}

	return nil
}

// scheduler starts a run whenever one is due.
func (d *deps) scheduler(ctx context.Context) {
	ticker := time.NewTicker(tick)
	defer ticker.Stop()

	// One check immediately, so a machine that has been off all day does not
	// wait a further minute before catching up.
	d.maybeRun(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.maybeRun(ctx)
		}
	}
}

// maybeRun starts a backup if the schedule says so.
func (d *deps) maybeRun(ctx context.Context) {
	plan, err := d.plan.Get(ctx)

	switch {
	case errors.Is(err, planbus.ErrNoPlan):
		return
	case err != nil:
		d.log.Error("could not read the backup plan", "err", err)

		return
	}

	if plan.Paused {
		return
	}

	// The gate. A plan nobody at this machine has said yes to does not run —
	// see planbus.Plan.ConfirmedAt, and app/domain/statusapp's setup page,
	// which is the only thing that clears it.
	//
	// Once per hour rather than on every tick: this is checked every minute,
	// and a line a minute would bury everything else in the log of a machine
	// somebody has not got round to setting up.
	if !plan.Confirmed() {
		d.sayWaiting()

		return
	}

	if _, running := d.backups.Running(); running {
		return
	}

	// Deliberately after the running check and before the schedule: a run that
	// is already going is not stopped by the connection changing under it, and
	// asking the operating system about the network on every tick of a machine
	// that has nothing due would be a wake-up a minute for nothing.
	if plan.SkipOnMetered {
		if metered, saidBy := d.metered(ctx); metered {
			d.log.Debug("not starting a scheduled backup on a metered connection",
				"said_by", saidBy,
				"note", `"Back up now" and "sion-backup run" are unaffected`)

			return
		}
	}

	var lastRun time.Time

	switch last, err := d.backups.Last(ctx); {
	case err == nil:
		lastRun = last.StartedAt
	case !errors.Is(err, backupbus.ErrNoRuns):
		d.log.Error("could not read the run history", "err", err)

		return
	}

	if !plan.Schedule.Due(plan.NodeID, time.Now(), lastRun) {
		return
	}

	go supervise(d.log, "scheduled backup", func() {
		if err := d.backup(ctx, plan); err != nil && !errors.Is(err, backupbus.ErrAlreadyRunning) {
			d.log.Error("the scheduled backup failed to start", "err", err)
		}
	})
}

// flusher pushes runs the fleet dashboard has not heard about.
func (d *deps) flusher(ctx context.Context) {
	ticker := time.NewTicker(flushEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.flush(ctx)
		}
	}
}

// flush sends pending run events and any diagnostic reports, and never fails
// loudly.
func (d *deps) flush(ctx context.Context) {
	d.flushDiagnostics(ctx)

	pending, err := d.backups.Unreported(ctx, 50)
	if err != nil {
		d.log.Error("could not read unreported runs", "err", err)

		return
	}

	if len(pending) == 0 {
		return
	}

	events := make([]fleetbus.Pending, 0, len(pending))
	for _, r := range pending {
		events = append(events, fleetbus.Pending{ID: r.ID, Event: eventFor(r)})
	}

	sent, err := d.fleet.Flush(ctx, events, func(ctx context.Context, id int64) error {
		return d.backups.MarkReported(ctx, id, time.Now())
	})

	if sent > 0 {
		d.log.Info("reported runs to the fleet dashboard", "count", sent)
	}

	if err != nil {
		d.log.Debug("some runs are still waiting to be reported",
			"remaining", len(pending)-sent, "err", err)
	}
}

// flushDiagnostics sends crash and install reports that are waiting on disk.
//
// Separate from the run events above and unconditional, because the machine
// with reports waiting is often the machine with no runs to report — an
// install that failed leaves one and not the other.
func (d *deps) flushDiagnostics(ctx context.Context) {
	sent, err := d.diag.Flush(ctx)

	if sent > 0 {
		d.log.Info("sent diagnostic reports", "count", sent)
	}

	if err != nil {
		d.log.Debug("diagnostic reports are still waiting", "err", err)
	}
}

// startRun returns the closure the status page's "Back up now" button calls.
//
// The context is the daemon's, not the request's: a backup must not be
// cancelled because somebody closed the browser tab that started it.
func (d *deps) startRun(daemonCtx context.Context) func(context.Context) error {
	return func(ctx context.Context) error {
		plan, err := d.plan.Get(ctx)
		if err != nil {
			return err
		}

		if _, running := d.backups.Running(); running {
			return backupbus.ErrAlreadyRunning
		}

		go supervise(d.log, "requested backup", func() {
			if err := d.backup(daemonCtx, plan); err != nil &&
				!errors.Is(err, backupbus.ErrAlreadyRunning) {
				d.log.Error("the requested backup failed to start", "err", err)
			}
		})

		return nil
	}
}

// backup fetches the credentials, takes one backup, and reports it.
//
// This is the only function in the program that holds all three secrets at
// once. They arrive over the network, live for the length of the run, and are
// wiped on the way out — nothing reaches the disk.
func (d *deps) backup(ctx context.Context, plan planbus.Plan) error {
	// Before the credentials, and before anything is reported as started:
	// this is the machine making sure it still has the program that does the
	// work. Ordinarily it is one exec of `restic version` and nothing else.
	if err := d.ensureRestic(ctx); err != nil {
		return err
	}

	// Fetched now, used once, wiped on the way out. Nothing here is written to
	// this machine's disk — see business/domain/credential.
	set, err := d.creds.ForRun(ctx)
	if err != nil {
		return err
	}
	defer set.Wipe()

	// The server is the authority on which repository this machine writes to,
	// so a rotation takes effect on the next run with no local change. The
	// plan's copy of the URL is what the status page shows; a disagreement is
	// worth a line in the log rather than a refusal to back up.
	if set.RepositoryURL != plan.Repository {
		d.log.Info("the server has moved this machine to a different repository",
			"was", plan.Repository, "now", set.RepositoryURL)

		plan.Repository = set.RepositoryURL

		if err := d.plan.Put(ctx, plan, time.Now()); err != nil {
			d.log.Warn("could not record the new repository locally", "err", err)
		}
	}

	// Both events one run produces carry the same identity and the same answer
	// to "is this the run that fills a new bucket?", so both are settled here,
	// before anything starts. Seeding is a question about the repository the
	// server has just named, which is why it is asked after the reconciliation
	// above rather than before it.
	runUUID := newRunUUID()

	seeding, err := d.backups.Seeding(ctx, plan.Repository)
	if err != nil {
		// Not fatal. A wrong seeding flag makes one dashboard row misleading;
		// refusing to back up over it would be absurd.
		d.log.Warn("could not tell whether this is the first run against this repository",
			"repository", plan.Repository, "err", err)
	}

	req := backupbus.Request{
		NodeID:     plan.NodeID,
		RunUUID:    runUUID,
		Seeding:    seeding,
		Repository: set.Credentials.Repository(plan.Repository, plan.PackSizeMiB, plan.ReadConcurrency),
		Options:    backupOptions(plan),
	}

	// The dashboard is told a run has started before it starts, so a machine
	// that dies mid-backup leaves a start with no end rather than no trace.
	_ = d.fleet.Report(ctx, startEvent(plan, runUUID, seeding))

	run, err := d.backups.Run(ctx, req, time.Now)
	if err != nil {
		return err
	}

	d.log.Info("backup finished",
		"outcome", run.Outcome, "verified", run.Verified,
		"files", run.TotalFilesProcessed, "added", run.DataAdded,
		"took", run.Duration().Round(time.Second), "message", run.Message)

	if err := d.fleet.Report(ctx, eventFor(run)); err == nil {
		if err := d.backups.MarkReported(ctx, run.ID, time.Now()); err != nil {
			d.log.Warn("the run was reported but not marked as such", "run", run.ID, "err", err)
		}
	}

	// Measuring walks the whole repository index, so it happens weekly rather
	// than after every backup, and only when a snapshot was actually written
	// — there is nothing to measure otherwise. It reuses the credentials this
	// run already has, which is why it lives here and not in its own job:
	// fetching a second set just to count bytes would double the number of
	// audited credential reads for no benefit.
	if run.SnapshotID != "" {
		d.measure(ctx, plan, set)
		d.checkRepository(ctx, plan, set)
	}

	// Last, and only now that the backup is over: see selfUpdate on why this
	// does not happen at the start of a run.
	if d.selfUpdate(ctx) && d.updated != nil {
		select {
		case d.updated <- struct{}{}:
		default:
		}
	}

	return nil
}

// measureEvery is how often the repository size is recounted.
const measureEvery = 7 * 24 * time.Hour

// measure updates the figures behind the rotation suggestion.
//
// Failures are logged and swallowed. A backup that succeeded must not be
// reported as anything else because a statistics call timed out.
func (d *deps) measure(ctx context.Context, plan planbus.Plan, set credentialbus.Set) {
	now := time.Now()

	m, err := d.plan.Measurement(ctx, plan.Repository, now)
	if err != nil {
		d.log.Warn("could not read the stored repository measurement", "err", err)

		return
	}

	if !m.Stale(now, measureEvery) {
		return
	}

	size, err := d.restic.Measure(ctx,
		set.Credentials.Repository(plan.Repository, plan.PackSizeMiB, plan.ReadConcurrency))
	if err != nil {
		d.log.Warn("could not measure the repository", "err", err)

		return
	}

	if err := d.plan.RecordMeasurement(ctx, plan.Repository, now, m.Since,
		planbus.Size{Now: size.Now, Fresh: size.Fresh, Snapshots: size.Snapshots}); err != nil {
		d.log.Warn("could not record the repository measurement", "err", err)

		return
	}

	d.log.Info("measured the repository",
		"total", size.Now, "fresh", size.Fresh, "reclaimable", size.Reclaimable())
}

// hostname is the machine's own name, for the snapshot. Errors are swallowed:
// a snapshot with no host set is a cosmetic problem, and refusing to back up
// over one would be absurd.
func hostname() string {
	name, err := os.Hostname()
	if err != nil {
		return ""
	}

	return name
}

// osName identifies the platform in fleet events.
func osName() string { return runtime.GOOS + "/" + runtime.GOARCH }

// checkRepository reads part of the repository back to prove it is still
// sound, weekly, and never over a connection somebody is paying for.
//
// # Why this is here and not its own job
//
// It reuses the credentials the run already holds. Fetching a second set just
// to verify would double the audited credential reads on the server for no
// benefit, and would need a machine to be enrolled and online twice rather
// than once.
//
// # Why it can be skipped, and what that costs
//
// A metered connection means a phone, and reading back a slice of somebody's
// backups over their phone is a bill they did not agree to. A skip is recorded
// rather than passed over in silence, because a machine that has skipped every
// week for two months looks identical to a healthy one unless somebody wrote
// down why.
//
// Failures are recorded and swallowed, like the measurement. A backup that
// succeeded must not be reported as anything else because a verification did
// not finish -- but a repository that is actually damaged is reported, because
// that is the one thing here nobody else will notice.
func (d *deps) checkRepository(ctx context.Context, plan planbus.Plan, set credentialbus.Set) {
	now := time.Now()

	last, err := d.plan.Integrity(ctx, plan.Repository)
	if err != nil {
		d.log.Warn("could not read the last repository check", "err", err)

		return
	}

	if !last.Due(plan.Repository, now) {
		return
	}

	record := func(i planbus.Integrity) {
		i.RepositoryURL = plan.Repository
		i.CheckedAt = now

		if err := d.plan.RecordIntegrity(ctx, i); err != nil {
			d.log.Warn("could not record the repository check", "err", err)
		}
	}

	// The config file first, because on Windows and macOS the machine cannot
	// find this out and a person may have written it down. Then the operating
	// system, where Unknown is not metered: see foundation/netcost. Two thirds
	// of this fleet cannot tell, and reading Unknown as metered would mean no
	// Windows or Mac ever verified anything.
	//
	// Unconditional, unlike the scheduler's skip above: this one is not a
	// setting. Re-reading pack data is the optional, large work netcost exists
	// for, and it is never worth somebody's phone bill.
	if metered, saidBy := d.metered(ctx); metered {
		d.log.Info("skipping the repository check on a metered connection", "said_by", saidBy)

		record(planbus.Integrity{SkippedReason: "metered connection"})

		return
	}

	// Sized from the last measurement, which the call above this one has just
	// refreshed if it was stale. An unmeasured repository gets the floor,
	// which is the right answer for one nobody has counted yet.
	m, _ := d.plan.Measurement(ctx, plan.Repository, now)
	subset := planbus.CheckSubset(m.Now)

	d.log.Info("checking the repository", "reading_back", subset)

	repo := set.Credentials.Repository(plan.Repository, plan.PackSizeMiB, plan.ReadConcurrency)

	if err := d.restic.Check(ctx, repo, restic.ReadDataSubset(subset)); err != nil {
		d.log.Error("the repository did not pass its check", "err", err)

		record(planbus.Integrity{Subset: subset, Detail: err.Error()})

		// Reported, and this is the only check in the program whose failure
		// means the backups already taken may not be worth anything. Every
		// other report says a backup did not happen; this one says the ones
		// that did may not come back.
		_ = d.diag.Record(diagbus.Report{
			Kind:   diagbus.KindRepositoryDamaged,
			Step:   "repository-check",
			Detail: err.Error(),
		})

		return
	}

	record(planbus.Integrity{Subset: subset, OK: true})

	d.log.Info("the repository is sound", "read_back", subset)
}
