package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/sion-backup/business/domain/legacy/legacybus"
)

// TestBucketFromURL. Eumaeus adopts a bucket by name; the legacy scripts hold
// a restic URL. Getting this wrong does not fail — `eumaeus backup adopt`
// would be handed a name that is not a bucket, and the operator would find
// out after the code was issued.
func TestBucketFromURL(t *testing.T) {
	for _, tc := range []struct {
		url    string
		bucket string
		prefix string
	}{
		{"s3:https://s3.us-central-1.wasabisys.com/bucket123", "bucket123", ""},
		{"s3:http://minio.example.org:9000/bucket123", "bucket123", ""},
		{"s3:s3.amazonaws.com/bucket123", "bucket123", ""},
		{"s3:https://s3.example.com/bucket123/dell3", "bucket123", "dell3"},
		{"s3:https://s3.example.com/bucket123/dell3/2019", "bucket123", "dell3/2019"},

		// Not S3 at all, and not something to guess at: the legacy fleet also
		// had a rest-server or two.
		{"rest:https://backup.example.org/dell3", "", ""},
		{"/mnt/usb/repo", "", ""},
		{"s3:https://s3.example.com", "", ""},
		{"s3:https://s3.example.com/", "", ""},
	} {
		bucket, prefix := bucketFromURL(tc.url)

		if bucket != tc.bucket || prefix != tc.prefix {
			t.Errorf("bucketFromURL(%q) = %q, %q; want %q, %q",
				tc.url, bucket, prefix, tc.bucket, tc.prefix)
		}
	}
}

// TestAssembleRefusesAMachineItCouldNotLookAt is the one refusal in this
// command that matters.
//
// An ordinary account cannot read /home/restic, which is where these installs
// live. Migrating on the strength of "nothing found" there gives the machine a
// second bucket, a full re-upload, and the old cron job still running beside
// the new install — and every step of it reports success.
func TestAssembleRefusesAMachineItCouldNotLookAt(t *testing.T) {
	_, err := assemble(Recon{Blocked: []string{"/home/restic"}})
	if err == nil {
		t.Fatal("assemble accepted a report that was not allowed to look")
	}

	if !strings.Contains(err.Error(), "/home/restic") {
		t.Errorf("the refusal does not name what could not be read: %v", err)
	}
}

// TestAssembleRefusesANewMachineWithSomewhereToGo. Nothing to adopt is not an
// error in the machine; it is the wrong command, and the message says which
// one is right.
func TestAssembleRefusesANewMachineWithSomewhereToGo(t *testing.T) {
	_, err := assemble(Recon{})
	if err == nil {
		t.Fatal("assemble accepted a machine with no legacy install")
	}

	if !strings.Contains(err.Error(), "enroll --code") {
		t.Errorf("the refusal does not say what to run instead: %v", err)
	}
}

// TestAssembleCarriesBothHalvesOfTheExcludeList.
//
// The excludes live in two places — the pseudo-filesystems on the command
// line, everything else in a file — and the file is the half nobody will
// write again. A plan that carried one and not the other would report that
// the excludes had been preserved.
func TestAssembleCarriesBothHalvesOfTheExcludeList(t *testing.T) {
	file := filepath.Join(t.TempDir(), "excludes.txt")

	if err := os.WriteFile(file, []byte("/home/user/Games\n# a note\n*.iso\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	a, err := assemble(Recon{Legacy: &legacybus.Install{
		Layout:          legacybus.LayoutLinux,
		Version:         "1.1",
		Dir:             "/home/restic/bin",
		Script:          "/home/restic/bin/backup.sh",
		NodeID:          "dell3-backup",
		RepositoryURL:   "s3:https://s3.us-central-1.wasabisys.com/bucket123",
		Targets:         []string{"/"},
		Excludes:        []string{"/dev", "/proc"},
		ExcludeFile:     file,
		ExcludeCount:    2,
		PackSizeMiB:     16,
		ReadConcurrency: 5,
		HasCredentials:  true,
	}})
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"/dev", "/proc", "/home/user/Games", "*.iso"}

	if !slices.Equal(a.plan.Excludes, want) {
		t.Errorf("excludes = %q, want %q", a.plan.Excludes, want)
	}

	if a.fromFile != 2 {
		t.Errorf("fromFile = %d, want 2", a.fromFile)
	}

	if a.plan.PackSizeMiB != 16 || a.plan.ReadConcurrency != 5 {
		t.Errorf("the tuning the legacy script had per machine was dropped: %+v", a.plan)
	}

	if a.bucket != "bucket123" {
		t.Errorf("bucket = %q", a.bucket)
	}

	// It is about to be seeded, and planbus refuses a plan that would produce
	// no usable backup. Finding that out here is cheaper than finding it out
	// in front of somebody's desk.
	if err := a.plan.Validate(); err != nil {
		t.Errorf("the assembled plan would not store: %v", err)
	}
}

// TestAssembleSaysWhenTheExcludeListCouldNotBeRead. Silently carrying the
// command-line half is the failure worth catching: the plan looks complete and
// the first backup includes things somebody chose to leave out.
func TestAssembleSaysWhenTheExcludeListCouldNotBeRead(t *testing.T) {
	a, err := assemble(Recon{Legacy: &legacybus.Install{
		NodeID:        "dell3-backup",
		Script:        "/home/restic/bin/backup.sh",
		RepositoryURL: "s3:https://s3.example.com/bucket123",
		Targets:       []string{"/"},
		Excludes:      []string{"/dev"},
		ExcludeFile:   filepath.Join(t.TempDir(), "gone.txt"),
		ExcludeCount:  12,
	}})
	if err != nil {
		t.Fatal(err)
	}

	if a.fromFile != 0 {
		t.Errorf("fromFile = %d on a file that could not be read", a.fromFile)
	}

	if !slices.ContainsFunc(a.notes, func(n string) bool { return strings.Contains(n, "gone.txt") }) {
		t.Errorf("nothing in the notes says the exclude list was not read: %q", a.notes)
	}
}

