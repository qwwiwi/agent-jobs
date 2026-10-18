package jobs

import (
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// A resumed attempt runs in a new cgroup scope exactly as a fresh job does (cgroup_scope auto): the
// stop ends the first attempt's scope, the next attempt gets the same unit name again, its wrapper
// lives in that scope and starts with the job environment (brain token dropped), and the scope is
// gone once the attempt is collected.
func TestResumeRunsInNewCgroupScope(t *testing.T) {
	requireUserScope(t)
	m := newManager(t)
	t.Setenv("AGENT_JOBS_CGROUP_SCOPE", ScopeAuto)
	sessionsHome(t, true)
	m.Cfg.Brain.TokenEnv = "AJ_TEST_BRAIN_TOKEN"
	t.Setenv("AJ_TEST_BRAIN_TOKEN", "secret-value")
	j := enqueue(t, m, "FAKE_WIP FAKE_SLEEP_ONCE=30", nil)
	t.Cleanup(func() { _ = m.stopScope(j.ID) })
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	unit := ScopeUnit("test", j.ID)
	if got := get(t, m, j.ID); got.ScopeUnit != unit {
		t.Fatalf("first attempt: scope %q, want %q", got.ScopeUnit, unit)
	}
	waitFor(t, "work in progress", 15*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(m.JobDir(j.ID), "work", "wip.txt"))
		return err == nil
	})
	st, err := m.Stop(j.ID, "switch plan")
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != store.StatusStopped || st.ScopePending {
		t.Fatalf("stopped row: %s pending=%v %q", st.Status, st.ScopePending, st.Error)
	}
	if s, _, err := scopeState(unit); err != nil || s != "inactive" {
		t.Fatalf("scope after stop: %q %v", s, err)
	}
	if _, err := m.Resume(j.ID, "carry on FAKE_SLEEP=3", Limits{MaxBudgetUSD: 4}); err != nil {
		t.Fatal(err)
	}
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	r := get(t, m, j.ID)
	if r.Status != store.StatusRunning || r.Attempt != 2 || r.ScopeUnit != unit {
		t.Fatalf("resumed row: %s attempt %d scope %q", r.Status, r.Attempt, r.ScopeUnit)
	}
	// systemd-run creates the unit and moves itself into it right after the spawn returns.
	waitFor(t, "resumed wrapper in its scope", 15*time.Second, func() bool {
		s, _, err := scopeState(unit)
		cg, _ := procCgroup(r.PID)
		return err == nil && s == "active" && strings.HasSuffix(cg, "/"+unit)
	})
	env, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(r.PID), "environ"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(env), "AJ_TEST_BRAIN_TOKEN=") {
		t.Fatal("the resumed wrapper got the brain token variable")
	}
	waitFor(t, "exit_code", 20*time.Second, func() bool {
		_, ok := readExitCode(m.JobDir(j.ID))
		return ok
	})
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if done := get(t, m, j.ID); done.Status != store.StatusSucceeded || done.RunMode != store.RunResume {
		t.Fatalf("after the resumed attempt: %s %s %q", done.Status, done.RunMode, done.Error)
	}
	if s, _, err := scopeState(unit); err != nil || s != "inactive" {
		t.Fatalf("scope after the resumed attempt: %q %v", s, err)
	}
}

