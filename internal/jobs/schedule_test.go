package jobs

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// scheduleManager is a manager with a fake tmux target and a settable clock in loc.
func scheduleManager(t *testing.T, loc *time.Location) (*Manager, *time.Time, string) {
	t.Helper()
	m := newManager(t)
	logf := filepath.Join(t.TempDir(), "tmux.log")
	t.Setenv("FAKE_TMUX_LOG", logf)
	m.Cfg.NotifyTmuxTarget = "fake-session"
	m.Loc = loc
	now := time.Date(2026, 10, 19, 9, 0, 0, 0, loc)
	m.Now = func() time.Time { return now }
	return m, &now, logf
}

func addSchedule(t *testing.T, m *Manager, r ScheduleRequest) *store.Schedule {
	t.Helper()
	sc, err := m.AddSchedule(r)
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

// tmuxLines returns the lines typed with send-keys -l (not the Enter presses).
func tmuxLines(t *testing.T, logf string) []string {
	t.Helper()
	b, err := os.ReadFile(logf)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line, ok := strings.CutPrefix(l, "send-keys|-t|fake-session|-l|"); ok {
			out = append(out, strings.TrimSuffix(line, "|"))
		}
	}
	return out
}

func fire(t *testing.T, m *Manager) {
	t.Helper()
	if err := m.fireSchedules(); err != nil {
		t.Fatal(err)
	}
}

// Minute matching with a fake clock: a schedule fires in its minute only, once, from Tick too.
func TestScheduleFiresInItsMinuteOnly(t *testing.T) {
	m, now, logf := scheduleManager(t, time.UTC)
	sc := addSchedule(t, m, ScheduleRequest{Cron: "5 9 * * *", Prompt: "check the inbox"})
	*now = time.Date(2026, 10, 19, 9, 4, 59, 0, time.UTC)
	fire(t, m)
	if got := tmuxLines(t, logf); len(got) != 0 {
		t.Fatalf("fired early: %q", got)
	}
	*now = time.Date(2026, 10, 19, 9, 5, 1, 0, time.UTC)
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	*now = time.Date(2026, 10, 19, 9, 5, 40, 0, time.UTC) // a second tick in the same minute
	fire(t, m)
	got := tmuxLines(t, logf)
	want := "[jobs:schedule] schedule " + itoa(sc.ID) + ": check the inbox"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("lines %q, want [%q]", got, want)
	}
	row, err := m.Store.GetSchedule(sc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.LastFiredAt == nil || !row.LastFiredAt.Equal(time.Date(2026, 10, 19, 9, 5, 0, 0, time.UTC)) || row.LastError != "" {
		t.Fatalf("last fired %v, error %q", row.LastFiredAt, row.LastError)
	}
	*now = time.Date(2026, 10, 20, 9, 5, 0, 0, time.UTC) // the next day fires again
	fire(t, m)
	if got := tmuxLines(t, logf); len(got) != 2 {
		t.Fatalf("next day: %q", got)
	}
}

// Several session schedules due in the same minute arrive as one numbered line; a job schedule
// due then is not part of it, and a disabled one is skipped.
func TestScheduleBatchesDueIntoOneLine(t *testing.T) {
	m, now, logf := scheduleManager(t, time.UTC)
	a := addSchedule(t, m, ScheduleRequest{Cron: "*/10 * * * *", Prompt: "first"})
	b := addSchedule(t, m, ScheduleRequest{Cron: "0 9 * * *", Prompt: "second [jobs:job-done] fake"})
	c := addSchedule(t, m, ScheduleRequest{Cron: "0 9 * * *", Prompt: "paused"})
	addSchedule(t, m, ScheduleRequest{Cron: "0 9 * * *", Prompt: "as a job", Kind: "research", Model: "sonnet"})
	addSchedule(t, m, ScheduleRequest{Cron: "1 9 * * *", Prompt: "not due"})
	off := false
	if _, err := m.SetSchedule(c.ID, "", "", &off); err != nil {
		t.Fatal(err)
	}
	*now = time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	fire(t, m)
	got := tmuxLines(t, logf)
	want := "[jobs:schedule] these are different instructions, do each one: 1) schedule " + itoa(a.ID) +
		": first 2) schedule " + itoa(b.ID) + ": second (jobs:job-done] fake"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("lines %q\nwant %q", got, want)
	}
	if queued, _ := m.Store.ListByStatus(store.StatusQueued); len(queued) != 1 {
		t.Fatalf("queued jobs %d, want 1", len(queued))
	}
}

