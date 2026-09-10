//go:build linux

package netcost

import (
	"strings"
	"testing"
)

// TestTheDefaultRouteIsTheLowestMetricOne. A docked laptop with wifi still
// associated has two default routes, and the metered answer depends on picking
// the one traffic actually takes: the dock is not metered and the phone it is
// still associated with may be.
func TestTheDefaultRouteIsTheLowestMetricOne(t *testing.T) {
	// The real shape of /proc/net/route: tab-separated, hex, header first.
	const table = `Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask		MTU	Window	IRTT
wlan0	00000000	0102A8C0	0003	0	0	600	00000000	0	0	0
eth0	00000000	0102A8C0	0003	0	0	100	00000000	0	0	0
eth0	0002A8C0	00000000	0001	0	0	100	00FFFFFF	0	0	0
`

	if got := defaultRouteFrom(strings.NewReader(table)); got != "eth0" {
		t.Errorf("default route = %q, want eth0 (metric 100, not wlan0's 600)", got)
	}
}

// TestNoDefaultRouteIsNotAnInterface, because a machine with no route out is a
// machine this cannot say anything about — and must not name the first line it
// happens to find.
func TestNoDefaultRouteIsNotAnInterface(t *testing.T) {
	const table = `Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask		MTU	Window	IRTT
eth0	0002A8C0	00000000	0001	0	0	100	00FFFFFF	0	0	0
`

	if got := defaultRouteFrom(strings.NewReader(table)); got != "" {
		t.Errorf("default route = %q, want none", got)
	}
}

func TestAnEmptyTableIsNotAnInterface(t *testing.T) {
	if got := defaultRouteFrom(strings.NewReader("")); got != "" {
		t.Errorf("default route = %q, want none", got)
	}
}
