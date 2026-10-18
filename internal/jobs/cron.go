package jobs

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// A small parser for standard 5-field cron expressions, evaluated in local time (no dependency:
// the syntax is small and the matching rule must be exactly the documented one).
//
//	minute hour day-of-month month day-of-week
//
// Each field is "*" or a comma list of items; an item is a value, a range a-b, or either of them
// (or "*") with a step /n ("a/n" means a to the field's maximum). Months and weekdays also take
// three-letter English names (jan…dec, sun…sat); weekday 0 and 7 are both Sunday. The macros
// @hourly, @daily (@midnight), @weekly, @monthly and @yearly (@annually) are accepted.
// Day-of-month and day-of-week follow the classic rule: when both are restricted (neither starts
// with "*"), a day matches if either matches; otherwise both must match.

// Cron is a parsed cron expression.
type Cron struct {
	minute, hour, dom, month, dow uint64 // bit i set = value i matches
	domStar, dowStar              bool   // the field starts with "*" (unrestricted for the DOM/DOW rule)
}

type cronField struct {
	name     string
	min, max int
	names    []string // names[i] is value min+i
}

var cronFields = [5]cronField{
	{name: "minute", min: 0, max: 59},
	{name: "hour", min: 0, max: 23},
	{name: "day-of-month", min: 1, max: 31},
	{name: "month", min: 1, max: 12, names: []string{"jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"}},
	{name: "day-of-week", min: 0, max: 7, names: []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}},
}

var cronMacros = map[string]string{
	"@hourly":   "0 * * * *",
	"@daily":    "0 0 * * *",
	"@midnight": "0 0 * * *",
	"@weekly":   "0 0 * * 0",
	"@monthly":  "0 0 1 * *",
	"@yearly":   "0 0 1 1 *",
	"@annually": "0 0 1 1 *",
}

// MaxCronLen caps the length of a cron expression.
const MaxCronLen = 200

// ParseCron parses a 5-field cron expression or a macro.
func ParseCron(expr string) (*Cron, error) {
	s := strings.TrimSpace(expr)
	if s == "" {
		return nil, errors.New("cron expression is required")
	}
	if len(s) > MaxCronLen || containsControl(s) {
		return nil, errors.New("cron expression is too long or contains a control character")
	}
	if strings.HasPrefix(s, "@") {
		m, ok := cronMacros[strings.ToLower(s)]
		if !ok {
			return nil, fmt.Errorf("unknown cron macro %q (want @hourly, @daily, @weekly, @monthly or @yearly)", s)
		}
		s = m
	}
	f := strings.Fields(s)
	if len(f) != 5 {
		return nil, fmt.Errorf("cron expression %q must have 5 fields (minute hour day-of-month month day-of-week), got %d", s, len(f))
	}
	var c Cron
	bits := [5]*uint64{&c.minute, &c.hour, &c.dom, &c.month, &c.dow}
	for i, spec := range f {
		b, err := parseCronField(spec, cronFields[i])
		if err != nil {
			return nil, err
		}
		*bits[i] = b
	}
	if c.dow&(1<<7) != 0 { // 7 is Sunday too
		c.dow = c.dow&^(1<<7) | 1
	}
	c.domStar, c.dowStar = strings.HasPrefix(f[2], "*"), strings.HasPrefix(f[4], "*")
	return &c, nil
}

func parseCronField(spec string, f cronField) (uint64, error) {
	var bits uint64
	for _, item := range strings.Split(spec, ",") {
		b, err := parseCronItem(item, f)
		if err != nil {
			return 0, fmt.Errorf("cron %s field %q: %w", f.name, spec, err)
		}
		bits |= b
	}
	return bits, nil
}

func parseCronItem(item string, f cronField) (uint64, error) {
	if item == "" {
		return 0, errors.New("empty list item")
	}
	rng, stepS, hasStep := strings.Cut(item, "/")
	step := 1
	if hasStep {
		n, err := strconv.Atoi(stepS)
		if err != nil || n < 1 || n > f.max-f.min+1 {
			return 0, fmt.Errorf("step %q must be a number 1..%d", stepS, f.max-f.min+1)
		}
		step = n
	}
	var lo, hi int
	switch {
	case rng == "*":
		lo, hi = f.min, f.max
		if f.name == "day-of-week" {
			hi = 6 // "*" is every day once; 7 only as an explicit value
		}
	case strings.Contains(rng, "-"):
		a, b, _ := strings.Cut(rng, "-")
		var err error
		if lo, err = cronValue(a, f); err != nil {
			return 0, err
		}
		if hi, err = cronValue(b, f); err != nil {
			return 0, err
		}
		if lo > hi {
			return 0, fmt.Errorf("range %q runs backwards", rng)
		}
	default:
		v, err := cronValue(rng, f)
		if err != nil {
			return 0, err
		}
		lo, hi = v, v
		if hasStep { // "a/n" = a to the maximum, every n
			hi = f.max
		}
	}
	var bits uint64
	for v := lo; v <= hi; v += step {
		bits |= 1 << v
	}
	return bits, nil
}

func cronValue(s string, f cronField) (int, error) {
	for i, n := range f.names {
		if strings.EqualFold(s, n) {
			return f.min + i, nil
		}
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < f.min || v > f.max {
		return 0, fmt.Errorf("value %q must be %d..%d", s, f.min, f.max)
	}
	return v, nil
}

// Match reports whether the minute of t (its own location's wall clock) is due.
func (c *Cron) Match(t time.Time) bool {
	return c.minute&(1<<t.Minute()) != 0 && c.hour&(1<<t.Hour()) != 0 && c.dayMatch(t)
}

func (c *Cron) dayMatch(t time.Time) bool {
	if c.month&(1<<int(t.Month())) == 0 {
		return false
	}
	dom, dow := c.dom&(1<<t.Day()) != 0, c.dow&(1<<int(t.Weekday())) != 0
	if !c.domStar && !c.dowStar {
		return dom || dow
	}
	return dom && dow
}

// cronHorizon bounds Next: the longest gap a valid expression can have is a leap day on a given
// weekday (28 years); 30 years covers it.
const cronHorizon = 30

// Next returns the first due minute strictly after t, in t's location, or the zero time when the
// expression never fires (e.g. "0 0 30 2 *"). A wall-clock minute that does not exist on that day
// (skipped by a DST change) is never due.
func (c *Cron) Next(t time.Time) time.Time {
	loc := t.Location()
	t = t.Truncate(time.Minute).Add(time.Minute)
	y, mo, d := t.Date()
	end := time.Date(y+cronHorizon, mo, d, 0, 0, 0, 0, loc)
	for day := time.Date(y, mo, d, 0, 0, 0, 0, loc); day.Before(end); day = time.Date(day.Year(), day.Month(), day.Day()+1, 0, 0, 0, 0, loc) {
		if !c.dayMatch(day) {
			continue
		}
		for h := 0; h < 24; h++ {
			if c.hour&(1<<h) == 0 {
				continue
			}
			for mi := 0; mi < 60; mi++ {
				if c.minute&(1<<mi) == 0 {
					continue
				}
				cand := time.Date(day.Year(), day.Month(), day.Day(), h, mi, 0, 0, loc)
				if cand.Hour() != h || cand.Minute() != mi || cand.Before(t) {
					continue // a skipped wall-clock minute (DST), or already past
				}
				return cand
			}
		}
	}
	return time.Time{}
}
