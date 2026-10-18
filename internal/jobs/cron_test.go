package jobs

import (
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // the DST tests need Europe/Berlin on any host
)

func berlin(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// Fields, ranges, steps, lists, names, macros and Sunday as 0 or 7.
func TestCronMatchFields(t *testing.T) {
	loc := time.UTC
	// 2026-10-19 is a Monday.
	mon := func(h, m int) time.Time { return time.Date(2026, 10, 19, h, m, 0, 0, loc) }
	for _, c := range []struct {
		expr string
		t    time.Time
		want bool
	}{
		{"* * * * *", mon(3, 7), true},
		{"7 3 * * *", mon(3, 7), true},
		{"7 3 * * *", mon(3, 8), false},
		{"0-10 * * * *", mon(1, 10), true},
		{"0-10 * * * *", mon(1, 11), false},
		{"*/15 * * * *", mon(1, 45), true},
		{"*/15 * * * *", mon(1, 46), false},
		{"5-50/15 * * * *", mon(1, 35), true}, // 5, 20, 35, 50
		{"5-50/15 * * * *", mon(1, 30), false},
		{"10/20 * * * *", mon(1, 50), true}, // 10, 30, 50
		{"1,2,40 * * * *", mon(1, 40), true},
		{"1,2,40 * * * *", mon(1, 41), false},
		{"0 9-17/2 * * *", mon(13, 0), true},
		{"0 9-17/2 * * *", mon(14, 0), false},
		{"0 9 * * mon-fri", mon(9, 0), true},
		{"0 9 * * SAT,sun", mon(9, 0), false},
		{"0 9 * oct *", mon(9, 0), true},
		{"0 9 * jan-sep *", mon(9, 0), false},
		{"0 9 19 * *", mon(9, 0), true},
		{"0 0 * * 7", time.Date(2026, 10, 18, 0, 0, 0, 0, loc), true}, // Sunday as 7
		{"0 0 * * 0", time.Date(2026, 10, 18, 0, 0, 0, 0, loc), true},
		{"0 0 * * 5-7", time.Date(2026, 10, 18, 0, 0, 0, 0, loc), true},
		{"@hourly", mon(5, 0), true},
		{"@daily", mon(0, 0), true},
		{"@daily", mon(0, 1), false},
		{"@weekly", time.Date(2026, 10, 18, 0, 0, 0, 0, loc), true},
		{"@monthly", time.Date(2026, 11, 1, 0, 0, 0, 0, loc), true},
		{"@yearly", time.Date(2027, 1, 1, 0, 0, 0, 0, loc), true},
	} {
		cr, err := ParseCron(c.expr)
		if err != nil {
			t.Fatalf("%q: %v", c.expr, err)
		}
		if got := cr.Match(c.t); got != c.want {
			t.Fatalf("%q at %s: got %t, want %t", c.expr, c.t, got, c.want)
		}
	}
}

// The DOM/DOW rule: both restricted = either matches; one is "*" = both must match.
func TestCronDayOfMonthDayOfWeekRule(t *testing.T) {
	friday13 := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	monday1 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	tuesday2 := time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		expr string
		t    time.Time
		want bool
	}{
		{"0 0 1 * mon", monday1, true},   // both
		{"0 0 1 * fri", monday1, true},   // day of month only: either
		{"0 0 13 * mon", friday13, true}, // day of month only
		{"0 0 2 * fri", friday13, true},  // weekday only
		{"0 0 1 * fri", tuesday2, false}, // neither
		{"0 0 * * mon", tuesday2, false}, // DOM "*": weekday must match
		{"0 0 1 * *", tuesday2, false},   // DOW "*": day must match
		{"0 0 */2 * mon", monday1, true}, // "*/2" counts as unrestricted: both must match (1 is odd: on)
		{"0 0 */2 * mon", tuesday2, false},
	}
	for _, c := range cases {
		cr, err := ParseCron(c.expr)
		if err != nil {
			t.Fatal(err)
		}
		if got := cr.Match(c.t); got != c.want {
			t.Fatalf("%q at %s: got %t, want %t", c.expr, c.t.Format("Mon 2006-01-02"), got, c.want)
		}
	}
}

