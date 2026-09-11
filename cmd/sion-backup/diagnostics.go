package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jroedel/sion-backup/business/domain/diag/diagbus"
	"github.com/jroedel/sion-backup/business/domain/diag/sources/eumaeusdiag"
	"github.com/jroedel/sion-backup/business/domain/diag/stores/diagfile"
	"github.com/jroedel/sion-backup/foundation/eumaeusapi"
	"github.com/jroedel/sion-backup/foundation/paths"
	"github.com/jroedel/sion-backup/foundation/token"
)

// diagnostics builds the reporter without needing anything else to have
// worked.
//
// Every other domain hangs off wire(), which opens a database, reads a plan
// and resolves restic. This one deliberately does not: the reports worth
// having most are from a machine where one of those failed, or from an
// installer running before any of it exists. It needs a directory and, if
// there happens to be one, a token.
//
// It never fails. A Business with no queue discards what it is given, which
// means the panic handler and the installer never have to check whether
// reporting is available before reporting — and a diagnostic subsystem that
// can break the program it is diagnosing is worse than none.
func diagnostics(log *slog.Logger) *diagbus.Business {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	p, err := paths.Resolve()
	if err != nil {
		return diagbus.NewBusiness(nil, nil, version, log)
	}

	queue, err := diagfile.NewStore(p.Diag)
	if err != nil {
		return diagbus.NewBusiness(nil, nil, version, log)
	}

	// The config file may not exist yet; EumaeusURL falls back to the fleet's
	// own server, which is the whole reason that default exists.
	cfg, _, _ := LoadConfig(p.Config)

	// Anonymous when there is no token, and that is the interesting case: an
	// install that failed before enrollment has no token and is exactly the
	// report somebody needs. See jroedel/eumaeus#113.
	tok, _ := token.Load(p.Token)

	client, err := eumaeusapi.New(eumaeusapi.Config{
		BaseURL:   cfg.EumaeusURL(),
		Token:     tok,
		Timeout:   15 * time.Second,
		UserAgent: "sion-backup/" + version,
	})
	if err != nil {
		return diagbus.NewBusiness(queue, nil, version, log)
	}

	return diagbus.NewBusiness(queue, eumaeusdiag.NewSink(client), version, log)
}

// capturePanic records a crash on the way out, then crashes.
//
// Deferred from main. Recovering and re-printing rather than letting the
// runtime print is a real cost — the stack it shows is this frame's rather
// than the crash's original goroutine dump — which is why the recovered stack
// is printed in full. What it buys is that the crash reaches somebody. A panic
// on an unattended service at three in the morning is otherwise a line in a
// log file on a laptop, read by nobody, ever.
//
// The exit code is 2, matching what the runtime uses for an unrecovered panic.
func capturePanic() {
	r := recover()
	if r == nil {
		return
	}

	stack := debug.Stack()

	_ = diagnostics(nil).Record(diagbus.PanicReport(r, stack, ""))

	fmt.Fprintf(os.Stderr, "sion-backup: panic: %v\n\n%s", r, stack)
	os.Exit(2)
}

// supervise is capturePanic for a goroutine.
//
// A deferred recover in main cannot see a panic in a goroutine — the runtime
// takes the whole process down without unwinding through it — so every
// goroutine this program starts and expects to outlive a request wraps its
// body in this.
func supervise(log *slog.Logger, what string, fn func()) {
	defer func() {
		r := recover()
		if r == nil {
			return
		}

		stack := debug.Stack()

		_ = diagnostics(log).Record(diagbus.PanicReport(r, stack, ""))

		log.Error("a background task panicked", "task", what, "panic", r)

		fmt.Fprintf(os.Stderr, "sion-backup: panic in %s: %v\n\n%s", what, r, stack)
		os.Exit(2)
	}()

	fn()
}

const reportUsage = `sion-backup report — tell Eumaeus that something went wrong

Usage:
  sion-backup report --kind install-failed --step download-restic \
      --detail "sha256 mismatch" [--install-id UUID]

Meant for the installers, and for anybody debugging one. It writes a report to
the queue in the data directory and tries to send it. A report that cannot be
sent — no network, or a server that does not serve this endpoint yet — stays
on disk and goes with the next run.

Nothing in a report may identify the person using the machine. It carries this
program's own error text, the step that failed, and what was on the machine
before. Paths under a home directory are rewritten to ~ before it is stored.

Flags:
  --kind           install-failed (default), panic, or update-failed
  --step           which part of the install failed, e.g. "verify-restic-hash"
  --detail         the error text; "-" reads it from standard input
  --install-id     ties several reports from one install together (generated if absent)
  --prior-version  what was on the machine before, e.g. "legacy-linux-1.1"
  --node           this machine's node ID, if it has got that far
  --queue-only     write it to disk and do not try to send
`

func reportCmd(args []string) error {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, reportUsage) }

	kind := fs.String("kind", string(diagbus.KindInstallFailed), "what happened")
	step := fs.String("step", "", "which part failed")
	detail := fs.String("detail", "", `the error text, or "-" for standard input`)
	installID := fs.String("install-id", "", "correlation ID for one install")
	prior := fs.String("prior-version", "", "what was on the machine before")
	node := fs.String("node", "", "this machine's node ID")
	queueOnly := fs.Bool("queue-only", false, "do not try to send it now")

	if err := fs.Parse(args); err != nil {
		return err
	}

	switch diagbus.Kind(*kind) {
	case diagbus.KindInstallFailed, diagbus.KindPanic, diagbus.KindUpdateFailed:
	default:
		fmt.Fprint(os.Stderr, reportUsage)

		return fmt.Errorf("unknown --kind %q", *kind)
	}

	text := *detail

	if text == "-" {
		raw, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("reading the detail from standard input: %w", err)
		}

		text = string(raw)
	}

	if strings.TrimSpace(text) == "" {
		fmt.Fprint(os.Stderr, reportUsage)

		return errors.New("a report needs --detail")
	}

	if *installID == "" {
		*installID = uuid.NewString()
	}

	// `report` is run by the installers and by a person, and never by the
	// daemon — so it talks like a command rather than like a log. It does not
	// go through wire(): see the comment on diagnostics.
	diag := diagnostics(newLogger(forPerson, false))

	if err := diag.Record(diagbus.Report{
		Kind:         diagbus.Kind(*kind),
		InstallID:    *installID,
		NodeID:       *node,
		Step:         *step,
		PriorVersion: *prior,
		Detail:       text,
	}); err != nil {
		return err
	}

	if *queueOnly {
		fmt.Println("recorded; it will be sent with the next run")

		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// A failure to send is not a failure of this command. The installer that
	// called it is already handling something that went wrong, and telling it
	// that telling somebody went wrong helps nobody.
	sent, err := diag.Flush(ctx)
	if err != nil {
		fmt.Printf("recorded; it will be sent with the next run (%v)\n", err)

		return nil
	}

	fmt.Printf("recorded; %d report(s) sent\n", sent)

	return nil
}
