//go:build windows

package restic

import "os"

// interrupt stops restic.
//
// Windows has no signal to send a process that is not sharing this one's
// console, and restic here is a child with its own pipes rather than a
// program at a terminal. So it is killed, which leaves the repository lock
// behind — see backupbus, which clears it after a cancelled run for exactly
// this reason.
func interrupt(p *os.Process) error {
	return p.Kill()
}
