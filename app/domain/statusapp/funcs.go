package statusapp

import (
	"fmt"
	"html/template"
	"strings"
	"time"

	"github.com/jroedel/sion-backup/business/domain/backup/backupbus"
)

// funcs are the template helpers.
//
// Formatting lives here rather than in the templates because a template is a
// bad place for a conditional and a worse place for arithmetic, and because
// these are the parts worth testing: "3 days ago" and "1.2 GiB" are what
// somebody actually reads off the page.
var funcs = template.FuncMap{
	"bytes":    humanBytes,
	"ago":      func(t time.Time) string { return humanDuration(time.Since(t)) },
	"duration": humanDuration,
	"clock":    clock,
	"day":      day,
	"pill":     pill,
	"percent":  func(f float64) string { return fmt.Sprintf("%.0f%%", f*100) },
	"rate":     humanRate,
	"join":     strings.Join,
	"nonzero":  func(t time.Time) bool { return !t.IsZero() },
}

// humanBytes renders a byte count the way a person reads one.
func humanBytes(n int64) string {
	const unit = 1024

	if n < unit {
		return fmt.Sprintf("%d B", n)
	}

	div, exp := int64(unit), 0

	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}

	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// humanDuration renders an elapsed time at one significant unit.
//
// Deliberately coarse. "2 days" is what somebody wants to know; "2 days, 4
// hours and 11 minutes" is the same fact, harder to read, and implies a
// precision that the answer to "when did this last work" does not have.
func humanDuration(d time.Duration) string {
	switch {
	case d < 0:
		return "in the future"
	case d < time.Minute:
		return "less than a minute"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute")
	case d < 48*time.Hour:
		return plural(int(d.Hours()), "hour")
	default:
		return plural(int(d.Hours()/24), "day")
	}
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}

	return fmt.Sprintf("%d %ss", n, unit)
}

// clock renders a time of day in the machine's own zone, which is the zone the
// person reading it is standing in.
func clock(t time.Time) string {
	if t.IsZero() {
		return "—"
	}

	return t.Local().Format("15:04")
}

// day renders a date and time for the history table.
func day(t time.Time) string {
	if t.IsZero() {
		return "—"
	}

	return t.Local().Format("Mon 2 Jan 15:04")
}

// pill maps an outcome to the colour class it is shown in.
func pill(o backupbus.Outcome) string {
	switch o {
	case backupbus.OutcomeSuccess:
		return "good"
	case backupbus.OutcomeDegraded, backupbus.OutcomeIncomplete:
		return "warn"
	default:
		return "bad"
	}
}

// humanRate renders an upload speed the way somebody's internet contract does.
//
// Megabits per second, not mebibytes. Every connection anybody has ever been
// sold is quoted in megabits — "50 down, 10 up" — and a page that answers a
// question about that connection in MiB/s is asking the reader to do a
// division before they can tell whether the number is plausible. The bytes per
// second are the honest unit for the arithmetic and the wrong one for the
// sentence, so the arithmetic keeps them and this converts once, here.
func humanRate(bytesPerSecond float64) string {
	if bytesPerSecond <= 0 {
		return "unknown"
	}

	mbit := bytesPerSecond * 8 / 1_000_000

	if mbit < 10 {
		return fmt.Sprintf("%.1f Mbit/s", mbit)
	}

	return fmt.Sprintf("%.0f Mbit/s", mbit)
}
