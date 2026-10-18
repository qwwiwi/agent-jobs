package jobs

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// Dead-man's switch (PLAN Wave B): a check is a named expectation, a cron expression in local time
// saying when something that stays silent while healthy must have reported, plus a grace. That
// something leaves a completion mark (check_mark, `agent-jobs check mark`). In every daemon tick
// the watchdog takes each enabled check's latest expected time E whose E+grace has passed and
// judges it once: E is ok when a mark lies in its window (P, E+grace], P the previous expected
// time (unbounded for the first one; no mark predates the check), and missed otherwise. Windows of
// consecutive expected times overlap (E1's grace lies in E2's window), so each mark satisfies at
// most one E: the earliest mark in the window that did not already satisfy an earlier E counts,
// and the mark that satisfied E is the floor for every later one. The judgement is the evaluation
// watermark stored on the check (E, its local minute, ok or missed, the counted mark), claimed in
// one statement conditional on the check's revision (a check_set in between voids it) and on the
// schedules' watermark rule (absolute time and local wall-clock minute strictly later), so an E is
// judged once across restarts and daemons and never re-judged from marks: marks are pruned only
// below the watermark. Only the latest E is looked at, so a long downtime gives at most one alert,
// and expected times before the check was armed are never judged. Expected times follow the
// schedules' DST rule: a skipped wall-clock minute is not expected, a repeated one is expected once
// (its first instant).
//
// A missed E becomes a pending alert. Delivery runs after the tick released the lifecycle lock,
// under tmux.lock only (sendTmuxLocked, bounded by TmuxDeliveryTimeout): each attempt is claimed
// first (attempt counter and next-attempt time in one statement), then the line is typed, then the
// outcome is stored. A failure is retried with backoff (1, 2, 4, 8 minutes) for up to
// MaxCheckAlertAttempts attempts, then the alert is failed. Delivery is at-least-once: a crash
// after the line was typed and before "delivered" was stored types it again at the next attempt.

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
			Printable(s, 80), MaxCheckNameLen)
	}
	return s, nil
}

// MarkNote validates a mark's optional note: valid UTF-8, one line (no control or format character,
// bidi overrides included), at most MaxMarkNoteRunes, then Printable and defanged (defangTags).
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
	return defangTags(Printable(s, 0)), nil
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
// the job that marked it through the daemon's proxy ("" = the orchestrator or the CLI).
func (m *Manager) MarkCheck(name, note, jobID string) (*store.Check, error) {
	name, err := CheckName(name)
	if err != nil {
		return nil, err
	}
	if note, err = MarkNote(note); err != nil {
		return nil, err
	}
	c, err := m.Store.AddMark(name, note, jobID, m.Now())
	if errors.Is(err, store.ErrCheckNotFound) {
		return nil, fmt.Errorf("check %q not found (check_add creates it)", name)
	}
	return c, err
}

// Check states in a CheckView.
const (
	CheckOK       = "ok"
	CheckMissed   = "missed"
	CheckWaiting  = "waiting"
	CheckDisabled = "disabled"
)

// MaxCheckAlertAttempts bounds the delivery attempts of one missed alert.
const MaxCheckAlertAttempts = 5

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

// evaluation is a check's latest expected time whose grace has passed (ok=false when none since
// it was armed) and the mark that satisfies it (nil = missed).
type evaluation struct {
	expected time.Time
	ok       bool
	mark     *store.MarkRef
}

// dueExpected is the latest expected time of the check whose grace has passed at now, false when
// none since it was armed. One second past E+grace: a mark stored in the same second as E+grace
// (unix seconds) is already in the store when E is looked at, so an alert never contradicts the
// window.
func dueExpected(ck *store.Check, c *Cron, now time.Time) (time.Time, bool) {
	grace := time.Duration(ck.GraceMinutes) * time.Minute
	e, ok := latestExpected(c, now.Add(-grace-time.Second))
	if !ok || e.Before(ck.ArmedAt) {
		return time.Time{}, false
	}
	return e, true
}

