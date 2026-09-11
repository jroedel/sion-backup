// Package legacyscan looks on this machine for the backup that is already
// running.
//
// The reading half of business/domain/legacy: where to look, and what counts
// as a find. Kept apart from legacybus so that the parsing — the part with
// the decisions in it — can be tested without a filesystem, and so that this
// half can be as full of platform trivia as it needs to be.
//
// Nothing here writes, executes or changes anything, with one exception:
// Windows scheduled tasks can only be listed by running schtasks, and there
// is no file to read instead.
package legacyscan

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jroedel/sion-backup/business/domain/legacy/legacybus"
)

// Result is what a scan found, and — as importantly — what it was not
// allowed to look at.
type Result struct {
	// Install is the legacy backup on this machine, or nil.
	Install *legacybus.Install

	// Notes are sentences for the person reading the report.
	Notes []string

	// Blocked are directories that exist and could not be read from this
	// account.
	//
	// This field is the whole reason Result is a struct. The scan runs as an
	// ordinary user, and the commonest legacy layout puts the install in
	// /home/restic — a directory that account cannot enter. os.Stat answers
	// "permission denied", which is not "there is nothing here", and treating
	// the two the same is how recon comes to print "this is a new machine as
	// far as backups go" about a machine that has been backing up nightly for
	// two years. The consequence of believing that is a second bucket, a full
	// re-upload, and the old cron job still running beside the new one.
	//
	// So: not found and not allowed to look are different answers, and the
	// caller must be able to say which one it got.
	Blocked []string
}

// Find looks for the legacy install on this machine.
//
// It returns a [Result] rather than an install and an error because "there is
// nothing here" and "I was not allowed to look" are different answers and the
// caller has to be able to tell them apart.
//
// The first install found wins. A machine with two is a machine somebody has
// already been confused by, and the note says so.
//
// extra are directories to look in before the usual ones. They exist because
// these installs were done by hand from a PDF: the notes named a place, most
// people used it, and somebody will have put it under /srv or in a home
// directory that no longer belongs to anybody. Whoever is standing at the
// machine can say where to look.
func Find(ctx context.Context, extra ...string) Result {
	var out Result

	// Every candidate is visited even after a find, because what could not be
	// read is worth reporting whether or not something else turned up: an
	// install found in one place does not mean the unreadable one elsewhere
	// is not also running.
	for _, dir := range append(extra, candidates()...) {
		script, layout, blocked := lookIn(dir)

		if blocked != "" {
			out.Blocked = append(out.Blocked, blocked)
		}

		if script == "" || out.Install != nil {
			continue
		}

		raw, err := os.ReadFile(script)
		if err != nil {
			out.Notes = append(out.Notes, "found "+script+" but could not read it: "+err.Error())

			continue
		}

		out.Install = describe(ctx, dir, script, layout, string(raw))

		if more := others(dir, extra); len(more) > 0 {
			out.Notes = append(out.Notes,
				"there is more than one legacy install here: also "+strings.Join(more, ", "))
		}
	}

	out.Blocked = dedupe(out.Blocked)

	// Deliberately not also appended to Notes: Blocked is structured, the
	// caller renders it prominently, and a fact stated twice in one report
	// reads like two facts.
	return out
}

// dedupe removes repeats while keeping the order they were found in.
//
// One unreadable home directory blocks three candidate paths under it, and
// three identical notes about /home/restic help nobody.
func dedupe(in []string) []string {
	if len(in) == 0 {
		return nil
	}

	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))

	for _, s := range in {
		if !seen[s] {
			seen[s] = true

			out = append(out, s)
		}
	}

	return out
}

// describe fills in everything known about one install.
func describe(ctx context.Context, dir, script string, layout legacybus.Layout, raw string) *legacybus.Install {
	fields := legacybus.Parse(raw)

	found := &legacybus.Install{
		Layout:          layout,
		Version:         legacybus.Version(layout, raw, dir),
		Dir:             dir,
		Script:          script,
		Binaries:        binariesIn(dir),
		RepositoryURL:   fields.RepositoryURL,
		NodeID:          fields.NodeID,
		Targets:         fields.Targets,
		Excludes:        fields.Excludes,
		ExcludeFile:     fields.ExcludeFile,
		UsesFSSnapshot:  fields.UsesFSSnapshot,
		PackSizeMiB:     fields.PackSizeMiB,
		ReadConcurrency: fields.ReadConcurrency,
		HasCredentials:  fields.HasCredentials,
		PasswordFile:    fields.PasswordFile,
	}

	if found.ExcludeFile != "" {
		found.ExcludeCount = countLines(found.ExcludeFile)
	}

	if found.PasswordFile != "" {
		// Its existence is worth knowing; its contents are the repository
		// password and are nobody's business but the person migrating.
		if _, err := os.Stat(found.PasswordFile); err == nil {
			found.HasCredentials = true
		}
	}

	found.Account = accountFor(dir)
	found.Schedule = scheduleFor(ctx, script, found.Account)

	return found
}

