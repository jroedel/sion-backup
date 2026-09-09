// Package restic runs the restic binary and reads what it says back.
//
// It is a wrapper around a subprocess, not a reimplementation. restic's
// repository format, its deduplication and its cryptography are the reason
// this program exists at all; nothing here is going to do that better. What
// this package provides is the three things a shell script kept getting wrong:
//
//   - secrets that never appear in a command line;
//   - exit codes read correctly, including the partial-success one;
//   - machine-readable progress, so the status page can say what is happening
//     rather than "running".
//
// # Secrets go in the environment, never in argv
//
// On every platform in this fleet, any local user can read another process's
// command line — `ps` on Unix, WMIC or Get-CimInstance on Windows. A
// credential passed as a flag is therefore public for the duration of the
// backup, which on a full first run is hours.
//
// The environment of a running process is better protected: on Linux
// /proc/<pid>/environ is owner-only, and on Windows it needs a debug-level
// handle. It is not perfect — a process running as the same user can still
// read it, and so can anything with administrator rights — but the same user
// can already read the machine token and fetch these afresh, so nothing is
// lost there.
//
// # The environment is built, not inherited
//
// [Repository.Env] starts from the process environment with every RESTIC_* and
// AWS_* variable removed, then sets exactly what this run needs.
//
// Stripping matters more than it sounds. A workstation that has ever had
// AWS_PROFILE, AWS_DEFAULT_REGION or RESTIC_REPOSITORY exported from a shell
// profile would otherwise hand those to restic, and the failure mode is not an
// error — it is a backup that succeeds against the wrong repository, or that
// silently picks up an unrelated set of credentials. The legacy shell scripts
// this replaces exported into an inherited environment and had exactly that
// exposure.
package restic

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Repository is one restic repository and everything needed to open it.
//
// The three secret fields are []byte rather than string so they can be wiped
// after use — see foundation/token.Wipe. A string cannot be overwritten,
// which means a restic password read into one stays in the heap until the
// process exits.
type Repository struct {
	// URL is the repository location in restic's own syntax, e.g.
	// "s3:https://s3.us-central-1.wasabisys.com/example-node-bucket".
	URL string

	// Password unlocks the repository. Without it the backup is unreadable by
	// anyone, this program included.
	Password []byte

	// AccessKeyID and SecretAccessKey authenticate to the S3 endpoint.
	AccessKeyID     []byte
	SecretAccessKey []byte

	// PackSizeMiB is restic's target pack size. 16 is restic's own default and
	// is right for most connections; a machine on 300Mbit or better does
	// meaningfully fewer round trips at 32. Zero leaves restic to decide.
	PackSizeMiB int

	// ReadConcurrency is how many files restic reads at once. The default of 2
	// suits a spinning disk; an NVMe is not saturated below about 6. Zero
	// leaves restic to decide.
	ReadConcurrency int
}

// Env builds the environment for one restic invocation.
//
// See the package comment for why it strips rather than appends.
func (r Repository) Env() []string {
	out := make([]string, 0, len(os.Environ())+6)

	for _, kv := range os.Environ() {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}

		if strings.HasPrefix(name, "RESTIC_") || strings.HasPrefix(name, "AWS_") {
			continue
		}

		out = append(out, kv)
	}

	out = append(out,
		"RESTIC_REPOSITORY="+r.URL,
		"RESTIC_PASSWORD="+string(r.Password),
		"AWS_ACCESS_KEY_ID="+string(r.AccessKeyID),
		"AWS_SECRET_ACCESS_KEY="+string(r.SecretAccessKey),
	)

	if r.PackSizeMiB > 0 {
		out = append(out, "RESTIC_PACK_SIZE="+strconv.Itoa(r.PackSizeMiB))
	}

	if r.ReadConcurrency > 0 {
		out = append(out, "RESTIC_READ_CONCURRENCY="+strconv.Itoa(r.ReadConcurrency))
	}

	return out
}

// Validate reports what is missing, before a subprocess is started with it.
//
// Worth doing here rather than letting restic complain, because restic's own
// message for an empty password is about the repository being unreadable,
// which sends somebody looking at the bucket rather than at the server that
// just handed over a half-populated record.
func (r Repository) Validate() error {
	switch {
	case r.URL == "":
		return errors.New("restic: the repository has no URL")
	case len(r.Password) == 0:
		return errors.New("restic: the repository password is empty; nothing could ever be restored")
	case len(r.AccessKeyID) == 0, len(r.SecretAccessKey) == 0:
		return errors.New("restic: the S3 credentials are incomplete")
	default:
		return nil
	}
}

// Runner invokes one restic binary.
type Runner struct {
	bin string
}

// New locates the restic binary.
//
// The binary is shipped beside this one rather than taken from PATH, and the
// installer passes its absolute path. A PATH lookup for a program about to be
// handed the credentials to the only off-site copy of somebody's work is not a
// lookup worth doing — but an empty bin falls back to PATH so that a developer
// can use the restic they already have.
func New(bin string) (*Runner, error) {
	if bin == "" {
		bin = "restic"
	}

	resolved, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("restic: cannot find the restic binary %q: %w", bin, err)
	}

	return &Runner{bin: resolved}, nil
}

// Bin reports the binary this runner will execute, for the doctor command.
func (r *Runner) Bin() string { return r.bin }

// Version asks the binary what it is.
//
// It takes no repository, so it is the one call that can be made before a
// machine is enrolled — which is what makes it useful in `sion-backup doctor`.
func (r *Runner) Version(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, r.bin, "version")

	// A bare environment: this asks the binary about itself and has no
	// business carrying credentials.
	cmd.Env = []string{}

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("restic: asking %s for its version: %w", r.bin, err)
	}

	return strings.TrimSpace(string(out)), nil
}

