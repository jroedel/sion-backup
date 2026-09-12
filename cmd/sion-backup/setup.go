package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jroedel/sion-backup/foundation/netcost"
	"github.com/jroedel/sion-backup/foundation/s3probe"
)

// This file is the composition root's half of the setup page: the two things
// that page needs which cannot live in a domain, because both of them need a
// credential.

// probeTimeout bounds one upload measurement.
//
// Generous against the probe's own budget, which aims for about ten seconds of
// data. What eats the rest is the first connection: a machine behind a slow
// proxy, or one where DNS is answering in seconds, and on either of those the
// interesting result is the measurement rather than a timeout.
const probeTimeout = 3 * time.Minute

// probeUpload measures how fast this machine can upload to its own bucket.
//
// It is the closure surveybus is built with, and it is here rather than there
// for the reason every other credential-shaped closure in this program is: the
// composition root is the only place allowed to fetch a secret, and the domain
// that renders a page must never be handed one.
//
// The credentials are fetched, used for one probe and wiped, exactly as a
// backup's are. That is an audited read on the server for a speed test, which
// is the right cost: the alternative is a test that does not measure the path
// it is predicting.
func (d *deps) probeUpload(ctx context.Context) (s3probe.Result, error) {
	set, err := d.creds.ForRun(ctx)
	if err != nil {
		return s3probe.Result{}, fmt.Errorf("the credentials for this machine "+
			"could not be fetched: %w", err)
	}
	defer set.Wipe()

	// The server's URL rather than the plan's copy of it, for the same reason
	// a backup uses the server's: the server is the authority on which bucket
	// this machine writes to, and measuring the speed to the previous one
	// across a rotation would answer the wrong question convincingly.
	target, err := s3probe.TargetFor(set.RepositoryURL)
	if err != nil {
		return s3probe.Result{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	d.log.Info("measuring the upload speed to the repository",
		"bucket", target.Bucket, "region", target.Region,
		"note", "a multipart upload that is aborted; nothing is stored")

	return s3probe.Measure(ctx, target, s3probe.Credentials{
		AccessKeyID:     set.Credentials.AccessKeyID,
		SecretAccessKey: set.Credentials.SecretAccessKey,
	}, s3probe.Options{})
}

// metered reports whether somebody is paying for these bytes, and who said so.
//
// The config file first, because on Windows and macOS the machine cannot work
// it out and a person may have written it down. Then the operating system,
// where Unknown is not metered — see foundation/netcost.
func (d *deps) metered(ctx context.Context) (bool, string) {
	if m, configured := d.cfg.Tuning.MeteredOverride(); configured {
		return m, "config.toml"
	}

	return netcost.Of(ctx).Metered(), "the operating system"
}

// meteredKnown reports whether this machine can tell at all, so that the pages
// offering the setting can say when it would do nothing.
//
// A config override counts as knowing. Somebody who has written `metered =
// true` in config.toml has told the machine the answer, and a page saying "this
// computer cannot tell" beside a setting that is in fact working would be
// wrong in the direction that gets the setting switched off.
func meteredKnown(cfg Config) bool {
	if _, configured := cfg.Tuning.MeteredOverride(); configured {
		return true
	}

	return netcost.Known()
}

// waitingEvery is how often a daemon says it is waiting to be set up.
//
// The scheduler asks every minute. A line a minute about a machine nobody has
// got round to configuring would be the whole of that machine's log, and would
// bury the one line that mattered when somebody finally read it.
const waitingEvery = time.Hour

var waiting struct {
	sync.Mutex
	last time.Time
}

// sayWaiting logs that the scheduler is holding, at most hourly.
func (d *deps) sayWaiting() {
	waiting.Lock()
	defer waiting.Unlock()

	if time.Since(waiting.last) < waitingEvery {
		return
	}

	waiting.last = time.Now()

	d.log.Info("not backing up: nobody has chosen what should be in the backup yet",
		"fix", "open "+d.statusPage()+"/setup",
		"note", "this is deliberate; the plan is held until somebody at this machine confirms it")
}

// statusPage is the address the status page is reachable at, as a URL.
//
// The address it is actually listening on where that is known, and the
// configured one otherwise. The difference is --addr, which only the daemon
// has: a line telling somebody to open a port the daemon is not on is worse
// than no line, because they will try it.
func (d *deps) statusPage() string {
	if d.listen != "" {
		return "http://" + d.listen
	}

	return "http://" + d.cfg.Addr()
}
