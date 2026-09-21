package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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

// TestJitterCanBeTurnedOff. Read as a plain int, "jitter_minutes = 0" was
// indistinguishable from saying nothing, so the 30-minute default applied and
// a machine could not be asked to run at the time its own config named. The
// gate found it by scheduling a backup for 09:50 and watching the machine
// decide on 10:06.
func TestJitterCanBeTurnedOff(t *testing.T) {
	for _, tc := range []struct {
		name string
		toml string
		want int
	}{
		{"unset", "", 30},
		{"zero", "jitter_minutes = 0", 0},
		{"explicit", "jitter_minutes = 7", 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.toml")

			body := "node_id = \"m1\"\nrepository = \"s3:https://example.invalid/b\"\n" +
				"targets = [\"" + filepath.ToSlash(dir) + "\"]\n\n[schedule]\ntimes = [\"02:00\"]\n" + tc.toml + "\n"

			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}

			cfg, _, err := LoadConfig(path)
			if err != nil {
				t.Fatal("reading the config:", err)
			}

			plan, err := cfg.Plan()
			if err != nil {
				t.Fatal("turning it into a plan:", err)
			}

			if plan.Schedule.JitterMinutes != tc.want {
				t.Errorf("jitter = %d, want %d", plan.Schedule.JitterMinutes, tc.want)
			}
		})
	}
}
