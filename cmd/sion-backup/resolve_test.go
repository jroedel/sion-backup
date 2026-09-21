package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestResolvingReportsWhatItFound(t *testing.T) {
	// localhost, because it answers from /etc/hosts on every machine this
	// ever runs on, including a CI runner with no network.
	got, err := resolves(context.Background(), []string{"localhost"})
	if err != nil {
		t.Fatal("localhost did not resolve:", err)
	}

	if !strings.Contains(got, "localhost →") {
		t.Errorf("detail = %q, want the name and what it resolved to", got)
	}
}

func TestResolvingSaysWhichNameFailed(t *testing.T) {
	// .invalid is reserved by RFC 6761 precisely so that it never resolves.
	_, err := resolves(context.Background(), []string{"no-such-host.invalid"})
	if err == nil {
		t.Fatal("a name that cannot exist resolved")
	}

	// The name, because a machine with two hosts to reach needs to know which
	// one it cannot see.
	if !strings.Contains(err.Error(), "no-such-host.invalid") {
		t.Errorf("error = %q, want the name in it", err)
	}

	// And where to look, because "lookup ...: no such host" reads as a typo
	// in the URL rather than as a broken resolver.
	if !strings.Contains(err.Error(), "name resolution") {
		t.Errorf("error = %q, want it to point at name resolution", err)
	}
}

func TestResolvingNothingIsNotAPass(t *testing.T) {
	// A machine with no plan has no repository host, and telling it that its
	// name resolution is fine would be telling it about a check that did not
	// happen.
	if _, err := resolves(context.Background(), []string{"", ""}); !errors.Is(err, errSkipCheck) {
		t.Errorf("err = %v, want errSkipCheck", err)
	}
}
