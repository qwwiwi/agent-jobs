package jobs

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// Dead-man's switch (PLAN Wave B): a check is a named expectation, a cron expression in local time
// saying when something that stays silent while healthy must have reported, plus a grace. That
// something leaves a completion mark (check_mark, `agent-jobs check mark`).
//
// Mark ownership is a pure function of the mark time and the check's cron, grace and arm time A
// (creation, or the last change of the cron or the grace, or enabling): the expected times are the
// cron's instants E > A (a repeated DST minute once, a skipped one never), the window of E is
// (P+grace, E+grace] with P the previous expected time, or [A, E+grace] for the first one, and
// the windows partition the time from A on. owner(m) is the E whose window holds m, i.e. the
// earliest E > A with E >= m-grace; a mark before A has none. E is satisfied iff a stored mark has
// owner E. No consumption state: the result depends neither on insertion order nor on what was
// evaluated before or on downtime.
//
// In every daemon tick (lifecycle lock) the watchdog walks each enabled check's unevaluated expected
// times in chronological order, every E with E+grace+1 s <= now after the evaluation watermark (at
// most MaxCheckEvaluationsPerTick per check and tick; the rest the next tick), judges each from the
// marks and claims them in one transaction conditional on the check's revision (a check_set in
// between voids it) and the schedules' watermark rule (absolute time and local minute strictly
// later), so each E is judged once across restarts and daemons. Each missed E becomes its own
// pending alert row. A mark is pruned only once its owner is evaluated (or it predates A); a window
// stores at most store.MaxMarksPerWindow marks (later ones are accepted, not stored).
//
// Delivery runs after the tick released the lifecycle lock (deliverCheckAlerts): each due pending
// row is claimed (attempt counter, backoff, an attempt token) in one conditional statement; under
// tmux.lock the rows still holding the token are re-read right before typing (a paused stale
// attempt whose rows were claimed again or delivered meanwhile types nothing), the line is typed
// and the outcome stored before tmux.lock is released. A check's due rows go as one part of the line
// ("<name>: 3 missed, latest <E>"). A failure is retried with backoff (1, 2, 4, 8 minutes) per row
// for up to MaxCheckAlertAttempts attempts, then the row is failed; no notify target counts as a
// failed attempt. Failed rows stay in status and system_doctor until a check_set acknowledges them
// (a later healthy E does not). Disabling a check pauses its evaluation; alerts already pending are
// still delivered. Delivery is at-least-once: a crash after typing and before "delivered" was
// stored types it again at the next attempt.

// CheckMissedTag starts the watchdog's line in the session.
const CheckMissedTag = "[jobs:check-missed]"

// Grace bounds of a check, in minutes.
const (
	DefaultCheckGrace = 60
	MaxCheckGrace     = 7 * 24 * 60
)

// MaxCheckNameLen caps a check's name; MaxMarkNoteRunes a mark's note.
const (
	MaxCheckNameLen  = 64
	MaxMarkNoteRunes = 300
)

// checkLocalLayout is an expected time's local wall-clock minute as stored (the claim's watermark).
const checkLocalLayout = scheduleLocalLayout

// checkShownLayout is an expected time in the alert line and the views.
const checkShownLayout = "2006-01-02 15:04 MST"

var reCheckName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// CheckName validates a check's name: a slug of lower-case letters, digits, '-' and '_', starting
// with a letter or digit, at most MaxCheckNameLen bytes.
func CheckName(s string) (string, error) {
	if s == "" {
		return "", errors.New("name is required")
	}
	if len(s) > MaxCheckNameLen || !reCheckName.MatchString(s) {
		return "", fmt.Errorf("name %q must be a slug (a-z, 0-9, '-', '_'; starting with a letter or digit) of at most %d characters",
			RedactPrintable(s, 80), MaxCheckNameLen)
	}
	return s, nil
}

// MarkNote validates a mark's optional note: valid UTF-8, one line (no control or format character,
// bidi overrides included), at most MaxMarkNoteRunes, then redacted, Printable (RedactPrintable) and
// defanged (defangTags).
func MarkNote(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", errors.New("note is not valid UTF-8")
	}
	if containsControl(s) {
		return "", errors.New("note contains a control or format character (newline, tab, escape, bidi, …); a note is one line")
	}
	if n := utf8.RuneCountInString(s); n > MaxMarkNoteRunes {
		return "", fmt.Errorf("note has %d characters, at most %d", n, MaxMarkNoteRunes)
	}
	return defangTags(RedactPrintable(s, 0)), nil
}

