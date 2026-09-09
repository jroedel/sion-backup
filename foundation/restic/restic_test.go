package restic

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func testRepo() Repository {
	return Repository{
		URL:             "s3:https://s3.example.invalid/bucket",
		Password:        []byte("repo-password"),
		AccessKeyID:     []byte("AKIAEXAMPLE"),
		SecretAccessKey: []byte("secret-key"),
	}
}

// TestEnvStripsInheritedResticAndAWSVariables is the property the package
// comment argues for. A workstation with RESTIC_REPOSITORY exported from a
// shell profile must not back up to that repository instead of this one — the
// failure is silent and produces a green run against the wrong bucket.
func TestEnvStripsInheritedResticAndAWSVariables(t *testing.T) {
	t.Setenv("RESTIC_REPOSITORY", "s3:https://wrong.example.invalid/somebody-elses-bucket")
	t.Setenv("RESTIC_PASSWORD", "inherited-password")
	t.Setenv("AWS_PROFILE", "personal")
	t.Setenv("AWS_DEFAULT_REGION", "eu-west-1")
	t.Setenv("PATH", "/usr/bin")

	env := testRepo().Env()

	counts := map[string]int{}

	for _, kv := range env {
		name, value, _ := strings.Cut(kv, "=")
		counts[name]++

		switch name {
		case "RESTIC_REPOSITORY":
			if value != testRepo().URL {
				t.Errorf("RESTIC_REPOSITORY is %q, want the configured repository", value)
			}
		case "RESTIC_PASSWORD":
			if value != "repo-password" {
				t.Errorf("RESTIC_PASSWORD is %q, want the configured password", value)
			}
		case "AWS_PROFILE", "AWS_DEFAULT_REGION":
			t.Errorf("%s survived; an inherited AWS variable can redirect the credentials", name)
		}
	}

	for _, name := range []string{"RESTIC_REPOSITORY", "RESTIC_PASSWORD", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"} {
		if counts[name] != 1 {
			t.Errorf("%s appears %d times; a duplicate makes which one wins depend on the platform", name, counts[name])
		}
	}

	if counts["PATH"] != 1 {
		t.Error("PATH did not survive; restic needs an ordinary environment")
	}
}

// TestEnvOmitsTuningWhenUnset keeps restic's own defaults in play rather than
// pinning them to zero, which restic reads as "no packs" and "no readers".
func TestEnvOmitsTuningWhenUnset(t *testing.T) {
	for _, kv := range testRepo().Env() {
		if strings.HasPrefix(kv, "RESTIC_PACK_SIZE=") || strings.HasPrefix(kv, "RESTIC_READ_CONCURRENCY=") {
			t.Errorf("unset tuning was passed anyway: %s", kv)
		}
	}
}

func TestValidateRefusesAnEmptyPassword(t *testing.T) {
	repo := testRepo()
	repo.Password = nil

	err := repo.Validate()
	if err == nil {
		t.Fatal("an empty password was accepted; the backup would be unrecoverable")
	}

	if !strings.Contains(err.Error(), "restored") {
		t.Errorf("the message does not say what is actually at stake: %v", err)
	}
}

// TestBackupArgsPutTargetsAfterADoubleDash covers a real configuration: a
// directory whose name begins with a dash.
func TestBackupArgsPutTargetsAfterADoubleDash(t *testing.T) {
	args := backupArgs(BackupOptions{
		Targets:     []string{"-Archive", "/home/user"},
		ExcludeFile: "/etc/excludes.txt",
		Excludes:    []string{"*.iso"},
		Tags:        []string{"sion-backup"},
	})

	sep := slices.Index(args, "--")
	if sep < 0 {
		t.Fatalf("no -- separator in %v", args)
	}

	if got := args[sep+1:]; !slices.Equal(got, []string{"-Archive", "/home/user"}) {
		t.Errorf("targets after --: %v", got)
	}

	for _, want := range []string{"--json", "--exclude-file", "--exclude", "--tag"} {
		if !slices.Contains(args[:sep], want) {
			t.Errorf("%s missing from %v", want, args[:sep])
		}
	}
}

func TestBackupRefusesNoTargets(t *testing.T) {
	r := &Runner{bin: "restic-not-executed"}

	_, err := r.Backup(context.Background(), testRepo(), BackupOptions{}, nil)
	if err == nil {
		t.Fatal("a backup with no targets was accepted; it would write an empty snapshot")
	}
}

