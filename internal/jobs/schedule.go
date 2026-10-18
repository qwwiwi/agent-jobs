package jobs

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// Schedules (PLAN Wave B): a cron expression in local time plus a prompt, checked every minute in
// the daemon tick. Without a kind the prompt is typed into the live session as one tagged line
// (several schedules due in the same minute arrive as one line); with a kind it starts a job with
// the model and limits stored at creation, and the session only sees that job's job-done line.
// A firing is claimed in the store before delivery: a missed minute (daemon down, a failed
// delivery) is lost, never queued. Every firing must be later than the last one both in absolute
// time and on the local wall clock (a watermark), so nothing fires twice, restarts and the hour
// repeated when DST ends included.

// MaxSchedulePromptRunes caps a schedule's prompt (longer is refused, never cut: it is an instruction).
const MaxSchedulePromptRunes = 2000

// ScheduleTag starts every schedule line in the session.
const ScheduleTag = "[jobs:schedule]"

// scheduleBatchIntro opens a line that carries several schedules due in the same minute.
const scheduleBatchIntro = "these are different instructions, do each one:"

// ScheduleRequest is the input of schedule_add. Kind "" delivers the prompt into the session; with
// a kind the prompt is the brief of a job started with the job fields (validated like job_start).
type ScheduleRequest struct {
	Cron            string
	Prompt          string
	Kind            string
	Title           string
	Model           string
	Complexity      string
	MaxTurns        int
	MaxBudgetUSD    float64
	TimeoutMinutes  int
	Repo            string
	Base            string
	Runner          string
	ReasoningEffort string
	// Skills are skill edits (as StartRequest.Skills), stored and applied at each firing.
	Skills []string
}

// SchedulePrompt validates a schedule's prompt: required, valid UTF-8, one line (no control or
// format character, bidi overrides included), at most MaxSchedulePromptRunes, then Printable and
// defanged so it never reads as a tag of its own (defangTags, the notify_owner rule).
func SchedulePrompt(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", errors.New("prompt is not valid UTF-8")
	}
	if containsControl(s) {
		return "", errors.New("prompt contains a control or format character (newline, tab, escape, bidi, …); a prompt is one line")
	}
	if n := utf8.RuneCountInString(s); n > MaxSchedulePromptRunes {
		return "", fmt.Errorf("prompt has %d characters, at most %d", n, MaxSchedulePromptRunes)
	}
	t := defangTags(Printable(s, 0))
	if t == "" {
		return "", errors.New("prompt is required")
	}
	return t, nil
}

func (m *Manager) loc() *time.Location {
	if m.Loc != nil {
		return m.Loc
	}
	return time.Local
}

// parseScheduleCron parses the cron expression and refuses one that never fires.
func (m *Manager) parseScheduleCron(expr string) (string, error) {
	c, err := ParseCron(expr)
	if err != nil {
		return "", err
	}
	if _, err := c.Next(m.Now().In(m.loc())); err != nil {
		return "", fmt.Errorf("cron expression %q never fires (%v)", strings.TrimSpace(expr), err)
	}
	return strings.Join(strings.Fields(expr), " "), nil
}

// AddSchedule validates and stores a schedule, enabled. A job schedule is validated like job_start
// (codex rules included) and stores the resolved model, runner, effort and limits; a session
// schedule needs notify_tmux_target and takes no job fields.
func (m *Manager) AddSchedule(r ScheduleRequest) (*store.Schedule, error) {
	cron, err := m.parseScheduleCron(r.Cron)
	if err != nil {
		return nil, err
	}
	prompt, err := SchedulePrompt(r.Prompt)
	if err != nil {
		return nil, err
	}
	sc := &store.Schedule{Cron: cron, Prompt: prompt, Enabled: true}
	if r.Kind == "" {
		if r.Title != "" || r.Model != "" || r.Complexity != "" || r.MaxTurns != 0 || r.MaxBudgetUSD != 0 ||
			r.TimeoutMinutes != 0 || r.Repo != "" || r.Base != "" || r.Runner != "" || r.ReasoningEffort != "" || len(r.Skills) > 0 {
			return nil, errors.New("job fields (title, model, complexity, limits, repo, base, runner, reasoning_effort, skills) " +
				"need a kind; without a kind the prompt goes into the session")
		}
		if m.Cfg.NotifyTmuxTarget == "" {
			return nil, errors.New("a session schedule needs notify_tmux_target in config.json " +
				"(there is no session to deliver to); give a kind to run it as a job")
		}
		return m.Store.AddSchedule(sc, m.Now())
	}
	// Without a title the stored title is neutral ("schedule <id>", set by the store at insert), never
	// derived from the prompt: titles are shown where prompts are not (the HTTP API).
	title := "schedule"
	if r.Title != "" {
		if !utf8.ValidString(r.Title) || containsControl(r.Title) {
			return nil, errors.New("title contains a control or format character, or is not valid UTF-8")
		}
		title = Printable(r.Title, MaxTitleRunes)
	}
	j, err := m.newJob(StartRequest{
		Title: title, Kind: r.Kind, Brief: prompt, RepoPath: r.Repo, Base: r.Base, Model: r.Model,
		Complexity: r.Complexity, MaxTurns: r.MaxTurns, MaxBudgetUSD: r.MaxBudgetUSD, TimeoutMinutes: r.TimeoutMinutes,
		Runner: r.Runner, ReasoningEffort: r.ReasoningEffort, Skills: r.Skills,
	})
	if err != nil {
		return nil, err
	}
	for _, e := range r.Skills {
		if e = strings.TrimSpace(e); e != "" {
			if strings.Contains(e, ",") {
				return nil, fmt.Errorf("skill edit %q: one name per entry", e)
			}
			sc.Skills = append(sc.Skills, e)
		}
	}
	if r.Title == "" {
		j.Title = ""
	}
	sc.Kind, sc.Title, sc.Model, sc.Complexity, sc.Runner, sc.ReasoningEffort = j.Kind, j.Title, j.Model, j.Complexity, j.Runner, j.ReasoningEffort
	sc.MaxTurns, sc.MaxBudgetUSD, sc.TimeoutMinutes, sc.RepoPath = j.MaxTurns, j.MaxBudgetUSD, j.TimeoutMinutes, j.RepoPath
	if j.RepoPath != "" {
		sc.Base = strings.TrimSpace(r.Base) // a ref, resolved again at each firing ("" = HEAD)
	}
	return m.Store.AddSchedule(sc, m.Now())
}

