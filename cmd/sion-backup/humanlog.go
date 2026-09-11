package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
)

// A log line is not a sentence, and a person running a command is owed
// sentences.
//
// # The bug this exists to end
//
// Every command in this program prints formatted output that somebody reads:
// `enroll` prints a restore card, `update` says what it replaced and what
// happens next, `run` prints a backup's outcome. Underneath, the same code
// paths the daemon uses write structured logs — and those logs went to the
// same terminal, so a person would get this in the middle of theirs:
//
//	time=2026-09-11T19:53:05.689+02:00 level=INFO msg="replaced this binary
//	with a newer build" was=v0.4.0 now=v0.5.0 path=/home/user/sion-backup
//	on_probation_for=3
//	updated to v0.5.0; restart the service to run it
//
// It has been fixed twice by hand, both times by silencing a level, and it
// came back both times — because the level was never the problem. The problem
// is that one writer was serving two readers with one format. A threshold also
// fixes only the half of it that is Info: `deps.backup` is shared between the
// daemon and `sion-backup run`, and every Warn and Error in it reached a
// person's terminal as key=value soup whatever the threshold said.
//
// So the two readers get two formats, and the choice is made once, in
// [newLogger], rather than at each of the several dozen places that log.
//
//   - The daemon writes a record: structured, timestamped, machine-readable,
//     because that is what a log file and a journal are for.
//   - A person gets prose on stderr, warnings and errors only.
//   - `-v` gets the record, whoever asked for it. It means "show me the log",
//     and it is how a harness or somebody debugging gets what a person does
//     not want. scripts/backup-e2e reads key=value pairs out of `run -v`.
//
// Nothing is lost and nothing is silenced: a warning a person needs still
// reaches them, in the voice the rest of the command is written in.

// humanHandler renders records as sentences for somebody at a terminal.
type humanHandler struct {
	mu    *sync.Mutex
	w     io.Writer
	level slog.Leveler

	// attrs are those attached by WithAttrs, already rendered.
	attrs []slog.Attr

	// group prefixes keys added under WithGroup. Nothing in this program uses
	// groups; it is here because a handler that silently dropped them would be
	// a trap for whoever first does.
	group string
}

// newHumanHandler builds one writing to w.
func newHumanHandler(w io.Writer, level slog.Leveler) slog.Handler {
	return &humanHandler{mu: &sync.Mutex{}, w: w, level: level}
}

func (h *humanHandler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= h.level.Level()
}

func (h *humanHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := *h
	out.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)

	return &out
}

func (h *humanHandler) WithGroup(name string) slog.Handler {
	out := *h

	if name != "" {
		out.group = strings.TrimPrefix(h.group+"."+name, ".")
	}

	return &out
}

// Handle writes one record as a sentence.
//
// The shape is: what happened, then why, then anything else in brackets.
//
//	warning: could not measure the repository: connection reset by peer
//	error: the repository did not pass its check: pack 3f2a is damaged
//
// The "why" is the err attribute, lifted out of the list and put after a
// colon, because it is the half of the line a person actually reads and
// because "err=..." at the end of a sentence is the thing that made the old
// output look like machinery rather than like this program talking.
func (h *humanHandler) Handle(_ context.Context, r slog.Record) error {
	var why string

	rest := make([]string, 0, 4)

	add := func(a slog.Attr) {
		key := a.Key
		if h.group != "" {
			key = h.group + "." + key
		}

		// The error is the sentence's own second half, not an aside.
		if key == "err" && why == "" {
			why = a.Value.String()

			return
		}

		rest = append(rest, fmt.Sprintf("%s=%s", key, a.Value))
	}

	for _, a := range h.attrs {
		add(a)
	}

	r.Attrs(func(a slog.Attr) bool {
		add(a)

		return true
	})

	var b strings.Builder

	b.WriteString(prefixFor(r.Level))
	b.WriteString(r.Message)

	if why != "" {
		b.WriteString(": ")
		b.WriteString(why)
	}

	if len(rest) > 0 {
		b.WriteString(" (")
		b.WriteString(strings.Join(rest, ", "))
		b.WriteString(")")
	}

	b.WriteString("\n")

	// Locked, and the mutex is shared with every handler derived from this one
	// by WithAttrs: two goroutines writing a line each must not interleave
	// them, and the daemon's own handler has the same guarantee.
	h.mu.Lock()
	defer h.mu.Unlock()

	_, err := io.WriteString(h.w, b.String())

	return err
}

// prefixFor names the levels a person should see differently.
//
// Nothing for Info and below: at the threshold this handler is built with they
// do not appear at all, and if somebody lowers it, narration should read as
// narration rather than be shouted at.
func prefixFor(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "error: "
	case l >= slog.LevelWarn:
		return "warning: "
	default:
		return ""
	}
}