func checkGrace(g int, def bool) (int, error) {
	if g == 0 && def {
		return DefaultCheckGrace, nil
	}
	if g < 1 || g > MaxCheckGrace {
		return 0, fmt.Errorf("grace_minutes %d is outside 1..%d", g, MaxCheckGrace)
	}
	return g, nil
}

// latestFire returns the latest due instant of c not after t (in t's location), false when there
// is none within cronHorizon years. Next is monotone in its argument, so a doubling search finds a
// point before the answer and a binary search narrows it to the minute: a bounded number of Next
// calls whatever the expression.
func latestFire(c *Cron, t time.Time) (time.Time, bool) {
	t = t.Truncate(time.Minute)
	var lo time.Time
	found := false
	for span := time.Hour; span <= cronHorizon*366*24*time.Hour; span *= 2 {
		x := t.Add(-span)
		n, err := c.Next(x)
		if err != nil {
			return time.Time{}, false
		}
		if !n.After(t) {
			lo, found = x, true
			break
		}
	}
	if !found {
		return time.Time{}, false
	}
	hi := t // Next(hi) > t: invariant Next(lo) <= t < Next(hi)
	for hi.Sub(lo) > time.Minute {
		mid := lo.Add(hi.Sub(lo) / 2).Truncate(time.Minute)
		n, err := c.Next(mid)
		if err != nil {
			return time.Time{}, false
		}
		if !n.After(t) {
			lo = mid
		} else {
			hi = mid
		}
	}
	n, err := c.Next(lo)
	if err != nil {
		return time.Time{}, false
	}
	return n, true
}

// secondPass reports whether instant e lies in the second pass of a wall-clock interval repeated
// when DST ends, with the transition instant (where the second pass starts) and the end of that
// pass. Every due instant there is a repeated minute: the same date and wall-clock minute was due
// in the first pass (cron matches the wall clock), so it is not an expected time of its own (the
// schedules' watermark). No cron search: the transition is found by bisecting the zone offset (at
// most one transition within 3 hours).
func secondPass(e time.Time) (start, end time.Time, ok bool) {
	lo, hi := e.Add(-3*time.Hour), e
	_, before := lo.Zone()
	_, after := hi.Zone()
	if before <= after {
		return time.Time{}, time.Time{}, false // no fall-back within the window
	}
	for hi.Sub(lo) > time.Second {
		mid := lo.Add(hi.Sub(lo) / 2)
		if _, off := mid.Zone(); off == before {
			lo = mid
		} else {
			hi = mid
		}
	}
	end = hi.Add(time.Duration(before-after) * time.Second)
	return hi, end, e.Before(end)
}

// latestExpected is the latest expected time of c not after t (a due instant that is not a repeated
// minute), false when there is none. A due instant in a second pass sends the search to just
// before the transition, so at most two latestFire calls are made.
func latestExpected(c *Cron, t time.Time) (time.Time, bool) {
	for i := 0; i < 3; i++ {
		e, ok := latestFire(c, t)
		if !ok {
			return time.Time{}, false
		}
		start, _, rep := secondPass(e)
		if !rep {
			return e, true
		}
		t = start.Add(-time.Second)
	}
	return time.Time{}, false
}

// nextExpected is the first expected time of c after t, false when there is none. A due instant
// in a second pass sends the search to the end of that pass.
func nextExpected(c *Cron, t time.Time) (time.Time, bool) {
	for i := 0; i < 3; i++ {
		n, err := c.Next(t)
		if err != nil {
			return time.Time{}, false
		}
		_, end, rep := secondPass(n)
		if !rep {
			return n, true
		}
		t = end.Add(-time.Minute) // Next is strictly after t's minute: the first due minute from end on
	}
	return time.Time{}, false
}