// legacyScheduleTitle is the prefix of the title a schedule without one got before titles turned
// neutral: "schedule: " and the first 60 characters of the prompt.
const legacyScheduleTitle = "schedule: "

// NeutralScheduleTitle is the title of a schedule (and of the jobs it fires) created without one.
func NeutralScheduleTitle(id string) string { return "schedule " + id }

// ScheduleTitle is a schedule's title as jobs and the HTTP API get it: the neutral title for one
// created without a title, including an older row whose stored title is the prompt prefix.
func ScheduleTitle(sc *store.Schedule) string {
	if sc.Title == "" || strings.HasPrefix(sc.Title, legacyScheduleTitle) {
		return NeutralScheduleTitle(strconv.FormatInt(sc.ID, 10))
	}
	return sc.Title
}

// JobTitle is a job's title as the HTTP API shows it: the neutral schedule title for a job fired by
// a schedule whose title came from its prompt (older rows), else the job's own title.
func JobTitle(j *store.Job) string {
	if j.ScheduleID != "" && strings.HasPrefix(j.Title, legacyScheduleTitle) {
		return NeutralScheduleTitle(j.ScheduleID)
	}
	return j.Title
}

// ListSchedules returns every schedule, oldest first.
func (m *Manager) ListSchedules() ([]*store.Schedule, error) { return m.Store.ListSchedules(false) }

// scheduleLocalLayout is a firing's local wall-clock minute as stored (last_fired_local); it sorts
// as a string, which the claim's watermark relies on.
const scheduleLocalLayout = "2006-01-02 15:04"

// maxNextFireSkips bounds NextFire's walk past due minutes the watermark suppresses (the rest of a
// repeated DST hour, or a clock stepped back: a day of every-minute firings).
const maxNextFireSkips = 2 * 24 * 60

// NextFire is the next minute the schedule fires (nil when disabled, it never fires, or the
// watermark suppresses every firing within maxNextFireSkips). Like the claim, it skips a due
// minute whose instant or local wall-clock minute is not later than the last firing's, so a
// suppressed second pass of a repeated DST hour is never advertised. The walk starts at the later
// of now and the last firing, so a clock stepped back past the watermark (by any amount) still
// shows the firing that resumes the schedule instead of exhausting the skips on suppressed minutes.
func (m *Manager) NextFire(sc *store.Schedule) *time.Time {
	if !sc.Enabled {
		return nil
	}
	c, err := ParseCron(sc.Cron)
	if err != nil {
		return nil
	}
	t := m.Now()
	if sc.LastFiredAt != nil && sc.LastFiredAt.After(t) {
		t = *sc.LastFiredAt
	}
	t = t.In(m.loc())
	for i := 0; i < maxNextFireSkips; i++ {
		n, err := c.Next(t)
		if err != nil {
			return nil
		}
		if n.Format(scheduleLocalLayout) > sc.LastFiredLocal && (sc.LastFiredAt == nil || n.After(*sc.LastFiredAt)) {
			return &n
		}
		t = n
	}
	return nil
}

