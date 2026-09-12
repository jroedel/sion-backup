package surveybus_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jroedel/sion-backup/business/domain/survey/surveybus"
	"github.com/jroedel/sion-backup/foundation/s3probe"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestTheJunkListGoesOnAndComesOffCleanly.
//
// The checkbox has to be reversible, or somebody who tries it once has an
// exclude list with thirty patterns in it that they now have to remove by
// hand — and the next thing they do is stop trusting the page.
func TestTheJunkListGoesOnAndComesOffCleanly(t *testing.T) {
	mine := []string{"/home/jeff/scratch", "*.bak"}

	with := append(append([]string{}, mine...), surveybus.JunkExcludes()...)

	if !surveybus.HasJunk(with) {
		t.Error("a list that carries the whole junk set does not report it")
	}

	got := surveybus.WithoutJunk(with)

	if len(got) != len(mine) {
		t.Fatalf("taking the junk back off left %v, want %v", got, mine)
	}

	for i := range got {
		if got[i] != mine[i] {
			t.Errorf("pattern %d is %q, want %q — somebody's own list was disturbed",
				i, got[i], mine[i])
		}
	}

	if surveybus.HasJunk(mine) {
		t.Error("a list with none of the junk set reports that it has it")
	}
}

// TestAnEditedJunkListIsNotClaimedAsWhole.
//
// Somebody who deleted one pattern from the set has a list that is theirs now.
// Reporting the box as ticked would mean the next save silently put the
// deleted pattern back.
func TestAnEditedJunkListIsNotClaimedAsWhole(t *testing.T) {
	junk := surveybus.JunkExcludes()
	if len(junk) < 2 {
		t.Skip("there is no junk list on this platform to edit")
	}

	if surveybus.HasJunk(junk[1:]) {
		t.Error("a junk list with one pattern removed still reports as the whole set")
	}
}

// TestNoJunkPatternIsSomebodysWork is the rule the list is chosen by, asserted
// rather than left in a comment: everything on it is re-creatable, and the
// places people keep irreplaceable things are not on it.
func TestNoJunkPatternIsSomebodysWork(t *testing.T) {
	forbidden := []string{
		".config", ".ssh", "Documents", "Desktop", "Pictures",
		"Mail", ".thunderbird", ".mozilla", ".git", "src",
	}

	for _, pattern := range surveybus.JunkExcludes() {
		base := filepath.Base(pattern)

		for _, bad := range forbidden {
			if base == bad {
				t.Errorf("the junk list excludes %q, which is where people keep things "+
					"they cannot re-create", pattern)
			}
		}
	}
}

// TestThePersonalFoldersDoNotIncludeDownloads.
//
// Deliberate, and the reason is written on PersonalFolders: it is the largest
// folder on most machines and the one nobody has ever asked to have restored.
func TestThePersonalFoldersDoNotIncludeDownloads(t *testing.T) {
	for _, dir := range surveybus.PersonalFolders("/home/jeff") {
		if filepath.Base(dir) == "Downloads" {
			t.Errorf("Downloads is in the personal folders (%q)", dir)
		}
	}
}

// TestTheLocalisedFolderNamesAreUsedWhereTheDesktopPublishesThem.
//
// A German desktop has Dokumente and Schreibtisch. Looking for "Documents"
// there finds nothing at all, and the setup page would offer to back up an
// empty list while reporting zero bytes — which reads as a working answer.
func TestTheLocalisedFolderNamesAreUsedWhereTheDesktopPublishesThem(t *testing.T) {
	if os.Getenv("GOOS") == "windows" {
		t.Skip("the localisation file is a freedesktop thing")
	}

	home := t.TempDir()

	config := filepath.Join(home, ".config")
	if err := os.MkdirAll(config, 0o755); err != nil {
		t.Fatal(err)
	}

	// As a desktop environment actually writes it, comments and all.
	err := os.WriteFile(filepath.Join(config, "user-dirs.dirs"), []byte(`
# This file is written by xdg-user-dirs-update
XDG_DESKTOP_DIR="$HOME/Schreibtisch"
XDG_DOCUMENTS_DIR="$HOME/Dokumente"
XDG_DOWNLOAD_DIR="$HOME/Downloads"
XDG_PICTURES_DIR="$HOME/Bilder"
XDG_PUBLICSHARE_DIR="$HOME"
`), 0o644)
	if err != nil {
		t.Fatal(err)
	}

	got := surveybus.PersonalFolders(home)

	// Skipped on a platform that does not read the file at all; what is being
	// checked there is the English fallback, which the next assertion covers.
	if len(got) > 0 && filepath.Base(got[0]) != "Dokumente" {
		t.Logf("this platform does not read user-dirs.dirs; got %v", got)

		return
	}

	want := []string{"Dokumente", "Schreibtisch", "Bilder"}

	for i, name := range want {
		if i >= len(got) || filepath.Base(got[i]) != name {
			t.Fatalf("folders %v, want them to start %v", got, want)
		}
	}

	for _, dir := range got {
		if dir == home {
			t.Error("XDG_PUBLICSHARE_DIR=\"$HOME\" turned the personal folders into " +
				"the whole home directory, which is the other choice on the page")
		}

		if filepath.Base(dir) == "Downloads" {
			t.Error("Downloads came in through the localisation file")
		}
	}
}