// AddCheck validates and stores a check, enabled and armed now (grace 0 = DefaultCheckGrace).
func (m *Manager) AddCheck(name, cron string, grace int) (*store.Check, error) {
	name, err := CheckName(name)
	if err != nil {
		return nil, err
	}
	if cron, err = m.parseScheduleCron(cron); err != nil {
		return nil, err
	}
	if grace, err = checkGrace(grace, true); err != nil {
		return nil, err
	}
	c, err := m.Store.AddCheck(name, cron, grace, m.Now())
	if errors.Is(err, store.ErrCheckExists) {
		return nil, fmt.Errorf("check %q already exists (check_set edits it)", name)
	}
	return c, err
}

// SetCheck edits a check in place: its cron, its grace, enabled, or several (at least one). A new
// cron or grace, or enabling it, re-arms the check: expected times before now never alert. Every
// edit bumps the check's revision, so a watchdog judgement read before it is not stored.
func (m *Manager) SetCheck(name, cron string, grace int, enabled *bool) (*store.Check, error) {
	name, err := CheckName(name)
	if err != nil {
		return nil, err
	}
	if cron == "" && grace == 0 && enabled == nil {
		return nil, errors.New("give a cron, grace_minutes, enabled or several of them")
	}
	if cron != "" {
		if cron, err = m.parseScheduleCron(cron); err != nil {
			return nil, err
		}
	}
	if grace != 0 {
		if grace, err = checkGrace(grace, false); err != nil {
			return nil, err
		}
	}
	c, err := m.Store.SetCheck(name, cron, grace, enabled, m.Now())
	if errors.Is(err, store.ErrCheckNotFound) {
		return nil, fmt.Errorf("check %q not found", name)
	}
	return c, err
}

// RemoveCheck deletes a check and its marks (only on the owner's instruction).
func (m *Manager) RemoveCheck(name string) error {
	name, err := CheckName(name)
	if err != nil {
		return err
	}
	err = m.Store.RemoveCheck(name)
	if errors.Is(err, store.ErrCheckNotFound) {
		return fmt.Errorf("check %q not found", name)
	}
	return err
}

// MarkCheck records a completion mark for the check now, with an optional one-line note; jobID is
// the job that marked it through the daemon's proxy ("" = the orchestrator or the CLI). A mark
// whose window already holds store.MaxMarksPerWindow marks is accepted but not stored
// (MarkResult "ok, already marked").
func (m *Manager) MarkCheck(name, note, jobID string) (*store.Check, error) {
	name, err := CheckName(name)
	if err != nil {
		return nil, err
	}
	if note, err = MarkNote(note); err != nil {
		return nil, err
	}
	at := m.Now()
	c, err := m.Store.AddMark(name, note, jobID, at, func(ck *store.Check) (store.MarkWindow, error) {
		s, err := m.specOf(ck)
		if err != nil {
			return store.MarkWindow{}, err
		}
		e, ok := s.owner(at)
		if !ok {
			return store.MarkWindow{}, nil
		}
		lo, hi := s.window(e)
		return store.MarkWindow{Lo: lo, Hi: hi, OK: true}, nil
	})
	if errors.Is(err, store.ErrCheckNotFound) {
		return nil, fmt.Errorf("check %q not found (check_add creates it)", name)
	}
	return c, err
}

// checkSpec is what mark ownership depends on: the cron, the grace and the arm time, in the
// manager's location.
type checkSpec struct {
	cron  *Cron
	grace time.Duration
	arm   time.Time
	loc   *time.Location
}

func (m *Manager) specOf(ck *store.Check) (checkSpec, error) {
	c, err := ParseCron(ck.Cron)
	if err != nil {
		return checkSpec{}, fmt.Errorf("check %s: %w", ck.Name, err)
	}
	loc := m.loc()
	return checkSpec{cron: c, grace: time.Duration(ck.GraceMinutes) * time.Minute, arm: ck.ArmedAt.In(loc), loc: loc}, nil
}

// firstExpectedFrom is the first expected time at or after t, false when there is none.
func firstExpectedFrom(c *Cron, t time.Time) (time.Time, bool) {
	up := t.Truncate(time.Minute)
	if up.Before(t) {
		up = up.Add(time.Minute)
	}
	return nextExpected(c, up.Add(-time.Minute)) // Next is strictly after its argument's minute
}

