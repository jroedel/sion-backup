//go:build windows

package main

import (
	"errors"

	"golang.org/x/sys/windows"
)

// snapshotCheck reports whether this process could take a Volume Shadow Copy.
//
// It is a Windows-only check because VSS is a Windows-only feature, and it is
// worth its own line in doctor because the alternative way to discover the
// answer is to read the outcome of a backup that has already been taken
// wrongly. A machine whose task was registered without -Elevated backs up
// every open file from the live tree -- Outlook's .pst above all -- and
// records every run as degraded, which is amber on a dashboard rather than
// red, and therefore exactly the sort of thing that is still true a year
// later.
func snapshotCheck(fellBack bool, haveRun bool) (string, error) {
	elevated := windows.GetCurrentProcessToken().IsElevated()

	switch {
	case !elevated:
		return "", errors.New("this process is not elevated, so Volume Shadow Copy is " +
			"unavailable: open files will be backed up from the live tree and every run " +
			"recorded as degraded. Re-register the task with \"install.ps1 -Elevated\"")

	case haveRun && fellBack:
		// Elevated and it still failed. Rarer and more interesting: the VSS
		// service is disabled, or the volume has no shadow storage.
		return "", errors.New("elevated, but the last run still could not take a " +
			"filesystem snapshot. Check that the Volume Shadow Copy service is running " +
			"(\"sc query VSS\") and that the volume has shadow storage " +
			"(\"vssadmin list shadowstorage\")")

	case !haveRun:
		return "elevated; no run yet to confirm a snapshot was actually taken", nil
	}

	return "elevated, and the last run took a filesystem snapshot", nil
}
