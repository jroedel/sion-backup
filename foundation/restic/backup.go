package restic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// BackupOptions is one backup invocation's shape.
type BackupOptions struct {
	// Targets are the paths to back up. Empty is refused rather than
	// interpreted: restic with no target reads nothing, exits zero, and writes
	// an empty snapshot — a green run that backed up nothing at all, which is
	// the single worst outcome this program can produce.
	Targets []string

	// ExcludeFile is a restic exclude file, or empty for none.
	ExcludeFile string

	// Excludes are patterns given directly, in addition to the file.
	Excludes []string

	// Tags are attached to the snapshot, so `restic snapshots --tag` can find
	// the ones this program made.
	Tags []string

	// Host overrides the hostname recorded in the snapshot. Empty uses the
	// machine's own, which is what a fleet wants.
	Host string

	// UseFSSnapshot asks restic to back up from a filesystem snapshot rather
	// than the live tree. On Windows this is VSS, and it is the difference
	// between backing up a user's Outlook file and skipping it because it is
	// locked. It requires administrator rights, so it is a request rather than
	// a guarantee: see AllowVSSFallback.
	UseFSSnapshot bool

	// AllowVSSFallback retries once without --use-fs-snapshot when the first
	// attempt fails because VSS was refused.
	//
	// This exists because the alternative is worse in both directions. Without
	// it, a machine whose user is not an administrator takes no backups at
	// all. With it always on and silent, a machine quietly stops snapshotting
	// open files and nobody knows. So the fallback happens, and the run is
	// recorded as degraded — see [Summary.VSSFellBack].
	AllowVSSFallback bool

	// OneFileSystem stops restic descending into mounted filesystems: network
	// shares, external drives, and on Linux the pseudo-filesystems that the
	// legacy scripts had to exclude by hand.
	OneFileSystem bool
}

// Progress is one status line from a running backup.
type Progress struct {
	PercentDone  float64  `json:"percent_done"`
	TotalFiles   int64    `json:"total_files"`
	FilesDone    int64    `json:"files_done"`
	TotalBytes   int64    `json:"total_bytes"`
	BytesDone    int64    `json:"bytes_done"`
	ErrorCount   int64    `json:"error_count"`
	CurrentFiles []string `json:"current_files"`

	// SecondsRemaining is restic's own estimate. It is wildly wrong for the
	// first few minutes of a first run and settles afterwards, which is worth
	// knowing before putting it in front of somebody as a promise.
	SecondsRemaining int64 `json:"seconds_remaining"`
}

// Summary is what restic reports when a backup finishes.
type Summary struct {
	FilesNew            int64   `json:"files_new"`
	FilesChanged        int64   `json:"files_changed"`
	FilesUnmodified     int64   `json:"files_unmodified"`
	DirsNew             int64   `json:"dirs_new"`
	DirsChanged         int64   `json:"dirs_changed"`
	DataAdded           int64   `json:"data_added"`
	DataAddedPacked     int64   `json:"data_added_packed"`
	TotalFilesProcessed int64   `json:"total_files_processed"`
	TotalBytesProcessed int64   `json:"total_bytes_processed"`
	TotalDuration       float64 `json:"total_duration"`
	SnapshotID          string  `json:"snapshot_id"`

	// Errors are the per-file failures restic reported as it went. A backup
	// can finish with exit 3 and a valid snapshot while this list is long, and
	// the list is the only place that says which files are not in it.
	Errors []FileError

	// VSSFellBack records that a filesystem snapshot was asked for, refused,
	// and the backup was retried against the live tree. The snapshot is real;
	// files that were open at the time may be missing or torn.
	VSSFellBack bool
}

// FileError is one path restic could not read.
type FileError struct {
	Item  string `json:"item"`
	Error string `json:"-"`
}

// message is the envelope every line of restic's --json output shares.
type message struct {
	MessageType string `json:"message_type"`

	// Error carries a nested object on message_type "error", whose shape has
	// changed across restic versions. Decoded as a raw message and reduced to
	// a sentence rather than modelled, because the sentence is all that is
	// wanted and a model would break on the next release.
	Error json.RawMessage `json:"error"`
	Item  string          `json:"item"`
}

// Backup takes one snapshot, reporting progress as it goes.
//
// onProgress may be nil. It is called from the goroutine reading restic's
// stdout, so an implementation that blocks stalls the parse — the status page
// stores the latest value and returns, rather than writing to the database on
// every line.
//
// A returned *Error with Incomplete() true still carries a usable Summary: the
// snapshot exists and the ID is in it. Callers must decide what that means to
// them rather than treating any error as "no backup happened".
func (r *Runner) Backup(ctx context.Context, repo Repository, opts BackupOptions,
	onProgress func(Progress)) (Summary, error) {

	if len(opts.Targets) == 0 {
		return Summary{}, errors.New("restic: refusing to back up with no targets; " +
			"that would write an empty snapshot and report success")
	}

	summary, err := r.backupOnce(ctx, repo, opts, onProgress)

	// The VSS retry. Only when a filesystem snapshot was asked for, only when
	// the failure looks like VSS being refused, and only once.
	if err != nil && opts.UseFSSnapshot && opts.AllowVSSFallback && vssRefused(err) {
		retry := opts
		retry.UseFSSnapshot = false

		summary, err = r.backupOnce(ctx, repo, retry, onProgress)
		summary.VSSFellBack = true
	}

	return summary, err
}