// Candidates are the directories a scan looks in without being told to.
//
// Exported so that a caller printing "run this next" can tell whether the
// install it found needs --legacy-dir repeating on that command, or whether it
// sits somewhere the next scan will look anyway. A flag printed needlessly is
// one somebody learns to drop, including on the machine where it mattered.
func Candidates() []string { return candidates() }

// candidates are the directories a legacy install could be in.
//
// The install notes named one place per version and people followed them
// approximately, so this is the union of what three versions of the notes
// said and the shapes those instructions produce when somebody adapts them.
func candidates() []string {
	var dirs []string

	if runtime.GOOS == "windows" {
		for _, home := range userDirs(`C:\Users`) {
			dirs = append(dirs,
				filepath.Join(home, "Documents", "backup"),
				filepath.Join(home, "backup"),
			)
		}

		return append(dirs, `C:\backup`, `C:\ProgramData\backup`)
	}

	// The notes said /home/restic/bin at 1.1 and /home/backup/bin before it.
	dirs = append(dirs, "/home/restic/bin", "/home/backup/bin", "/opt/backup", "/usr/local/backup")

	for _, home := range userDirs("/home") {
		dirs = append(dirs,
			filepath.Join(home, "bin"),
			filepath.Join(home, "backup"),
			filepath.Join(home, "Documents", "backup"),
		)
	}

	return dirs
}

// userDirs lists the immediate subdirectories of a home root.
//
// A root that cannot be read at all returns nothing rather than an error: the
// per-directory stat in lookIn is what reports the blocked ones, and it does
// it with the path a person can act on. A completely unreadable /home is
// caught there too, because the candidate paths under it stat as denied.
func userDirs(root string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}

	var out []string

	for _, e := range entries {
		if e.IsDir() {
			out = append(out, filepath.Join(root, e.Name()))
		}
	}

	return out
}

// scriptNames are the backup scripts three vintages of the install notes
// produced.
var scriptNames = []struct {
	name   string
	layout legacybus.Layout
}{
	{"backup.sh", legacybus.LayoutLinux},
	{"backup.bat", legacybus.LayoutWindows},
	{"nightly-whole-system.sh", legacybus.LayoutLinux},
}

// lookIn reports the backup script in a directory, and separately the path
// that stopped it from being able to tell.
//
// The blocked return is the point: a stat that fails with "permission denied"
// is not a directory without a script in it. See Result.Blocked.
func lookIn(dir string) (script string, layout legacybus.Layout, blocked string) {
	for _, c := range scriptNames {
		path := filepath.Join(dir, c.name)

		info, err := os.Stat(path)

		switch {
		case err == nil && !info.IsDir():
			return path, c.layout, ""

		case errors.Is(err, fs.ErrPermission):
			blocked = deepestVisible(path)
		}
	}

	return "", "", blocked
}

// scriptIn is lookIn for callers that only want the yes or no.
func scriptIn(dir string) (string, legacybus.Layout, bool) {
	script, layout, _ := lookIn(dir)

	return script, layout, script != ""
}

// deepestVisible walks up from a path that could not be read to the last
// ancestor that can be, which is the directory to name in the report.
//
// /home/restic/bin/backup.sh cannot be stat'ed from an ordinary account
// because /home/restic is mode 0750 — but /home/restic itself is perfectly
// visible, and it is the thing to tell somebody about. Naming the leaf would
// be technically accurate and useless: three candidate paths under one home
// directory would produce three reports of the same one fact.
func deepestVisible(path string) string {
	for dir := filepath.Dir(path); ; {
		if _, err := os.Stat(dir); err == nil {
			return dir
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			// Reached the root without finding anything readable, which
			// should not happen; the original path is still the honest
			// answer to "what could not be read".
			return path
		}

		dir = parent
	}
}

// others reports further legacy installs beyond the one already found.
func others(found string, extra []string) []string {
	var out []string

	for _, dir := range append(extra, candidates()...) {
		if dir == found {
			continue
		}

		if _, _, ok := scriptIn(dir); ok {
			out = append(out, dir)
		}
	}

	return out
}

// binariesIn lists the legacy binaries beside a script.
func binariesIn(dir string) []string {
	var out []string

	for _, name := range []string{"restic", "restic.exe", "nodes", "nodes-amd64.exe"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			out = append(out, name)
		}
	}

	return out
}

// countLines counts the non-empty, non-comment lines in a file.
//
// The exclude list is somebody's directory names, so it is counted and not
// read out.
func countLines(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()

	var n int

	scanner := bufio.NewScanner(f)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if line != "" && !strings.HasPrefix(line, "#") {
			n++
		}
	}

	// A read error mid-file gives an undercount, which is the right answer to
	// give for a count nobody makes a decision on: the number is shown to a
	// person beside the path, and the path is what they open.
	_ = scanner.Err()

	return n
}