// armed reports whether e is an expected time of the current configuration: strictly after the
// arm time (an expected time at the arm instant would give the watched work no time at all).
func (s checkSpec) armed(e time.Time) bool { return e.After(s.arm) }

// owner is the expected time whose window holds a mark at m (second resolution, as stored): the
// earliest armed E >= m-grace; false for a mark before the arm time.
func (s checkSpec) owner(m time.Time) (time.Time, bool) {
	m = time.Unix(m.Unix(), 0).In(s.loc)
	if m.Before(s.arm) {
		return time.Time{}, false
	}
	from := m.Add(-s.grace)
	if !s.armed(from) {
		from = s.arm.Add(time.Second)
	}
	return firstExpectedFrom(s.cron, from)
}

// window is the owner window of expected time e, [lo, hi] in seconds: (P+grace, e+grace] with P
// the previous expected time, or [arm, e+grace] when e is the first one since the arm time.
func (s checkSpec) window(e time.Time) (lo, hi time.Time) {
	hi = e.Add(s.grace)
	if p, ok := latestExpected(s.cron, e.Add(-time.Minute)); ok && s.armed(p) {
		return p.Add(s.grace + time.Second), hi
	}
	return s.arm, hi
}

// Check states in a CheckView.
const (
	CheckOK       = "ok"
	CheckMissed   = "missed"
	CheckWaiting  = "waiting"
	CheckDisabled = "disabled"
)

// MaxCheckAlertAttempts bounds the delivery attempts of one missed alert row.
const MaxCheckAlertAttempts = 5

// MaxCheckEvaluationsPerTick bounds the expected times one tick judges per check (the rest wait
// for the next tick).
const MaxCheckEvaluationsPerTick = 1000

// checkAlertBackoff is the wait after delivery attempt n (1-based) before the next one may start.
func checkAlertBackoff(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	return time.Minute << (n - 1)
}

// CheckView is a check with its expected times and its state now.
type CheckView struct {
	*store.Check
	// State is about the latest expected time whose grace has passed: ok (a mark satisfied it) or
	// missed (none did, a mark after its grace included: see LateMarkAt); waiting (no expected time
	// since the check was armed has passed its grace yet) or disabled. A time the watchdog judged
	// keeps its stored outcome; one it has not judged yet (no daemon) is evaluated from the marks.
	State string `json:"state"`
	// LastExpectedAt is the expected time State is about (local time), empty when waiting.
	LastExpectedAt string `json:"last_expected_at,omitempty"`
	// LateMarkAt is a mark after the missed time's grace (the work reported late); the time stays
	// missed.
	LateMarkAt *time.Time `json:"late_mark_at,omitempty"`
	// NextExpectedAt is the next expected time (local time), empty when disabled.
	NextExpectedAt string `json:"next_expected_at,omitempty"`

	expected time.Time
	loc      *time.Location
}

// dueExpected is the latest expected time of the check whose grace has passed at now, false when
// none since it was armed. One second past E+grace: a mark stored in the same second as E+grace
// (unix seconds) is already in the store when E is looked at, so an alert never contradicts the
// window.
func dueExpected(s checkSpec, now time.Time) (time.Time, bool) {
	e, ok := latestExpected(s.cron, now.Add(-s.grace-time.Second))
	if !ok || !s.armed(e) {
		return time.Time{}, false
	}
	return e, true
}

// unevaluated are the expected times of the check after its evaluation watermark (or from the arm
// time, when the watermark belongs to an earlier configuration) whose grace has passed at now, in
// chronological order, at most limit.
func unevaluated(ck *store.Check, s checkSpec, now time.Time, limit int) []time.Time {
	until := now.Add(-s.grace - time.Second)
	var e time.Time
	var ok bool
	if ck.EvaluatedAt != nil && s.armed(*ck.EvaluatedAt) {
		e, ok = nextExpected(s.cron, ck.EvaluatedAt.In(s.loc))
	} else {
		e, ok = firstExpectedFrom(s.cron, s.arm.Add(time.Second))
	}
	var out []time.Time
	for ok && !e.After(until) && len(out) < limit {
		out = append(out, e)
		e, ok = nextExpected(s.cron, e)
	}
	return out
}