// TestTheRunnerCannotDeleteBackupData guards a decision rather than a
// behaviour, which is why it is written by reflection over the type.
//
// This fleet does not prune: a single prune on a real repository over a
// gigabit uplink took more than a day, because prune downloads and re-uploads
// most of the repository rather than editing metadata. Space is reclaimed by
// rotating the bucket instead.
//
// The security property that falls out of that is the one worth protecting:
// no key anywhere in the system can delete backup data, because no code path
// here asks to. Re-adding Forget or Prune would quietly require handing some
// component a delete-capable credential again, and the failure would not show
// up until somebody read the IAM policy.
func TestTheRunnerCannotDeleteBackupData(t *testing.T) {
	forbidden := map[string]string{
		"Forget": "forget removes snapshots and, with --prune, the data behind them",
		"Prune":  "prune rewrites and deletes packs",
	}

	rt := reflect.TypeOf(&Runner{})

	for i := range rt.NumMethod() {
		name := rt.Method(i).Name

		if why, bad := forbidden[name]; bad {
			t.Errorf("Runner has a %s method: %s.\n\n"+
				"If pruning is genuinely wanted again, that is a design decision to make "+
				"deliberately — see docs/model.md §5.4 — and it costs the append-only "+
				"property that the machine and restore keys currently rely on.", name, why)
		}
	}
}

// TestParseBackup walks a realistic stream: progress, two unreadable files, a
// line of noise, then the summary.
func TestParseBackup(t *testing.T) {
	stream := strings.Join([]string{
		`{"message_type":"status","percent_done":0.25,"total_files":100,"files_done":25,"total_bytes":1000,"bytes_done":250}`,
		`this line is not JSON and must not stop the parse`,
		`{"message_type":"error","item":"/home/user/.gnupg/S.gpg-agent","error":{"message":"socket not supported"}}`,
		`{"message_type":"status","percent_done":0.9,"files_done":90}`,
		`{"message_type":"error","item":"/home/user/locked.pst","error":{"message":"permission denied"}}`,
		`{"message_type":"summary","files_new":12,"files_changed":3,"data_added":4096,` +
			`"total_files_processed":98,"total_bytes_processed":950,"total_duration":12.5,` +
			`"snapshot_id":"a1b2c3d4"}`,
	}, "\n")

	var seen []Progress

	summary, err := parseBackup(strings.NewReader(stream), func(p Progress) {
		seen = append(seen, p)
	})
	if err != nil {
		t.Fatalf("parseBackup: %v", err)
	}

	if summary.SnapshotID != "a1b2c3d4" {
		t.Errorf("snapshot ID %q, want a1b2c3d4", summary.SnapshotID)
	}

	if summary.FilesNew != 12 || summary.DataAdded != 4096 {
		t.Errorf("summary fields did not decode: %+v", summary)
	}

	if got := summary.Duration().Seconds(); got != 12.5 {
		t.Errorf("duration %v, want 12.5s", got)
	}

	if len(seen) != 2 {
		t.Errorf("saw %d progress lines, want 2", len(seen))
	}

	if len(summary.Errors) != 2 {
		t.Fatalf("kept %d file errors, want 2: %+v", len(summary.Errors), summary.Errors)
	}

	// The unreadable files are the whole reason this program does not treat a
	// finished backup as a good one.
	if summary.Errors[1].Item != "/home/user/locked.pst" ||
		summary.Errors[1].Error != "permission denied" {
		t.Errorf("second error decoded as %+v", summary.Errors[1])
	}
}

// TestParseBackupSurvivesAVeryLongStatusLine guards the scanner buffer. A run
// over deeply nested paths writes status lines past bufio's 64KB default, and
// the default would end the scan silently — losing the summary, and with it
// the snapshot ID of a backup that succeeded.
func TestParseBackupSurvivesAVeryLongStatusLine(t *testing.T) {
	long := `{"message_type":"status","current_files":["` + strings.Repeat("a", 200<<10) + `"]}`
	stream := long + "\n" + `{"message_type":"summary","snapshot_id":"survived"}`

	summary, err := parseBackup(strings.NewReader(stream), nil)
	if err != nil {
		t.Fatalf("parseBackup: %v", err)
	}

	if summary.SnapshotID != "survived" {
		t.Error("the summary was lost behind a long status line")
	}
}

