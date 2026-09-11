package planbus

import (
	"errors"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"time"
)

// Schedule is when a machine should back up.
//
// # Why not cron
//
// cron, systemd timers and Task Scheduler all answer "run at 13:00" and none
// of them answers the question this fleet actually has, which is: the laptop
// was shut in a bag at 13:00, so when does it back up?
//
// A cron job on a machine that was asleep simply does not run. systemd's
// Persistent=true and Task Scheduler's "run as soon as possible after a missed
// start" both exist precisely because that is wrong, and both are configured
// differently on each of the three platforms in this fleet. Owning the
// scheduling here means one behaviour, described in one place, testable
// without a machine that sleeps.
//
// # What it does
//
// A run is due when a scheduled time has passed that the machine has not yet
// backed up for. That single sentence covers the ordinary case (13:00 arrives,
// it runs) and the laptop case (the 13:00 slot passed while it was asleep; it
// opens at 16:40 and runs then), with no separate catch-up mechanism to get
// wrong.
type Schedule struct {
	// Times are the local times of day to run at, as "HH:MM". Usually one.
	//
	// Local, not UTC, and that is a decision rather than a convenience: these
	// exist to run while a machine is switched on and not while somebody is
	// trying to use it, and both of those are facts about the wall clock in
	// front of the person. A fleet spread over two time zones running at 13:00
	// local is doing the right thing in both.
	Times []string

	// JitterMinutes spreads the fleet out. Forty machines starting at exactly
	// 13:00 is forty simultaneous connections to one bucket, which the
	// provider rate-limits and which makes every machine's backup slower than
	// it needed to be.
	//
	// The jitter is derived from the node ID and the date rather than drawn at
	// random, so it survives a restart: a machine that reboots at 13:05 must
	// not roll a new offset and run a second time.
	JitterMinutes int

	// MinInterval is the floor between two runs, whatever the times say.
	//
	// It is the guard against a configuration that would thrash — several
	// times close together, or a clock that jumps backwards over a daylight
	// saving boundary and makes an already-run slot look due again.
	MinInterval time.Duration
}

// DefaultSchedule is what a machine gets when nobody says otherwise.
//
// 13:00 comes from the legacy fleet, and it is a better choice than it looks:
// a work computer is switched on, awake and on a network in the middle of the
// working day, which is exactly when an overnight schedule fails. The cost is
// that the backup competes with the person using the machine, and restic's
// read concurrency is the dial for that.
func DefaultSchedule() Schedule {
	return Schedule{
		Times:         []string{"13:00"},
		JitterMinutes: 30,
		MinInterval:   6 * time.Hour,
	}
}

// Validate reports a schedule that would never run, or would run constantly.
func (s Schedule) Validate() error {
	if len(s.Times) == 0 {
		return errors.New("planbus: the schedule has no times, so nothing would ever run")
	}

	for _, t := range s.Times {
		if _, _, err := parseTimeOfDay(t); err != nil {
			return err
		}
	}

	if s.JitterMinutes < 0 {
		return errors.New("planbus: jitter cannot be negative")
	}

	if s.MinInterval < 0 {
		return errors.New("planbus: the minimum interval cannot be negative")
	}

	return nil
}

// parseTimeOfDay reads "HH:MM".
func parseTimeOfDay(s string) (hour, minute int, err error) {
	h, m, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok {
		return 0, 0, fmt.Errorf("planbus: %q is not a time of day; write it as HH:MM", s)
	}

	hour, err = strconv.Atoi(h)
	if err != nil || hour < 0 || hour > 23 {
		return 0, 0, fmt.Errorf("planbus: %q has no valid hour", s)
	}

	minute, err = strconv.Atoi(m)
	if err != nil || minute < 0 || minute > 59 {
		return 0, 0, fmt.Errorf("planbus: %q has no valid minute", s)
	}

	return hour, minute, nil
}