// hasMarkIn reports whether a time of the ascending marks lies in [lo, hi].
func hasMarkIn(marks []time.Time, lo, hi time.Time) bool {
	i := sort.Search(len(marks), func(i int) bool { return !marks[i].Before(lo) })
	return i < len(marks) && !marks[i].After(hi)
}

// CheckViewOf is the view of a check now.
func (m *Manager) CheckViewOf(ck *store.Check) (CheckView, error) {
	v := CheckView{Check: ck, State: CheckDisabled, loc: m.loc()}
	s, err := m.specOf(ck)
	if err != nil {
		return v, err
	}
	now := m.Now().In(m.loc())
	if !ck.Enabled {
		return v, nil
	}
	if n, ok := nextExpected(s.cron, now); ok {
		v.NextExpectedAt = n.Format(time.RFC3339)
	}
	e, ok := dueExpected(s, now)
	if !ok {
		v.State = CheckWaiting
		return v, nil
	}
	missed := false
	if ck.EvaluatedAt != nil && ck.EvaluatedAt.Equal(e) {
		missed = ck.EvaluatedResult == store.EvalMissed
	} else {
		lo, hi := s.window(e)
		marks, err := m.Store.MarkTimes(ck.ID, lo, hi)
		if err != nil {
			return v, err
		}
		missed = len(marks) == 0
	}
	v.expected = e
	v.LastExpectedAt = e.Format(time.RFC3339)
	v.State = CheckOK
	if missed {
		v.State = CheckMissed
		if until := e.Add(s.grace); ck.LastMarkAt != nil && ck.LastMarkAt.After(until) {
			late := ck.LastMarkAt.In(m.loc())
			v.LateMarkAt = &late
		}
	}
	return v, nil
}

// CheckViews returns every check with its state, oldest first.
func (m *Manager) CheckViews() ([]CheckView, error) {
	list, err := m.Store.ListChecks(false)
	if err != nil {
		return nil, err
	}
	out := make([]CheckView, 0, len(list))
	var errs []error
	for _, ck := range list {
		v, err := m.CheckViewOf(ck)
		if err != nil {
			errs = append(errs, err)
		}
		out = append(out, v)
	}
	return out, errors.Join(errs...)
}

// MissedChecks are the enabled checks whose latest expected time passed its grace without a mark
// (a late mark keeps them here), and any check with a missed alert still pending delivery or
// failed and not acknowledged by a check_set.
func (m *Manager) MissedChecks() ([]CheckView, error) {
	all, err := m.CheckViews()
	out := []CheckView{}
	for _, v := range all {
		if v.State == CheckMissed || v.AlertsPending > 0 || v.AlertsFailed > 0 {
			out = append(out, v)
		}
	}
	return out, err
}

// CheckStateText is a view's state as status shows it: "missed (late mark at …)" for a missed time
// that was reported after its grace.
func CheckStateText(v CheckView) string {
	if v.State == CheckMissed && v.LateMarkAt != nil {
		return fmt.Sprintf("%s (late mark at %s)", CheckMissed, v.LateMarkAt.Format(checkShownLayout))
	}
	return v.State
}

// CheckViewText is one line of status and system_doctor for a missed check (MissedChecks): the
// state, the expected time, the delivery of the newest missed alert and the other pending or
// failed ones.
func CheckViewText(v CheckView) string {
	var b strings.Builder
	b.WriteString(v.Name + " " + CheckStateText(v))
	loc := v.loc
	if loc == nil {
		loc = time.Local
	}
	if v.State == CheckMissed {
		fmt.Fprintf(&b, ": expected %s, no mark within %dm", v.expected.In(loc).Format(checkShownLayout), v.GraceMinutes)
	}
	if v.LastMissedAt != nil && (v.State != CheckMissed || !v.LastMissedAt.Equal(v.expected)) {
		fmt.Fprintf(&b, "; missed %s", v.LastMissedAt.In(loc).Format(checkShownLayout))
	}
	if v.AlertMerged > 0 {
		fmt.Fprintf(&b, " (with %d earlier missed times)", v.AlertMerged)
	}
	pending, failed := v.AlertsPending, v.AlertsFailed
	switch v.AlertState {
	case store.AlertDelivered:
		b.WriteString("; alert delivered")
	case store.AlertPending:
		fmt.Fprintf(&b, "; alert pending (%d of %d attempts made)", v.AlertAttempts, MaxCheckAlertAttempts)
		pending -= 1 + v.AlertMerged
	case store.AlertFailed:
		fmt.Fprintf(&b, "; alert not delivered after %d attempts", v.AlertAttempts)
		if v.AlertAcknowledged {
			b.WriteString(" (acknowledged)")
		} else {
			failed -= 1 + v.AlertMerged
		}
	}
	if v.LastAlertError != "" && v.AlertState != store.AlertDelivered {
		b.WriteString(": " + Printable(v.LastAlertError, 120))
	}
	if pending > 0 {
		fmt.Fprintf(&b, "; %d earlier alerts pending", pending)
	}
	if failed > 0 {
		fmt.Fprintf(&b, "; %d earlier alerts not delivered after %d attempts", failed, MaxCheckAlertAttempts)
	}
	if v.AlertsFailed > 0 {
		b.WriteString(" (check_set acknowledges failed alerts)")
	}
	return b.String()
}

