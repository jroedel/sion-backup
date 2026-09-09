package main

import (
	"time"

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

		Host:             hostname(),
		UseFSSnapshot:    plan.UseFSSnapshot,
		AllowVSSFallback: plan.AllowVSSFallback,
		OneFileSystem:    plan.OneFileSystem,
	}
}

// startEvent announces that a run has begun.
//
// It carries no run ID, because the row is created inside backupbus and this
// is sent before that happens. The dashboard matches it to the finished event
// by node and time, and the point of it is not bookkeeping — it is that a
// machine which dies mid-backup leaves a start with no end.
func startEvent(plan planbus.Plan) fleetbus.Event {
	return fleetbus.Event{
		NodeID:     plan.NodeID,
		Repository: plan.Repository,
		Phase:      fleetbus.PhaseStarted,
		StartedAt:  time.Now(),
		Agent:      version,
		OS:         osName(),
	}
}

// eventFor turns a finished run into what the fleet dashboard is told.
//
// The paths of unreadable files are deliberately reduced to a count. They are
// on the machine's own status page in full; sending them would put a list of
// somebody's filenames in a server log this program does not control.
func eventFor(r backupbus.Run) fleetbus.Event {
	finished := r.FinishedAt

	return fleetbus.Event{
		NodeID:          r.NodeID,
		Repository:      r.Repository,
		RunID:           r.ID,
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
