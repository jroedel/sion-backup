//go:build !windows

package restic

import "os"

// interrupt asks restic to stop the way a person at a terminal would, with
// Ctrl-C.
//
// It matters which signal: restic handles an interrupt by finishing the pack
// it is uploading, removing its lock and exiting. Killed instead, it leaves
// the lock behind, and the next backup is refused by a repository that looks
// busy to a machine that is not.
func interrupt(p *os.Process) error {
	return p.Signal(os.Interrupt)
}