// missedItem is one check's part of the alert line: count missed expected times, the latest one.
type missedItem struct {
	name     string
	expected time.Time
	grace    int
	count    int
}

// CheckMissedText is how one missed expected time reads in the alert line.
func CheckMissedText(name string, expected time.Time, grace int) string {
	return fmt.Sprintf("%s expected %s (no mark within %dm)", name, expected.Format(checkShownLayout), grace)
}

func checkMissedLine(items []missedItem) string {
	parts := make([]string, 0, len(items))
	for _, it := range items {
		if it.count > 1 {
			parts = append(parts, fmt.Sprintf("%s: %d missed, latest %s (no mark within %dm)",
				it.name, it.count, it.expected.Format(checkShownLayout), it.grace))
			continue
		}
		parts = append(parts, CheckMissedText(it.name, it.expected, it.grace))
	}
	return CheckMissedTag + " " + strings.Join(parts, "; ")
}

// missedItems groups alert rows (ordered by check) into one item per check.
func (m *Manager) missedItems(rows []*store.CheckAlert) []missedItem {
	var out []missedItem
	for _, a := range rows {
		e := a.Expected.In(m.loc())
		if n := len(out); n > 0 && out[n-1].name == a.Name {
			out[n-1].count += 1 + a.Merged
			if e.After(out[n-1].expected) {
				out[n-1].expected = e
			}
			continue
		}
		out = append(out, missedItem{name: a.Name, expected: e, grace: a.Grace, count: 1 + a.Merged})
	}
	return out
}

