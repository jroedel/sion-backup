// Package netcost answers one question: is this machine on a connection
// somebody is paying for by the byte?
//
// # Why a backup program cares
//
// It mostly does not, and that is the important half. A backup that does not
// happen is the failure this whole program exists to prevent, so a backup runs
// on whatever connection is there — a person tethering in a hotel still wants
// last night's work safe, and a machine that quietly skipped a fortnight
// because it never saw an Ethernet cable is exactly the silent stop the fleet
// dashboard was built to catch.
//
// What this gates is the work that is optional and large: re-reading pack data
// out of the repository to prove it is still sound. That is worth doing, it is
// worth doing regularly, and it is not worth doing over somebody's phone.
//
// # What it can actually tell
//
// Less than one would like, and the honesty about which is the point:
//
//   - Linux: NetworkManager knows, and says so. This is the one platform where
//     the answer is a real answer.
//   - Windows: the answer exists — WinRT's ConnectionCost — and reaching it
//     from a program that does not use cgo means hand-rolled COM activation.
//     Not done. [Unknown].
//   - macOS: the equivalent is NWPathMonitor's isExpensive, which is
//     Objective-C. Not done. [Unknown].
//
// So on two of three platforms this reports [Unknown], and the config override is
// how an administrator says what the machine cannot work out. That is a real
// gap rather than a design: see docs/todo.md.
//
// # What Unknown should mean to a caller
//
// Not "assume metered". Two thirds of the fleet would then never verify a
// repository, which trades a certain harm for a possible one. The caller's
// policy is in business/domain/plan: run the slice, keep it small enough that
// being wrong costs a few megabytes rather than a phone bill.
package netcost

import "context"

// Cost is what a byte costs here.
type Cost int

const (
	// Unknown means this platform cannot say. It is the usual answer on
	// Windows and macOS and must not be read as either of the others.
	Unknown Cost = iota

	// Unmetered is a connection nobody is billed by the byte for.
	Unmetered

	// Metered is a connection somebody is: a phone tether, a mobile
	// broadband stick, a hotel plan sold by the gigabyte.
	Metered
)

func (c Cost) String() string {
	switch c {
	case Unmetered:
		return "unmetered"
	case Metered:
		return "metered"
	default:
		return "unknown"
	}
}

// Metered reports whether c is known to be metered. Unknown is not metered,
// deliberately — see the package comment on why the doubt resolves this way.
func (c Cost) Metered() bool { return c == Metered }

// Of reports what this machine's current connection costs.
//
// It never returns an error. Every failure to find out is [Unknown], because
// there is nothing a caller could do with the difference between "NetworkManager
// is not installed" and "NetworkManager did not answer" that it would not also
// do with "cannot tell".
func Of(ctx context.Context) Cost { return detect(ctx) }