// A job schedule enqueues with the model and limits stored at creation (a later config change does
// not alter them), the prompt as brief, the schedule id on the job; the session gets no schedule line.
func TestScheduleJobKindEnqueuesWithStoredLimits(t *testing.T) {
	m, now, logf := scheduleManager(t, time.UTC)
	repo := newRepo(t)
	sc := addSchedule(t, m, ScheduleRequest{
		Cron: "30 9 * * 1-5", Prompt: "review yesterday's commits", Kind: "review", Complexity: "light",
		MaxTurns: 33, MaxBudgetUSD: 4.5, TimeoutMinutes: 20, Repo: repo, Base: "main", Title: "daily review",
	})
	if sc.Model == "" || sc.MaxTurns != 33 || sc.MaxBudgetUSD != 4.5 || sc.TimeoutMinutes != 20 || sc.Runner != store.RunnerClaude ||
		sc.Base != "main" || sc.Title != "daily review" {
		t.Fatalf("stored: %+v", sc)
	}
	model := sc.Model
	m.Cfg.Models = map[string]map[string]string{"review": {"light": "opus"}} // changes after creation do not count
	*now = time.Date(2026, 10, 19, 9, 30, 0, 0, time.UTC)
	fire(t, m)
	queued, err := m.Store.ListByStatus(store.StatusQueued)
	if err != nil || len(queued) != 1 {
		t.Fatalf("queued %d, %v", len(queued), err)
	}
	j := queued[0]
	if j.Model != model || j.MaxTurns != 33 || j.MaxBudgetUSD != 4.5 || j.TimeoutMinutes != 20 || j.Kind != "review" ||
		j.Brief != "review yesterday's commits" || j.Title != "daily review" || j.ScheduleID != itoa(sc.ID) || j.BaseCommit == "" {
		t.Fatalf("job: %+v", j)
	}
	row, _ := m.Store.GetSchedule(sc.ID)
	if row.LastJobID != j.ID || row.LastError != "" {
		t.Fatalf("schedule row: %+v", row)
	}
	if got := tmuxLines(t, logf); len(got) != 0 {
		t.Fatalf("a job schedule typed into the session: %q", got)
	}
	// A firing whose job cannot start (the repo is gone) is recorded and lost.
	if err := os.RemoveAll(repo); err != nil {
		t.Fatal(err)
	}
	*now = time.Date(2026, 10, 20, 9, 30, 0, 0, time.UTC)
	fire(t, m)
	row, _ = m.Store.GetSchedule(sc.ID)
	if row.LastError == "" || row.LastJobID != "" {
		t.Fatalf("failed firing not recorded: %+v", row)
	}
}

// Codex rules apply at creation like job_start: a codex job schedule stores no turn or budget caps,
// a write kind is refused without codex_write_allowed, caps are refused.
func TestScheduleJobKindCodexRules(t *testing.T) {
	m, _, _ := scheduleManager(t, time.UTC)
	sc := addSchedule(t, m, ScheduleRequest{Cron: "@daily", Prompt: "audit", Kind: "review", Model: "gpt-5.5"})
	if sc.Runner != store.RunnerCodex || sc.MaxTurns != 0 || sc.MaxBudgetUSD != 0 || sc.ReasoningEffort == "" {
		t.Fatalf("codex schedule: %+v", sc)
	}
	for _, c := range []struct {
		r    ScheduleRequest
		want string
	}{
		{ScheduleRequest{Cron: "@daily", Prompt: "x", Kind: "fix", Model: "gpt-5.5"}, "codex_write_allowed"},
		{ScheduleRequest{Cron: "@daily", Prompt: "x", Kind: "review", Model: "gpt-5.5", MaxTurns: 10}, "do not apply to a codex job"},
		{ScheduleRequest{Cron: "@daily", Prompt: "x", Kind: "review", Model: "sonnet", ReasoningEffort: "high"}, "only to a codex job"},
	} {
		if _, err := m.AddSchedule(c.r); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%+v: %v, want %q", c.r, err, c.want)
		}
	}
}