// watchChecks is the watchdog's judging half, run in every daemon tick under the lifecycle lock:
// each enabled check's unevaluated expected times are judged and claimed (judgeCheck). A missed one
// becomes a pending alert row; deliverCheckAlerts types it, outside the lifecycle lock.
func (m *Manager) watchChecks() error {
	list, err := m.Store.ListChecks(true)
	if err != nil || len(list) == 0 {
		return err
	}
	now := m.Now().In(m.loc())
	var errs []error
	for _, ck := range list {
		if _, err := m.judgeCheck(ck, now); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// judgeCheck judges ck's unevaluated expected times (as read, at its revision) at now, in
// chronological order, and claims the judgements; true when this call stored them.
func (m *Manager) judgeCheck(ck *store.Check, now time.Time) (bool, error) {
	s, err := m.specOf(ck)
	if err != nil {
		m.Log.Printf("%v", err)
		return false, nil
	}
	es := unevaluated(ck, s, now, MaxCheckEvaluationsPerTick)
	if len(es) == 0 {
		return false, nil
	}
	lo, _ := s.window(es[0])
	marks, err := m.Store.MarkTimes(ck.ID, lo, es[len(es)-1].Add(s.grace))
	if err != nil {
		return false, err
	}
	outs := make([]store.EvalOutcome, len(es))
	var missed []string
	for i, e := range es {
		if i > 0 { // consecutive expected times: the previous window ends where this one starts
			lo = es[i-1].Add(s.grace + time.Second)
		}
		ok := hasMarkIn(marks, lo, e.Add(s.grace))
		outs[i] = store.EvalOutcome{Expected: e, Local: e.Format(checkLocalLayout), OK: ok}
		if !ok {
			missed = append(missed, e.Format(checkShownLayout))
		}
	}
	claimed, err := m.Store.ClaimCheckEvaluations(ck.ID, ck.Revision, outs)
	if err != nil || !claimed {
		return false, err // !claimed: judged already, or edited since it was read
	}
	if len(missed) > 0 {
		m.Log.Printf("check watchdog: %s missed %d expected times (latest %s, no mark within %dm), alert pending",
			ck.Name, len(missed), missed[len(missed)-1], ck.GraceMinutes)
	}
	return true, nil
}

// deliverCheckAlerts types the pending alert rows whose next attempt is due as one line. It must
// run without the lifecycle lock: claimCheckAlerts takes the attempts under a fresh token,
// sendCheckAlerts types the rows still holding it under tmux.lock.
func (m *Manager) deliverCheckAlerts() error {
	token, err := m.claimCheckAlerts()
	if token == "" {
		return err
	}
	return errors.Join(err, m.sendCheckAlerts(token))
}

// claimCheckAlerts claims the next attempt of every due pending alert row under one new token ("" =
// none claimed). A row that already made MaxCheckAlertAttempts attempts (the last one cut short
// before its outcome was stored) is failed instead.
func (m *Manager) claimCheckAlerts() (string, error) {
	now := m.Now()
	due, err := m.Store.DueCheckAlerts(now)
	if err != nil || len(due) == 0 {
		return "", err
	}
	token := newToken()
	claimed := 0
	var errs []error
	for _, a := range due {
		if a.Attempts >= MaxCheckAlertAttempts {
			errText := a.Error
			if errText == "" {
				errText = "delivery interrupted"
			}
			if err := m.Store.FailCheckAlert(a.ID, a.Attempts, errText); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		ok, err := m.Store.ClaimCheckAlertAttempt(a.ID, a.Attempts, token, now, now.Add(checkAlertBackoff(a.Attempts+1)))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if ok {
			claimed++
		}
	}
	if claimed == 0 {
		return "", errors.Join(errs...)
	}
	return token, errors.Join(errs...)
}

// sendCheckAlerts types the alert rows claimed under token. The fence: under tmux.lock the rows
// still pending with this token are re-read right before typing, and the outcome is stored before
// tmux.lock is released, so an attempt paused after its claim (whose rows were claimed again, or
// delivered, meanwhile) types nothing. A failed attempt keeps the error on the rows it still holds
// (failed after MaxCheckAlertAttempts); one that could not take tmux.lock typed nothing and counts
// as failed the same way.
func (m *Manager) sendCheckAlerts(token string) error {
	target := m.Config().NotifyTmuxTarget
	if target == "" {
		ferr := "notify off (notify_tmux_target not configured)"
		if rows, err := m.Store.TokenCheckAlerts(token); err == nil && len(rows) > 0 {
			m.Log.Printf("check watchdog: %s: %s", ferr, checkMissedLine(m.missedItems(rows)))
		}
		return m.Store.FailCheckAlertAttempt(token, ferr, MaxCheckAlertAttempts)
	}
	done := false
	err := m.withTmuxLock(func(ctx context.Context) error {
		rows, err := m.Store.TokenCheckAlerts(token)
		if err != nil {
			return err
		}
		done = true
		if len(rows) == 0 {
			m.Log.Printf("check watchdog: a stale delivery attempt typed nothing (its alerts were claimed again or delivered)")
			return nil
		}
		line := checkMissedLine(m.missedItems(rows))
		if serr := m.sendTmux(ctx, target, line); serr != nil {
			m.Log.Printf("check watchdog: alert not delivered: %v: %s", serr, line)
			return m.Store.FailCheckAlertAttempt(token, serr.Error(), MaxCheckAlertAttempts)
		}
		m.Log.Printf("check watchdog: %s", line)
		ids := make([]int64, len(rows))
		for i, a := range rows {
			ids[i] = a.ID
		}
		return m.Store.DeliverCheckAlerts(ids)
	})
	if done {
		return err
	}
	m.Log.Printf("check watchdog: alert not delivered: %v", err)
	return m.Store.FailCheckAlertAttempt(token, err.Error(), MaxCheckAlertAttempts)
}