// SetSchedule edits a schedule in place: its cron, its prompt, enabled, or any of them (at least one).
func (m *Manager) SetSchedule(id int64, cron, prompt string, enabled *bool) (*store.Schedule, error) {
	if id <= 0 {
		return nil, errors.New("id is required")
	}
	if cron == "" && prompt == "" && enabled == nil {
		return nil, errors.New("give a cron, a prompt, enabled or several of them")
	}
	var err error
	if cron != "" {
		if cron, err = m.parseScheduleCron(cron); err != nil {
			return nil, err
		}
	}
	if prompt != "" {
		if prompt, err = SchedulePrompt(prompt); err != nil {
			return nil, err
		}
	}
	sc, err := m.Store.SetSchedule(id, cron, prompt, enabled, m.Now())
	if errors.Is(err, store.ErrScheduleNotFound) {
		return nil, fmt.Errorf("schedule %d not found", id)
	}
	return sc, err
}

// RemoveSchedule deletes a schedule (only on the owner's instruction; jobs it started keep its id).
func (m *Manager) RemoveSchedule(id int64) error {
	if id <= 0 {
		return errors.New("id is required")
	}
	err := m.Store.RemoveSchedule(id)
	if errors.Is(err, store.ErrScheduleNotFound) {
		return fmt.Errorf("schedule %d not found", id)
	}
	return err
}

// ScheduleLine is the one session line for the session schedules due in the same minute (oldest
// first): one prompt as is, several as a numbered list behind scheduleBatchIntro.
func ScheduleLine(due []*store.Schedule) string {
	if len(due) == 1 {
		return fmt.Sprintf("%s schedule %d: %s", ScheduleTag, due[0].ID, due[0].Prompt)
	}
	var b strings.Builder
	b.WriteString(ScheduleTag + " " + scheduleBatchIntro)
	for i, sc := range due {
		fmt.Fprintf(&b, " %d) schedule %d: %s", i+1, sc.ID, sc.Prompt)
	}
	return b.String()
}

// fireSchedules fires every enabled schedule due at the current minute (local wall clock): each one
// is claimed in the store first (past the watermark of its last firing, restart-safe), then job
// schedules are queued with their stored model and limits and session schedules are typed as one
// line through the bounded, serialized delivery (sendTmuxLocked: tmux.lock, TmuxDeliveryTimeout),
// so a stalled tmux never holds the tick. A failed delivery is logged and recorded on the schedule;
// the firing is lost, never retried later.
func (m *Manager) fireSchedules() error {
	list, err := m.Store.ListSchedules(true)
	if err != nil || len(list) == 0 {
		return err
	}
	now := m.Now().In(m.loc())
	minute := now.Truncate(time.Minute)
	local := minute.Format(scheduleLocalLayout)
	var session []*store.Schedule
	var errs []error
	for _, sc := range list {
		c, err := ParseCron(sc.Cron)
		if err != nil {
			m.Log.Printf("schedule %d: %v", sc.ID, err)
			continue
		}
		if !c.Match(now) {
			continue
		}
		ok, err := m.Store.ClaimScheduleFiring(sc.ID, minute, local)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !ok {
			continue // already fired for this minute
		}
		if sc.Kind == "" {
			session = append(session, sc)
			continue
		}
		jobID, ferr := "", ""
		j, err := m.Enqueue(StartRequest{
			Title: ScheduleTitle(sc), Kind: sc.Kind, Brief: sc.Prompt, RepoPath: sc.RepoPath, Base: sc.Base, Model: sc.Model,
			Complexity: sc.Complexity, MaxTurns: sc.MaxTurns, MaxBudgetUSD: sc.MaxBudgetUSD, TimeoutMinutes: sc.TimeoutMinutes,
			Runner: sc.Runner, ReasoningEffort: sc.ReasoningEffort, ScheduleID: strconv.FormatInt(sc.ID, 10), Skills: sc.Skills,
		})
		if err != nil {
			ferr = err.Error()
			m.Log.Printf("schedule %d at %s: job not started (firing lost): %v", sc.ID, local, err)
		} else {
			jobID = j.ID
			m.Log.Printf("schedule %d at %s: job %s queued", sc.ID, local, j.ID)
		}
		if err := m.Store.RecordScheduleFiring(sc.ID, minute, jobID, ferr); err != nil {
			errs = append(errs, err)
		}
	}
	if len(session) > 0 {
		ferr := ""
		line := ScheduleLine(session)
		if t := m.Cfg.NotifyTmuxTarget; t == "" {
			ferr = "notify off (notify_tmux_target not configured)"
			m.Log.Printf("schedule: %s, firing lost: %s", ferr, line)
		} else if err := m.sendTmuxLocked(t, line); err != nil {
			ferr = err.Error()
			m.Log.Printf("schedule: delivery failed, firing lost: %v", err)
		}
		for _, sc := range session {
			if err := m.Store.RecordScheduleFiring(sc.ID, minute, "", ferr); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}
