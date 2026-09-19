package restic

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// scripted builds a Runner over a small program that answers `cat config` and
// `init` the way a real restic would, from a file on disk that stands in for
// the bucket.
//
// A script rather than a mock, because what is being tested is the sequence of
// calls and what is concluded from their exit codes — and exit 10 is the whole
// of how this program tells "there is nothing there" from "I could not look".
func scripted(t *testing.T, exists bool, initFails bool) (*Runner, func() bool) {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("the stub is a shell script")
	}

	dir := t.TempDir()
	marker := filepath.Join(dir, "repository")

	if exists {
		if err := os.WriteFile(marker, []byte("config"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	failInit := ""
	if initFails {
		failInit = "exit 1"
	}

	bin := filepath.Join(dir, "restic")
	script := fmt.Sprintf(`#!/bin/sh
case "$1" in
  cat)
    if [ -f %[1]q ]; then echo '{}'; exit 0; fi
    echo "Fatal: unable to open config file" >&2
    exit 10
    ;;
  init)
    %[2]s
    echo created > %[1]q
    exit 0
    ;;
esac
exit 1
`, marker, failInit)

	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	runner, err := New(bin)
	if err != nil {
		t.Fatal(err)
	}

	return runner, func() bool {
		_, err := os.Stat(marker)

		return err == nil
	}
}

// TestEnsureOpensWhatIsAlreadyThere, and creates nothing.
func TestEnsureOpensWhatIsAlreadyThere(t *testing.T) {
	r, _ := scripted(t, true, false)

	// Permitted to create, and must not: the permission is not an instruction,
	// and a bucket that already holds a repository is one this program has no
	// business writing a fresh one into.
	created, err := r.Ensure(context.Background(), testRepo(), true)
	if err != nil {
		t.Fatal(err)
	}

	if created {
		t.Error("a repository that was already there was created again")
	}
}

// TestEnsureRefusesToCreateWithoutPermission is the doctrine this program is
// built on, held at the one place it could be given away.
//
// An empty bucket on the backup path means the URL is wrong, or the bucket has
// been emptied. Creating one there reports success while starting a brand-new,
// empty backup — which is the failure mode every other check in this program
// exists to prevent.
func TestEnsureRefusesToCreateWithoutPermission(t *testing.T) {
	r, created := scripted(t, false, false)

	_, err := r.Ensure(context.Background(), testRepo(), false)

	if !errors.Is(err, ErrAbsent) {
		t.Fatalf("err = %v, want ErrAbsent", err)
	}

	if created() {
		t.Fatal("a repository was created without permission")
	}

	// And the URL is in the sentence, because "there is no repository there"
	// is only useful to somebody who can see which "there" is meant.
	if !strings.Contains(err.Error(), testRepo().URL) {
		t.Errorf("the refusal does not name the URL: %v", err)
	}
}

// TestEnsureCreatesOnlyWhenTold, and reads it back rather than trusting the
// exit code — the same argument this program makes about backups, applied to
// the repository itself.
func TestEnsureCreatesOnlyWhenTold(t *testing.T) {
	r, created := scripted(t, false, false)

	made, err := r.Ensure(context.Background(), testRepo(), true)
	if err != nil {
		t.Fatal(err)
	}

	if !made || !created() {
		t.Fatal("nothing was created")
	}
}

// TestACreateThatFailsIsNotASuccess.
func TestACreateThatFailsIsNotASuccess(t *testing.T) {
	r, created := scripted(t, false, true)

	made, err := r.Ensure(context.Background(), testRepo(), true)

	if err == nil {
		t.Fatal("a failed init was reported as success")
	}

	if made || created() {
		t.Error("a failed init reported that it created something")
	}
}