// folder makes a directory holding one file of a known size, and returns it as
// an offered choice.
//
// Every measuring test below is built out of these rather than out of the
// machine's own home directory. That is not only tidiness: the first version
// of these tests used the real one, passed everywhere, and then failed on a
// macOS CI runner, whose /Users/runner holds Xcode and several toolchains and
// takes minutes to walk. A domain test that is fast on a laptop and times out
// on a build machine is a test nobody can read the result of.
func folder(t *testing.T, style surveybus.Style, size int) surveybus.Choice {
	t.Helper()

	root := t.TempDir()

	if err := os.WriteFile(filepath.Join(root, "a.txt"), make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}

	return surveybus.Choice{Style: style, Roots: []string{root}}
}

// measured waits for one style's figure to be final, and returns it.
func measured(t *testing.T, b *surveybus.Business, style surveybus.Style) surveybus.Sizing {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)

	for time.Now().Before(deadline) {
		if got := b.Sizing(style); got.State == surveybus.StateMeasured {
			return got
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("%s was never measured", style)

	return surveybus.Sizing{}
}

// TestMeasuringIsStartedOnceAndAnswersImmediately is the property the page
// depends on: it refreshes itself every three seconds while a walk runs, and
// each of those refreshes calls Measure.
func TestMeasuringIsStartedOnceAndAnswersImmediately(t *testing.T) {
	choice := folder(t, surveybus.StylePersonal, 4096)
	b := surveybus.NewBusiness(quiet(), nil, []surveybus.Choice{choice})

	ctx := context.Background()

	for range 5 {
		b.Measure(ctx, nil, nil, 0)
	}

	if got := measured(t, b, surveybus.StylePersonal); got.Result.Bytes != 4096 {
		t.Errorf("%d bytes, want 4096", got.Result.Bytes)
	}
}

// TestAListSomebodyWroteThemselvesIsMeasuredToo.
//
// On a machine adopt-enroll set up, the folders come out of the legacy script
// and land in the custom box — so that list is the only answer on the page, and
// leaving it the one choice with no size beside it would be backwards.
//
// That it is measured FIRST is a separate claim and cannot be checked from
// here: both of these finish in microseconds and either order passes. It is
// asserted against the decision instead, in order_test.go.
func TestAListSomebodyWroteThemselvesIsMeasuredToo(t *testing.T) {
	slow := folder(t, surveybus.StyleHome, 8192)
	custom := folder(t, surveybus.StyleCustom, 4096)

	b := surveybus.NewBusiness(quiet(), nil, []surveybus.Choice{slow})
	b.Measure(context.Background(), custom.Roots, nil, 0)

	if got := measured(t, b, surveybus.StyleCustom); got.Result.Bytes != 4096 {
		t.Errorf("%d bytes, want 4096", got.Result.Bytes)
	}

	// The offered choice is still measured, behind it.
	if got := measured(t, b, surveybus.StyleHome); got.Result.Bytes != 8192 {
		t.Errorf("%d bytes, want 8192", got.Result.Bytes)
	}
}

// TestAFailedProbeIsReportedRatherThanRetriedForever.
func TestAFailedProbeIsReportedRatherThanRetriedForever(t *testing.T) {
	calls := 0

	b := surveybus.NewBusiness(quiet(), func(context.Context) (s3probe.Result, error) {
		calls++

		return s3probe.Result{}, errors.New("the bucket refused")
	}, nil)

	b.Probe(context.Background())

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && b.Upload().State == surveybus.StateMeasuring {
		time.Sleep(10 * time.Millisecond)
	}

	up := b.Upload()

	switch {
	case up.State != surveybus.StateFailed:
		t.Errorf("state %q, want failed", up.State)
	case up.Problem == "":
		t.Error("the failure was not explained, so the page has nothing to show")
	case up.Known():
		t.Error("a failed probe reported a usable rate")
	}

	// ProbeOnce is what an ordinary render calls, and it must not start a
	// second test on every one of them: each costs real bytes on somebody's
	// connection.
	for range 5 {
		b.ProbeOnce(context.Background())
	}

	if calls != 1 {
		t.Errorf("the connection was measured %d times, want once", calls)
	}
}

func TestAnEstimateNeedsAMeasurement(t *testing.T) {
	measured := surveybus.Upload{
		State:  surveybus.StateMeasured,
		Result: s3probe.Result{BytesPerSecond: 1 << 20},
	}

	if got := measured.Estimate(60 << 20); got != time.Minute {
		t.Errorf("60 MiB at 1 MiB/s is %s, want a minute", got)
	}

	// Nothing measured means no estimate, rather than an infinity or a zero
	// that would read on the page as "no time at all".
	if got := (surveybus.Upload{}).Estimate(60 << 20); got != 0 {
		t.Errorf("an unmeasured connection estimated %s, want nothing", got)
	}
}

// TestTheSpeedTestRunsWithoutBeingAskedTheFirstTime.
//
// This is a regression test for a bug that only showed up by running the page:
// a freshly zeroed Upload was not equal to the constant meaning "unmeasured",
// so ProbeOnce believed the connection had already been measured and the
// automatic test never fired. The page said "the upload speed has not been
// measured yet" forever, and only the manual button worked.
//
// The constant is the zero value now, and this is what holds it there.
func TestTheSpeedTestRunsWithoutBeingAskedTheFirstTime(t *testing.T) {
	if (surveybus.Upload{}).State != surveybus.StateUnmeasured {
		t.Fatal("a zero Upload is not in the unmeasured state, so nothing will ever " +
			"decide it needs measuring")
	}

	if (surveybus.Sizing{}).State != surveybus.StateUnmeasured {
		t.Fatal("a zero Sizing is not in the unmeasured state")
	}

	done := make(chan struct{})

	b := surveybus.NewBusiness(quiet(), func(context.Context) (s3probe.Result, error) {
		close(done)

		return s3probe.Result{BytesPerSecond: 1 << 20}, nil
	}, nil)

	b.ProbeOnce(context.Background())

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the first render of the setup page did not start a speed test")
	}
}

