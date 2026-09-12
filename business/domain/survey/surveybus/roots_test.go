package surveybus_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jroedel/sion-backup/business/domain/survey/surveybus"
)

// TestAFolderThatCannotBeBackedUpIsFoundBeforeItIsSaved.
//
// The failure being closed is a silent one. restic reports a missing target on
// its own output, backs up everything else, and exits zero: the run is
// recorded as a success, the status page is green, and the folder somebody
// mistyped has not been in a backup since the day they typed it.
func TestAFolderThatCannotBeBackedUpIsFoundBeforeItIsSaved(t *testing.T) {
	good := t.TempDir()

	file := filepath.Join(good, "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name  string
		root  string
		wants string
	}{
		{"a folder that is there", good, ""},
		{"a file, which restic also accepts", file, ""},
		{"a typo", filepath.Join(good, "Documnets"), "is not on this computer"},
		{
			// The quiet one: it would resolve against whatever directory the
			// daemon happened to start in.
			name:  "a relative path",
			root:  "Documents",
			wants: "full path",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := surveybus.CheckRoots([]string{c.root})

			if c.wants == "" {
				if len(got) != 0 {
					t.Fatalf("%q was refused: %v", c.root, got)
				}

				return
			}

			if len(got) != 1 {
				t.Fatalf("%q produced %v, want one problem", c.root, got)
			}

			if !strings.Contains(got[0].Why, c.wants) {
				t.Errorf("the reason is %q, want it to mention %q", got[0].Why, c.wants)
			}
		})
	}
}

// TestEveryBadFolderIsReportedNotJustTheFirst. Somebody who pasted four paths
// out of the old backup script and got two wrong should be told twice once,
// rather than once twice.
func TestEveryBadFolderIsReportedNotJustTheFirst(t *testing.T) {
	good := t.TempDir()

	got := surveybus.CheckRoots([]string{
		filepath.Join(good, "nope"), good, filepath.Join(good, "also-nope"),
	})

	if len(got) != 2 {
		t.Fatalf("%d problems, want 2: %v", len(got), got)
	}
}

// TestAFolderThisAccountCannotReadIsRefused.
//
// Not the same as a missing one, and the difference is what somebody does
// next: a typo is retyped, an unreadable folder is a permissions decision. It
// is also the shape of the mistake this program has already made once, by
// enrolling as root into a directory the daemon could not read.
func TestAFolderThisAccountCannotReadIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits do not work this way on Windows")
	}

	if os.Geteuid() == 0 {
		t.Skip("root can read anything, which is the whole problem with running as root")
	}

	shut := filepath.Join(t.TempDir(), "shut")
	if err := os.Mkdir(shut, 0o000); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { os.Chmod(shut, 0o700) })

	got := surveybus.CheckRoots([]string{shut})

	if len(got) != 1 {
		t.Fatalf("an unreadable folder produced %v, want one problem", got)
	}

	if !strings.Contains(got[0].Why, "account") {
		t.Errorf("the reason is %q, want it to say whose permission is missing", got[0].Why)
	}
}

// TestWhatIsInsideAFolderIsNotChecked is the other half of the rule, and it
// matters more than it looks. Every home directory has something in it that
// cannot be read — a stale gvfs mount, another account's file left behind —
// and refusing to save a plan over one of those would mean refusing to back up
// the machine at all. The walk counts those and the page reports them as "N
// folders could not be read", which is the right weight for them.
func TestWhatIsInsideAFolderIsNotChecked(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs an account that mode bits apply to")
	}

	root := t.TempDir()

	shut := filepath.Join(root, "shut")
	if err := os.Mkdir(shut, 0o000); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { os.Chmod(shut, 0o700) })

	if got := surveybus.CheckRoots([]string{root}); len(got) != 0 {
		t.Errorf("a folder was refused for something inside it: %v", got)
	}
}

// TestTheExclusionsShownAreTheOnesThatCouldApply.
//
// The value of showing somebody what their choice comes to is that it is short
// enough to read. A machine backing up /opt/projects does not need to be told
// that /home/jeff/.cache is excluded: it is true, it is irrelevant, and three
// lines of it teach somebody that this part of the page is not worth reading.
func TestTheExclusionsShownAreTheOnesThatCouldApply(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("these are posix paths")
	}

	got := surveybus.ExcludesFor([]string{"/opt/projects"}, []string{
		"/home/jeff/.cache",
		"/home/jeff/Downloads",
		"/opt/projects/scratch",
		"*.iso",
		"node_modules",
	})

	want := []string{"/opt/projects/scratch", "*.iso", "node_modules"}

	if len(got) != len(want) {
		t.Fatalf("shown %v, want %v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Errorf("shown %v, want %v", got, want)

			break
		}
	}
}

// TestTheHomeFolderKeepsItsOwnExclusions is the case somebody actually looks
// at: "everything in my user folder", and the Downloads folder that is not in
// it after all.
func TestTheHomeFolderKeepsItsOwnExclusions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("these are posix paths")
	}

	got := surveybus.ExcludesFor([]string{"/home/jeff"},
		[]string{"/home/jeff/Downloads", "/home/jeff/.cache", "/opt/projects/scratch"})

	if len(got) != 2 {
		t.Fatalf("shown %v, want the two under /home/jeff", got)
	}
}