// jitter is the deterministic offset for one node on one slot.
//
// FNV-1a over the node ID and the slot's calendar date and time. Two machines
// get different offsets; one machine gets the same offset however many times
// it asks, including after a reboot.
func (s Schedule) jitter(nodeID string, slot time.Time) time.Duration {
	if s.JitterMinutes <= 0 {
		return 0
	}

	h := fnv.New64a()
	fmt.Fprintf(h, "%s|%s", nodeID, slot.Format("2006-01-02T15:04"))

	return time.Duration(h.Sum64()%uint64(s.JitterMinutes+1)) * time.Minute
}

// slotsOn returns the jittered run times for one calendar day, in order.
func (s Schedule) slotsOn(nodeID string, day time.Time) []time.Time {
	out := make([]time.Time, 0, len(s.Times))

	for _, t := range s.Times {
		hour, minute, err := parseTimeOfDay(t)
		if err != nil {
			// A stored schedule was validated before it was stored. One that
			// is unparseable here means the row was edited underneath us, and
			// skipping the entry is better than panicking inside a daemon.
			continue
		}

		// time.Date normalises, which is what handles a daylight saving jump:
		// a 02:30 slot on the morning the clocks go forward resolves to a real
		// instant rather than an impossible one.
		slot := time.Date(day.Year(), day.Month(), day.Day(), hour, minute, 0, 0, day.Location())

		out = append(out, slot.Add(s.jitter(nodeID, slot)))
	}

	// Jitter can reorder same-day slots that were listed in order.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Before(out[j-1]); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}

	return out
}

// lookBackDays bounds the search for a missed slot.
//
// A machine that has been off for a month does not need to find the slot it
// missed four weeks ago — it needs to back up now, which the first missed slot
// it finds already achieves. The bound exists so the loop terminates on a
// machine whose clock is wrong by years.
const lookBackDays = 3

// Due reports whether a backup should start now.
//
// lastRun is when this machine last completed a run — zero if it never has, in
// which case it is due immediately. That is deliberate: the administrator is
// standing at the machine when it is installed, and the worst moment to
// discover the credentials are wrong is the first night after they have left.
func (s Schedule) Due(nodeID string, now, lastRun time.Time) bool {
	if lastRun.IsZero() {
		return true
	}

	if s.MinInterval > 0 && now.Sub(lastRun) < s.MinInterval {
		return false
	}

	slot := s.lastSlotAtOrBefore(nodeID, now)
	if slot.IsZero() {
		return false
	}

	return lastRun.Before(slot)
}

// lastSlotAtOrBefore finds the most recent scheduled instant not in the future.
func (s Schedule) lastSlotAtOrBefore(nodeID string, now time.Time) time.Time {
	for back := range lookBackDays + 1 {
		day := now.AddDate(0, 0, -back)

		slots := s.slotsOn(nodeID, day)

		for i := len(slots) - 1; i >= 0; i-- {
			if !slots[i].After(now) {
				return slots[i]
			}
		}
	}

	return time.Time{}
}

// Next is the next instant a run would start, for the status page.
//
// It answers "when will this happen next" and not "when is it due", so it
// ignores MinInterval and a missed slot: the honest answer to somebody looking
// at a machine that missed yesterday is that a run is due now, which the page
// gets from Due.
func (s Schedule) Next(nodeID string, after time.Time) time.Time {
	// Today and tomorrow are enough: a schedule with at least one time always
	// has a slot within 24 hours, and Validate refuses one with none.
	for ahead := range 2 {
		for _, slot := range s.slotsOn(nodeID, after.AddDate(0, 0, ahead)) {
			if slot.After(after) {
				return slot
			}
		}
	}

	return time.Time{}
}

// # The four shapes a schedule comes in
//
// Everything below is about one question a person is asked once, on the setup
// page: how often. The answer is stored as times, jitter and a floor, because
// that is what the scheduler above reads — but a person does not think in
// "13:00, 30 minutes of jitter, a six hour floor", they think "once a day,
// after lunch". These are the translation, in both directions.
//
// The presets are values rather than a stored enum, deliberately. The plan
// holds times; [Schedule.Preset] reads them back to decide which radio button
// to fill in. So a schedule assembled by hand in config.toml, or one edited on
// the settings page to something none of these produce, is not a broken preset
// — it is Custom, and the page says so and leaves it alone.