// evaluateCheck judges the latest due expected time E from the marks: the earliest mark in
// (P, E+grace] above the mark counted for the last time judged ok.
func (m *Manager) evaluateCheck(ck *store.Check, c *Cron, now time.Time) (evaluation, error) {
	e, ok := dueExpected(ck, c, now)
	if !ok {
		return evaluation{}, nil
	}
	var from time.Time // unbounded below when there is no P
	if p, ok := latestExpected(c, e.Add(-time.Minute)); ok {
		from = p
	}
	mark, err := m.Store.FirstMark(ck.ID, ck.CountedMarkID, from, e.Add(time.Duration(ck.GraceMinutes)*time.Minute))
	if err != nil {
		return evaluation{}, err
	}
	return evaluation{expected: e, ok: true, mark: mark}, nil
}

// CheckViewOf is the view of a check now.
func (m *Manager) CheckViewOf(ck *store.Check) (CheckView, error) {
	v := CheckView{Check: ck, State: CheckDisabled, loc: m.loc()}
	c, err := ParseCron(ck.Cron)
	if err != nil {
		return v, fmt.Errorf("check %s: %w", ck.Name, err)
	}
	now := m.Now().In(m.loc())
	if !ck.Enabled {
		return v, nil
	}
	if n, ok := nextExpected(c, now); ok {
		v.NextExpectedAt = n.Format(time.RFC3339)
	}
	e, ok := dueExpected(ck, c, now)
	if !ok {
		v.State = CheckWaiting
		return v, nil
	}
	missed := false
	if ck.EvaluatedAt != nil && ck.EvaluatedAt.Equal(e) {
		missed = ck.EvaluatedResult == store.EvalMissed
	} else {
		ev, err := m.evaluateCheck(ck, c, now)
		if err != nil {
			return v, err
		}
		missed = ev.mark == nil
	}
	v.expected = e
	v.LastExpectedAt = e.Format(time.RFC3339)
	v.State = CheckOK
	if missed {
		v.State = CheckMissed
		if until := e.Add(time.Duration(ck.GraceMinutes) * time.Minute); ck.LastMarkAt != nil && ck.LastMarkAt.After(until) {
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
// (a late mark keeps them here), and any check whose missed alert is still pending delivery.
func (m *Manager) MissedChecks() ([]CheckView, error) {
	all, err := m.CheckViews()
	out := []CheckView{}
	for _, v := range all {
		if v.State == CheckMissed || v.AlertState == store.AlertPending {
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
// state, the expected time and the delivery of the last missed alert.
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
	switch v.AlertState {
	case store.AlertDelivered:
		b.WriteString("; alert delivered")
	case store.AlertPending:
		fmt.Fprintf(&b, "; alert pending (%d of %d attempts made)", v.AlertAttempts, MaxCheckAlertAttempts)
	case store.AlertFailed:
		fmt.Fprintf(&b, "; alert not delivered after %d attempts", v.AlertAttempts)
	}
	if v.LastAlertError != "" && v.AlertState != store.AlertDelivered {
		b.WriteString(": " + Printable(v.LastAlertError, 120))
	}
	return b.String()
}

// missedItem is one missed expected time in the alert line.
type missedItem struct {
	name     string
	expected time.Time
	grace    int
}

// CheckMissedText is how one missed expected time reads in the alert line.
func CheckMissedText(name string, expected time.Time, grace int) string {
	return fmt.Sprintf("%s expected %s (no mark within %dm)", name, expected.Format(checkShownLayout), grace)
}

func checkMissedLine(items []missedItem) string {
	parts := make([]string, 0, len(items))
	for _, it := range items {
		parts = append(parts, CheckMissedText(it.name, it.expected, it.grace))
	}
	return CheckMissedTag + " " + strings.Join(parts, "; ")
}

// watchChecks is the watchdog's judging half, run in every daemon tick under the lifecycle lock:
// each enabled check's latest due expected time not judged yet is judged and claimed (judgeCheck).
// A missed one becomes a pending alert; deliverCheckAlerts types it, outside the lifecycle lock.
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

// judgeCheck judges ck (as read, at its revision) at now and claims the judgement; true when this
// call stored a new judgement.
func (m *Manager) judgeCheck(ck *store.Check, now time.Time) (bool, error) {
	c, err := ParseCron(ck.Cron)
	if err != nil {
		m.Log.Printf("check %s: %v", ck.Name, err)
		return false, nil
	}
	e, ok := dueExpected(ck, c, now)
	if !ok || (ck.EvaluatedAt != nil && !e.After(*ck.EvaluatedAt)) {
		return false, nil
	}
	ev, err := m.evaluateCheck(ck, c, now)
	if err != nil || !ev.ok {
		return false, err
	}
	claimed, err := m.Store.ClaimCheckEvaluation(ck.ID, ck.Revision, ev.expected, ev.expected.Format(checkLocalLayout), ev.mark)
	if err != nil || !claimed {
		return false, err // !claimed: judged already, or edited since it was read
	}
	if ev.mark == nil {
		m.Log.Printf("check watchdog: %s missed, alert pending", CheckMissedText(ck.Name, ev.expected, ck.GraceMinutes))
	}
	return true, nil
}

// deliverCheckAlerts types the pending missed alerts whose next attempt is due as one line through
// the bounded delivery (sendTmuxLocked: tmux.lock, TmuxDeliveryTimeout). It must run without the
// lifecycle lock. Each attempt is claimed before the line is typed, so two daemons never make the
// same attempt; a failure stays pending until MaxCheckAlertAttempts, then the alert is failed. An
// attempt cut short (a crash after its claim) is retried after its backoff; one cut short after
// the line was typed types it again (at-least-once).
func (m *Manager) deliverCheckAlerts() error {
	now := m.Now().In(m.loc())
	list, err := m.Store.PendingCheckAlerts(now)
	if err != nil || len(list) == 0 {
		return err
	}
	var items []missedItem
	var taken []*store.Check
	var errs []error
	for _, ck := range list {
		if ck.LastMissedAt == nil {
			continue
		}
		if ck.AlertAttempts >= MaxCheckAlertAttempts {
			// The last attempt was cut short before its outcome was stored.
			errText := ck.LastAlertError
			if errText == "" {
				errText = "delivery interrupted"
			}
			if err := m.Store.RecordCheckAlert(ck.ID, *ck.LastMissedAt, errText, true); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		n := ck.AlertAttempts + 1
		ok, err := m.Store.ClaimCheckAlertAttempt(ck.ID, *ck.LastMissedAt, ck.AlertAttempts, now.Add(checkAlertBackoff(n)))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !ok {
			continue // another daemon took this attempt, or a new missed time replaced it
		}
		ck.AlertAttempts = n
		items = append(items, missedItem{name: ck.Name, expected: ck.LastMissedAt.In(m.loc()), grace: ck.GraceMinutes})
		taken = append(taken, ck)
	}
	if len(items) == 0 {
		return errors.Join(errs...)
	}
	line, ferr := checkMissedLine(items), ""
	if t := m.Cfg.NotifyTmuxTarget; t == "" {
		ferr = "notify off (notify_tmux_target not configured)"
		m.Log.Printf("check watchdog: %s: %s", ferr, line)
	} else if err := m.sendTmuxLocked(t, line); err != nil {
		ferr = err.Error()
		m.Log.Printf("check watchdog: alert not delivered: %v: %s", err, line)
	} else {
		m.Log.Printf("check watchdog: %s", line)
	}
	for _, ck := range taken {
		final := ferr != "" && ck.AlertAttempts >= MaxCheckAlertAttempts
		if err := m.Store.RecordCheckAlert(ck.ID, *ck.LastMissedAt, ferr, final); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
