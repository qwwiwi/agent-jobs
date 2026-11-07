package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// A result whose cost is missing, null or negative keeps the call at its whole budget (cost unknown):
// a zero or negative cost would release the reservation the caps count.
func TestAdvisorCostMissingOrInvalidKeepsReservation(t *testing.T) {
	for _, c := range []struct{ name, out string }{
		{"missing", `{"type":"result","subtype":"success","is_error":false,"num_turns":1,"result":"ok"}`},
		{"null", `{"type":"result","subtype":"success","is_error":false,"num_turns":1,"total_cost_usd":null,"result":"ok"}`},
		{"negative", `{"type":"result","subtype":"success","is_error":false,"num_turns":1,"total_cost_usd":-5,"result":"ok"}`},
		{"failed without cost", `{"type":"result","subtype":"error_max_turns","is_error":true,"num_turns":12}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newAdvisor(t)
			j := e.crewJob(t, 2, 1.0)
			if err := os.WriteFile(filepath.Join(e.dir, "out"), []byte(c.out), 0o600); err != nil {
				t.Fatal(err)
			}
			_, _ = e.m.AskAdvisor(context.Background(), j, "q", "")
			calls, err := e.m.Store.AdvisorCalls(j.ID, e.m.Now(), advisorStaleAfter)
			if err != nil || len(calls) != 1 {
				t.Fatalf("calls: %+v %v", calls, err)
			}
			if !calls[0].CostUnknown || calls[0].CostUSD != 0 {
				t.Fatalf("call recorded at a known cost: %+v", calls[0])
			}
			if s := store.AdvisorSpend(calls); s != calls[0].BudgetUSD || s <= 0 {
				t.Fatalf("spend %v, want the call's budget %v", s, calls[0].BudgetUSD)
			}
		})
	}
}

// Each call gets an equal share of the budget left over the calls left, so one call whose cost is
// unknown (a timeout, a lost row) does not use up the whole crew budget.
func TestAdvisorBudgetSharedAcrossCalls(t *testing.T) {
	e := newAdvisor(t)
	j := e.crewJob(t, 2, 2.0)
	if err := os.WriteFile(filepath.Join(e.dir, "out"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.AskAdvisor(context.Background(), j, "q1", ""); err == nil {
		t.Fatal("a call without a JSON result succeeded")
	}
	if b, _ := argAfter(e.argv(t, 1), "--max-budget-usd"); b != "1" {
		t.Fatalf("first call budget %q, want 1 (2.0 over 2 calls)", b)
	}
	e.answer(t, "success", "ok", 1, 0.3)
	a, err := e.m.AskAdvisor(context.Background(), j, "q2", "")
	if err != nil {
		t.Fatalf("second call after a failure at its share: %v", err)
	}
	if b, _ := argAfter(e.argv(t, 2), "--max-budget-usd"); b != "1" {
		t.Fatalf("second call budget %q, want 1 (the budget left)", b)
	}
	if a.CallsLeft != 0 || a.BudgetLeftUSD < 0.699 || a.BudgetLeftUSD > 0.701 {
		t.Fatalf("left: %+v", a)
	}
}

// The advisor runs with the runner's environment hardening: the job marker, the job's temp dir, the
// job's git environment, no fork subagents, no user bus, no inherited GIT_* variables.
func TestAdvisorRunnerEnvironment(t *testing.T) {
	e := newAdvisor(t)
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/nonexistent/bus")
	t.Setenv("XDG_RUNTIME_DIR", "/nonexistent/run")
	t.Setenv("GIT_DIR", "/nonexistent/git")
	t.Setenv(ForkSubagentEnvVar, "1")
	t.Setenv("TMPDIR", "/nonexistent/tmp")
	j := e.crewJob(t, 2, 1.0)
	if _, err := e.m.AskAdvisor(context.Background(), j, "q", ""); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(e.dir, "env.1"))
	if err != nil {
		t.Fatal(err)
	}
	env := strings.Split(strings.TrimSpace(string(b)), "\n")
	jobDir, _ := filepath.Abs(e.m.JobDir(j.ID))
	for _, want := range []string{JobEnvVar + "=" + j.ID, ForkSubagentEnvVar + "=0",
		"TMPDIR=" + filepath.Join(jobDir, JobTmpDir), "GIT_CONFIG_GLOBAL=" + filepath.Join(jobDir, gitConfigFile)} {
		if !slices.Contains(env, want) {
			t.Errorf("advisor env lacks %s", want)
		}
	}
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if k == "DBUS_SESSION_BUS_ADDRESS" || k == "XDG_RUNTIME_DIR" || k == "GIT_DIR" || kv == ForkSubagentEnvVar+"=1" {
			t.Errorf("advisor env keeps %s", kv)
		}
	}
}

// In a cgroup scope systemd-run needs the user bus, the advisor does not get it: the scope command
// reaches claude through env -u, with the hardened environment.
func TestAdvisorScopeCommandDropsUserBus(t *testing.T) {
	e := newAdvisor(t)
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/nonexistent/bus")
	j := e.crewJob(t, 2, 1.0)
	call := &store.AdvisorCall{JobID: j.ID, N: 1, Model: "opus", BudgetUSD: 1}
	argv, env, err := e.m.advisorCommand(j, call, e.m.AdvisorDir(j.ID), "agent-jobs-test-x.scope")
	if err != nil {
		t.Fatal(err)
	}
	i := slices.Index(argv, "--")
	if argv[0] != "systemd-run" || i < 0 || !slices.Equal(argv[i+1:i+6], []string{"env", "-u", "DBUS_SESSION_BUS_ADDRESS", "-u", "XDG_RUNTIME_DIR"}) ||
		argv[i+6] != e.m.Config().claudeBin() {
		t.Fatalf("scope argv: %q", argv)
	}
	if !slices.Contains(env, "DBUS_SESSION_BUS_ADDRESS=unix:path=/nonexistent/bus") || !slices.Contains(env, JobEnvVar+"="+j.ID) {
		t.Fatalf("scope env: %q", env)
	}
}

// An advisor call ends with its job: once the job leaves running (cancelled, timed out) the call is
// killed instead of running on to its own timeout, and it is recorded as failed.
func TestAdvisorStopsWithJob(t *testing.T) {
	e := newAdvisor(t)
	old := advisorWatchEvery
	advisorWatchEvery = 50 * time.Millisecond
	t.Cleanup(func() { advisorWatchEvery = old })
	j := e.crewJob(t, 2, 1.0)
	if err := os.WriteFile(filepath.Join(e.dir, "slow"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := e.m.AskAdvisor(context.Background(), j, "q", "")
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for e.calls() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if err := e.m.Store.Finish(j.ID, "", store.StatusCancelled, 0, 0, 0, "cancelled", e.m.Now()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "left running") {
			t.Fatalf("call of a cancelled job: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the advisor call outlived its job")
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("the call ended after %s", d)
	}
	calls, err := e.m.Store.AdvisorCalls(j.ID, e.m.Now(), advisorStaleAfter)
	if err != nil || len(calls) != 1 || calls[0].Outcome != store.AdvisorFailed || !calls[0].CostUnknown {
		t.Fatalf("recorded: %+v %v", calls, err)
	}
}

// The main cost of a crew view and of a pair side counts every attempt of a resumed job, and says
// when part of it is unknown.
func TestCrewMainCostCountsEarlierAttempts(t *testing.T) {
	e := newAdvisor(t)
	j := e.crewJob(t, 2, 1.0)
	if err := e.m.Store.FinishAttempt(j.ID, "tok", store.StatusFailed, 5, 0.7, false, nil, 60, "x", e.m.Now()); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Store.Requeue(j.ID, store.ResumeLimits{MaxTurns: j.MaxTurns, MaxBudgetUSD: j.MaxBudgetUSD, TimeoutMinutes: j.TimeoutMinutes}, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Store.Claim(j.ID, "tok2", e.m.Now()); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Store.FinishAttempt(j.ID, "tok2", store.StatusFailed, 2, 0, true, nil, 30, "killed", e.m.Now()); err != nil {
		t.Fatal(err)
	}
	j, err := e.m.Store.Get(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	v, err := e.m.Crew(j)
	if err != nil {
		t.Fatal(err)
	}
	if v.CostMainUSD != 0.7 || !v.CostMainUnknown || v.CostTotalUSD != 0.7 {
		t.Fatalf("crew view: main %v unknown %v total %v", v.CostMainUSD, v.CostMainUnknown, v.CostTotalUSD)
	}
	if tl := CrewTimeline(v); len(tl) == 0 || !strings.Contains(tl[0], "main cost partly unknown") {
		t.Fatalf("timeline: %q", tl)
	}
	s, err := e.m.pairSide(j)
	if err != nil || s.CostMainUSD != 0.7 || !s.CostUnknown {
		t.Fatalf("pair side: %+v %v", s, err)
	}
}

// job_stats reports the advisor spend of the crew jobs in the window next to the totals, which sum
// only the jobs' own cost.
func TestStatsAdvisorCost(t *testing.T) {
	e := newAdvisor(t)
	j := e.crewJob(t, 2, 1.0)
	if _, err := e.m.AskAdvisor(context.Background(), j, "q", ""); err != nil {
		t.Fatal(err)
	}
	other := e.crewJob(t, 2, 1.0)
	if _, err := e.m.AskAdvisor(context.Background(), other, "q", ""); err != nil {
		t.Fatal(err)
	}
	got, err := e.m.crewAdvisorCost(context.Background(), []*store.JobStats{{ID: j.ID}})
	if err != nil || got != 0.1 {
		t.Fatalf("advisor cost in the window: %v %v, want 0.1 (only the job in the window)", got, err)
	}
}

// Prune removes a pruned job's advisor dir (<state>/advisor/<id>).
func TestPruneRemovesAdvisorDir(t *testing.T) {
	e := newAdvisor(t)
	j := e.crewJob(t, 2, 1.0)
	if _, err := e.m.AskAdvisor(context.Background(), j, "q", ""); err != nil {
		t.Fatal(err)
	}
	old := e.m.Now().Add(-60 * 24 * time.Hour)
	if err := e.m.Store.Finish(j.ID, "tok", store.StatusSucceeded, 1, 0.1, 1, "", old); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.m.AdvisorDir(j.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Prune(true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.m.AdvisorDir(j.ID)); err != nil {
		t.Fatalf("a dry run removed the advisor dir: %v", err)
	}
	rep, err := e.m.Prune(false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.m.AdvisorDir(j.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("advisor dir after prune: %v (report %+v)", err, rep)
	}
}

// The crew job's MCP server learns it may offer advisor_ask from its command line, not from an env
// block the client has to merge.
func TestCrewMCPConfigUsesArgs(t *testing.T) {
	e := newAdvisor(t)
	crew := e.crewJob(t, 2, 1)
	srv := e.m.jobMCPConfigFor(crew)["mcpServers"].(map[string]any)[JobMCPServerName].(map[string]any)
	if args, _ := srv["args"].([]string); !slices.Contains(args, JobMCPAdvisorFlag) {
		t.Fatalf("crew job mcp args: %v", srv["args"])
	}
	if _, ok := srv["env"]; ok {
		t.Fatalf("crew job mcp config still sets env: %v", srv)
	}
	plain := running(t, e.m)
	if strings.Contains(mustJSON(t, e.m.jobMCPConfigFor(plain)), JobMCPAdvisorFlag) {
		t.Fatal("plain job mcp config offers the advisor")
	}
}
