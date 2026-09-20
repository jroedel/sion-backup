package main

import (
	"testing"

	"github.com/jroedel/sion-backup/foundation/selfupdate"
)

// TestAMachineWithNoRepositoryStillHasASource is the settings page telling
// every ordinary machine in the fleet that its updates were switched off.
//
// An empty `repository` means the fleet's own -- selfupdate.GitHub substitutes
// DefaultRepository, and config.example.toml ships the line commented out --
// so the normal machine had never set it. The page reads an empty source as
// "switched off for this machine" and hides "Check for updates" behind it, so
// a machine that was updating itself after every backup said it was not, and
// offered no way to ask for one now.
func TestAMachineWithNoRepositoryStillHasASource(t *testing.T) {
	d := &deps{log: quietLog()}

	source, _ := d.updateSource()
	if source != selfupdate.DefaultRepository {
		t.Errorf("update source is %q, want %q", source, selfupdate.DefaultRepository)
	}
}

// TestAConfiguredRepositoryIsReportedAsItIs. Testing against a fork is the
// only reason to set this, and the page must name the fork rather than the
// fleet's own.
func TestAConfiguredRepositoryIsReportedAsItIs(t *testing.T) {
	d := &deps{log: quietLog()}
	d.cfg.Update.Repository = "someone/their-fork"

	source, _ := d.updateSource()
	if source != "someone/their-fork" {
		t.Errorf("update source is %q, want the configured fork", source)
	}
}

// TestUpdatesSwitchedOffHaveNoSource, which is the one thing an empty source
// is now allowed to mean: the page says so, and hides the button, because
// somebody chose that in this machine's config.
func TestUpdatesSwitchedOffHaveNoSource(t *testing.T) {
	off := false

	d := &deps{log: quietLog()}
	d.cfg.Update.Enabled = &off

	if source, _ := d.updateSource(); source != "" {
		t.Errorf("update source is %q on a machine with updates switched off", source)
	}
}