// TestAssembleKeepsVSSAndTheNodeID. Losing --use-fs-snapshot is a silent
// regression on exactly the machines that need it, and the node ID is the
// name the history is attached to.
func TestAssembleKeepsVSSAndTheNodeID(t *testing.T) {
	a, err := assemble(Recon{Legacy: &legacybus.Install{
		Layout:         legacybus.LayoutWindows,
		NodeID:         "gonzalo-backup",
		Script:         `C:\Users\backup\Documents\backup\backup.bat`,
		RepositoryURL:  "s3:https://s3.example.com/bucket123",
		Targets:        []string{`C:\Users\Gonzalo`},
		UsesFSSnapshot: true,
	}})
	if err != nil {
		t.Fatal(err)
	}

	if !a.plan.UseFSSnapshot {
		t.Error("--use-fs-snapshot was not carried over")
	}

	if a.plan.NodeID != "gonzalo-backup" {
		t.Errorf("node id = %q, want the legacy one", a.plan.NodeID)
	}
}

// TestAssembleFallsBackToTheHostnameAndSaysSo. Some of these scripts never
// set a node ID. A silent hostname would put a name nobody recognises on the
// dashboard beside years of history.
func TestAssembleFallsBackToTheHostnameAndSaysSo(t *testing.T) {
	a, err := assemble(Recon{Legacy: &legacybus.Install{
		Script:        "/home/restic/bin/backup.sh",
		RepositoryURL: "s3:https://s3.example.com/bucket123",
		Targets:       []string{"/"},
	}})
	if err != nil {
		t.Fatal(err)
	}

	if a.plan.NodeID != hostname() {
		t.Errorf("node id = %q, want the hostname", a.plan.NodeID)
	}

	if !slices.ContainsFunc(a.notes, func(n string) bool { return strings.Contains(n, "node ID") }) {
		t.Errorf("the fallback is not mentioned in the notes: %q", a.notes)
	}
}

// TestExcludeSummarySaysWhereTheyCameFrom. "20" on its own does not tell an
// operator whether their file arrived.
func TestExcludeSummarySaysWhereTheyCameFrom(t *testing.T) {
	a := adoption{install: &legacybus.Install{ExcludeFile: "/x/excludes.txt"}}

	if got := a.excludeSummary(); got != "none" {
		t.Errorf("empty summary = %q", got)
	}

	a.plan.Excludes = []string{"/dev", "/proc", "/home/user/Games"}
	a.fromFile = 1

	got := a.excludeSummary()

	for _, want := range []string{"3", "2 from the script", "1 from /x/excludes.txt"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary %q does not contain %q", got, want)
		}
	}
}

// TestSSHTargetComesFromTheServerTheMachineTalksTo. The Eumaeus commands are
// printed to be run somewhere else, and printing them bare means somebody
// retypes them into a second terminal — the same transcription this command
// exists to remove.
func TestSSHTargetComesFromTheServerTheMachineTalksTo(t *testing.T) {
	for _, tc := range []struct {
		override string
		url      string
		want     string
	}{
		{"", "https://terraboskamp.org", "terraboskamp.org"},
		{"", "https://terraboskamp.org:8443/", "terraboskamp.org"},
		{"admin@backup.example.org", "https://terraboskamp.org", "admin@backup.example.org"},

		// A loopback server is a test, or the machine you are already sitting
		// at. Neither wants an ssh line in front of the command.
		{"", "http://127.0.0.1:8088", ""},
		{"", "http://localhost:8088", ""},
		{"", "", ""},
	} {
		if got := sshTarget(tc.override, tc.url); got != tc.want {
			t.Errorf("sshTarget(%q, %q) = %q, want %q", tc.override, tc.url, got, tc.want)
		}
	}
}

// TestTheClaimSaysWhereTheMachineWritesEvenWhenItCouldNotLook.
//
// The repository URL is read out of the script and costs nothing, so it goes
// whether or not the repository opened — it is what lets Eumaeus refuse a code
// pointing at the wrong bucket. The count does not: a zero sent as a fact
// would be a claim that the old repository is empty.
func TestTheClaimSaysWhereTheMachineWritesEvenWhenItCouldNotLook(t *testing.T) {
	a := adoption{install: &legacybus.Install{
		RepositoryURL: "s3:https://s3.example.com/bucket123",
	}}

	got := a.legacy()

	if got.RepositoryURL != "s3:https://s3.example.com/bucket123" {
		t.Errorf("repository url = %q", got.RepositoryURL)
	}

	if got.Snapshots != 0 || !got.OldestSnapshot.IsZero() {
		t.Errorf("an unmeasured repository reported %d snapshots from %v",
			got.Snapshots, got.OldestSnapshot)
	}

	a.snapshots, a.oldest = 1412, time.Date(2019, 3, 1, 0, 0, 0, 0, time.UTC)

	if got := a.legacy(); got.Snapshots != 1412 || !got.OldestSnapshot.Equal(a.oldest) {
		t.Errorf("a measured repository reported %d snapshots from %v",
			got.Snapshots, got.OldestSnapshot)
	}
}