// TestParseBackupCapsTheErrorList keeps a run against an unreadable network
// share from putting a hundred megabytes of near-identical strings in the
// history database.
func TestParseBackupCapsTheErrorList(t *testing.T) {
	var b strings.Builder

	for i := range maxErrorsKept + 50 {
		b.WriteString(`{"message_type":"error","item":"/f`)
		b.WriteString(string(rune('a' + i%26)))
		b.WriteString(`","error":{"message":"denied"}}` + "\n")
	}

	b.WriteString(`{"message_type":"summary","snapshot_id":"x"}`)

	summary, err := parseBackup(strings.NewReader(b.String()), nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(summary.Errors) != maxErrorsKept+1 {
		t.Fatalf("kept %d errors, want %d plus one overflow note", len(summary.Errors), maxErrorsKept)
	}

	if last := summary.Errors[len(summary.Errors)-1].Error; !strings.Contains(last, "50 more") {
		t.Errorf("the overflow note does not say how many were dropped: %q", last)
	}
}

// TestIncompleteIsNotSuccess is the bug in the shell scripts this replaces,
// written down as a test so it cannot come back.
func TestIncompleteIsNotSuccess(t *testing.T) {
	err := &Error{Args: []string{"backup"}, Code: ExitIncomplete}

	if !err.Incomplete() {
		t.Error("exit 3 was not recognised as an incomplete backup")
	}

	if err.Retryable() {
		t.Error("exit 3 is not fixed by retrying; the files will still be locked")
	}

	if !strings.Contains(err.Error(), "some files could not be read") {
		t.Errorf("the message does not explain exit 3: %v", err)
	}
}

func TestOnlyALockedRepositoryIsRetryable(t *testing.T) {
	for code, want := range map[int]bool{
		ExitLocked:        true,
		ExitOK:            false,
		ExitFailed:        false,
		ExitWrongPassword: false,
		ExitNoRepository:  false,
	} {
		if got := (&Error{Code: code}).Retryable(); got != want {
			t.Errorf("exit %d: Retryable() = %v, want %v", code, got, want)
		}
	}
}

// TestErrorMessageCarriesNoSecret is the payoff of keeping credentials out of
// argv: the error is safe to write to a log file and to send to the fleet
// dashboard.
func TestErrorMessageCarriesNoSecret(t *testing.T) {
	repo := testRepo()
	err := &Error{Args: backupArgs(BackupOptions{Targets: []string{"/home"}}), Code: 1,
		Stderr: "Fatal: unable to open config file"}

	msg := err.Error()

	for _, secret := range []string{string(repo.Password), string(repo.SecretAccessKey), string(repo.AccessKeyID)} {
		if strings.Contains(msg, secret) {
			t.Errorf("a credential reached the error message: %q", msg)
		}
	}
}

func TestTailKeepsTheEnd(t *testing.T) {
	got := tail(strings.Repeat("x\n", 10_000)+"the last line", 64)

	if !strings.HasSuffix(got, "the last line") {
		t.Errorf("tail dropped the end: %q", got)
	}

	if len(got) > 64+len("…\n") {
		t.Errorf("tail kept %d bytes, want at most %d", len(got), 64)
	}
}

// TestBackupAgainstAFakeRestic exercises Start/parse/Wait ordering end to end.
// A Wait before the pipe is drained loses the summary line, and that bug does
// not show up in a parser test.
func TestBackupAgainstAFakeRestic(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stub is a shell script")
	}

	dir := t.TempDir()
	bin := filepath.Join(dir, "restic")

	script := `#!/bin/sh
# Refuse to be a test that passes without the environment being right.
[ "$RESTIC_REPOSITORY" = "s3:https://s3.example.invalid/bucket" ] || { echo "wrong repo: $RESTIC_REPOSITORY" >&2; exit 9; }
[ "$RESTIC_PASSWORD" = "repo-password" ] || { echo "wrong password" >&2; exit 9; }
i=0
while [ $i -lt 200 ]; do
  echo '{"message_type":"status","percent_done":0.5,"files_done":'$i'}'
  i=$((i+1))
done
echo '{"message_type":"error","item":"/locked","error":{"message":"denied"}}'
echo '{"message_type":"summary","snapshot_id":"deadbeef","files_new":7,"total_duration":1.5}'
echo "warning: 1 file could not be read" >&2
exit 3
`

	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	r, err := New(bin)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var lines int

	summary, err := r.Backup(context.Background(), testRepo(),
		BackupOptions{Targets: []string{"/home/user"}}, func(Progress) { lines++ })

	// Exit 3: an error, and a real snapshot. Both halves matter.
	var rerr *Error
	if !errors.As(err, &rerr) || !rerr.Incomplete() {
		t.Fatalf("want an incomplete-backup error, got %v", err)
	}

	if summary.SnapshotID != "deadbeef" {
		t.Errorf("the snapshot ID was lost: %+v", summary)
	}

	if summary.FilesNew != 7 {
		t.Errorf("files_new = %d, want 7", summary.FilesNew)
	}

	if lines != 200 {
		t.Errorf("saw %d progress callbacks, want 200", lines)
	}

	if len(summary.Errors) != 1 || summary.Errors[0].Item != "/locked" {
		t.Errorf("the unreadable file was not recorded: %+v", summary.Errors)
	}

	if !strings.Contains(rerr.Stderr, "could not be read") {
		t.Errorf("stderr was not captured: %q", rerr.Stderr)
	}
}