// A job has one job_stats row whatever the number of attempts: it is rewritten at every attempt end
// (stopped, failed, succeeded) with the totals of all attempts so far, so a resume never counts an
// attempt twice, and the archive taken by prune (attempts/ included) yields the same totals.
func TestStoppedResumedJobStatsNotDoubleCounted(t *testing.T) {
	m := newManager(t)
	sessionsHome(t, true)
	st := startAndStop(t, m, nil) // attempt 1: 1 turn, cost unknown, no tools
	s1, err := m.Store.GetStats(st.ID)
	if err != nil {
		t.Fatalf("a stopped job must have its stats row: %v", err)
	}
	if s1.Status != store.StatusStopped || s1.Turns != 1 || s1.CostUSD != 0 {
		t.Fatalf("stats after stop: %+v", s1)
	}

	// Attempt 2: tools, no report (failed, resumable): 2 turns, $0.01.
	if _, err := m.Resume(st.ID, "FAKE_TOOLS FAKE_NOREPORT", Limits{MaxBudgetUSD: 4}); err != nil {
		t.Fatal(err)
	}
	if j := runToEnd(t, m, st.ID); j.Status != store.StatusFailed {
		t.Fatalf("attempt 2: %s %q", j.Status, j.Error)
	}
	// A partial report the job left (kept as report.attempt-2.md once attempt 3 runs).
	if err := os.WriteFile(filepath.Join(m.JobDir(st.ID), "report.md"), []byte("## Summary\npartial\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Attempt 3: tools again, succeeds: 3 turns, $0.25.
	if _, err := m.Resume(st.ID, "FAKE_TOOLS", Limits{MaxBudgetUSD: 2}); err != nil {
		t.Fatal(err)
	}
	if j := runToEnd(t, m, st.ID); j.Status != store.StatusSucceeded {
		t.Fatalf("attempt 3: %s %q", j.Status, j.Error)
	}
	check := func(when string, wantSource string) {
		t.Helper()
		s, err := m.Store.GetStats(st.ID)
		if err != nil {
			t.Fatalf("%s: %v", when, err)
		}
		if s.Status != store.StatusSucceeded || s.Turns != 6 || math.Abs(s.CostUSD-0.26) > 1e-9 ||
			s.ToolCalls["Bash"] != 2 || s.ToolCalls["Skill"] != 2 || s.Skills["deploy"] != 2 || s.GuardRefusals != 2 ||
			s.Source != wantSource {
			t.Fatalf("%s: stats %+v", when, s)
		}
		rep, err := m.Stats(time.Time{}, "kind")
		if err != nil {
			t.Fatal(err)
		}
		if rep.Totals.Jobs != 1 || rep.Totals.ByStatus[store.StatusSucceeded] != 1 || rep.Totals.Turns != 6 ||
			math.Abs(rep.Totals.CostTotalUSD-0.26) > 1e-9 {
			t.Fatalf("%s: summary %+v", when, rep.Totals)
		}
	}
	check("after attempt 3", "job_dir")
	// A full backfill recomputes the same row.
	if _, err := m.BackfillStats(true); err != nil {
		t.Fatal(err)
	}
	check("after backfill", "job_dir")

	// Prune (48 h later) archives every attempt's stream; the stats come back the same from the archive.
	real := m.Now
	m.Now = func() time.Time { return real().Add(PruneAge + time.Hour) }
	if _, err := m.Prune(false); err != nil {
		t.Fatal(err)
	}
	j := get(t, m, st.ID)
	adir := m.archiveJobDir(j)
	for _, rel := range []string{"stream.jsonl.gz", "attempts/1/stream.jsonl.gz", "attempts/2/stream.jsonl.gz",
		"attempts/2/resume.md", AttemptReportName(2)} {
		if _, err := os.Lstat(filepath.Join(adir, rel)); err != nil {
			t.Fatalf("archive lacks %s: %v", rel, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(m.JobDir(st.ID), attemptsDir)); err == nil {
		t.Fatal("prune left attempts/ in the job folder after archiving it")
	}
	if _, err := m.BackfillStats(true); err != nil {
		t.Fatal(err)
	}
	check("from the archive", "archive")
}

// The archive alone never takes a stopped job's records: they are what its resume continues from.
func TestArchiveSkipsStoppedJob(t *testing.T) {
	m := newManager(t)
	sessionsHome(t, true)
	st := startAndStop(t, m, nil)
	real := m.Now
	m.Now = func() time.Time { return real().Add(PruneAge + time.Hour) }
	rep, err := m.Archive(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Jobs) != 0 {
		t.Fatalf("archived a stopped job: %+v", rep.Jobs)
	}
	if _, err := os.Lstat(filepath.Join(m.JobDir(st.ID), "stream.jsonl")); err != nil {
		t.Fatalf("stopped job's stream moved: %v", err)
	}
}
