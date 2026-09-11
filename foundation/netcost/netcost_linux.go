//go:build linux

package netcost

import (
	"bufio"
	"context"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// detectTimeout bounds the lookup. This runs before a backup and a slow answer
// must never delay one; not knowing is a perfectly good answer.
const detectTimeout = 3 * time.Second

// detect asks NetworkManager about the interface carrying the default route.
//
// NetworkManager rather than a heuristic, because it is the only thing on the
// machine that actually knows: it learns the answer from the mobile broadband
// modem, from a tethered phone's DHCP options, or from somebody ticking the
// box in the connection editor. Guessing from the interface name would call a
// USB-tethered phone "usb0" and a docking station "usb0" as well.
//
// Through nmcli rather than D-Bus. The property wanted is one uint32 on one
// object, and reaching it directly means either a D-Bus dependency this
// repository does not have or several hundred lines of hand-rolled protocol
// for a question whose answer is allowed to be "cannot tell".
func detect(ctx context.Context) Cost {
	iface := defaultRouteInterface()
	if iface == "" {
		return Unknown
	}

	ctx, cancel := context.WithTimeout(ctx, detectTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, "nmcli",
		"-t", "-f", "GENERAL.METERED", "device", "show", iface).Output()
	if err != nil {
		// No NetworkManager, or it does not manage this device. Both are
		// ordinary on a server and neither is worth an error.
		return Unknown
	}

	// "GENERAL.METERED:yes (guessed)" and friends. The word is what matters;
	// NetworkManager's numeric codes are 1 yes, 2 no, 3 guessed yes, 4
	// guessed no, and a guess is still the best information anybody has.
	field := strings.TrimSpace(string(out))

	if _, value, found := strings.Cut(field, ":"); found {
		switch {
		case strings.HasPrefix(value, "yes"):
			return Metered
		case strings.HasPrefix(value, "no"):
			return Unmetered
		}
	}

	return Unknown
}

// defaultRouteInterface reads /proc/net/route for the interface carrying the
// default route.
//
// From /proc rather than by running `ip`, because it is one file, it is always
// there, and a program that shells out to find out which program to shell out
// to is a program with two ways to fail.
func defaultRouteInterface() string {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return ""
	}
	defer f.Close()

	return defaultRouteFrom(f)
}

// defaultRouteFrom is defaultRouteInterface's parsing, split out so it can be
// given a file rather than only the one at /proc/net/route.
func defaultRouteFrom(r io.Reader) string {
	scanner := bufio.NewScanner(r)

	// The header.
	if !scanner.Scan() {
		return ""
	}

	best := ""
	bestMetric := -1

	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 7 {
			continue
		}

		// Destination 00000000 is the default route. Several may exist — a
		// docked laptop with wifi still associated has two — so the one with
		// the lowest metric is the one traffic actually takes.
		if fields[1] != "00000000" {
			continue
		}

		metric, err := strconv.Atoi(fields[6])
		if err != nil {
			continue
		}

		if bestMetric == -1 || metric < bestMetric {
			best, bestMetric = fields[0], metric
		}
	}

	return best
}

// supported is true here: NetworkManager is asked, and it actually knows.
//
// True even on a Linux machine with no NetworkManager on it, which is the
// honest reading: what this reports is whether the build can tell, and on this
// platform it can. A machine where the lookup then fails gets Unknown from
// [Of], which is the same answer it would get if the network were simply
// unplugged.
const supported = true