// TestClassifyOlderResticExitCodes is the fleet's reality: restic gained the
// distinct 10/11/12 exit codes in 0.17, and machines here run 0.14, where
// every one of those is exit 1 with a different sentence.
//
// The consequence of getting this wrong is not cosmetic. Exists could not tell
// an empty bucket from a broken one, so enrollment refused to initialise a
// repository that genuinely was not there.
func TestClassifyOlderResticExitCodes(t *testing.T) {
	cases := []struct {
		name   string
		code   int
		stderr string
		want   int
	}{
		{
			name: "0.14 says there is no repository",
			code: 1,
			stderr: "Fatal: unable to open config file: stat /srv/repo/config: no such file or directory\n" +
				"Is there a repository at the following location?\n/srv/repo",
			want: ExitNoRepository,
		},
		{
			name:   "0.14 says the repository is locked",
			code:   1,
			stderr: "unable to create lock in backend: repository is already locked exclusively by PID 4242",
			want:   ExitLocked,
		},
		{
			name:   "0.14 says the password is wrong",
			code:   1,
			stderr: "Fatal: wrong password or no key found",
			want:   ExitWrongPassword,
		},
		{
			name:   "an ordinary failure stays an ordinary failure",
			code:   1,
			stderr: "Fatal: Fatal: unable to save snapshot: server responded with 500",
			want:   ExitFailed,
		},
		{
			name:   "a newer restic's own code is never second-guessed",
			code:   ExitIncomplete,
			stderr: "warning: could not read /home/user/locked.pst",
			want:   ExitIncomplete,
		},
		{
			name:   "success is left alone",
			code:   0,
			stderr: "",
			want:   0,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classify(c.code, c.stderr); got != c.want {
				t.Errorf("classify(%d) = %d, want %d", c.code, got, c.want)
			}
		})
	}
}

// TestMeasureAgainstRealRestic exercises the stats JSON against the actual
// binary rather than a stub, because the field names are restic's and a stub
// would only ever confirm what this file already assumed.
//
// It also demonstrates the fact the whole rotation design rests on: backing up
// again into the same repository reclaims nothing, because there is no such
// thing as a full backup.
func TestMeasureAgainstRealRestic(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell setup")
	}

	bin, err := exec.LookPath("restic")
	if err != nil {
		t.Skip("no restic on this machine")
	}

	dir := t.TempDir()
	repoDir := filepath.Join(dir, "repo")
	src := filepath.Join(dir, "src")

	if err := os.MkdirAll(src, 0o700); err != nil {
		t.Fatal(err)
	}

	// Incompressible and undedupable on purpose. Repetitive text would collapse
	// to a few kilobytes, and then restic's own per-snapshot metadata would be
	// a large fraction of the repository — which makes the ratio this test
	// checks meaningless. Random bytes keep the payload dominant.
	big := make([]byte, 4<<20)
	if _, err := rand.Read(big); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(src, "a.bin"), big, 0o600); err != nil {
		t.Fatal(err)
	}

	r, err := New(bin)
	if err != nil {
		t.Fatal(err)
	}

	repo := Repository{
		URL:      repoDir,
		Password: []byte("test-password"),
		// A local repository ignores these, and Validate insists on them.
		AccessKeyID:     []byte("unused"),
		SecretAccessKey: []byte("unused"),
	}

	ctx := context.Background()

	if err := r.Init(ctx, repo); err != nil {
		t.Fatalf("init: %v", err)
	}

	for i := range 2 {
		if _, err := r.Backup(ctx, repo, BackupOptions{Targets: []string{src}}, nil); err != nil {
			t.Fatalf("backup %d: %v", i+1, err)
		}
	}

	size, err := r.Measure(ctx, repo)
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}

	if size.Now <= 0 || size.Fresh <= 0 {
		t.Fatalf("Measure returned nothing useful: %+v", size)
	}

	if size.Snapshots != 2 {
		t.Errorf("snapshots = %d, want 2", size.Snapshots)
	}

	// The point: two backups of unchanged data store that data once. If restic
	// had a "full backup" mode that re-uploaded everything, Now would be about
	// twice Fresh here and bucket rotation would be pointless.
	//
	// Not exactly zero, and the difference is worth knowing about. Each
	// snapshot carries a little metadata of its own that the next one does not
	// share — a few hundred bytes on 0.19.1, and it varies by platform and by
	// restic version. An earlier version of this test asserted zero, passed
	// against the 0.14 that happened to be installed locally, and failed on CI
	// the moment CI started using the pinned 0.19.1. What matters is the order
	// of magnitude: overhead, not a second copy.
	const tolerance = 0.01

	if limit := int64(float64(size.Fresh) * tolerance); size.Reclaimable() > limit {
		t.Errorf("backing up unchanged data twice left %d bytes reclaimable, "+
			"more than %.0f%% of the %d bytes stored — that is a second copy, "+
			"not snapshot metadata, and it would mean restic had stopped "+
			"deduplicating", size.Reclaimable(), tolerance*100, size.Fresh)
	}
}
