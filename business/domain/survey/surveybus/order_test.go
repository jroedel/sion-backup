package surveybus

import "testing"

// TestTheChosenListIsWalkedFirst.
//
// An internal test, and it is the only one in this package, because what it
// checks is a decision rather than a behaviour: the walks happen one after
// another, and this is the order. From outside the package that order is
// invisible — every style ends up measured either way, and waiting for two
// small temporary directories cannot tell you which was measured first.
//
// What it protects: on a machine adopt-enroll set up, the folders came out of
// the legacy script and are the only answer on the page. Walking the whole home
// directory before them puts the figure somebody is waiting for last, which on
// a large home directory is minutes. That is not hypothetical — it is how the
// first version of this behaved, and it turned up as a CI timeout on a macOS
// runner rather than as anything anybody had noticed.
func TestTheChosenListIsWalkedFirst(t *testing.T) {
	offered := []Choice{
		{Style: StylePersonal, Roots: []string{"/home/jeff/Documents"}},
		{Style: StyleHome, Roots: []string{"/home/jeff"}},
	}

	t.Run("with a list somebody wrote", func(t *testing.T) {
		got := order(offered, []string{"/srv/work"})

		if len(got) != 3 {
			t.Fatalf("%d choices, want 3", len(got))
		}

		if got[0].Style != StyleCustom {
			t.Errorf("the first walk is %q, want the chosen list", got[0].Style)
		}

		if got[0].Roots[0] != "/srv/work" {
			t.Errorf("the custom choice walks %v, want /srv/work", got[0].Roots)
		}

		// And the offered ones keep their own order behind it, which is
		// cheapest first.
		if got[1].Style != StylePersonal || got[2].Style != StyleHome {
			t.Errorf("the offered choices were reordered: %q then %q", got[1].Style, got[2].Style)
		}
	})

	t.Run("with no list, on a machine just enrolled", func(t *testing.T) {
		got := order(offered, nil)

		if len(got) != 2 || got[0].Style != StylePersonal {
			t.Errorf("got %v, want the offered choices unchanged", got)
		}
	})

	t.Run("the caller's slice is not modified", func(t *testing.T) {
		// append onto a slice with spare capacity would otherwise write
		// through into the Business's own choices, and the second render of
		// the page would offer the custom list as though it were one of them.
		choices := make([]Choice, 2, 8)
		copy(choices, offered)

		order(choices, []string{"/srv/work"})

		if choices[0].Style != StylePersonal || choices[1].Style != StyleHome {
			t.Errorf("ordering scribbled on the choices it was given: %v", choices)
		}
	})
}
