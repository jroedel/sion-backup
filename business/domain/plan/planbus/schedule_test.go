package planbus_test

import (
	"testing"
	"time"

	"github.com/jroedel/sion-backup/business/domain/plan/planbus"
)

func at(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, time.UTC)
	if err != nil {
		panic(err)
	}

	return t
}

// noJitter keeps the arithmetic tests readable. Jitter has its own tests.
func noJitter() planbus.Schedule {
	return planbus.Schedule{Times: []string{"13:00"}, MinInterval: 6 * time.Hour}
}

func TestDueAtTheScheduledTime(t *testing.T) {
	s := noJitter()

	cases := []struct {
		name    string
		now     string
		lastRun string
		want    bool
	}{
		{"before the slot", "2026-09-09 12:59", "2026-09-08 13:00", false},
		{"at the slot", "2026-09-09 13:00", "2026-09-08 13:00", true},
		{"after the slot", "2026-09-09 17:30", "2026-09-08 13:00", true},
		{"already ran today", "2026-09-09 17:30", "2026-09-09 13:05", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := s.Due("node", at(c.now), at(c.lastRun)); got != c.want {
				t.Errorf("Due = %v, want %v", got, c.want)
			}
		})
	}
}

// TestTheLaptopCase is the reason this package does its own scheduling. The
// machine was shut in a bag over the 13:00 slot and opened at 16:40; cron
// would have skipped the day entirely.
func TestTheLaptopCase(t *testing.T) {
	s := noJitter()

	if !s.Due("node", at("2026-09-09 16:40"), at("2026-09-08 13:02")) {
		t.Error("a missed slot did not become due when the machine woke up")
	}
}

// TestAMachineThatHasNeverRunIsDueImmediately covers the install. Finding out
// that the credentials are wrong should happen while the administrator is
// still standing there.
func TestAMachineThatHasNeverRunIsDueImmediately(t *testing.T) {
	if !noJitter().Due("node", at("2026-09-09 09:00"), time.Time{}) {
		t.Error("a machine with no run history was not due")
	}
}

// TestMinIntervalStopsThrashing guards the case where a slot looks perpetually
// missed — several close times, or a clock that jumped backwards.
func TestMinIntervalStopsThrashing(t *testing.T) {
	s := planbus.Schedule{
		Times:       []string{"13:00", "13:05", "13:10"},
		MinInterval: 6 * time.Hour,
	}

	if s.Due("node", at("2026-09-09 13:11"), at("2026-09-09 13:01")) {
		t.Error("ran again ten minutes later; MinInterval did not hold")
	}
}

// TestAMachineOffForAMonthBacksUpOnReturn: the look-back window bounds the
// search, and must not make an old miss invisible.
func TestAMachineOffForAMonthBacksUpOnReturn(t *testing.T) {
	if !noJitter().Due("node", at("2026-09-09 15:00"), at("2026-08-01 13:00")) {
		t.Error("a machine back from a month away was not due")
	}
}

// TestJitterIsStableAcrossRestarts is the property that keeps a reboot at
// 13:05 from rolling a new offset and running a second backup.
func TestJitterIsStableAcrossRestarts(t *testing.T) {
	s := planbus.Schedule{Times: []string{"13:00"}, JitterMinutes: 30}

	first := s.Next("office-laptop-1", at("2026-09-09 09:00"))

	for range 10 {
		if again := s.Next("office-laptop-1", at("2026-09-09 09:00")); !again.Equal(first) {
			t.Fatalf("the jittered slot moved: %v then %v", first, again)
		}
	}

	if first.Before(at("2026-09-09 13:00")) || first.After(at("2026-09-09 13:30")) {
		t.Errorf("the jittered slot %v is outside 13:00–13:30", first)
	}
}

// TestJitterSpreadsTheFleet is the whole point of jitter: forty machines must
// not all connect to the bucket in the same minute.
func TestJitterSpreadsTheFleet(t *testing.T) {
	s := planbus.Schedule{Times: []string{"13:00"}, JitterMinutes: 30}

	minutes := map[int]int{}

	for _, node := range []string{
		"laptop-1", "laptop-2", "laptop-3", "office-pc", "reception", "laptop-7",
		"studio", "front-desk", "archive", "workshop",
	} {
		minutes[s.Next(node, at("2026-09-09 09:00")).Minute()]++
	}

	if len(minutes) < 5 {
		t.Errorf("ten machines landed in only %d distinct minutes: %v", len(minutes), minutes)
	}
}

func TestNextRollsToTomorrow(t *testing.T) {
	s := noJitter()

	got := s.Next("node", at("2026-09-09 18:00"))

	if want := at("2026-09-10 13:00"); !got.Equal(want) {
		t.Errorf("Next = %v, want %v", got, want)
	}
}

func TestNextPicksTheEarlierOfSeveralTimes(t *testing.T) {
	s := planbus.Schedule{Times: []string{"22:00", "07:00", "13:00"}}

	got := s.Next("node", at("2026-09-09 09:00"))

	if want := at("2026-09-09 13:00"); !got.Equal(want) {
		t.Errorf("Next = %v, want %v", got, want)
	}
}

// TestSpringForward: on the morning the clocks go forward, 02:30 does not
// exist. time.Date normalises it rather than producing an instant that never
// arrives, which would leave a machine on that schedule never backing up.
func TestSpringForward(t *testing.T) {
	tz, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no zoneinfo on this machine")
	}

	s := planbus.Schedule{Times: []string{"02:30"}}

	// 2026-03-08 is the US spring-forward date: 02:00 becomes 03:00.
	before := time.Date(2026, 3, 8, 0, 15, 0, 0, tz)

	next := s.Next("node", before)
	if next.IsZero() {
		t.Fatal("no slot at all on the day the clocks change")
	}

	if !next.After(before) || next.Sub(before) > 26*time.Hour {
		t.Errorf("the slot resolved to %v, which is not a sensible instant after %v", next, before)
	}
}

func TestValidate(t *testing.T) {
	if err := planbus.DefaultSchedule().Validate(); err != nil {
		t.Fatalf("the default schedule is invalid: %v", err)
	}

	for name, s := range map[string]planbus.Schedule{
		"no times":       {},
		"bad format":     {Times: []string{"1pm"}},
		"hour too big":   {Times: []string{"25:00"}},
		"minute too big": {Times: []string{"13:99"}},
		"negative jitter": {
			Times: []string{"13:00"}, JitterMinutes: -1,
		},
	} {
		if err := s.Validate(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
