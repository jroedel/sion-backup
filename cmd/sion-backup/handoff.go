package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// The last thing enrolment does, and the reason this file exists at all.
//
// Enrolment used to end by printing the restore card and stopping, leaving the
// person who ran it to remember four more steps: start the service, open the
// status page, choose what to back up, take the first run. Two of those got
// done at the desk and two of them got done later or not at all — which is how
// a machine ends up enrolled, green on the dashboard, and backing up an empty
// list of folders somebody meant to fill in.
//
// So the command now finishes the job: it starts the service, waits for the
// page to answer, and opens it. What it deliberately does NOT do is start a
// backup. See planbus.Plan.ConfirmedAt.

// serviceStartTimeout is how long to wait for the status page to answer after
// the service has been asked to start.
//
// Fifteen seconds. Starting the daemon means opening a database and applying
// migrations, and on a machine with a year of run history and a slow disk that
// is not instant. Giving up early would print "could not start it" about a
// service that was three seconds from being up.
const serviceStartTimeout = 15 * time.Second

// handoff starts the daemon, opens the setup page, and says what is left.
//
// Everything in it is best effort and nothing in it can fail the command that
// calls it: the machine is enrolled by the time this runs, the token is
// written, and a browser that would not open is not a reason to report that
// enrolment did not work. What it cannot do it says, with the command to run
// by hand.
func handoff(ctx context.Context, d *deps, start, open bool) {
	url := d.statusPage()

	fmt.Printf("\nwhat happens now\n")
	fmt.Printf("  NOTHING is backed up yet, on purpose. This computer is enrolled and\n")
	fmt.Printf("  ready, and it waits for somebody using it to choose what should be in\n")
	fmt.Printf("  the backup — which is this page:\n\n")
	fmt.Printf("      %s/setup\n\n", url)
	fmt.Printf("  It offers the usual choices with the folders measured, times the\n")
	fmt.Printf("  first backup against this machine's own bucket, and starts it.\n")

	switch {
	case !start:
		fmt.Printf("\n  The service was not started (--no-start). Start it, then open the\n")
		fmt.Printf("  page above:\n\n      %s\n", startCommand())

		return

	case elevatedBySudo():
		// The daemon is a per-user service and its data directory is per-user.
		// Starting it from a root shell would either fail or start the wrong
		// one, and opening a browser as root on somebody's desktop is worse
		// than not opening one.
		fmt.Printf("\n  This command is running with sudo, so the service was not started\n")
		fmt.Printf("  from here: it runs as the person whose files are backed up, not as\n")
		fmt.Printf("  root. As that account:\n\n      %s\n\n", startCommand())
		fmt.Printf("  and then open the page above.\n")

		return
	}

	switch alive(ctx, url) {
	case true:
		fmt.Printf("\n  The service is already running.\n")

	default:
		fmt.Printf("\n  Starting the service.\n")

		if err := startService(ctx); err != nil {
			fmt.Printf("  It could not be started from here (%v). Start it by hand:\n\n      %s\n",
				err, startCommand())

			return
		}

		if !waitFor(ctx, url) {
			fmt.Printf("  It was started, but the status page has not answered yet. Give it\n")
			fmt.Printf("  a moment and open the page above.\n")

			return
		}

		fmt.Printf("  It is up.\n")
	}

	if !open {
		return
	}

	if err := openBrowser(url + "/setup"); err != nil {
		// Not worth a paragraph: the URL is printed above and this is a
		// convenience. A headless machine reached over SSH lands here every
		// time, and it is not a fault.
		d.log.Debug("could not open a browser", "err", err)

		return
	}

	fmt.Printf("  Opened it in your browser.\n")
}

// alive reports whether a status page is already answering on this address.
func alive(ctx context.Context, url string) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/healthz", nil)
	if err != nil {
		return false
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	return resp.StatusCode == http.StatusOK
}

// waitFor polls until the status page answers or the timeout runs out.
func waitFor(ctx context.Context, url string) bool {
	deadline := time.Now().Add(serviceStartTimeout)

	for time.Now().Before(deadline) {
		if alive(ctx, url) {
			return true
		}

		select {
		case <-ctx.Done():
			return false
		case <-time.After(500 * time.Millisecond):
		}
	}

	return false
}

// startService asks this platform's service manager to start the daemon.
//
// The three commands are the ones the three installers in deploy/ register,
// and they are written here a second time, which is a duplication worth
// naming: the installers are shell and PowerShell and cannot be imported. If a
// service name changes in one of them, this prints a command that does
// nothing, and what the person sees is the fallback below — the wrong command,
// printed clearly. That is the failure mode to accept rather than to add a
// fourth place that defines the names.
func startService(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, serviceStartTimeout)
	defer cancel()

	name, args := startArgs()

	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		if detail := strings.TrimSpace(string(out)); detail != "" {
			return fmt.Errorf("%s: %s", err, firstLine(detail))
		}

		return err
	}

	return nil
}

func startArgs() (string, []string) {
	switch runtime.GOOS {
	case "darwin":
		return "launchctl", []string{"kickstart", "-k",
			fmt.Sprintf("gui/%d/us.schoenstatt.sion-backup", os.Getuid())}

	case "windows":
		return "schtasks", []string{"/Run", "/TN", "sion-backup"}

	default:
		return "systemctl", []string{"--user", "start", "sion-backup"}
	}
}

// startCommand is the same thing as a line somebody can paste.
func startCommand() string {
	name, args := startArgs()

	return name + " " + strings.Join(args, " ")
}

// openBrowser opens a URL in whatever this platform uses.
func openBrowser(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()

	case "windows":
		// Through the shell handler rather than by name: there is no
		// "open" on Windows, and this is the documented way to reach
		// whatever the user has set as their browser.
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()

	default:
		// A machine reached over SSH has no display, and xdg-open there
		// either fails or, worse, opens a text browser in the terminal the
		// operator is using. Neither is wanted, and the URL is printed anyway.
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return fmt.Errorf("there is no graphical session here")
		}

		return exec.Command("xdg-open", url).Start()
	}
}

// firstLine keeps the part of a service manager's complaint that fits in a
// sentence.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}

	return s
}
