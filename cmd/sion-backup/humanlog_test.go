package main

import (
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// captured runs fn against a logger built the way a real command builds one,
// and returns everything it wrote.
func captured(who audience, verbose bool, fn func(*slog.Logger)) string {
	var b strings.Builder

	fn(loggerTo(&b, who, verbose))

	return b.String()
}

// TestAPersonNeverSeesALogLine is the regression test for a bug that has
// arrived three times: once through adopt-enroll, once through update, and
// once through every Warn in the backup path that `sion-backup run` shares
// with the daemon.
//
// It asserts the shape of the thing rather than any one call site, because
// patching call sites is what let it come back.
func TestAPersonNeverSeesALogLine(t *testing.T) {
	got := captured(forPerson, false, func(log *slog.Logger) {
		log.Warn("could not measure the repository", "err", errors.New("connection reset by peer"))
		log.Error("the repository did not pass its check", "err", errors.New("pack 3f2a is damaged"))
	})

	// The three things that made the old output look like machinery.
	for _, soup := range []string{"time=", "level=", "msg="} {
		if strings.Contains(got, soup) {
			t.Errorf("a person was shown %q:\n%s", soup, got)
		}
	}

	for _, want := range []string{
		"warning: could not measure the repository: connection reset by peer",
		"error: the repository did not pass its check: pack 3f2a is damaged",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// TestAPersonIsNotNarratedAt. Info is the level the daemon uses to describe
// what it is doing, and a person running a command is already being told that
// by the command.
func TestAPersonIsNotNarratedAt(t *testing.T) {
	got := captured(forPerson, false, func(log *slog.Logger) {
		log.Info("replaced this binary with a newer build", "was", "v0.4.0", "now", "v0.5.0")
		log.Debug("self-update is unavailable", "err", "no")
	})

	if got != "" {
		t.Errorf("a person was narrated at:\n%s", got)
	}
}

// TestTheDaemonStillWritesARecord: its log is a file and a journal, and
// structured is exactly right there.
func TestTheDaemonStillWritesARecord(t *testing.T) {
	got := captured(forRecord, false, func(log *slog.Logger) {
		log.Info("backup finished", "outcome", "success", "files", 4211)
	})

	for _, want := range []string{"level=INFO", "msg=", "outcome=success", "files=4211"} {
		if !strings.Contains(got, want) {
			t.Errorf("the daemon's log is missing %q:\n%s", want, got)
		}
	}
}

// TestVerboseIsTheLogWhoeverAsked.
//
// -v means "show me the log", and the log is the structured one. It is also a
// contract with the release gate: scripts/backup-e2e greps `run -v` output for
// `reading_back=`, and that grep silently stopped matching once before —
// nothing failed for a release and a half, because the gate only runs on a tag.
func TestVerboseIsTheLogWhoeverAsked(t *testing.T) {
	got := captured(forPerson, true, func(log *slog.Logger) {
		log.Info("checking the repository", "reading_back", "64M")
	})

	for _, want := range []string{"level=INFO", "reading_back=64M"} {
		if !strings.Contains(got, want) {
			t.Errorf("-v did not produce the log the gate reads; missing %q:\n%s", want, got)
		}
	}
}

func TestTheSentenceCarriesEverythingTheRecordDid(t *testing.T) {
	for _, c := range []struct {
		name string
		emit func(*slog.Logger)
		want string
	}{
		{
			name: "no attributes at all",
			emit: func(l *slog.Logger) { l.Warn("backups are paused") },
			want: "warning: backups are paused\n",
		},
		{
			// The error is the second half of the sentence, not an aside:
			// "err=..." on the end is what made the old form unreadable.
			name: "an error becomes the rest of the sentence",
			emit: func(l *slog.Logger) { l.Warn("could not reach the server", "err", errors.New("timeout")) },
			want: "warning: could not reach the server: timeout\n",
		},
		{
			name: "anything else goes in brackets",
			emit: func(l *slog.Logger) { l.Warn("skipped a folder", "path", "/srv/x", "size", 12) },
			want: "warning: skipped a folder (path=/srv/x, size=12)\n",
		},
		{
			name: "an error and other attributes together",
			emit: func(l *slog.Logger) {
				l.Error("the run failed", "err", errors.New("exit 1"), "node", "laptop-3")
			},
			want: "error: the run failed: exit 1 (node=laptop-3)\n",
		},
		{
			name: "attributes attached earlier are kept",
			emit: func(l *slog.Logger) { l.With("node", "laptop-3").Warn("paused") },
			want: "warning: paused (node=laptop-3)\n",
		},
		{
			// Nothing in this program groups, which is exactly why a handler
			// that silently dropped them would be a trap for whoever first does.
			name: "a group prefixes its keys",
			emit: func(l *slog.Logger) { l.WithGroup("repo").Warn("odd", "packs", 3) },
			want: "warning: odd (repo.packs=3)\n",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := captured(forPerson, false, c.emit); got != c.want {
				t.Errorf("got  %q\nwant %q", got, c.want)
			}
		})
	}
}
