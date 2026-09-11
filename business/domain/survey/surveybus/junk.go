package surveybus

import (
	"os"
	"path/filepath"
	"runtime"
)

// JunkExcludes are the things that are large, change constantly, and that
// nobody has ever asked to have restored.
//
// This list is the whole of the "leave out the usual junk" checkbox on the
// setup page, and the reason that checkbox exists rather than a box somebody
// types restic patterns into. Every item on it is defensible on its own, and
// the argument for each is the same one the settings page already makes: a
// browser cache is regenerated in seconds, a downloaded installer can be
// downloaded again, and a 40 GB virtual machine image is a file that exists to
// be rebuilt.
//
// # What is deliberately NOT here
//
// Anything somebody might have put work into. No source-code directories
// beyond the two that are definitionally regenerable (node_modules, which is a
// download, and __pycache__, which is a build product); no ~/.config, which is
// where a great deal of irreplaceable setup lives; no mail. The test for
// inclusion is "can this be recreated from something else on the machine, or
// from the internet, without anybody losing a day" — and when the answer is
// arguable, it is left out, because the cost of wrongly excluding something is
// paid the day somebody needs it and the cost of wrongly including it is a few
// gigabytes.
//
// The patterns are restic's, and they go into the plan as ordinary excludes.
// Somebody who disagrees with one can delete it on the settings page, which is
// why the checkbox writes them into the list rather than hiding them behind a
// flag.
func JunkExcludes() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}

	// Everywhere: files that are by definition a copy of something else, and
	// directories that are by definition regenerated.
	out := []string{
		"*.iso",
		"*.vmdk",
		"*.vdi",
		"*.vhd",
		"*.vhdx",
		"*.qcow2",
		"node_modules",
		"__pycache__",
		".Trash",
		"Trash",
	}

	switch runtime.GOOS {
	case "windows":
		out = append(out,
			"$RECYCLE.BIN",
			"pagefile.sys",
			"hiberfil.sys",
			"swapfile.sys",
			under(home, "AppData", "Local", "Temp"),
			under(home, "AppData", "Local", "Microsoft", "Windows", "INetCache"),
			under(home, "AppData", "Local", "Microsoft", "Windows", "Explorer"),
			under(home, "AppData", "Local", "Packages"),
			under(home, "Downloads"),
		)

	case "darwin":
		out = append(out,
			under(home, "Library", "Caches"),
			under(home, "Library", "Application Support", "Steam"),
			under(home, "Library", "Developer", "Xcode", "DerivedData"),
			under(home, "Downloads"),
		)

	default:
		out = append(out,
			under(home, ".cache"),
			under(home, ".local", "share", "Trash"),
			under(home, ".local", "share", "Steam"),
			under(home, ".var", "app"),
			under(home, "snap"),
			under(home, "Downloads"),
		)
	}

	return compact(out)
}

// under joins a path inside the home directory, or returns empty when there is
// no home directory to anchor it to.
//
// Empty rather than a relative pattern, deliberately. "Downloads" as a bare
// pattern matches a folder of that name ANYWHERE in the backup, including one
// somebody keeps inside a project they care about; anchored to the home
// directory it means the one folder it is meant to mean.
func under(home string, parts ...string) string {
	if home == "" {
		return ""
	}

	return filepath.Join(append([]string{home}, parts...)...)
}

// compact drops the empty entries left by a machine with no home directory.
func compact(in []string) []string {
	out := make([]string, 0, len(in))

	for _, s := range in {
		if s != "" {
			out = append(out, s)
		}
	}

	return out
}

// WithoutJunk removes this list from a set of excludes, so that unticking the
// box takes back exactly what ticking it added and leaves everything somebody
// wrote themselves alone.
func WithoutJunk(excludes []string) []string {
	junk := make(map[string]bool, 32)
	for _, j := range JunkExcludes() {
		junk[j] = true
	}

	out := make([]string, 0, len(excludes))

	for _, e := range excludes {
		if !junk[e] {
			out = append(out, e)
		}
	}

	return out
}

// HasJunk reports whether the list already carries this set, so the checkbox
// on a page rendered from a stored plan comes up the way it was left.
//
// "Carries it" means all of it. A list somebody has edited one pattern out of
// answers no, and the box comes up unticked with their list untouched — which
// is right: ticking it then restores the full set, and that is the only thing
// ticking it can honestly mean.
func HasJunk(excludes []string) bool {
	have := make(map[string]bool, len(excludes))
	for _, e := range excludes {
		have[e] = true
	}

	junk := JunkExcludes()
	if len(junk) == 0 {
		return false
	}

	for _, j := range junk {
		if !have[j] {
			return false
		}
	}

	return true
}
