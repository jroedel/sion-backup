package netcost_test

import (
	"context"
	"testing"

	"github.com/jroedel/sion-backup/foundation/netcost"
)

// TestUnknownIsNotMetered pins the decision the package comment argues for.
//
// Two of the three platforms in this fleet cannot tell, so reading Unknown as
// metered would mean no Windows or Mac ever verified its repository — trading
// a certain harm for a possible one.
func TestUnknownIsNotMetered(t *testing.T) {
	if netcost.Unknown.Metered() {
		t.Error("Unknown reported itself as metered")
	}

	if netcost.Unmetered.Metered() {
		t.Error("Unmetered reported itself as metered")
	}

	if !netcost.Metered.Metered() {
		t.Error("Metered did not report itself as metered")
	}
}

// TestOfNeverPanics. It runs before a backup on every platform, including ones
// where nothing it looks for exists.
func TestOfNeverPanics(t *testing.T) {
	t.Log("this machine reports:", netcost.Of(context.Background()))
}
