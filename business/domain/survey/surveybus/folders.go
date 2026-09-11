package surveybus

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Style is one of the answers to "what should be backed up".
//
// Two are offered, and that is a decision rather than an omission. The third
// one every backup product has — the whole machine — is deliberately not here:
// this daemon runs as the signed-in user, so it cannot read other accounts or
// most of the operating system, and what it would actually produce is a run
// that reports files missing every night forever. A work computer's operating
// system is reinstalled rather than restored, and a whole-machine backup taken
// from inside a running system is not a bootable one anyway.
//
// What stands in for it is [StyleCustom]: the folders are a list, and somebody
// who wants /etc in it can put /etc in it.
type Style string

const (
	// StylePersonal is the standard document folders, and nothing else.
	StylePersonal Style = "personal"

	// StyleHome is the whole user folder, minus whatever the excludes say.
	StyleHome Style = "home"

	// StyleCustom is a list somebody wrote themselves. It is not measured as
	// a choice — there is nothing to measure until they have written it — and
	// it never appears in Choices.
	StyleCustom Style = "custom"
)

// Choice is one backup style as it is offered on the setup page.
type Choice struct {
	Style Style

	// Title is the sentence on the radio button.
	Title string

	// Detail says what it means, in the same voice.
	Detail string

	// Roots are the folders it would back up, as they would be written into
	// the plan. Shown to the person, not just used: "your home folder" means
	// nothing next to /home/jeff, and the second one is what actually happens.
	Roots []string
}

// Choices are the styles this machine can offer, cheapest to measure first.
//
// Only folders that exist are included, so a work machine with no ~/Videos
// does not offer to back one up, and the estimate is not quietly counting a
// directory that is not there.
func Choices() []Choice {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		// A machine whose home directory cannot be resolved can still be set
		// up; it just has nothing to suggest, and the setup page falls back to
		// asking for the folders directly.
		return nil
	}

	var out []Choice

	if personal := existing(PersonalFolders(home)); len(personal) > 0 {
		out = append(out, Choice{
			Style: StylePersonal,
			Title: "My documents, desktop and pictures",
			Detail: "The folders people keep work in. Smaller and quicker, and it " +
				"leaves out program settings and anything installed.",
			Roots: personal,
		})
	}

	out = append(out, Choice{
		Style: StyleHome,
		Title: "Everything in my user folder",
		Detail: "The above and the rest of it: mail, program settings, browser " +
			"profiles, anything saved anywhere under your own name.",
		Roots: []string{home},
	})

	return out
}

// PersonalFolders are the standard document folders inside a home directory.
//
// # Why Downloads is not one of them
//
// It is the largest folder on most work machines and the one nobody has ever
// asked to have restored: it holds installers, PDFs somebody has already read,
// and copies of files that exist somewhere else. The settings page says the
// same thing about caches, and this is the same argument applied once rather
// than left for everybody to discover.
//
// Somebody who wants it backed up can add it, on the setup page, in the box
// under "or choose the folders myself".
func PersonalFolders(home string) []string {
	names := []string{"Documents", "Desktop", "Pictures", "Music", "Videos"}

	if runtime.GOOS == "linux" {
		// On Linux the names are localised: a German desktop has Dokumente and
		// Schreibtisch, and looking for "Documents" there finds nothing at all
		// — which would silently offer to back up an empty list. The desktop
		// environment writes the real names in a file, so ask it.
		if dirs := xdgUserDirs(home); len(dirs) > 0 {
			return dirs
		}
	}

	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, filepath.Join(home, n))
	}

	return out
}

// xdgUserDirs reads the localised folder names out of the freedesktop file
// every Linux desktop writes.
//
// The file is shell syntax by design and is sourced by scripts:
//
//	XDG_DESKTOP_DIR="$HOME/Schreibtisch"
//	XDG_DOCUMENTS_DIR="$HOME/Dokumente"
//
// Parsed rather than sourced, obviously. Anything that is not one of the four
// keys wanted here is ignored, and a file that cannot be read means fall back
// to the English names — which is right, because a machine with no desktop
// environment has the English ones or has none.
func xdgUserDirs(home string) []string {
	f, err := os.Open(filepath.Join(home, ".config", "user-dirs.dirs"))
	if err != nil {
		return nil
	}
	defer f.Close()

	// The order is the order they are offered in and the order they go into
	// the plan, so it is fixed here rather than taken from the file.
	wanted := []string{
		"XDG_DOCUMENTS_DIR", "XDG_DESKTOP_DIR",
		"XDG_PICTURES_DIR", "XDG_MUSIC_DIR", "XDG_VIDEOS_DIR",
	}

	found := map[string]string{}

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if strings.HasPrefix(line, "#") {
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}

		value = strings.Trim(strings.TrimSpace(value), `"`)

		// $HOME is the only expansion these files use, and the only one worth
		// supporting: a path that still says $HOME would be created as a
		// directory of that name by something less careful downstream.
		switch {
		case value == "$HOME", value == "$HOME/":
			// The desktop's way of saying "there is no such folder, use the
			// home directory" — which is not a personal folder and must not
			// quietly turn this choice into the other one.
			continue

		case strings.HasPrefix(value, "$HOME/"):
			value = filepath.Join(home, strings.TrimPrefix(value, "$HOME/"))

		case !filepath.IsAbs(value):
			continue
		}

		found[strings.TrimSpace(key)] = value
	}

	var out []string

	for _, key := range wanted {
		if dir := found[key]; dir != "" {
			out = append(out, dir)
		}
	}

	return out
}

// existing keeps the paths that are there.
func existing(paths []string) []string {
	var out []string

	for _, p := range paths {
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			out = append(out, p)
		}
	}

	return out
}
