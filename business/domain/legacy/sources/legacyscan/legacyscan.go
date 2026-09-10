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
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jroedel/sion-backup/business/domain/legacy/legacybus"
)

// Find returns the legacy install on this machine, or nil.
//
// The first one found wins. A machine with two is a machine somebody has
// already been confused by, and the note says so.
//
// extra are directories to look in before the usual ones. They exist because
// these installs were done by hand from a PDF: the notes named a place, most
// people used it, and somebody will have put it under /srv or in a home
// directory that no longer belongs to anybody. Whoever is standing at the
// machine can say where to look.
func Find(ctx context.Context, extra ...string) (*legacybus.Install, []string) {
	var notes []string

	for _, dir := range append(extra, candidates()...) {
		script, layout, ok := scriptIn(dir)
		if !ok {
			continue
		}

		raw, err := os.ReadFile(script)
		if err != nil {
			notes = append(notes, "found "+script+" but could not read it: "+err.Error())

			continue
		}

		found := describe(ctx, dir, script, layout, string(raw))

		if more := others(dir, extra); len(more) > 0 {
			notes = append(notes,
				"there is more than one legacy install here: also "+strings.Join(more, ", "))
		}

		return found, notes
	}

	return nil, notes
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

// scriptIn reports the backup script in a directory, if there is one.
func scriptIn(dir string) (string, legacybus.Layout, bool) {
	for _, c := range []struct {
		name   string
		layout legacybus.Layout
	}{
		{"backup.sh", legacybus.LayoutLinux},
		{"backup.bat", legacybus.LayoutWindows},
		{"nightly-whole-system.sh", legacybus.LayoutLinux},
	} {
		path := filepath.Join(dir, c.name)

		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path, c.layout, true
		}
	}

	return "", "", false
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
