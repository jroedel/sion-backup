// Package legacybus finds the backup that is already on a machine.
//
// Every machine in this fleet except a brand-new one is already backing up,
// with the scripts this program replaces: a shell script or a .bat file, a
// restic binary, a "nodes" binary that reports to an older server, and a cron
// job or a scheduled task. Installing over the top of that without looking
// gives a machine two backup systems, two schedules, and two opinions about
// which bucket is current.
//
// # Why this reads rather than assumes
//
// The legacy installs were done by hand, from a PDF, over about two years and
// by more than one person. The install notes say /home/restic/bin; one machine
// has /home/backup/bin, because version 1.0 of the notes said that. The
// Windows one lives under a local account called backup, except where it does
// not. So this looks in the places the notes named, and in the places the
// notes would have led somebody to, and reports what it actually finds — with
// the version of the layout it matched, so an operator can see which vintage
// they are standing in front of.
//
// # What it deliberately does not do
//
// It does not touch anything. Reading is safe to do on a machine somebody
// depends on; disabling a cron job is not, and belongs to an installer that
// has been told to, by a person who is standing there.
//
// It does not print credentials. The legacy scripts carry the S3 keys and the
// repository password in plain text, which is one of the reasons they are
// being replaced, and reporting "there are credentials in this file" is all
// anybody needs to go and get them.
package legacybus

import (
	"regexp"
	"strconv"
	"strings"
)

// Layout is which vintage of the legacy install was found.
type Layout string

const (
	// LayoutLinux is the shell-script install, versions 0.2 through 1.1.
	LayoutLinux Layout = "linux"

	// LayoutWindows is the .bat install, version 1.3.
	LayoutWindows Layout = "windows"
)

// Install is a legacy backup found on this machine.
type Install struct {
	Layout Layout `json:"layout"`

	// Version is the vintage this most resembles: "1.1", "1.0", "0.2",
	// "1.3". Inferred from what the script contains, because the scripts
	// themselves carry no version number — only the PDF beside them did.
	Version string `json:"version"`

	// Dir is where the script and binaries live.
	Dir string `json:"dir"`

	// Script is the backup script itself.
	Script string `json:"script"`

	// Binaries are the ones found beside it: restic, nodes.
	Binaries []string `json:"binaries,omitempty"`

	// Account is the OS account it runs as, where that could be determined.
	Account string `json:"account,omitempty"`

	// Schedule is the cron line or scheduled task that starts it, verbatim.
	Schedule string `json:"schedule,omitempty"`

	// RepositoryURL is the bucket it has been writing to. The single most
	// important thing here: it is a year of somebody's history, and whether
	// the new install adopts it or starts a new one is the decision the
	// migration turns on.
	RepositoryURL string `json:"repository_url,omitempty"`

	// NodeID is what it calls this machine on the old dashboard.
	NodeID string `json:"node_id,omitempty"`

	// Targets are the paths it backs up.
	Targets []string `json:"targets,omitempty"`

	// ExcludeFile is the exclude list it uses, and ExcludeCount how many
	// lines are in it. The contents are somebody's directory names and are
	// not reported.
	ExcludeFile  string `json:"exclude_file,omitempty"`
	ExcludeCount int    `json:"exclude_count,omitempty"`

	// UsesFSSnapshot records --use-fs-snapshot in the script. On Windows the
	// legacy install had it from the start, and losing it in the migration
	// would be a quiet regression on exactly the machines that need it.
	UsesFSSnapshot bool `json:"uses_fs_snapshot"`

	PackSizeMiB     int `json:"pack_size_mib,omitempty"`
	ReadConcurrency int `json:"read_concurrency,omitempty"`

	// HasCredentials records that the script carries an S3 key or a
	// repository password in plain text. The values are never read out of it.
	HasCredentials bool `json:"has_credentials"`

	// PasswordFile is a RESTIC_PASSWORD_FILE the script points at, which the
	// Windows install used. Named so somebody can go and get it; never read.
	PasswordFile string `json:"password_file,omitempty"`
}

// Fields is what a legacy script says about itself.
type Fields struct {
	RepositoryURL   string
	NodeID          string
	Targets         []string
	ExcludeFile     string
	PasswordFile    string
	UsesFSSnapshot  bool
	PackSizeMiB     int
	ReadConcurrency int
	HasCredentials  bool
}

// assignment matches both forms the legacy scripts use:
//
//	export RESTIC_REPOSITORY="s3:https://..."     (sh)
//	set RESTIC_REPOSITORY=s3:https://...          (bat)
//
// Deliberately loose about quoting and spacing, because these were edited by
// hand on each machine and one of them has a space before the equals sign.
var assignment = regexp.MustCompile(`(?im)^\s*(?:export\s+|set\s+)?([A-Z_][A-Z0-9_]*)\s*=\s*"?([^"\r\n]*)"?\s*$`)