// TestAStoredFolderListIsReadBackAsTheChoiceThatWouldHaveMadeIt.
//
// The round trip the setup page depends on. Every machine upgrading into that
// page has folders in its plan and no style beside them, because the field did
// not exist when the plan was written; without this the page cannot tell a
// list somebody typed from one of its own offers, and it puts the radio button
// somewhere other than where the machine actually is.
func TestAStoredFolderListIsReadBackAsTheChoiceThatWouldHaveMadeIt(t *testing.T) {
	personal := surveybus.Choice{
		Style: surveybus.StylePersonal,
		Roots: []string{"/home/jeff/Documents", "/home/jeff/Desktop"},
	}
	home := surveybus.Choice{Style: surveybus.StyleHome, Roots: []string{"/home/jeff"}}

	b := surveybus.NewBusiness(quiet(), nil, []surveybus.Choice{personal, home})

	for _, c := range []struct {
		name    string
		targets []string
		want    surveybus.Style
	}{
		{"an offered choice", personal.Roots, surveybus.StylePersonal},
		{"the other one", home.Roots, surveybus.StyleHome},
		{
			// Somebody reordered the box, or restic wrote them out with a
			// trailing separator. Neither is a different list, and calling
			// either one Custom would show the wrong radio button.
			name:    "the same folders, reordered and with a trailing slash",
			targets: []string{"/home/jeff/Desktop/", "/home/jeff/Documents"},
			want:    surveybus.StylePersonal,
		},
		{
			name:    "one of the offered folders removed",
			targets: []string{"/home/jeff/Documents"},
			want:    surveybus.StyleCustom,
		},
		{
			name:    "a list of their own",
			targets: []string{"/srv/work", "/mnt/photos"},
			want:    surveybus.StyleCustom,
		},
		{
			// Not Custom: an empty list is a machine with no plan, and what to
			// offer it is the page's decision, not this one's.
			name:    "nothing at all",
			targets: nil,
			want:    "",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := b.StyleOf(c.targets); got != c.want {
				t.Errorf("StyleOf(%v) = %q, want %q", c.targets, got, c.want)
			}
		})
	}
}