// Preset is one of the answers the setup page offers to "how often".
type Preset string

const (
	// PresetHourly backs up at the top of every hour.
	PresetHourly Preset = "hourly"

	// PresetThriceDaily is morning, lunchtime and evening.
	PresetThriceDaily Preset = "thrice-daily"

	// PresetDaily is once a day at a time the person chooses.
	PresetDaily Preset = "daily"

	// PresetCustom is a list of times that is none of the above. It is not
	// offered as a choice; it is what [Schedule.Preset] answers about a
	// schedule somebody has written themselves.
	PresetCustom Preset = "custom"
)

// Hourly backs up at the top of every hour.
//
// The jitter is ten minutes rather than thirty, and the floor thirty minutes
// rather than six hours, and both have to move together with the times: a
// six-hour floor would silently turn this into four runs a day, and half an
// hour of jitter on an hourly schedule would let two adjacent slots overlap.
//
// It is the right answer for a machine holding work that is expensive to redo
// — an hour of lost editing rather than a day — and the wrong one for a laptop
// on a phone tether, which is what [Plan.SkipOnMetered] is for.
func Hourly() Schedule {
	times := make([]string, 0, 24)
	for h := range 24 {
		times = append(times, fmt.Sprintf("%02d:00", h))
	}

	return Schedule{Times: times, JitterMinutes: 10, MinInterval: 30 * time.Minute}
}

// ThriceDaily is morning, lunchtime and evening.
//
// 09:00 and 13:00 are inside the working day for the reason DefaultSchedule
// gives: a work computer is switched on, awake and on a network then, which is
// exactly when an overnight schedule fails. 21:00 is the one that catches the
// afternoon's work on a machine that is shut at five — and if it is shut, the
// slot is not lost, it runs when the machine is next opened.
func ThriceDaily() Schedule {
	return Schedule{
		Times:         []string{"09:00", "13:00", "21:00"},
		JitterMinutes: 20,
		MinInterval:   2 * time.Hour,
	}
}

// DailyAt is once a day, at the time somebody chose.
//
// The time is validated by the returned schedule's own Validate, not here, so
// that a bad one is reported by the same path as every other bad schedule
// rather than by a second one that says something different.
func DailyAt(hhmm string) Schedule {
	s := DefaultSchedule()
	s.Times = []string{strings.TrimSpace(hhmm)}

	return s
}

// Preset reports which of the answers above produced this schedule.
//
// Read back out of the times rather than stored beside them, so that there is
// one source of truth about when a machine runs. A schedule edited on the
// settings page to something no preset produces answers PresetCustom, which is
// how the setup page knows to show it as it is rather than quietly rounding it
// to the nearest button.
func (s Schedule) Preset() Preset {
	switch len(s.Times) {
	case 1:
		return PresetDaily

	case 3:
		if sameTimes(s.Times, ThriceDaily().Times) {
			return PresetThriceDaily
		}

	case 24:
		if sameTimes(s.Times, Hourly().Times) {
			return PresetHourly
		}
	}

	return PresetCustom
}

// DailyTime is the single time a once-a-day schedule runs at, for the box on
// the setup page. Empty for any other shape.
func (s Schedule) DailyTime() string {
	if len(s.Times) != 1 {
		return ""
	}

	return s.Times[0]
}

// sameTimes compares two lists of times of day for equality, ignoring order
// and surrounding space.
//
// Ignoring order because the plan's times survive a round trip through a text
// box, and "13:00, 09:00, 21:00" is the same schedule as the one this program
// wrote — reporting it as Custom would show somebody a form that had forgotten
// what they chose last week.
func sameTimes(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	seen := make(map[string]int, len(b))
	for _, t := range b {
		seen[strings.TrimSpace(t)]++
	}

	for _, t := range a {
		key := strings.TrimSpace(t)

		if seen[key] == 0 {
			return false
		}

		seen[key]--
	}

	return true
}
