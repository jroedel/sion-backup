package main

import (
	"time"

	"github.com/google/uuid"
	"github.com/jroedel/sion-backup/business/domain/backup/backupbus"
	"github.com/jroedel/sion-backup/business/domain/fleet/fleetbus"
	"github.com/jroedel/sion-backup/business/domain/plan/planbus"
	"github.com/jroedel/sion-backup/foundation/restic"
)

// backupOptions turns a plan into restic's arguments.
//
// It lives in the composition root because it is where two domains meet: the
// plan knows what the person wants, foundation/restic knows what the binary
// accepts, and neither should have to import the other.
func backupOptions(plan planbus.Plan) restic.BackupOptions {
	return restic.BackupOptions{
		Targets:  plan.Targets,
		Excludes: plan.Excludes,

		// Tagged so that `restic snapshots --tag sion-backup` distinguishes
		// what this program made from anything else written to the same
		// repository. On a shared bucket that is the difference between a
		// confident prune and a nervous one.
		Tags: []string{"sion-backup"},

		Host:              hostname(),
		UseFSSnapshot:     plan.UseFSSnapshot,
		AllowVSSFallback:  plan.AllowVSSFallback,
		OneFileSystem:     plan.OneFileSystem,
		ExcludeLargerThan: int64(plan.SkipLargerThanGB) << 30,
	}
}

// newRunUUID mints the identity a run is known by on the dashboard.
//
// UUIDv7, because it sorts by time: the server can order one machine's runs
// without trusting the clock of a laptop that has crossed three time zones,
// and without an autoincrement that starts again at 1 on a reimaged machine.
//
// A failure here — crypto/rand being unavailable — must not stop a backup, so
// it returns the empty string and the run goes ahead unreportable. The server
// will reject its events with a 400 and fleetbus will discard them loudly,
// which is the right order of priorities: the files matter, the dashboard row
// does not.
func newRunUUID() string {
	id, err := uuid.NewV7()
	if err != nil {
		return ""
	}

	return id.String()
}

// startEvent announces that a run has begun.
//
// It carries the same run UUID the finished event will, which is what lets the
// dashboard pair them, and the point of it is not bookkeeping — it is that a
// machine which dies mid-backup leaves a start with no end.
func startEvent(plan planbus.Plan, runUUID string, seeding bool) fleetbus.Event {
	return fleetbus.Event{
		NodeID:        plan.NodeID,
		RunUUID:       runUUID,
		RepositoryURL: plan.Repository,
		Seeding:       seeding,
		Phase:         fleetbus.PhaseStarted,
		StartedAt:     time.Now(),
		Agent:         version,
		OS:            osName(),
	}
}

// eventFor turns a finished run into what the fleet dashboard is told.
//
// Everything it needs comes off the stored run, including the UUID and the
// seeding flag, so a run reported days after it happened reports the same
// thing it would have on the night.
//
// The paths of unreadable files are deliberately reduced to a count. They are
// on the machine's own status page in full; sending them would put a list of
// somebody's filenames in a server log this program does not control.
func eventFor(r backupbus.Run) fleetbus.Event {
	finished := r.FinishedAt

	return fleetbus.Event{
		NodeID:          r.NodeID,
		RunUUID:         r.RunUUID,
		RepositoryURL:   r.Repository,
		Seeding:         r.Seeding,
		Phase:           fleetbus.PhaseFinished,
		StartedAt:       r.StartedAt,
		FinishedAt:      &finished,
		Outcome:         string(r.Outcome),
		Message:         r.Message,
		SnapshotID:      r.SnapshotID,
		FilesProcessed:  r.TotalFilesProcessed,
		BytesProcessed:  r.TotalBytesProcessed,
		DataAdded:       r.DataAdded,
		UnreadableFiles: len(r.UnreadableFiles),
		Verified:        r.Verified,
		Agent:           version,
		OS:              osName(),
	}
}
