package restic

import (
	"runtime"
	"testing"
)

func TestTheHostOfARepositoryURL(t *testing.T) {
	for _, tc := range []struct{ url, want string }{
		{"s3:https://s3.us-central-1.wasabisys.com/dell3itrugnh", "s3.us-central-1.wasabisys.com"},
		{"s3:https://s3.us-central-1.wasabisys.com/bucket/prefix", "s3.us-central-1.wasabisys.com"},
		{"https://example.invalid/bucket", "example.invalid"},
		{"s3:s3.amazonaws.com/bucket", "s3.amazonaws.com"},
		{"/srv/backups", ""},
		{"", ""},
	} {
		if got := HostOf(tc.url); got != tc.want {
			t.Errorf("HostOf(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}

func TestAFailureToResolveNamesTheHost(t *testing.T) {
	// The real thing, from a machine whose resolver stub was not answering.
	stderr := `Save(<data/765ff5926d>) returned error, retrying after 1.074152134s: ` +
		`client.PutObject: Put "https://s3.us-central-1.wasabisys.com/dell3itrugnh/data/76/765ff": ` +
		`dial tcp: lookup s3.us-central-1.wasabisys.com on 127.0.0.53:53: ` +
		`read udp 127.0.0.1:42902->127.0.0.53:53: i/o timeout`

	host, ok := (&Error{Stderr: stderr}).Unresolvable()
	if !ok {
		t.Fatal("a wall of resolver timeouts was not recognised as one")
	}

	if host != "s3.us-central-1.wasabisys.com" {
		t.Errorf("host = %q, want the one it could not look up", host)
	}
}

func TestTheOtherShapeOfAResolverFailure(t *testing.T) {
	host, ok := (&Error{Stderr: `Fatal: unable to open repository: lookup terraboskamp.org: no such host`}).Unresolvable()
	if !ok || host != "terraboskamp.org" {
		t.Errorf("host = %q, ok = %v; want terraboskamp.org", host, ok)
	}
}

func TestAnOrdinaryFailureIsNotAResolverOne(t *testing.T) {
	// The word appears in plenty of output that is not a resolver failure,
	// and calling one of those a DNS problem sends somebody a long way from
	// the fault.
	for _, stderr := range []string{
		"Fatal: wrong password or no key found",
		"Fatal: unable to create lock in backend: repository is already locked",
		"",
	} {
		if host, ok := (&Error{Stderr: stderr}).Unresolvable(); ok {
			t.Errorf("%q was read as a failure to look up %q", stderr, host)
		}
	}
}

// TestWindowsIsNotAskedForOneFileSystem is a two-second failure that nothing
// would have caught: restic implements --one-file-system with device IDs,
// Windows has none, and it refuses the whole backup rather than the flag.
//
//	Fatal: Device IDs are not supported on Windows
//	exit 1, no snapshot
//
// Every backup on every Windows machine failed this way until a gate ran one.
func TestWindowsIsNotAskedForOneFileSystem(t *testing.T) {
	if supportsOneFileSystem("windows") {
		t.Error("windows would be asked for --one-file-system")
	}

	for _, goos := range []string{"linux", "darwin", "freebsd"} {
		if !supportsOneFileSystem(goos) {
			t.Errorf("%s lost --one-file-system, which it supports and needs", goos)
		}
	}
}

// TestTheFlagFollowsThePlatform asserts the rule reaches the command line, on
// whichever platform this test is running.
func TestTheFlagFollowsThePlatform(t *testing.T) {
	args := backupArgs(BackupOptions{OneFileSystem: true, Targets: []string{"/srv"}})

	var found bool

	for _, a := range args {
		if a == "--one-file-system" {
			found = true
		}
	}

	if want := supportsOneFileSystem(runtime.GOOS); found != want {
		t.Errorf("--one-file-system present = %v on %s, want %v", found, runtime.GOOS, want)
	}
}
