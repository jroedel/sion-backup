// Command brokenbuild is a release that passes every check self-update makes
// and then does not work.
//
// It answers "version" with whatever version it was stamped with, which is the
// entire smoke test in foundation/selfupdate — `version` returns from main's
// dispatch before wire() runs, so it opens no database, reads no config and
// touches no restic. Then it fails the way a real bad build fails: on the
// command the service manager actually runs.
//
// It exists because the harness needs to demonstrate the gap rather than argue
// about it. A build with a broken schema migration, a config field that no
// longer parses, or a nil dereference in the daemon's wiring is
// indistinguishable — to the update path — from a good one.
package main

import (
	"fmt"
	"os"
	"runtime"
)

// version is stamped the same way the real binary's is.
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Printf("sion-backup %s (%s/%s, %s)\n", version, runtime.GOOS, runtime.GOARCH, runtime.Version())

		return
	}

	// Where a bad build actually dies: after the service manager has started
	// it, having already replaced the one that worked.
	fmt.Fprintln(os.Stderr, "sion-backup: opening the database: migration 7: no such column: verified_at")
	os.Exit(1)
}
