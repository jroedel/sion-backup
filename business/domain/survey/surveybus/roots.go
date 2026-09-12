package surveybus

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Trouble is one folder on a list that cannot be backed up, and why.
//
// Why as a finished sentence fragment rather than an error: it is shown to
// somebody who has just typed the path into a box, and "permission denied" in
// that position is a worse answer than "cannot be read by this account".
type Trouble struct {
	Path string
	Why  string
}

// CheckRoots reports the folders on a list that cannot be backed up.
//
// It is the check the setup page makes before saving, and the reason it exists
// is that the failure it catches is otherwise invisible for months. restic is
// asked to back up a folder that has been renamed; it says so on its own
// output, backs up the rest, and exits successfully. The run is recorded as a
// success, the status page is green, and the folder somebody cares about has
// not been in a backup since the day they mistyped it. Doctor catches it
// eventually — see the "targets exist" check, which is a failure and not a
// warning for exactly this reason — but by then somebody has to notice doctor.
//
// Checked here is only the root itself: it exists, it is a folder, and this
// account can open it. What is inside is not checked and must not be. Every
// home directory has something in it that cannot be read — a stale gvfs mount,
// another account's file left behind — and refusing to save a plan over one of
// those would mean refusing to back up the machine. The walk counts those and
// the page reports them as "N folders could not be read", which is the right
// weight for them.
func CheckRoots(roots []string) []Trouble {
	var out []Trouble

	for _, root := range roots {
		path := strings.TrimSpace(root)
		if path == "" {
			continue
		}

		if why := checkRoot(path); why != "" {
			out = append(out, Trouble{Path: path, Why: why})
		}
	}

	return out
}

// checkRoot returns why one folder cannot be backed up, or empty.
func checkRoot(path string) string {
	// A relative path is the quiet one. It would resolve against whatever
	// directory the daemon happened to start in, which is not anywhere
	// somebody meant, and the backup would either be of the wrong thing or of
	// nothing.
	if !filepath.IsAbs(path) {
		return "is not a full path — it has to start from the top, like /home or C:\\"
	}

	info, err := os.Stat(path)

	switch {
	case errors.Is(err, os.ErrNotExist):
		return "is not on this computer"

	case errors.Is(err, os.ErrPermission):
		return "cannot be reached: a folder above it is not readable by this account"

	case err != nil:
		return fmt.Sprintf("cannot be looked at: %v", err)

	case info.IsDir():
		// Opening and reading one entry, rather than trusting the mode bits:
		// they are not the whole story on a machine with ACLs, SELinux, or a
		// network filesystem that decides at open time.
		f, err := os.Open(path)
		if err != nil {
			return "cannot be read by this account"
		}
		defer f.Close()

		if _, err := f.ReadDir(1); err != nil && !errors.Is(err, io.EOF) {
			return "cannot be listed by this account"
		}

	case info.Mode().IsRegular():
		f, err := os.Open(path)
		if err != nil {
			return "cannot be read by this account"
		}

		f.Close()

	default:
		return "is not a folder"
	}

	return ""
}

// ExcludesFor is the exclude list narrowed to the patterns that could actually
// match something inside these folders.
//
// The setup page shows somebody what their choice comes to, and the whole
// value of that is that it is short enough to read. A machine backing up
// /opt/projects does not need to be told that /home/jeff/.cache is excluded:
// it is true, it is irrelevant, and three lines of it teach somebody that this
// part of the page is not worth reading.
//
// The rule follows restic's own matching. A pattern with no separator in it —
// "node_modules", "*.iso" — matches at any depth, so it applies wherever the
// backup goes. An absolute pattern applies only under the root it names.
// Anything else is a relative path fragment, which restic matches as a suffix,
// so it too can apply anywhere.
func ExcludesFor(roots, excludes []string) []string {
	out := make([]string, 0, len(excludes))

	for _, pattern := range excludes {
		if !strings.ContainsRune(pattern, filepath.Separator) || !filepath.IsAbs(pattern) {
			out = append(out, pattern)

			continue
		}

		for _, root := range roots {
			if inside(root, pattern) {
				out = append(out, pattern)

				break
			}
		}
	}

	return out
}

// inside reports whether path is root or below it.
//
// Not named "under": junk.go already has one of those, building a path inside
// the home directory, and two functions of the same name doing different jobs
// in one package is how a reader loses an afternoon.
func inside(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil {
		return false
	}

	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
