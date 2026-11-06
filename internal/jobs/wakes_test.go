package jobs

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// wakeCounts is the report's deliveries and injections by category.
func wakeCounts(t *testing.T, m *Manager) map[string]store.Wake {
	t.Helper()
	ws, err := m.Store.Wakes(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]store.Wake{}
	for _, w := range ws {
		out[w.Kind+"/"+w.Category+"/"+fmt.Sprint(w.ID)] = w
	}
	return out
}

func wakesOf(t *testing.T, m *Manager, category string) []store.Wake {
	t.Helper()
	ws, err := m.Store.Wakes(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var out []store.Wake
	for _, w := range ws {
		if w.Category == category {
			out = append(out, w)
		}
	}
	return out
}

// #170: every kind of line typed into the session is recorded under its own category with the bytes
// that were typed and the things it carried.
func TestWakeCategoriesCounted(t *testing.T) {
	m, now, logf := scheduleManager(t, time.UTC)
	// job-done
	j := enqueue(t, m, "brief", nil)
	if err := m.Cancel(j.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.notifyPending(); err != nil {
		t.Fatal(err)
	}
	// notify-owner
	n, err := m.NotifyOwner("", "look at this", "")
	if err != nil {
		t.Fatal(err)
	}
	// schedule
	addSchedule(t, m, ScheduleRequest{Cron: "5 9 * * *", Prompt: "check the inbox"})
	*now = time.Date(2026, 10, 19, 9, 5, 1, 0, time.UTC)
	fire(t, m)
	// check-missed
	addCheck(t, m, "a", "0 10 * * *", 30)
	*now = time.Date(2026, 10, 19, 10, 30, 1, 0, time.UTC)
	watch(t, m)

	lines := tmuxLines(t, logf)
	if len(lines) != 4 {
		t.Fatalf("lines: %q", lines)
	}
	for i, cat := range []string{WakeJobDone, WakeNotifyOwner, WakeSchedule, WakeCheckMissed} {
		ws := wakesOf(t, m, cat)
		if len(ws) != 1 {
			t.Fatalf("%s: %d records, want 1 (%+v)", cat, len(ws), ws)
		}
		if ws[0].Kind != store.WakeDelivery || ws[0].Bytes != len(lines[i]) || ws[0].BatchSize != 1 {
			t.Fatalf("%s: record %+v for line %q", cat, ws[0], lines[i])
		}
	}
	if ws := wakesOf(t, m, WakeJobDone); ws[0].Members != j.ID {
		t.Fatalf("job-done members %q, want %q", ws[0].Members, j.ID)
	}
	if ws := wakesOf(t, m, WakeNotifyOwner); ws[0].Members != fmt.Sprint(n.ID) {
		t.Fatalf("notify-owner members %q", ws[0].Members)
	}
	rep, err := m.WakeStats(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Wakes != 4 || len(rep.Categories) != 4 || rep.Categories[0].Category != WakeJobDone {
		t.Fatalf("report: %+v", rep)
	}
}

// #170: the heartbeat wake is counted too.
func TestWakeHeartbeatCounted(t *testing.T) {
	e := newHeartbeat(t)
	e.answer(t, `{"wake": true, "reason": "go"}`)
	h, err := e.m.RunHeartbeat(context.Background(), HeartbeatTriggerDaemon)
	if err != nil {
		t.Fatal(err)
	}
	ws := wakesOf(t, e.m, WakeHeartbeat)
	if len(ws) != 1 || ws[0].Bytes != len(HeartbeatLine(h.ID)) || ws[0].Members != fmt.Sprint(h.ID) {
		t.Fatalf("heartbeat records: %+v", ws)
	}
}

// #170: jobs delivered as one coalesced line are one wake with the batch size of the jobs.
func TestWakeCoalescedBatchMembership(t *testing.T) {
	m := newManager(t)
	notifyTo(t, m)
	m.Cfg.NotifyCoalesceSeconds = 120
	now := coalesceClock(m)
	t0 := *now
	a := finishAt(t, m, now, t0, nil)
	b := finishAt(t, m, now, t0.Add(10*time.Second), nil)
	c := finishAt(t, m, now, t0.Add(20*time.Second), nil)
	notifyAt(t, m, now, t0.Add(10*time.Minute))
	ws := wakesOf(t, m, WakeJobDone)
	if len(ws) != 1 || ws[0].BatchSize != 3 {
		t.Fatalf("records: %+v", ws)
	}
	for _, id := range []string{a.ID, b.ID, c.ID} {
		if !strings.Contains(ws[0].Members, id) {
			t.Fatalf("members %q lack %s", ws[0].Members, id)
		}
	}
	rep, _ := m.WakeStats(time.Time{})
	if rep.Categories[0].Count != 1 || rep.Categories[0].Members != 3 || rep.Categories[0].MeanBatch != 3 {
		t.Fatalf("category: %+v", rep.Categories[0])
	}
}

// #170: a delivery tmux refused is not a wake; its retry that goes through is exactly one; the same
// delivery recorded again (its mark failed, the batch is tried once more) is not counted twice.
func TestWakeRetryNotDoubleCounted(t *testing.T) {
	m := newManager(t)
	t.Setenv("AGENT_JOBS_TMUX_BIN", "/usr/bin/false")
	m.Cfg.NotifyTmuxTarget = "fake-session"
	j := enqueue(t, m, "brief", nil)
	if err := m.Cancel(j.ID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := m.notifyPending(); err != nil {
			t.Fatal(err)
		}
	}
	if ws := wakesOf(t, m, WakeJobDone); len(ws) != 0 {
		t.Fatalf("failed deliveries counted: %+v", ws)
	}
	t.Setenv("AGENT_JOBS_TMUX_BIN", filepath.Join(fakeHelpr, "tmux"))
	logf := notifyTo(t, m)
	if err := m.notifyPending(); err != nil {
		t.Fatal(err)
	}
	if err := m.notifyPending(); err != nil {
		t.Fatal(err)
	}
	if ws := wakesOf(t, m, WakeJobDone); len(ws) != 1 || len(tmuxLines(t, logf)) != 1 {
		t.Fatalf("records %+v, lines %q", ws, tmuxLines(t, logf))
	}
	// The same delivery seen again.
	m.recordWake(WakeJobDone, WakeJobDone+":"+strings.Join(batchRefs([]*store.Job{get(t, m, j.ID)}), ","), []string{j.ID}, 10)
	if ws := wakesOf(t, m, WakeJobDone); len(ws) != 1 {
		t.Fatalf("same delivery counted twice: %+v", ws)
	}
	// A different delivery of the same category still counts.
	m.recordWake(WakeJobDone, WakeJobDone+":other", []string{"other"}, 10)
	if ws := wakesOf(t, m, WakeJobDone); len(ws) != 2 {
		t.Fatalf("second delivery lost: %+v", ws)
	}
}

// #170: per-day counts and the nearest-rank p50 / p95 of the bytes per delivery.
func TestWakeStatsPercentilesAndDays(t *testing.T) {
	m := newManager(t)
	day1 := time.Date(2026, 10, 19, 12, 0, 0, 0, m.loc())
	day2 := day1.Add(24 * time.Hour)
	for i := 1; i <= 20; i++ {
		at := day1
		if i > 15 {
			at = day2
		}
		if _, err := m.Store.AddWake(store.Wake{At: at, Kind: store.WakeDelivery, Category: WakeJobDone, Bytes: i * 10, BatchSize: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.Store.AddWake(store.Wake{At: day1, Kind: store.WakeInjection, Category: InjectBriefReminder, Bytes: 7}); err != nil {
		t.Fatal(err)
	}
	rep, err := m.WakeStats(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Days) != 2 || rep.Days[0].Counts[WakeJobDone] != 15 || rep.Days[1].Counts[WakeJobDone] != 5 ||
		rep.Days[0].Counts[InjectBriefReminder] != 1 {
		t.Fatalf("days: %+v", rep.Days)
	}
	c := rep.Categories[0]
	// 20 values 10..200: nearest rank p50 = 10th = 100, p95 = 19th = 190.
	if c.Category != WakeJobDone || c.Count != 20 || c.BytesP50 != 100 || c.BytesP95 != 190 || c.BytesMax != 200 || c.Bytes != 2100 {
		t.Fatalf("category: %+v", c)
	}
	if rep.Wakes != 20 || rep.InjectionBytes != 7 || rep.Categories[1].Kind != store.WakeInjection || rep.Categories[1].BytesP50 != 7 {
		t.Fatalf("report: %+v", rep)
	}
	since, err := m.WakeStats(day2)
	if err != nil || since.Wakes != 5 {
		t.Fatalf("since: %+v %v", since, err)
	}
}

// #170: hook injections are counted by their bytes, from a manager and from a hook's own short write.
func TestWakeInjectionsCounted(t *testing.T) {
	m := newManager(t)
	m.RecordInjection(InjectPendingOutcome, "twelve bytes")
	m.RecordInjection(InjectPendingOutcome, "")
	if err := RecordHookInjection("test", InjectBriefReminder, "abc", time.Now()); err != nil {
		t.Fatal(err)
	}
	rep, err := m.WakeStats(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Wakes != 0 || rep.InjectionBytes != 15 || len(rep.Categories) != 2 {
		t.Fatalf("report: %+v", rep)
	}
	if rep.Categories[0].Category != InjectBriefReminder || rep.Categories[1].Bytes != 12 {
		t.Fatalf("categories: %+v", rep.Categories)
	}
}

func TestSLORecordShow(t *testing.T) {
	m := newManager(t)
	m.Cfg.SLO.Targets = map[string]float64{"deploy": 15, "first_reply": 60}
	for i := 1; i <= 20; i++ {
		if _, err := m.RecordSLO("deploy", float64(i)); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := m.SLOShow(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows: %+v", rows)
	}
	d, f := rows[0], rows[1]
	if d.Name != "deploy" || d.Count != 20 || d.P50 != 10 || d.P95 != 19 || d.Last != 20 || d.Target != 15 || d.Over != 5 {
		t.Fatalf("deploy: %+v", d)
	}
	if f.Name != "first_reply" || f.Count != 0 || f.Target != 60 || !f.LastAt.IsZero() {
		t.Fatalf("first_reply: %+v", f)
	}
	for _, bad := range []struct {
		name string
		secs float64
	}{{"Bad Name", 1}, {"", 1}, {"deploy", -1}, {"deploy", 1e12}} {
		if _, err := m.RecordSLO(bad.name, bad.secs); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
}

// #170: a breach rides on the next delivery as one line, never as a wake of its own, is mentioned
// once, folds a burst of one name into one entry, and survives a delivery that did not go through.
func TestSLOBreachNoteDeduplicated(t *testing.T) {
	m, _, logf := scheduleManager(t, time.UTC)
	m.Cfg.SLO.Targets = map[string]float64{"deploy": 600, "first_reply": 60}
	for _, s := range []struct {
		name string
		v    float64
	}{{"deploy", 700}, {"deploy", 900}, {"first_reply", 59}, {"first_reply", 90}, {"other", 5000}} {
		if _, err := m.RecordSLO(s.name, s.v); err != nil {
			t.Fatal(err)
		}
	}
	if got := tmuxLines(t, logf); len(got) != 0 {
		t.Fatalf("recording typed into the session: %q", got)
	}
	if ws := wakesOf(t, m, WakeNotifyOwner); len(ws) != 0 {
		t.Fatal("recording is a wake")
	}
	// A refused delivery keeps the breaches pending.
	t.Setenv("AGENT_JOBS_TMUX_BIN", "/usr/bin/false")
	if _, err := m.NotifyOwner("", "first", ""); err == nil {
		t.Fatal("expected the refused delivery to fail")
	}
	if bs, _ := m.Store.UnnotedSLOBreaches(); len(bs) != 3 {
		t.Fatalf("breaches after a failed delivery: %+v", bs)
	}
	t.Setenv("AGENT_JOBS_TMUX_BIN", filepath.Join(fakeHelpr, "tmux"))
	logf = filepath2(t, m)
	if _, err := m.NotifyOwner("", "second", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.NotifyOwner("", "third", ""); err != nil {
		t.Fatal(err)
	}
	got := tmuxLines(t, logf)
	if len(got) != 2 {
		t.Fatalf("lines: %q", got)
	}
	want := " · slo breach: deploy 2x, worst 900s > 600s; first_reply 90s > 60s"
	if !strings.HasSuffix(got[0], want) || !strings.Contains(got[0], "second") {
		t.Fatalf("first line:\n got %q\nwant suffix %q", got[0], want)
	}
	if strings.Contains(got[1], "slo breach") {
		t.Fatalf("breach mentioned twice: %q", got[1])
	}
	// The line counted as typed includes the note.
	ws := wakesOf(t, m, WakeNotifyOwner)
	if len(ws) != 2 || ws[0].Bytes != len(got[0]) || ws[1].Bytes != len(got[1]) {
		t.Fatalf("records: %+v for %q", ws, got[0])
	}
	// A later breach is noted on its own.
	if _, err := m.RecordSLO("deploy", 601); err != nil {
		t.Fatal(err)
	}
	if _, err := m.NotifyOwner("", "fourth", ""); err != nil {
		t.Fatal(err)
	}
	if got = tmuxLines(t, logf); !strings.HasSuffix(got[2], " · slo breach: deploy 601s > 600s") {
		t.Fatalf("later breach: %q", got)
	}
}

// filepath2 points the fake tmux at a fresh log and returns its path.
func filepath2(t *testing.T, m *Manager) string {
	t.Helper()
	return notifyTo(t, m)
}

// #170: a duration within its norm, or of a name without a norm, is no breach.
func TestSLOWithinNormNoBreach(t *testing.T) {
	m := newManager(t)
	m.Cfg.SLO.Targets = map[string]float64{"deploy": 600}
	for _, s := range []struct {
		name string
		v    float64
		want bool
	}{{"deploy", 600, false}, {"deploy", 601, true}, {"free", 99999, false}} {
		r, err := m.RecordSLO(s.name, s.v)
		if err != nil || r.Breach != s.want {
			t.Fatalf("%+v: %+v %v", s, r, err)
		}
	}
	if bs, _ := m.Store.UnnotedSLOBreaches(); len(bs) != 1 {
		t.Fatalf("breaches: %+v", bs)
	}
}

func researchJobs(t *testing.T, m *Manager) []*store.Job {
	t.Helper()
	all, err := m.Store.ListByStatus(store.StatusQueued)
	if err != nil {
		t.Fatal(err)
	}
	var out []*store.Job
	for _, j := range all {
		if j.Kind == "research" {
			out = append(out, j)
		}
	}
	return out
}

// #170: slo.investigate is off by default; on, it starts one research job per breach, not within the
// cooldown of the last one and not past the daily cap; a skipped breach is never revisited.
func TestSLOInvestigateCapped(t *testing.T) {
	m := newManager(t)
	now := coalesceClock(m)
	t0 := *now
	m.Cfg.SLO.Targets = map[string]float64{"deploy": 600}
	record := func(at time.Time, v float64) {
		*now = at
		if _, err := m.RecordSLO("deploy", v); err != nil {
			t.Fatal(err)
		}
	}
	record(t0, 700)
	if err := m.sloInvestigate(); err != nil || len(researchJobs(t, m)) != 0 {
		t.Fatalf("investigated while off: %v %d", err, len(researchJobs(t, m)))
	}
	m.Cfg.SLO.Investigate = SLOInvestigateConfig{Enabled: true, CooldownMinutes: 10, DailyMax: 2}
	if err := m.sloInvestigate(); err != nil {
		t.Fatal(err)
	}
	if js := researchJobs(t, m); len(js) != 1 || !strings.Contains(js[0].Title, "deploy") {
		t.Fatalf("first breach: %+v", js)
	}
	// Inside the cooldown: no job, the breach is closed with the reason.
	record(t0.Add(time.Minute), 800)
	*now = t0.Add(time.Minute)
	if err := m.sloInvestigate(); err != nil || len(researchJobs(t, m)) != 1 {
		t.Fatalf("cooldown: %v %d", err, len(researchJobs(t, m)))
	}
	// A tick again starts nothing for the closed breach.
	*now = t0.Add(30 * time.Minute)
	if err := m.sloInvestigate(); err != nil || len(researchJobs(t, m)) != 1 {
		t.Fatalf("revisited: %v %d", err, len(researchJobs(t, m)))
	}
	// Past the cooldown: the second job of the day; then the cap holds.
	record(t0.Add(40*time.Minute), 900)
	if err := m.sloInvestigate(); err != nil || len(researchJobs(t, m)) != 2 {
		t.Fatalf("second: %v %d", err, len(researchJobs(t, m)))
	}
	record(t0.Add(2*time.Hour), 950)
	if err := m.sloInvestigate(); err != nil || len(researchJobs(t, m)) != 2 {
		t.Fatalf("cap: %v %d", err, len(researchJobs(t, m)))
	}
	bs, err := m.Store.UninvestigatedSLOBreaches()
	if err != nil || len(bs) != 0 {
		t.Fatalf("open breaches: %+v %v", bs, err)
	}
	all, _ := m.Store.UnnotedSLOBreaches()
	reasons := map[int]string{}
	for i, b := range all {
		reasons[i] = b.InvestigateSkip
	}
	if !strings.HasPrefix(reasons[1], "cooldown") || !strings.HasPrefix(reasons[3], "daily cap") || reasons[0] != "" || reasons[2] != "" {
		t.Fatalf("reasons: %v", reasons)
	}
	// A day later the cap has room again.
	record(t0.Add(27*time.Hour), 1000)
	if err := m.sloInvestigate(); err != nil || len(researchJobs(t, m)) != 3 {
		t.Fatalf("next day: %v %d", err, len(researchJobs(t, m)))
	}
}

func TestSLOConfigValidation(t *testing.T) {
	for name, c := range map[string]SLOConfig{
		"zero target":     {Targets: map[string]float64{"deploy": 0}},
		"negative target": {Targets: map[string]float64{"deploy": -5}},
		"bad name":        {Targets: map[string]float64{"Deploy Time": 5}},
		"cooldown":        {Investigate: SLOInvestigateConfig{CooldownMinutes: -1}},
		"daily":           {Investigate: SLOInvestigateConfig{DailyMax: -1}},
		"complexity":      {Investigate: SLOInvestigateConfig{Complexity: "huge"}},
	} {
		if err := c.validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	ok := SLOConfig{Targets: map[string]float64{"deploy": 600, "first_reply": 60}, Investigate: SLOInvestigateConfig{Enabled: true, Complexity: "light"}}
	if err := ok.validate(); err != nil {
		t.Fatal(err)
	}
}
