package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

func codexStart(model string) func(*StartRequest) {
	return func(r *StartRequest) { r.Kind = "review"; r.Model = model }
}

// With codex_jobs_per_day 2, a third codex job within 24 hours is refused with the count, the window
// and the setting; claude jobs are not affected, older codex jobs do not count, 0 turns the cap off.
func TestCodexDailyCap(t *testing.T) {
	m := newManager(t)
	two := 2
	m.Cfg.CodexJobsPerDay = &two
	now := time.Now()
	// A codex job of 25 hours ago is outside the window.
	if err := m.Store.Insert(&store.Job{ID: "old", Agent: m.Agent, Title: "old", Kind: "review", Brief: "b",
		Status: store.StatusSucceeded, Model: "gpt-5.5", Runner: store.RunnerCodex, TimeoutMinutes: 1,
		CreatedAt: now.Add(-25 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	enqueue(t, m, "one", codexStart("gpt-5.5"))
	enqueue(t, m, "two", codexStart("gpt-5.5"))
	if got, err := m.CodexJobsToday(); err != nil || got != "2/2" {
		t.Fatalf("today: %q %v", got, err)
	}
	_, err := m.Enqueue(StartRequest{Title: "three", Kind: "review", Brief: "three", Model: "gpt-5.5"})
	if err == nil || !strings.Contains(err.Error(), "codex daily cap reached: 2 codex jobs of agent test in the last 24 hours, limit 2") ||
		!strings.Contains(err.Error(), "codex_jobs_per_day") {
		t.Fatalf("third codex job: %v", err)
	}
	enqueue(t, m, "claude still runs", nil)
	zero := 0
	m.Cfg.CodexJobsPerDay = &zero
	enqueue(t, m, "cap off", codexStart("gpt-5.5"))
	if got, _ := m.CodexJobsToday(); got != "3 (cap off)" {
		t.Fatalf("cap off: %q", got)
	}
}

func TestCodexDailyCapConfig(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) error {
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := LoadConfig(dir)
		return err
	}
	if c, _ := LoadConfig(dir); c.CodexDailyCap() != DefaultCodexJobsPerDay {
		t.Fatalf("default cap %d", c.CodexDailyCap())
	}
	for body, want := range map[string]string{`{"codex_jobs_per_day": -1}`: "0 (off) or more", `{"codex_jobs_per_day": null}`: "not null"} {
		if err := write(body); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", body, err)
		}
	}
	if err := write(`{"codex_jobs_per_day": 0}`); err != nil {
		t.Fatal(err)
	}
}

// A schedule that starts codex jobs is skipped past the cap, with the reason on the schedule.
func TestScheduleCodexPastCap(t *testing.T) {
	m, now, _ := scheduleManager(t, time.UTC)
	one := 1
	m.Cfg.CodexJobsPerDay = &one
	sc := addSchedule(t, m, ScheduleRequest{Cron: "0 9 * * *", Prompt: "audit", Kind: "review", Model: "gpt-5.5"})
	*now = time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	fire(t, m)
	row, _ := m.Store.GetSchedule(sc.ID)
	if row.LastJobID == "" || row.LastError != "" {
		t.Fatalf("first firing: %+v", row)
	}
	*now = time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC).Add(24*time.Hour - time.Hour) // inside the window
	sc2 := addSchedule(t, m, ScheduleRequest{Cron: "0 8 * * *", Prompt: "audit again", Kind: "review", Model: "gpt-5.5"})
	fire(t, m)
	row, _ = m.Store.GetSchedule(sc2.ID)
	if row.LastJobID != "" || !strings.Contains(row.LastError, "codex daily cap reached: 1 codex jobs") {
		t.Fatalf("firing past the cap: %+v", row)
	}
}