// accountFor guesses which account the install belongs to, from where it
// lives. /home/restic/bin is the restic account; C:\Users\backup\... is the
// backup account.
func accountFor(dir string) string {
	clean := filepath.ToSlash(dir)

	for _, root := range []string{"/home/", "c:/Users/", "C:/Users/"} {
		if rest, ok := strings.CutPrefix(clean, root); ok {
			if name, _, found := strings.Cut(rest, "/"); found || name != "" {
				return name
			}
		}
	}

	return ""
}

// scheduleFor finds what starts the legacy backup.
func scheduleFor(ctx context.Context, script, account string) string {
	if runtime.GOOS == "windows" {
		return scheduledTask(ctx, script)
	}

	return cronLine(script, account)
}

// cronTables are the files a cron job could be in. The per-user spool is
// readable only by root, which is worth reporting rather than guessing at.
func cronTables(account string) []string {
	tables := []string{"/etc/crontab"}

	if account != "" {
		tables = append(tables,
			"/var/spool/cron/crontabs/"+account, // Debian, Ubuntu
			"/var/spool/cron/"+account,          // Red Hat
		)
	}

	entries, err := os.ReadDir("/etc/cron.d")
	if err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				tables = append(tables, filepath.Join("/etc/cron.d", e.Name()))
			}
		}
	}

	return tables
}

// cronLine returns the crontab line that runs the script, verbatim.
func cronLine(script, account string) string {
	base := filepath.Base(script)

	for _, table := range cronTables(account) {
		raw, err := os.ReadFile(table)
		if err != nil {
			continue
		}

		for line := range strings.Lines(string(raw)) {
			line = strings.TrimSpace(line)

			if strings.HasPrefix(line, "#") {
				continue
			}

			if strings.Contains(line, script) || strings.Contains(line, base) {
				return table + ": " + line
			}
		}
	}

	return ""
}

// schtasksTimeout bounds the one command this package runs.
const schtasksTimeout = 30 * time.Second

// scheduledTask finds the Windows task that runs the script.
//
// The only exec in this package, because Windows keeps its schedule in a
// database rather than in a file, and reading it any other way means COM.
func scheduledTask(ctx context.Context, script string) string {
	ctx, cancel := context.WithTimeout(ctx, schtasksTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, "schtasks", "/query", "/fo", "LIST", "/v").CombinedOutput()
	if err != nil {
		return ""
	}

	base := filepath.Base(script)

	var name string

	for line := range strings.Lines(string(out)) {
		line = strings.TrimSpace(line)

		if rest, ok := strings.CutPrefix(line, "TaskName:"); ok {
			name = strings.TrimSpace(rest)
		}

		if strings.Contains(strings.ToLower(line), strings.ToLower(base)) && name != "" {
			return "scheduled task " + name
		}
	}

	return ""
}

// ExcludePatterns reads the legacy exclude file.
//
// The reading half of the advice `recon` prints: copy the exclude list into
// the new configuration before the first run, or that run will back up things
// somebody chose to leave out. Whoever wrote that list is not going to write
// it again.
//
// Blank lines and comments are dropped; everything else is handed over as
// restic itself would read it. The patterns are directory names belonging to
// the person whose machine this is, so they go into the plan and not onto the
// screen — the caller reports how many there were.
//
// A missing or unreadable file is an error rather than an empty list. An
// empty list here means "this machine excludes nothing", which is a different
// and much worse claim than "the file could not be read".
func ExcludePatterns(in *legacybus.Install) ([]string, error) {
	if in == nil || in.ExcludeFile == "" {
		return nil, nil
	}

	f, err := os.Open(in.ExcludeFile)
	if err != nil {
		return nil, fmt.Errorf("legacyscan: reading the exclude list: %w", err)
	}
	defer f.Close()

	var out []string

	scanner := bufio.NewScanner(f)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("legacyscan: reading the exclude list: %w", err)
	}

	return out, nil
}

// Credentials reads the secrets the legacy install holds, including the
// password file when the script points at one.
//
// See [legacybus.Credentials] for why this is a call a caller has to make on
// purpose rather than a field of the scan. Nothing in this package calls it;
// `sion-backup adopt-enroll` does, once, with a person watching.
//
// Incomplete credentials come back with no error. A script somebody scrubbed
// before filing it, or a password file this account cannot read, is an
// ordinary thing to find on a machine and the caller's answer is to carry on
// without the measurement rather than to refuse to migrate.
func Credentials(in *legacybus.Install) (legacybus.Credentials, error) {
	if in == nil {
		return legacybus.Credentials{}, nil
	}

	raw, err := os.ReadFile(in.Script)
	if err != nil {
		return legacybus.Credentials{}, fmt.Errorf("legacyscan: reading %s: %w", in.Script, err)
	}

	c := legacybus.ParseCredentials(string(raw))

	if len(c.Password) == 0 && c.PasswordFile != "" {
		// Trailing newline stripped and nothing else: a repository password
		// is whatever bytes are in the file, and "helpfully" trimming spaces
		// out of one would produce a password that does not open the bucket
		// and no explanation of why.
		if pw, err := os.ReadFile(c.PasswordFile); err == nil {
			c.Password = []byte(strings.TrimRight(string(pw), "\r\n"))
		}
	}

	return c, nil
}
