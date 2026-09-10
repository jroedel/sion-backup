package legacyscan

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

// TestFindReportsWhatItCouldNotRead is the regression for the worst thing
// this scan can do, which is not "miss an install" but "miss an install
// silently".
//
// An ordinary account cannot look inside /home/restic, which is where the 1.1
// install notes put the backup. os.Stat says "permission denied", the old
// code read that as "no script here", and recon printed "none found" — which
// its plan turns into "this is a new machine as far as backups go". Acting on
// that sentence means a second bucket, a full re-upload of everything, and
// the old cron job still running beside the new install.
func TestFindReportsWhatItCouldNotRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permissions")
	}

	if os.Geteuid() == 0 {
		t.Skip("root can read everything, which is the case this is about")
	}

	// A home directory shaped like the real one: 0750, with the install
	// inside it where this account cannot see.
	root := t.TempDir()
	home := filepath.Join(root, "restic")
	dir := filepath.Join(home, "bin")

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "backup.sh"), []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(home, 0o000); err != nil {
		t.Fatal(err)
	}

	// Put it back, or the temp directory cannot be removed.
	t.Cleanup(func() { _ = os.Chmod(home, 0o755) })

	got := Find(t.Context(), dir)

	if got.Install != nil {
		t.Fatalf("read an install this account cannot see: %+v", got.Install)
	}

	if !slices.Contains(got.Blocked, home) {
		t.Fatalf("Blocked is %v, want it to name %s — an unreadable directory "+
			"must not be reported as an empty one", got.Blocked, home)
	}
}

// TestFindNamesTheDirectoryOnceRatherThanEveryPathUnderIt keeps the report
// readable: one unreadable home blocks three candidate paths beneath it, and
// three identical lines about the same directory is noise somebody learns to
// skip.
func TestFindNamesTheDirectoryOnceRatherThanEveryPathUnderIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permissions")
	}

	if os.Geteuid() == 0 {
		t.Skip("root can read everything")
	}

	root := t.TempDir()
	home := filepath.Join(root, "restic")

	if err := os.MkdirAll(filepath.Join(home, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(home, 0o000); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.Chmod(home, 0o755) })

	got := Find(t.Context(),
		filepath.Join(home, "bin"),
		filepath.Join(home, "backup"),
		filepath.Join(home, "Documents", "backup"))

	var times int

	for _, dir := range got.Blocked {
		if dir == home {
			times++
		}
	}

	if times != 1 {
		t.Errorf("%s is named %d times in %v, want once", home, times, got.Blocked)
	}
}

// TestLookInSaysNothingAboutADirectoryThatIsNotThere is the other half:
// absent must stay absent. A scan that reported every missing candidate as
// blocked would make the warning meaningless on the machines that really are
// new, and a warning nobody can act on is one everybody learns to skip.
//
// Against lookIn rather than Find, because Find also visits this machine's
// real candidate directories and one of those may well be unreadable — which
// is the whole subject of this file.
func TestLookInSaysNothingAboutADirectoryThatIsNotThere(t *testing.T) {
	for _, dir := range []string{
		filepath.Join(t.TempDir(), "nothing-here"), // no such directory
		t.TempDir(), // there, and empty
	} {
		script, _, blocked := lookIn(dir)

		if script != "" {
			t.Errorf("lookIn(%s) found %s", dir, script)
		}

		if blocked != "" {
			t.Errorf("lookIn(%s) reported %s as unreadable; it is simply not there",
				dir, blocked)
		}
	}
}

// TestFindReadsAnInstallItCanSee is the ordinary path, so that the two tests
// above are about permissions rather than about a scan that finds nothing at
// all.
func TestFindReadsAnInstallItCanSee(t *testing.T) {
	dir := t.TempDir()

	script := "#!/bin/sh\n" +
		"export RESTIC_REPOSITORY=s3:https://s3.example.invalid/a-bucket\n" +
		"restic backup /home/somebody\n"

	if err := os.WriteFile(filepath.Join(dir, "backup.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	got := Find(t.Context(), dir)

	if got.Install == nil {
		t.Fatal("did not find an install that is plainly there")
	}

	if got.Install.Dir != dir {
		t.Errorf("Dir is %s, want %s", got.Install.Dir, dir)
	}

	if slices.Contains(got.Blocked, dir) {
		t.Errorf("a directory that was read is also reported as blocked: %v", got.Blocked)
	}
}