// Exit codes restic documents. Reading these correctly is the single largest
// behavioural difference between this program and the shell scripts it
// replaces: those tested `!= 1` and therefore recorded a code 3 — "some files
// could not be read" — as a complete success. A backup missing the one locked
// Outlook data file the whole exercise was for would have reported green every
// night for a year.
const (
	// ExitOK is a complete backup.
	ExitOK = 0

	// ExitFailed is a command that did not do its job.
	ExitFailed = 1

	// ExitGoPanic is a Go runtime failure inside restic itself.
	ExitGoPanic = 2

	// ExitIncomplete means the snapshot was written but some source files
	// could not be read: locked, permission-denied, or deleted mid-run. The
	// snapshot is real and restorable, and it has holes.
	ExitIncomplete = 3

	// ExitNoRepository means nothing is at that URL yet.
	ExitNoRepository = 10

	// ExitLocked means another restic holds the repository lock.
	ExitLocked = 11

	// ExitWrongPassword means the password did not open the repository.
	ExitWrongPassword = 12
)

// classify turns a generic exit 1 into a specific code where the message says
// what actually happened.
//
// The distinct codes 10, 11 and 12 arrived in restic 0.17. Before that — and
// this fleet has machines on 0.14 — every one of those failures is exit 1 with
// a different sentence on stderr, and code that keys on the number alone reads
// "there is no repository here" as "something went wrong". The consequence is
// not cosmetic: Exists cannot tell an empty bucket from a broken one, so
// enrollment refuses to initialise a repository that genuinely is not there.
//
// Matching on message text is unpleasant. It is done here, once, in the one
// place that has the exit status too — so a newer restic that reports the
// proper code never reaches this function, and the matching quietly stops
// mattering as the fleet is upgraded.
func classify(code int, stderr string) int {
	if code != ExitFailed {
		return code
	}

	msg := strings.ToLower(stderr)

	switch {
	case contains(msg, "unable to open config file",
		"is there a repository at the following location",
		"repository does not exist",
		"no such file or directory") && contains(msg, "config", "repository"):
		return ExitNoRepository

	case contains(msg, "unable to create lock", "repository is already locked",
		"locked exclusively"):
		return ExitLocked

	case contains(msg, "wrong password", "invalid password"):
		return ExitWrongPassword

	default:
		return code
	}
}

// contains reports whether s holds any of the needles.
func contains(s string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}

	return false
}

// Error is a restic invocation that did not exit zero.
type Error struct {
	// Args is the command line, which is safe to keep and to log because no
	// secret is ever in it. That is the property the package comment argues
	// for, and this type is where it pays off.
	Args []string

	// Code is the process exit status, one of the Exit constants.
	Code int

	// Stderr is the tail of what restic complained about.
	Stderr string
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("restic %s: exit %d (%s)",
		strings.Join(e.Args, " "), e.Code, describeExit(e.Code))

	if e.Stderr != "" {
		msg += ": " + e.Stderr
	}

	return msg
}

// Incomplete reports the case that must never be mistaken for success: a
// snapshot was written, and it is missing files.
func (e *Error) Incomplete() bool { return e.Code == ExitIncomplete }

// Retryable reports whether waiting and trying again is likely to help.
//
// Only the lock. A wrong password will still be wrong in an hour, and a
// missing repository will still be missing — retrying those turns one clear
// failure into a night of identical ones.
func (e *Error) Retryable() bool { return e.Code == ExitLocked }

// describeExit turns a status into the sentence to put in front of a person.
func describeExit(code int) string {
	switch code {
	case ExitOK:
		return "success"
	case ExitFailed:
		return "the command failed"
	case ExitGoPanic:
		return "restic itself crashed"
	case ExitIncomplete:
		return "the snapshot was written but some files could not be read"
	case ExitNoRepository:
		return "there is no repository at that URL"
	case ExitLocked:
		return "the repository is locked by another restic"
	case ExitWrongPassword:
		return "the repository password is wrong"
	default:
		return "unrecognised exit status"
	}
}

// stderrTail is how much of restic's complaint is kept.
//
// A tail rather than the whole thing: a run that cannot read ten thousand
// files writes ten thousand lines, and putting all of them in the run history
// would make the database bigger than the thing it is recording. The tail is
// the part that says why it stopped.
const stderrTail = 8 << 10

// run executes restic and returns its stdout, or an *Error.
func (r *Runner) run(ctx context.Context, repo Repository, args ...string) ([]byte, error) {
	if err := repo.Validate(); err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(ctx, r.bin, args...)
	cmd.Env = repo.Env()

	var stdout, stderr bytes.Buffer

	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err == nil {
		return stdout.Bytes(), nil
	}

	var exit *exec.ExitError
	if errors.As(err, &exit) {
		detail := tail(stderr.String(), stderrTail)

		return stdout.Bytes(), &Error{
			Args:   args,
			Code:   classify(exit.ExitCode(), detail),
			Stderr: detail,
		}
	}

	return stdout.Bytes(), fmt.Errorf("restic %s: %w", strings.Join(args, " "), err)
}

// tail keeps the last n bytes of s, on a line boundary where it can.
func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}

	cut := s[len(s)-n:]
	if i := strings.IndexByte(cut, '\n'); i >= 0 && i < len(cut)-1 {
		cut = cut[i+1:]
	}

	return "…\n" + cut
}
