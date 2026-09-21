package backupbus_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jroedel/sion-backup/business/domain/backup/backupbus"
	"github.com/jroedel/sion-backup/foundation/restic"
)

// unresolvingRestic is a backup on a machine whose resolver does not answer.
// The stderr is shortened from a real one, which ran for seven hours and
// produced several hundred lines of exactly this.
func unresolvingRestic(t *testing.T) *restic.Runner {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("the stub is a shell script")
	}

	script := `#!/bin/sh
case "$1" in
backup)
  echo 'Save(<data/765ff5926d>) returned error, retrying after 1.074152134s: client.PutObject: Put "https://s3.us-central-1.wasabisys.com/bucket/data/76/765ff": dial tcp: lookup s3.us-central-1.wasabisys.com on 127.0.0.53:53: read udp 127.0.0.1:42902->127.0.0.53:53: i/o timeout' >&2
  echo 'Save(<data/d186c8119a>) returned error, retrying after 2.839669001s: client.PutObject: Put "https://s3.us-central-1.wasabisys.com/bucket/data/d1/d186c": dial tcp: lookup s3.us-central-1.wasabisys.com on 127.0.0.53:53: read udp 127.0.0.1:42902->127.0.0.53:53: i/o timeout' >&2
  echo 'Fatal: unable to save snapshot: context canceled' >&2
  exit 1
  ;;
unlock) exit 0 ;;
*) exit 1 ;;
esac
`

	bin := filepath.Join(t.TempDir(), "restic")
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	r, err := restic.New(bin)
	if err != nil {
		t.Fatal(err)
	}

	return r
}

// TestARunThatCouldNotResolveSaysSo. What this replaced was eight kilobytes of
// "returned error, retrying after 8.13s" on the status page, which is a wall
// somebody reads twice and gives up on, about a fault that has nothing to do
// with backups.
func TestARunThatCouldNotResolveSaysSo(t *testing.T) {
	b, _, _ := harness(t, unresolvingRestic(t))

	run, err := b.Run(context.Background(), request(), time.Now)
	if err != nil {
		t.Fatal("Run:", err)
	}

	if run.Outcome != backupbus.OutcomeFailed {
		t.Errorf("outcome = %q, want %q", run.Outcome, backupbus.OutcomeFailed)
	}

	want := "this computer could not look up s3.us-central-1.wasabisys.com, " +
		"so nothing could be uploaded; its DNS or network connection is not working"

	if run.Message != want {
		t.Errorf("message =\n  %q\nwant\n  %q", run.Message, want)
	}

	// And none of the wall of retries, which is the whole point.
	if strings.Contains(run.Message, "retrying after") {
		t.Error("the message still carries restic's retry log")
	}
}
