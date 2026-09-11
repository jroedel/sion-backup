package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jroedel/sion-backup/business/domain/plan/planbus"
	"github.com/jroedel/sion-backup/business/domain/plan/stores/plandb"
	"github.com/jroedel/sion-backup/foundation/sqldb"
)

// quietLog is a logger a test does not have to read.
func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// write puts a config file in a fresh data directory and returns what was
// parsed out of it.
func write(t *testing.T, body string) Config {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.toml")

	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, found, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	if !found {
		t.Fatal("the config file was not found")
	}

	return cfg
}

// seed runs the seeding path against a real database and returns the plan it
// produced.
func seed(t *testing.T, cfg Config) planbus.Plan {
	t.Helper()

	ctx := context.Background()

	db, err := sqldb.Open(ctx, filepath.Join(t.TempDir(), "sion.db"))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { db.Close() })

	if err := plandb.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}

	d := &deps{cfg: cfg, cfgFound: true, log: quietLog(), plan: planbus.NewBusiness(plandb.NewStore(db))}

	if err := d.seedPlan(ctx); err != nil {
		t.Fatal(err)
	}

	plan, err := d.plan.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}

	return plan
}

const unattended = `
node_id    = "office-laptop-1"
repository = "s3:https://s3.us-central-1.wasabisys.com/example-bucket"
targets    = ["/home/jeff"]
`

// TestAnUnattendedInstallCanConfirmItsOwnPlan.
//
// A machine built from a script, with its folders written in config.toml by
// whoever administers the fleet, has had the question answered. Making it wait
// for a web page nobody is going to open would mean it silently never backs up
// — which is the failure this whole program exists to prevent.
//
// Found by the release gate, which is an unattended install and was the first
// one to meet this.
func TestAnUnattendedInstallCanConfirmItsOwnPlan(t *testing.T) {
	before := time.Now().Add(-time.Second)

	plan := seed(t, write(t, unattended+"confirmed = true\n"))

	if !plan.Confirmed() {
		t.Fatal("an unattended install that said confirmed = true is still waiting for " +
			"somebody at a keyboard")
	}

	if plan.ConfirmedAt.Before(before) {
		t.Errorf("confirmed at %s, want roughly now", plan.ConfirmedAt)
	}
}

// TestAPlanFromConfigWaitsUnlessItSaysOtherwise is the default, and the safe
// direction: a machine that waits is visible on its own status page and in
// doctor, and a machine quietly backing up folders an administrator guessed at
// is not.
func TestAPlanFromConfigWaitsUnlessItSaysOtherwise(t *testing.T) {
	plan := seed(t, write(t, unattended))

	if plan.Confirmed() {
		t.Error("a config file that said nothing about confirming produced a confirmed plan")
	}

	// It is still a complete, usable plan — it is waiting, not broken.
	if err := plan.Validate(); err != nil {
		t.Errorf("the seeded plan is not usable: %v", err)
	}
}

// TestTheTwoMeteredSettingsAreNotTheSameSetting.
//
// `metered` is a FACT about the connection this machine is on, which it cannot
// work out for itself on Windows or macOS. `skip_on_metered` is a POLICY about
// what to do when it is. Neither implies the other, and a machine on a phone
// tether that must back up anyway sets the first and not the second.
func TestTheTwoMeteredSettingsAreNotTheSameSetting(t *testing.T) {
	cfg := write(t, unattended+"\n[tuning]\nmetered = true\n")

	if metered, set := cfg.Tuning.MeteredOverride(); !set || !metered {
		t.Error("metered = true was not read")
	}

	plan, err := cfg.Plan()
	if err != nil {
		t.Fatal(err)
	}

	if plan.SkipOnMetered {
		t.Error("saying the connection IS metered also turned on skipping backups on it")
	}
}

func TestTheSizeLimitAndTheMeteredPolicySeedThePlan(t *testing.T) {
	plan := seed(t, write(t, unattended+
		"\n[tuning]\nskip_larger_than_gb = 2\nskip_on_metered = true\n"))

	if plan.SkipLargerThanGB != 2 {
		t.Errorf("size limit %d, want 2", plan.SkipLargerThanGB)
	}

	if !plan.SkipOnMetered {
		t.Error("skip_on_metered = true did not reach the plan")
	}
}