func (r *Runner) backupOnce(ctx context.Context, repo Repository, opts BackupOptions,
	onProgress func(Progress)) (Summary, error) {

	if err := repo.Validate(); err != nil {
		return Summary{}, err
	}

	args := backupArgs(opts)

	cmd := exec.CommandContext(ctx, r.bin, args...)
	cmd.Env = repo.Env()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Summary{}, fmt.Errorf("restic: opening a pipe to backup's output: %w", err)
	}

	var stderr bytes.Buffer

	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return Summary{}, fmt.Errorf("restic backup: starting %s: %w", r.bin, err)
	}

	summary, parseErr := parseBackup(stdout, onProgress)

	// Wait after the pipe is drained, never before: Wait closes the pipe, and
	// a parse still reading from it would lose the summary line — which is the
	// one line that carries the snapshot ID.
	waitErr := cmd.Wait()

	if waitErr != nil {
		var exit *exec.ExitError
		if errors.As(waitErr, &exit) {
			detail := tail(stderr.String(), stderrTail)

			return summary, &Error{
				Args:   args,
				Code:   classify(exit.ExitCode(), detail),
				Stderr: detail,
			}
		}

		return summary, fmt.Errorf("restic backup: %w", waitErr)
	}

	if parseErr != nil {
		return summary, parseErr
	}

	return summary, nil
}

// backupArgs assembles the command line. Separate from the run so a test can
// assert the flags without a restic binary present.
func backupArgs(opts BackupOptions) []string {
	args := []string{"backup", "--json"}

	if opts.UseFSSnapshot {
		args = append(args, "--use-fs-snapshot")
	}

	if opts.OneFileSystem {
		args = append(args, "--one-file-system")
	}

	if opts.Host != "" {
		args = append(args, "--host", opts.Host)
	}

	for _, tag := range opts.Tags {
		args = append(args, "--tag", tag)
	}

	if opts.ExcludeFile != "" {
		args = append(args, "--exclude-file", opts.ExcludeFile)
	}

	for _, pattern := range opts.Excludes {
		args = append(args, "--exclude", pattern)
	}

	// Targets last, after a bare "--", so a path beginning with a dash is a
	// path rather than an unknown flag. A user configuring a directory called
	// "-Archive" should get a backup, not a usage message.
	args = append(args, "--")

	return append(args, opts.Targets...)
}

// maxErrorsKept bounds the per-file error list.
//
// A run against an unreadable network share produces one of these per file,
// and the useful information is the first few plus the count. Keeping all of
// them would put a hundred megabytes of near-identical strings in a SQLite
// database whose whole job is to stay small enough to be read instantly.
const maxErrorsKept = 100

// parseBackup reads restic's JSON stream to the end.
//
// It reads to the end even on a malformed line, deliberately: the summary is
// the last message, and abandoning the parse over one unrecognised line in the
// middle would discard the snapshot ID of a backup that actually succeeded.
func parseBackup(r interface{ Read([]byte) (int, error) }, onProgress func(Progress)) (Summary, error) {
	var summary Summary

	scanner := bufio.NewScanner(r)

	// restic's status lines list the files currently being read, and a run
	// over deeply nested paths produces lines far past bufio's 64KB default.
	// Hitting that limit ends the scan, which would silently drop the summary.
	scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)

	errorCount := 0

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}

		var msg message
		if err := json.Unmarshal(line, &msg); err != nil {
			// Not fatal. restic writes the occasional non-JSON line to stdout
			// — a warning from a dependency, a progress bar on a terminal —
			// and none of it is worth failing a completed backup over.
			continue
		}

		switch msg.MessageType {
		case "status":
			if onProgress == nil {
				continue
			}

			var p Progress
			if err := json.Unmarshal(line, &p); err == nil {
				onProgress(p)
			}

		case "error":
			errorCount++

			if len(summary.Errors) < maxErrorsKept {
				summary.Errors = append(summary.Errors, FileError{
					Item:  msg.Item,
					Error: describeError(msg.Error),
				})
			}

		case "summary":
			if err := json.Unmarshal(line, &summary); err != nil {
				return summary, fmt.Errorf("restic: its summary line was unreadable: %w", err)
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return summary, fmt.Errorf("restic: reading backup output: %w", err)
	}

	// Re-attached after the summary line overwrote the struct.
	if errorCount > len(summary.Errors) {
		summary.Errors = append(summary.Errors, FileError{
			Error: fmt.Sprintf("and %d more", errorCount-len(summary.Errors)),
		})
	}

	return summary, nil
}

// describeError reduces restic's error object to one sentence.
func describeError(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	// The common shape: {"message": "..."}.
	var obj struct {
		Message string `json:"message"`
	}

	if err := json.Unmarshal(raw, &obj); err == nil && obj.Message != "" {
		return obj.Message
	}

	// A plain string in older versions.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}

	return strings.TrimSpace(string(raw))
}

// vssRefused reports whether a failure looks like Volume Shadow Copy being
// unavailable rather than something wrong with the backup.
//
// Matching on message text is unpleasant and is done anyway, because restic
// exits 1 for this as it does for everything else — there is no distinct code
// to key on. The match is deliberately narrow: a false positive here retries a
// genuine failure once without VSS, which costs one wasted run; a false
// negative leaves a non-administrator machine with no backups at all.
func vssRefused(err error) bool {
	if runtime.GOOS != "windows" {
		return false
	}

	var rerr *Error
	if !errors.As(err, &rerr) {
		return false
	}

	msg := strings.ToLower(rerr.Stderr)

	for _, needle := range []string{
		"failed to create snapshot",
		"vss",
		"shadow copy",
		"access is denied",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}

	return false
}

// Duration renders a summary's elapsed time.
func (s Summary) Duration() time.Duration {
	return time.Duration(s.TotalDuration * float64(time.Second))
}