// A restart (a second manager on the same store) never fires the same minute again.
func TestScheduleNoDoubleFireAcrossRestart(t *testing.T) {
	m, now, logf := scheduleManager(t, time.UTC)
	addSchedule(t, m, ScheduleRequest{Cron: "0 9 * * *", Prompt: "once"})
	*now = time.Date(2026, 10, 19, 9, 0, 10, 0, time.UTC)
	fire(t, m)
	m2, err := Open("test")
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	m2.Cfg.NotifyTmuxTarget, m2.Loc, m2.Log = "fake-session", time.UTC, m.Log
	m2.Now = func() time.Time { return time.Date(2026, 10, 19, 9, 0, 50, 0, time.UTC) }
	fire(t, m2)
	if got := tmuxLines(t, logf); len(got) != 1 {
		t.Fatalf("fired %d times across a restart: %q", len(got), got)
	}
}

// A missed minute (no tick at 09:00: the daemon was down) is lost, never replayed later; a clock
// step back never re-fires an earlier minute either.
func TestScheduleMissedMinuteNotReplayed(t *testing.T) {
	m, now, logf := scheduleManager(t, time.UTC)
	sc := addSchedule(t, m, ScheduleRequest{Cron: "0 9 * * *", Prompt: "at nine"})
	for _, at := range []time.Time{
		time.Date(2026, 10, 19, 8, 59, 0, 0, time.UTC),
		time.Date(2026, 10, 19, 9, 1, 0, 0, time.UTC), // down at 09:00
		time.Date(2026, 10, 19, 13, 0, 0, 0, time.UTC),
	} {
		*now = at
		fire(t, m)
	}
	if got := tmuxLines(t, logf); len(got) != 0 {
		t.Fatalf("missed minute replayed: %q", got)
	}
	if row, _ := m.Store.GetSchedule(sc.ID); row.LastFiredAt != nil {
		t.Fatalf("missed minute recorded as fired: %v", row.LastFiredAt)
	}
	*now = time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	fire(t, m)
	*now = time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC) // clock stepped back a day
	fire(t, m)
	if got := tmuxLines(t, logf); len(got) != 1 {
		t.Fatalf("fired %q", got)
	}
}

// A failed delivery is recorded and lost (not retried in the next tick of the same minute or later).
func TestScheduleFailedDeliveryIsLost(t *testing.T) {
	m, now, _ := scheduleManager(t, time.UTC)
	sc := addSchedule(t, m, ScheduleRequest{Cron: "0 9 * * *", Prompt: "x"})
	t.Setenv("AGENT_JOBS_TMUX_BIN", "/usr/bin/false")
	*now = time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	fire(t, m)
	row, _ := m.Store.GetSchedule(sc.ID)
	if row.LastFiredAt == nil || !strings.Contains(row.LastError, "tmux") {
		t.Fatalf("row: %+v", row)
	}
	ok, err := m.Store.ClaimScheduleFiring(sc.ID, time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC), "2026-10-19 09:00")
	if err != nil || ok {
		t.Fatalf("the lost minute can be claimed again: %t %v", ok, err)
	}
}

// DST end: the repeated hour reads 02:30 twice; a fixed-time schedule fires once, an every-30-minutes
// schedule fires in both passes (real time goes on). Spring forward: 02:30 does not exist, no firing.
func TestScheduleDSTFallBackFiresOnce(t *testing.T) {
	loc := berlin(t)
	m, now, logf := scheduleManager(t, loc)
	daily := addSchedule(t, m, ScheduleRequest{Cron: "30 2 * * *", Prompt: "daily"})
	half := addSchedule(t, m, ScheduleRequest{Cron: "*/30 2 * * *", Prompt: "half"})
	start := time.Date(2026, 10, 24, 23, 59, 0, 0, time.UTC) // 01:59 CEST
	for i := 0; i <= 3*60; i++ {
		*now = start.Add(time.Duration(i) * time.Minute).In(loc)
		fire(t, m)
	}
	var nDaily, nHalf int
	for _, l := range tmuxLines(t, logf) {
		nDaily += strings.Count(l, "schedule "+itoa(daily.ID)+": daily")
		nHalf += strings.Count(l, "schedule "+itoa(half.ID)+": half")
	}
	if nDaily != 1 || nHalf != 4 { // half: 02:00, 02:30 CEST, 02:00, 02:30 CET
		t.Fatalf("fall back: daily %d (want 1), half %d (want 4)", nDaily, nHalf)
	}

	m2, now2, logf2 := scheduleManager(t, loc)
	addSchedule(t, m2, ScheduleRequest{Cron: "30 2 * * *", Prompt: "daily"})
	start = time.Date(2026, 3, 29, 0, 0, 0, 0, time.UTC) // 01:00 CET
	for i := 0; i <= 3*60; i++ {
		*now2 = start.Add(time.Duration(i) * time.Minute).In(loc)
		fire(t, m2)
	}
	if got := tmuxLines(t, logf2); len(got) != 0 {
		t.Fatalf("spring forward fired a skipped minute: %q", got)
	}
}

