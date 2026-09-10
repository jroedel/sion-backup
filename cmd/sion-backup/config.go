package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"runtime"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/jroedel/sion-backup/business/domain/plan/planbus"
)

// Config is the file an installer drops on a machine.
//
// # Two kinds of setting, and they behave differently
//
// **Deployment settings** — where the Eumaeus server is, which port to listen
// on, where the restic binary lives — are read from this file on every start.
// They are facts about how the machine was installed, they are the
// administrator's to change, and nothing in the program's own UI touches them.
//
// **The plan** — what to back up, when, how much history to keep — is read
// from this file exactly once, on a machine that has no plan yet, and lives in
// the database from then on. See planbus.
//
// Mixing the two in one file is deliberate. There is one thing to copy onto a
// new machine, and the difference in behaviour is the sort of thing that
// wants documenting rather than splitting into two files nobody can remember
// the names of.
type Config struct {
	NodeID     string   `toml:"node_id"`
	Repository string   `toml:"repository"`
	Targets    []string `toml:"targets"`
	Excludes   []string `toml:"excludes"`

	Schedule ScheduleConfig `toml:"schedule"`
	Update   UpdateConfig   `toml:"update"`
	Storage  StorageConfig  `toml:"storage"`
	Tuning   TuningConfig   `toml:"tuning"`
	Eumaeus  EumaeusConfig  `toml:"eumaeus"`
	Server   ServerConfig   `toml:"server"`
}

// ScheduleConfig is the seed schedule.
type ScheduleConfig struct {
	Times         []string `toml:"times"`
	JitterMinutes int      `toml:"jitter_minutes"`

	// MinInterval is a Go duration string, "6h". A string rather than a number
	// of seconds because this file is read by people, and "21600" in a config
	// file is a small unkindness.
	MinInterval string `toml:"min_interval"`
}

// StorageConfig is what the bucket costs, so the status page can turn
// reclaimable bytes into a number somebody recognises.
//
// Local because the client cannot know it and the server does not exist yet.
// When /machines/me arrives it should carry the price instead, so that a
// change of provider does not mean editing thirty config files.
type StorageConfig struct {
	// PricePerTiBMonth is the storage rate in whole currency units.
	//
	// Zero, the default, means the status page talks about gigabytes and not
	// money — which is the right behaviour when nobody has said what a
	// gigabyte costs. A wrong figure is worse than no figure: it is the sort
	// of thing that gets quoted in a meeting.
	PricePerTiBMonth float64 `toml:"price_per_tib_month"`
}

// TuningConfig is the per-machine performance and platform settings.
type TuningConfig struct {
	PackSizeMiB     int `toml:"pack_size_mib"`
	ReadConcurrency int `toml:"read_concurrency"`

	// UseFSSnapshot is a pointer so that "unset" and "false" are different.
	// Unset means "do the right thing for this platform", which is VSS on
	// Windows and nothing elsewhere; false means somebody decided against it.
	UseFSSnapshot    *bool `toml:"use_fs_snapshot"`
	AllowVSSFallback *bool `toml:"allow_vss_fallback"`
	OneFileSystem    *bool `toml:"one_file_system"`

	// Metered says this machine is on a connection somebody pays for by the
	// byte, when the machine cannot work that out itself.
	//
	// A pointer for the usual reason, and the "unset" case is the common one:
	// unset means ask the operating system, which only Linux answers. On
	// Windows and macOS the answer is always "cannot tell" — see
	// foundation/netcost — so this is how somebody says what the machine
	// does not know.
	//
	// It gates the weekly repository check and nothing else. Backups run
	// whatever this says: a backup that did not happen is the failure this
	// program exists to prevent, and no connection is expensive enough to be
	// worth choosing that one instead.
	Metered *bool `toml:"metered"`
}

// MeteredOverride reports what the config says about the connection, and
// whether it says anything at all.
func (t TuningConfig) MeteredOverride() (metered, set bool) {
	if t.Metered == nil {
		return false, false
	}

	return *t.Metered, true
}

// DefaultEumaeusURL is the installation this fleet belongs to.
//
// Hardcoded, because there is exactly one and every machine that runs this
// binary reports to it. A default is not a lock: [eumaeus] url in config.toml
// or --server on enroll still wins, which is what a staging server or a
// loopback test needs. What it buys is that a machine with no config file at
// all can still be enrolled with nothing but a code, and that thirty installs
// cannot end up with thirty spellings of the same host.
const DefaultEumaeusURL = "https://terraboskamp.org"

// EumaeusConfig points at the server this machine depends on.
//
// There is no token here, and that is the point of the enrollment flow: an
// admin signs in to Eumaeus, chooses the owner and the bucket, and carries a
// one-time code to the machine. `sion-backup enroll --code …` exchanges it for
// a machine token, which is written to the data directory and never appears in
// this file.
//
// The URL is not optional for a working machine. Nothing is cached locally, so
// a backup begins by asking this server for the credentials — which is why it
// has a default rather than an empty string. See DefaultEumaeusURL.
type EumaeusConfig struct {
	URL string `toml:"url"`
}