func TestCronParseErrors(t *testing.T) {
	for _, c := range []struct{ expr, want string }{
		{"", "required"},
		{"* * * *", "5 fields"},
		{"* * * * * *", "5 fields"},
		{"60 * * * *", "0..59"},
		{"* 24 * * *", "0..23"},
		{"* * 0 * *", "1..31"},
		{"* * * 13 *", "1..12"},
		{"* * * * 8", "0..7"},
		{"5-1 * * * *", "backwards"},
		{"*/0 * * * *", "step"},
		{"*/x * * * *", "step"},
		{"1,,2 * * * *", "empty list item"},
		{"a * * * *", "0..59"},
		{"* * * foo *", "1..12"},
		{"@reboot", "unknown cron macro"},
		{"*\t* * * *\n", "control character"},
		{strings.Repeat("1,", 200) + "1 * * * *", "too long"},
	} {
		if _, err := ParseCron(c.expr); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%q: error %v, want %q", c.expr, err, c.want)
		}
	}
}

func TestCronNext(t *testing.T) {
	loc := time.UTC
	from := time.Date(2026, 10, 19, 10, 15, 30, 0, loc)
	for _, c := range []struct {
		expr string
		want time.Time
	}{
		{"* * * * *", time.Date(2026, 10, 19, 10, 16, 0, 0, loc)},
		{"15 10 * * *", time.Date(2026, 10, 20, 10, 15, 0, 0, loc)}, // strictly after
		{"0 9 * * mon", time.Date(2026, 10, 26, 9, 0, 0, 0, loc)},
		{"0 0 29 2 *", time.Date(2028, 2, 29, 0, 0, 0, 0, loc)},
	} {
		cr, err := ParseCron(c.expr)
		if err != nil {
			t.Fatal(err)
		}
		if got := cr.Next(from); !got.Equal(c.want) {
			t.Fatalf("%q: next %s, want %s", c.expr, got, c.want)
		}
	}
	never, err := ParseCron("0 0 30 2 *")
	if err != nil {
		t.Fatal(err)
	}
	if n := never.Next(from); !n.IsZero() {
		t.Fatalf("30 February fires at %s", n)
	}
}

// DST in local time: a wall-clock minute skipped by spring-forward is not due that day; the hour
// repeated at fall-back reads the same wall clock twice (the firing logic keeps a fixed time to one
// firing, see TestScheduleDSTFallBackFiresOnce).
func TestCronDST(t *testing.T) {
	loc := berlin(t)
	cr, err := ParseCron("30 2 * * *")
	if err != nil {
		t.Fatal(err)
	}
	// 2026-03-29 02:00 CET jumps to 03:00 CEST: 02:30 does not exist that day.
	if n := cr.Next(time.Date(2026, 3, 28, 12, 0, 0, 0, loc)); !n.Equal(time.Date(2026, 3, 30, 2, 30, 0, 0, loc)) {
		t.Fatalf("spring forward: next %s", n)
	}
	for m := time.Date(2026, 3, 29, 0, 0, 0, 0, loc); m.Before(time.Date(2026, 3, 29, 6, 0, 0, 0, loc)); m = m.Add(time.Minute) {
		if cr.Match(m) {
			t.Fatalf("a skipped minute matched: %s", m)
		}
	}
	// 2026-10-25 03:00 CEST falls back to 02:00 CET: 02:30 is read twice, an hour apart.
	first := time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC).In(loc)
	second := first.Add(time.Hour)
	if !cr.Match(first) || !cr.Match(second) || first.Format("15:04") != second.Format("15:04") {
		t.Fatalf("fall back: %s %s", first, second)
	}
}