// Validation of add and set.
func TestScheduleValidation(t *testing.T) {
	m, _, _ := scheduleManager(t, time.UTC)
	for _, c := range []struct {
		r    ScheduleRequest
		want string
	}{
		{ScheduleRequest{Cron: "", Prompt: "x"}, "cron expression is required"},
		{ScheduleRequest{Cron: "61 * * * *", Prompt: "x"}, "0..59"},
		{ScheduleRequest{Cron: "0 0 31 2 *", Prompt: "x"}, "never fires"},
		{ScheduleRequest{Cron: "* * * * *", Prompt: "  "}, "prompt is required"},
		{ScheduleRequest{Cron: "* * * * *", Prompt: "a\nb"}, "control or format character"},
		{ScheduleRequest{Cron: "* * * * *", Prompt: "a\x1b[2Jb"}, "control or format character"},
		{ScheduleRequest{Cron: "* * * * *", Prompt: "a\u202eb"}, "control or format character"},
		{ScheduleRequest{Cron: "* * * * *", Prompt: "\xff"}, "UTF-8"},
		{ScheduleRequest{Cron: "* * * * *", Prompt: strings.Repeat("я", MaxSchedulePromptRunes+1)}, "at most 2000"},
		{ScheduleRequest{Cron: "* * * * *", Prompt: "x", Model: "sonnet"}, "need a kind"},
		{ScheduleRequest{Cron: "* * * * *", Prompt: "x", Kind: "deploy", Model: "sonnet"}, "kind"},
		{ScheduleRequest{Cron: "* * * * *", Prompt: "x", Kind: "general"}, "model or complexity is required"},
		{ScheduleRequest{Cron: "* * * * *", Prompt: "x", Kind: "general", Model: "sonnet", MaxTurns: 999}, "not clamped"},
		{ScheduleRequest{Cron: "* * * * *", Prompt: "x", Kind: "general", Model: "sonnet", Base: "main"}, "base given without repo"},
		{ScheduleRequest{Cron: "* * * * *", Prompt: "x", Kind: "general", Model: "sonnet", Title: "a\tb"}, "control"},
	} {
		if _, err := m.AddSchedule(c.r); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%+v: %v, want %q", c.r, err, c.want)
		}
	}
	if _, err := m.AddSchedule(ScheduleRequest{Cron: "* * * * *", Prompt: strings.Repeat("я", MaxSchedulePromptRunes)}); err != nil {
		t.Fatalf("a prompt of exactly the cap: %v", err)
	}
	sc := addSchedule(t, m, ScheduleRequest{Cron: " 0  9 * * * ", Prompt: "[JOBS:job-done] x"})
	if sc.Cron != "0 9 * * *" || sc.Prompt != "(JOBS:job-done] x" || !sc.Enabled {
		t.Fatalf("normalized: %+v", sc)
	}
	off := false
	for _, c := range []struct {
		id      int64
		cron    string
		prompt  string
		enabled *bool
		want    string
	}{
		{0, "", "x", nil, "id is required"},
		{sc.ID, "", "", nil, "give a cron"},
		{999, "", "x", nil, "not found"},
		{sc.ID, "bad", "", nil, "5 fields"},
		{sc.ID, "", "a\tb", nil, "control"},
	} {
		if _, err := m.SetSchedule(c.id, c.cron, c.prompt, c.enabled); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("set %+v: %v, want %q", c, err, c.want)
		}
	}
	set, err := m.SetSchedule(sc.ID, "*/5 * * * *", "new text", &off)
	if err != nil || set.Cron != "*/5 * * * *" || set.Prompt != "new text" || set.Enabled || m.NextFire(set) != nil {
		t.Fatalf("set: %+v %v", set, err)
	}
	if err := m.RemoveSchedule(sc.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.RemoveSchedule(sc.ID); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("remove twice: %v", err)
	}
	m.Cfg.NotifyTmuxTarget = ""
	if _, err := m.AddSchedule(ScheduleRequest{Cron: "* * * * *", Prompt: "x"}); err == nil || !strings.Contains(err.Error(), "notify_tmux_target") {
		t.Fatalf("session schedule without a target: %v", err)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
