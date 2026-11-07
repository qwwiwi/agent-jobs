package jobs

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// rawDB opens the manager's database directly, to plant faults (triggers) the store has no API for.
func rawDB(t *testing.T, m *Manager) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(m.StateDir, "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// #170 fix loop 1, H1: the heartbeat line is a fixed contract (HeartbeatLine); a pending breach never
// rides on it. The note stays pending and goes out on the next delivery of another category.
func TestHeartbeatLineExactWithPendingBreach(t *testing.T) {
	e := newHeartbeat(t)
	e.m.Cfg.SLO.Targets = map[string]float64{"deploy": 600}
	if _, err := e.m.RecordSLO("deploy", 700); err != nil {
		t.Fatal(err)
	}
	e.answer(t, `{"wake": true, "reason": "go"}`)
	h, err := e.m.RunHeartbeat(context.Background(), HeartbeatTriggerDaemon)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(e.tmux)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("send-keys|-t|orch|-l|[jobs:heartbeat] wake %d|\nsend-keys|-t|orch|Enter|\n", h.ID)
	if string(b) != want {
		t.Fatalf("heartbeat line changed by the breach note:\n got %q\nwant %q", b, want)
	}
	if bs, _ := e.m.Store.UnnotedSLOBreaches(); len(bs) != 1 {
		t.Fatalf("the breach was consumed by the heartbeat: %+v", bs)
	}
	if ws := wakesOf(t, e.m, WakeHeartbeat); len(ws) != 1 || ws[0].Bytes != len(HeartbeatLine(h.ID)) {
		t.Fatalf("heartbeat record: %+v", ws)
	}
	if _, err := e.m.NotifyOwner("", "later", ""); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(e.tmux)
	if !strings.Contains(string(b), "later · slo breach: deploy 700s > 600s") {
		t.Fatalf("the next delivery lacks the note:\n%s", b)
	}
}

// #170 fix loop 1, H2: the same job delivered again after a resume (a new attempt) is a new wake with
// its own bytes, not the retry of the first.
func TestWakeJobDoneAfterResumeCounted(t *testing.T) {
	m := newManager(t)
	notifyTo(t, m)
	now := coalesceClock(m)
	db := rawDB(t, m)
	t0 := *now
	fin := t0
	j := &store.Job{ID: "resumed", Agent: m.Agent, Title: "r", Kind: "fix", Brief: "b", Status: store.StatusFailed, Model: "sonnet",
		MaxTurns: 1, MaxBudgetUSD: 1, TimeoutMinutes: 1, CreatedAt: t0, StartedAt: &t0, FinishedAt: &fin, Attempt: 1}
	if err := m.Store.Insert(j); err != nil {
		t.Fatal(err)
	}
	notifyAt(t, m, now, t0.Add(time.Minute))
	if ws := wakesOf(t, m, WakeJobDone); len(ws) != 1 {
		t.Fatalf("first delivery: %+v", ws)
	}
	// Resume, run again (attempt 2) and finish once more.
	if err := m.Store.Requeue(j.ID, store.ResumeLimits{MaxTurns: 1, MaxBudgetUSD: 1, TimeoutMinutes: 1}, "more", ""); err != nil {
		t.Fatal(err)
	}
	later := t0.Add(time.Hour)
	mustExec(t, db, `UPDATE jobs SET status='failed', attempt=2, started_at=?, finished_at=? WHERE id=?`, later.Unix()-30, later.Unix(), j.ID)
	notifyAt(t, m, now, later.Add(time.Minute))
	ws := wakesOf(t, m, WakeJobDone)
	if len(ws) != 2 || ws[0].Members != j.ID || ws[1].Members != j.ID {
		t.Fatalf("second delivery of the resumed job lost: %+v", ws)
	}
}

// #170 fix loop 1, H2: a coalesced batch is acknowledged atomically. A store failure while marking
// leaves every member pending, so the retry is the same batch (one record), not a smaller one.
func TestCoalescedBatchAcknowledgedAtomically(t *testing.T) {
	m := newManager(t)
	logf := notifyTo(t, m)
	m.Cfg.NotifyCoalesceSeconds = 120
	now := coalesceClock(m)
	db := rawDB(t, m)
	t0 := *now
	a := finishAt(t, m, now, t0, nil)
	b := finishAt(t, m, now, t0.Add(10*time.Second), nil)
	mustExec(t, db, `CREATE TRIGGER fail_mark BEFORE UPDATE OF notified_at ON jobs WHEN NEW.id = '`+b.ID+`' AND NEW.notified_at IS NOT NULL
		BEGIN SELECT RAISE(ABORT, 'planted fault'); END`)
	*now = t0.Add(10 * time.Minute)
	if err := m.notifyPending(); err == nil {
		t.Fatal("the planted fault did not surface")
	}
	if got := get(t, m, a.ID); got.NotifiedAt != nil {
		t.Fatalf("job %s marked although its batch was not acknowledged", a.ID)
	}
	mustExec(t, db, `DROP TRIGGER fail_mark`)
	notifyAt(t, m, now, t0.Add(11*time.Minute))
	for _, id := range []string{a.ID, b.ID} {
		if get(t, m, id).NotifiedAt == nil {
			t.Fatalf("job %s not marked", id)
		}
	}
	ws := wakesOf(t, m, WakeJobDone)
	if len(ws) != 1 || ws[0].BatchSize != 2 {
		t.Fatalf("records: %+v (lines %q)", ws, tmuxLines(t, logf))
	}
}

// #170 fix loop 2, H2: the batch {a,b} is typed, its acknowledgement fails, and a is resumed before
// the retry: the retry is {b}. b was already counted, so the retry adds no record.
func TestCoalescedRetryRegroupedAfterResumeNotRecounted(t *testing.T) {
	m := newManager(t)
	notifyTo(t, m)
	m.Cfg.NotifyCoalesceSeconds = 120
	now := coalesceClock(m)
	db := rawDB(t, m)
	t0 := *now
	a := finishAt(t, m, now, t0, nil)
	b := finishAt(t, m, now, t0.Add(10*time.Second), nil)
	mustExec(t, db, `CREATE TRIGGER fail_mark BEFORE UPDATE OF notified_at ON jobs WHEN NEW.id = '`+b.ID+`' AND NEW.notified_at IS NOT NULL
		BEGIN SELECT RAISE(ABORT, 'planted fault'); END`)
	*now = t0.Add(10 * time.Minute)
	if err := m.notifyPending(); err == nil {
		t.Fatal("the planted fault did not surface")
	}
	if ws := wakesOf(t, m, WakeJobDone); len(ws) != 1 || ws[0].BatchSize != 2 {
		t.Fatalf("the typed batch is not recorded once: %+v", ws)
	}
	mustExec(t, db, `DROP TRIGGER fail_mark`)
	// a is resumed: it leaves the pending set while its batch stays unacknowledged.
	mustExec(t, db, `UPDATE jobs SET status='queued' WHERE id=?`, a.ID)
	notifyAt(t, m, now, t0.Add(11*time.Minute))
	if get(t, m, b.ID).NotifiedAt == nil {
		t.Fatal("b not marked by the retry")
	}
	if ws := wakesOf(t, m, WakeJobDone); len(ws) != 1 {
		t.Fatalf("the regrouped retry was counted again: %+v", ws)
	}
}

// #170 fix loop 1, H3: the breach is reserved before its job is queued, so a failure to link the job
// afterwards cannot make the next tick queue another one for the same breach.
func TestSLOInvestigateReservedBeforeEnqueue(t *testing.T) {
	m := newManager(t)
	now := coalesceClock(m)
	db := rawDB(t, m)
	m.Cfg.SLO.Targets = map[string]float64{"deploy": 600}
	m.Cfg.SLO.Investigate = SLOInvestigateConfig{Enabled: true, CooldownMinutes: 1, DailyMax: 1}
	if _, err := m.RecordSLO("deploy", 700); err != nil {
		t.Fatal(err)
	}
	// Linking the queued job to the breach fails (the reservation, which names no job, does not).
	mustExec(t, db, `CREATE TRIGGER fail_link BEFORE UPDATE OF investigate_job ON slo_breaches
		WHEN NEW.investigate_job NOT IN ('', 'pending') BEGIN SELECT RAISE(ABORT, 'planted fault'); END`)
	_ = m.sloInvestigate()
	*now = now.Add(time.Hour)
	if _, err := m.RecordSLO("deploy", 800); err != nil {
		t.Fatal(err)
	}
	_ = m.sloInvestigate()
	*now = now.Add(time.Hour)
	_ = m.sloInvestigate()
	if js := researchJobs(t, m); len(js) != 1 {
		t.Fatalf("%d research jobs for daily_max=1", len(js))
	}
	if bs, _ := m.Store.UninvestigatedSLOBreaches(); len(bs) != 0 {
		t.Fatalf("breaches left open: %+v", bs)
	}
}

// #170 fix loop 1, H3: a start that fails is released, so it neither counts against the cap nor blocks
// the next breach.
func TestSLOInvestigateFailedStartReleased(t *testing.T) {
	m := newManager(t)
	now := coalesceClock(m)
	m.Cfg.SLO.Targets = map[string]float64{"deploy": 600}
	m.Cfg.SLO.Investigate = SLOInvestigateConfig{Enabled: true, CooldownMinutes: 1, DailyMax: 1, Complexity: "no-such"}
	if _, err := m.RecordSLO("deploy", 700); err != nil {
		t.Fatal(err)
	}
	if err := m.sloInvestigate(); err != nil {
		t.Fatal(err)
	}
	if len(researchJobs(t, m)) != 0 {
		t.Fatal("a job started with a bad complexity")
	}
	m.Cfg.SLO.Investigate.Complexity = "light"
	*now = now.Add(time.Hour)
	if _, err := m.RecordSLO("deploy", 800); err != nil {
		t.Fatal(err)
	}
	if err := m.sloInvestigate(); err != nil {
		t.Fatal(err)
	}
	if len(researchJobs(t, m)) != 1 {
		t.Fatal("a failed start used up the daily cap")
	}
}

// #170 fix loop 1, M1: a cooldown longer than the 24 hours of the daily cap is honoured.
func TestSLOInvestigateCooldownLongerThanADay(t *testing.T) {
	m := newManager(t)
	now := coalesceClock(m)
	t0 := *now
	m.Cfg.SLO.Targets = map[string]float64{"deploy": 600}
	m.Cfg.SLO.Investigate = SLOInvestigateConfig{Enabled: true, CooldownMinutes: 48 * 60, DailyMax: 5}
	for i, at := range []time.Duration{0, 25 * time.Hour, 47 * time.Hour, 49 * time.Hour} {
		*now = t0.Add(at)
		if _, err := m.RecordSLO("deploy", 700+float64(i)); err != nil {
			t.Fatal(err)
		}
		if err := m.sloInvestigate(); err != nil {
			t.Fatal(err)
		}
		want := 1
		if i == 3 {
			want = 2
		}
		if got := len(researchJobs(t, m)); got != want {
			t.Fatalf("after the sample at +%s: %d jobs, want %d", at, got, want)
		}
	}
}

// #170 fix loop 1 (bounded notes): a burst of breaches of many names gives a short note; only the
// names it shows are marked, the rest follow on the next deliveries.
func TestSLOBreachNoteBounded(t *testing.T) {
	m, _, logf := scheduleManager(t, time.UTC)
	const n = 200
	m.Cfg.SLO.Targets = map[string]float64{}
	for i := 0; i < n; i++ {
		m.Cfg.SLO.Targets[fmt.Sprintf("slow-step-number-%03d", i)] = 1
	}
	for i := 0; i < n; i++ {
		if _, err := m.RecordSLO(fmt.Sprintf("slow-step-number-%03d", i), 5); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	for i := 0; i < n && len(seen) < n; i++ {
		if _, err := m.NotifyOwner("", fmt.Sprintf("m%d", i), ""); err != nil {
			t.Fatal(err)
		}
		lines := tmuxLines(t, logf)
		line := lines[len(lines)-1]
		if len(line) > 700 {
			t.Fatalf("delivery %d is %d bytes: %.200q", i, len(line), line)
		}
		_, note, ok := strings.Cut(line, " · slo breach: ")
		if !ok {
			break
		}
		for _, part := range strings.Split(note, "; ") {
			name, _, _ := strings.Cut(part, " ")
			if seen[name] {
				t.Fatalf("%s noted twice", name)
			}
			seen[name] = true
		}
	}
	if len(seen) != n {
		t.Fatalf("%d of %d breaches reached the session", len(seen), n)
	}
}

// #170 fix loop 1: the rounding of a line must not hide the breach (600.01 against 600).
func TestSLONoteKeepsDistinguishingPrecision(t *testing.T) {
	m, _, logf := scheduleManager(t, time.UTC)
	m.Cfg.SLO.Targets = map[string]float64{"deploy": 600}
	if _, err := m.RecordSLO("deploy", 600.01); err != nil {
		t.Fatal(err)
	}
	if _, err := m.NotifyOwner("", "x", ""); err != nil {
		t.Fatal(err)
	}
	if got := tmuxLines(t, logf); len(got) != 1 || !strings.HasSuffix(got[0], "deploy 600.01s > 600s") {
		t.Fatalf("lines: %q", got)
	}
}

// #170 fix loop 1: when the mark of a noted breach fails after tmux took the line, the next delivery
// does not repeat the note, and the mark is retried.
func TestSLONoteNotRepeatedAfterFailedMark(t *testing.T) {
	m, _, logf := scheduleManager(t, time.UTC)
	db := rawDB(t, m)
	m.Cfg.SLO.Targets = map[string]float64{"deploy": 600}
	if _, err := m.RecordSLO("deploy", 700); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `CREATE TRIGGER fail_note BEFORE UPDATE OF noted_at ON slo_breaches BEGIN SELECT RAISE(ABORT, 'planted fault'); END`)
	if _, err := m.NotifyOwner("", "one", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.NotifyOwner("", "two", ""); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `DROP TRIGGER fail_note`)
	if _, err := m.NotifyOwner("", "three", ""); err != nil {
		t.Fatal(err)
	}
	got := tmuxLines(t, logf)
	if len(got) != 3 || !strings.Contains(got[0], "slo breach") || strings.Contains(got[1], "slo breach") || strings.Contains(got[2], "slo breach") {
		t.Fatalf("lines: %q", got)
	}
	if bs, _ := m.Store.UnnotedSLOBreaches(); len(bs) != 0 {
		t.Fatalf("the mark was not retried: %+v", bs)
	}
}

// #170 fix loop 2: once the dropped-injections file is full, further losses leave a marker, so a report
// whose window no longer holds any of the file's old entries still says it is incomplete.
func TestWakeStatsIncompleteAfterDroppedFileSaturated(t *testing.T) {
	m := newManager(t)
	old := time.Now().Add(-72 * time.Hour)
	var sb strings.Builder
	for sb.Len() <= maxDroppedFileBytes {
		sb.WriteString(fmt.Sprintf("%d %s 10\n", old.Unix(), InjectBriefReminder))
	}
	if err := os.WriteFile(filepath.Join(m.StateDir, DroppedInjectionsFile), []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	db := rawDB(t, m)
	mustExec(t, db, `CREATE TRIGGER fail_inj BEFORE INSERT ON orchestrator_wakes BEGIN SELECT RAISE(ABORT, 'planted fault'); END`)
	if err := RecordHookInjection("test", InjectBriefReminder, "lost after saturation", time.Now()); err == nil {
		t.Fatal("the planted fault did not surface")
	}
	rep, err := m.WakeStats(time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if rep.InjectionsUnrecorded != 0 || !rep.InjectionsUncounted {
		t.Fatalf("a loss after saturation is not reported: %+v", rep)
	}
	if later, _ := m.WakeStats(time.Now().Add(time.Hour)); later.InjectionsUncounted {
		t.Fatalf("since is ignored: %+v", later)
	}
}

// #170 fix loop 1: an injection a hook could not store is counted in the report as unrecorded.
func TestWakeStatsReportsDroppedInjections(t *testing.T) {
	m := newManager(t)
	db := rawDB(t, m)
	mustExec(t, db, `CREATE TRIGGER fail_inj BEFORE INSERT ON orchestrator_wakes BEGIN SELECT RAISE(ABORT, 'planted fault'); END`)
	if err := RecordHookInjection("test", InjectBriefReminder, "twelve bytes", time.Now()); err == nil {
		t.Fatal("the planted fault did not surface")
	}
	rep, err := m.WakeStats(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.InjectionsUnrecorded != 1 || rep.InjectionBytesUnrecorded != 12 {
		t.Fatalf("report: %+v", rep)
	}
	if later, _ := m.WakeStats(time.Now().Add(time.Hour)); later.InjectionsUnrecorded != 0 {
		t.Fatalf("since is ignored: %+v", later)
	}
}