// placeholder is what somebody wrote in when they scrubbed a script before
// filing it. A run of x's is not a credential, and reporting it as one would
// send an operator looking for keys that are not there.
var placeholder = regexp.MustCompile(`^x+$`)

// Parse reads what a legacy backup script says about itself.
//
// Values are taken as written. No attempt is made to resolve a variable used
// inside another variable beyond the one substitution the Windows script
// needs (%BACKUP_PATH%), because these are five-line environment blocks and a
// shell interpreter is not the answer to reading one.
func Parse(script string) Fields {
	var (
		f    Fields
		vars = map[string]string{}
	)

	for _, m := range assignment.FindAllStringSubmatch(script, -1) {
		name, value := m[1], strings.TrimSpace(m[2])
		vars[name] = value

		switch name {
		case "RESTIC_REPOSITORY":
			f.RepositoryURL = value

		case "NODE_ID", "NODE":
			f.NodeID = value

		case "RESTIC_PASSWORD_FILE":
			f.PasswordFile = value

		case "RESTIC_PACK_SIZE":
			f.PackSizeMiB, _ = strconv.Atoi(value)

		case "RESTIC_READ_CONCURRENCY":
			f.ReadConcurrency, _ = strconv.Atoi(value)

		case "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "RESTIC_PASSWORD":
			if value != "" && !placeholder.MatchString(value) {
				f.HasCredentials = true
			}
		}
	}

	f.PasswordFile = expand(f.PasswordFile, vars)
	f.UsesFSSnapshot = strings.Contains(script, "--use-fs-snapshot")
	f.ExcludeFile = expand(excludeFile(script), vars)
	f.Targets = targets(script)

	return f
}

// expand substitutes the one form of reference these scripts contain:
// %NAME% on Windows, $NAME or ${NAME} on Linux.
func expand(value string, vars map[string]string) string {
	if value == "" {
		return ""
	}

	for name, v := range vars {
		if v == "" {
			continue
		}

		for _, ref := range []string{"%" + name + "%", "${" + name + "}", "$" + name} {
			value = strings.ReplaceAll(value, ref, v)
		}
	}

	return value
}

// excludeFileRe finds the argument to --exclude-file, quoted or not.
var excludeFileRe = regexp.MustCompile(`--exclude-file[ =]+"?([^"\s]+)"?`)

func excludeFile(script string) string {
	if m := excludeFileRe.FindStringSubmatch(script); m != nil {
		return m[1]
	}

	return ""
}

// backupLine finds the restic backup command, which is where the targets are.
var backupLine = regexp.MustCompile(`(?m)^.*restic(?:\.exe)?["']?\s+.*\bbackup\b.*$`)

// optionWithValue matches the flags whose argument must not be mistaken for a
// target path.
var optionWithValue = map[string]bool{
	"--exclude-file": true,
	"--exclude":      true,
	"-o":             true,
	"--tag":          true,
	"--host":         true,
	"--target":       true,
}

// targets pulls the paths off the end of the restic backup command.
//
// Positional arguments only, skipping flags and the values that belong to
// them. Imperfect by construction — this is a regular expression reading a
// shell command — so a caller shows it to a person rather than acting on it.
func targets(script string) []string {
	line := backupLine.FindString(script)
	if line == "" {
		return nil
	}

	fields := strings.Fields(line)

	var (
		out   []string
		after bool
		skip  bool
	)

	for _, field := range fields {
		switch {
		case skip:
			skip = false

		case !after:
			// Everything before the word "backup" is the binary and its
			// global options.
			if field == "backup" {
				after = true
			}

		case optionWithValue[field]:
			skip = true

		case strings.HasPrefix(field, "-"):
			// A flag, or --exclude={a,b,c} which is a brace expansion of
			// excludes rather than a target.

		default:
			out = append(out, strings.Trim(field, `"'`))
		}
	}

	return out
}

// Version guesses which vintage of the install notes produced a script.
//
// The scripts carry no version of their own — only the PDF beside them did —
// so this reads the differences between them. It is a hint for whoever is
// standing at the machine, not a decision anything is made on.
func Version(layout Layout, script, dir string) string {
	switch layout {
	case LayoutWindows:
		return "1.3"

	default:
		switch {
		case strings.Contains(script, "BACKUP_DIR=/home/restic"), strings.Contains(dir, "/home/restic"):
			// 1.1 moved the account from "backup" to "restic" and started
			// using $BACKUP_DIR throughout.
			return "1.1"

		case strings.Contains(script, "BACKUP_DIR="):
			return "1.0"

		default:
			// 0.2 hard-coded /home/backup/bin in every line.
			return "0.2"
		}
	}
}
