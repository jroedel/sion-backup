//go:build !windows

package main

// snapshotCheck is Windows-only. Everywhere else there is no filesystem
// snapshot to take and nothing to check: restic reads the live tree, and a
// file that changes underneath it is reported as such by the run itself.
//
// The signature is shared so that doctor.go does not need a build tag of its
// own -- see snapshots_windows.go for the check that matters.
func snapshotCheck(_ bool, _ bool) (string, error) {
	return "", errSkipCheck
}