// UpdateConfig is how this program replaces itself.
//
// On by default, which is a decision rather than a convenience: the fleet is
// laptops in several buildings with no management agent, and a version that
// has to be installed by hand is a version half of them will never get. A
// build whose version is not a release tag -- a developer's working copy --
// is never replaced whatever this says, so the default cannot surprise
// anybody who is working on the program.
type UpdateConfig struct {
	// Enabled is a pointer so that "unset" and "false" are different. Unset
	// means on.
	Enabled *bool `toml:"enabled"`

	// Repository is "owner/name" on GitHub. Empty means the fleet's own.
	Repository string `toml:"repository"`
}

// On reports whether self-update is wanted.
func (u UpdateConfig) On() bool { return boolOr(u.Enabled, true) }

// ServerConfig is how the daemon runs.
type ServerConfig struct {
	// Addr is the status page's listen address. Loopback, always: see
	// app/sdk/loopback.
	Addr string `toml:"addr"`

	// Restic is the absolute path to a restic binary, and is normally empty.
	//
	// Empty means the fleet's own copy: the pinned version, installed and
	// upgraded by this program into the data directory. Set, it names a
	// binary to use exactly as given and never to manage — the escape hatch
	// for a platform the pin has no build for. See restic.Resolve.
	Restic string `toml:"restic"`
}

// DefaultAddr is where the status page listens.
//
// 7391 is high, fixed, and not registered to anything. Fixed rather than
// chosen at random because the administrator has to be able to type it, and it
// is on loopback where a collision is the only thing at stake.
const DefaultAddr = "127.0.0.1:7391"

// LoadConfig reads the file. A missing file is not an error: a machine that
// has already been enrolled has everything it needs in the database and the
// keyring, and the file can be deleted afterwards.
func LoadConfig(path string) (Config, bool, error) {
	var cfg Config

	raw, err := os.ReadFile(path)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return cfg, false, nil
	case err != nil:
		return cfg, false, fmt.Errorf("reading %s: %w", path, err)
	}

	if err := toml.Unmarshal(raw, &cfg); err != nil {
		return cfg, false, fmt.Errorf("reading %s: %w", path, err)
	}

	return cfg, true, nil
}

// EumaeusURL is the server this machine talks to, with the default applied.
func (c Config) EumaeusURL() string {
	if c.Eumaeus.URL == "" {
		return DefaultEumaeusURL
	}

	return c.Eumaeus.URL
}

// Addr is the listen address, with the default applied.
func (c Config) Addr() string {
	if c.Server.Addr == "" {
		return DefaultAddr
	}

	return c.Server.Addr
}

// Plan turns the file into a seed plan.
//
// Where the file says nothing, the platform's sensible answer is used rather
// than a zero — a plan with no schedule would never run, and a Windows machine
// with no VSS setting should get VSS.
func (c Config) Plan() (planbus.Plan, error) {
	schedule := planbus.DefaultSchedule()

	if len(c.Schedule.Times) > 0 {
		schedule.Times = c.Schedule.Times
	}

	if c.Schedule.JitterMinutes > 0 {
		schedule.JitterMinutes = c.Schedule.JitterMinutes
	}

	if c.Schedule.MinInterval != "" {
		d, err := time.ParseDuration(c.Schedule.MinInterval)
		if err != nil {
			return planbus.Plan{}, fmt.Errorf("reading schedule.min_interval: %w", err)
		}

		schedule.MinInterval = d
	}

	return planbus.Plan{
		NodeID:           c.NodeID,
		Repository:       c.Repository,
		Targets:          c.Targets,
		Excludes:         c.Excludes,
		Schedule:         schedule,
		PackSizeMiB:      c.Tuning.PackSizeMiB,
		ReadConcurrency:  c.Tuning.ReadConcurrency,
		UseFSSnapshot:    boolOr(c.Tuning.UseFSSnapshot, runtime.GOOS == "windows"),
		AllowVSSFallback: boolOr(c.Tuning.AllowVSSFallback, true),
		OneFileSystem:    boolOr(c.Tuning.OneFileSystem, true),
	}, nil
}

// There is no retention configuration, because there is no pruning. Space is
// reclaimed by rotating the bucket and starting a fresh repository — see
// docs/model.md §5.4 and the note in foundation/restic/ops.go.
//
// The history a machine has is therefore the age of its current repository,
// and that is what the status page should say rather than implying a policy
// that nothing enforces.

func boolOr(p *bool, fallback bool) bool {
	if p == nil {
		return fallback
	}

	return *p
}
