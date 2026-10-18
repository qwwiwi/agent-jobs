package jobs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// fakeHeartbeatClaude stands in for the decision call: it records each call (argv, stdin, env) in
// $FAKE_HB_DIR, sleeps when $FAKE_HB_DIR/sleep exists, waits for $FAKE_HB_DIR/go when
// $FAKE_HB_DIR/hold exists (touching held first), and prints $FAKE_HB_DIR/out.
const fakeHeartbeatClaude = `#!/bin/bash
d="$FAKE_HB_DIR"
echo call >> "$d/calls"
printf '%s\n' "$@" > "$d/argv"
cat > "$d/stdin"
env > "$d/env"
if [[ -f "$d/sleep" ]]; then sleep 30; fi
if [[ -f "$d/hold" ]]; then touch "$d/held"; while [[ ! -f "$d/go" ]]; do sleep 0.05; done; fi
cat "$d/out" 2>/dev/null
`

// hbNoon is a day-time instant (outside the default night window 00-08).
var hbNoon = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

type hbEnv struct {
	m    *Manager
	dir  string // fake claude records
	now  *time.Time
	tmux string // fake tmux log
}

// newHeartbeat is a manager with the heartbeat on, a goals file, the fake decision binary, a fixed
// UTC clock at noon and a notify target on the fake tmux.
func newHeartbeat(t *testing.T) *hbEnv {
	t.Helper()
	m := newManager(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte(fakeHeartbeatClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_JOBS_CLAUDE_BIN", bin)
	t.Setenv("FAKE_HB_DIR", dir)
	tmux := filepath.Join(dir, "tmux.log")
	t.Setenv("FAKE_TMUX_LOG", tmux)
	goals := filepath.Join(t.TempDir(), "goals.md")
	if err := os.WriteFile(goals, []byte("# Goals\n- GOAL_MARKER: keep the release notes current\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m.Cfg.Heartbeat = HeartbeatConfig{Enabled: true, GoalsFile: goals}
	m.Cfg.NotifyTmuxTarget = "orch"
	m.Loc = time.UTC
	now := hbNoon
	m.Now = func() time.Time { return now }
	return &hbEnv{m: m, dir: dir, now: &now, tmux: tmux}
}

// answer makes the fake decision call print a claude JSON result with the given result text.
func (e *hbEnv) answer(t *testing.T, result string) {
	t.Helper()
	b := `{"type":"result","subtype":"success","is_error":false,"num_turns":1,"total_cost_usd":0.0123,"result":` + jsonString(result) + `}`
	if err := os.WriteFile(filepath.Join(e.dir, "out"), []byte(b), 0o600); err != nil {
		t.Fatal(err)
	}
}

func jsonString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return `"` + r.Replace(s) + `"`
}

func (e *hbEnv) calls() int {
	b, err := os.ReadFile(filepath.Join(e.dir, "calls"))
	if err != nil {
		return 0
	}
	return strings.Count(string(b), "call")
}

// decision stores a finished decision claimed at at.
func (e *hbEnv) decision(t *testing.T, at time.Time, decision string) {
	t.Helper()
	h, err := e.m.Store.ClaimHeartbeat(HeartbeatTriggerDaemon, at, store.HeartbeatClaimLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.m.Store.FinishHeartbeat(h.ID, decision, "r", 0, "", at); err != nil {
		t.Fatal(err)
	}
}

func (e *hbEnv) gate(t *testing.T) HeartbeatGateResult {
	t.Helper()
	g, err := e.m.HeartbeatGate(*e.now, false)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// The heartbeat is off unless config.json turns it on: no gate, no decision, no model call.
func TestHeartbeatDisabledByDefault(t *testing.T) {
	m := newManager(t)
	if m.Cfg.Heartbeat.Enabled {
		t.Fatal("heartbeat on by default")
	}
	if got := m.Cfg.Heartbeat.GoalsFile; got != filepath.Join(m.StateDir, DefaultHeartbeatGoalsFile) {
		t.Fatalf("default goals file %q", got)
	}
	hc := m.Cfg.Heartbeat
	if hc.Interval() != time.Hour || hc.MaxWakes() != 6 || hc.DecisionModel() != "sonnet" || hc.Budget() != 0.2 ||
		hc.Timeout() != time.Minute {
		t.Fatalf("defaults: %v %d %s %g %v", hc.Interval(), hc.MaxWakes(), hc.DecisionModel(), hc.Budget(), hc.Timeout())
	}
	if s, e := hc.Night(); s != 0 || e != 8 {
		t.Fatalf("night %d-%d", s, e)
	}
	dir := t.TempDir()
	t.Setenv("FAKE_HB_DIR", dir)
	if m.StartHeartbeat(context.Background()) {
		t.Fatal("StartHeartbeat ran while off")
	}
	if _, err := m.RunHeartbeat(context.Background(), HeartbeatTriggerDaemon); !IsHeartbeatGateClosed(err) ||
		!strings.Contains(err.Error(), "off") {
		t.Fatalf("run while off: %v", err)
	}
	if h, err := m.Store.LastHeartbeat(); err != nil || h != nil {
		t.Fatalf("decision stored while off: %+v %v", h, err)
	}
	if r, _, err := m.Store.HeartbeatGate(); err != nil || r != "" {
		t.Fatalf("gate recorded while off: %q %v", r, err)
	}
	st, err := m.Heartbeat()
	if err != nil || st.Enabled || HeartbeatText(st) != "off (heartbeat.enabled is false)" {
		t.Fatalf("status: %+v %v", st, err)
	}
}

// Every gate reason, in the gate's order; each is cheap (no model call) and only the daemon's last
// result is recorded.
func TestHeartbeatGateReasons(t *testing.T) {
	e := newHeartbeat(t)
	m := e.m
	if g := e.gate(t); !g.OK || g.Reason != "eligible" {
		t.Fatalf("fresh gate: %+v", g)
	}

	goals := m.Cfg.Heartbeat.GoalsFile
	m.Cfg.Heartbeat.GoalsFile = filepath.Join(t.TempDir(), "none.md")
	if g := e.gate(t); g.OK || g.Reason != "idle: goals file missing" {
		t.Fatalf("missing goals: %+v", g)
	}
	empty := filepath.Join(t.TempDir(), "empty.md")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	m.Cfg.Heartbeat.GoalsFile = empty
	if g := e.gate(t); g.OK || g.Reason != "idle: goals file empty" {
		t.Fatalf("empty goals: %+v", g)
	}
	m.Cfg.Heartbeat.GoalsFile = goals

	*e.now = time.Date(2026, 10, 18, 3, 0, 0, 0, time.UTC)
	if g := e.gate(t); g.OK || g.Reason != "night window 00:00-08:00" ||
		!g.NextEligible.Equal(time.Date(2026, 10, 18, 8, 0, 0, 0, time.UTC)) {
		t.Fatalf("night: %+v", g)
	}
	*e.now = hbNoon

	h, err := m.Store.ClaimHeartbeat(HeartbeatTriggerDaemon, hbNoon.Add(-time.Minute), store.HeartbeatClaimLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if g := e.gate(t); g.OK || !strings.HasPrefix(g.Reason, "a heartbeat decision is running") {
		t.Fatalf("running: %+v", g)
	}
	if err := m.Store.FinishHeartbeat(h.ID, store.HeartbeatWake, "r", 0, "", hbNoon); err != nil {
		t.Fatal(err)
	}
	if g := e.gate(t); g.OK || !strings.HasPrefix(g.Reason, "interval: last wake") ||
		!g.NextEligible.Equal(hbNoon.Add(59*time.Minute)) {
		t.Fatalf("interval since wake: %+v", g)
	}
	*e.now = hbNoon.Add(2 * time.Hour)
	e.decision(t, e.now.Add(-10*time.Minute), store.HeartbeatSleep)
	if g := e.gate(t); g.OK || !strings.HasPrefix(g.Reason, "interval: last decision") {
		t.Fatalf("interval since decision: %+v", g)
	}
	// run-now skips only the interval parts.
	if g, err := m.HeartbeatGate(*e.now, true); err != nil || !g.OK {
		t.Fatalf("run-now gate: %+v %v", g, err)
	}

	*e.now = hbNoon.Add(4 * time.Hour)
	m.Cfg.Heartbeat.DailyMaxWakes = 1
	if g := e.gate(t); g.OK || g.Reason != "daily cap: 1 of 1 wakes used today" ||
		!g.NextEligible.Equal(time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)) {
		t.Fatalf("daily cap: %+v", g)
	}
	m.Cfg.Heartbeat.DailyMaxWakes = 0

	// A finished job whose job-done line is not delivered yet.
	j := enqueue(t, m, "b", nil)
	if err := m.Store.Claim(j.ID, "tok", *e.now); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.Finish(j.ID, "tok", store.StatusSucceeded, 1, 0, 1, "", *e.now); err != nil {
		t.Fatal(err)
	}
	if g := e.gate(t); g.OK || g.Reason != "delivery pending: 1 job-done lines" {
		t.Fatalf("job-done pending: %+v", g)
	}
	if err := m.Store.MarkNotified(j.ID, *e.now); err != nil {
		t.Fatal(err)
	}
	// A notify_owner delivery in flight (stored, neither delivered nor failed yet).
	n, err := m.Store.AddNotification("", "x", "normal", *e.now, store.NotifyLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if g := e.gate(t); g.OK || g.Reason != "delivery pending: 1 owner notifications" {
		t.Fatalf("notification in flight: %+v", g)
	}
	if err := m.Store.SetNotificationResult(n.ID, true, ""); err != nil {
		t.Fatal(err)
	}
	if g := e.gate(t); !g.OK {
		t.Fatalf("open again: %+v", g)
	}

	// The daemon records its last gate result.
	m.Cfg.Heartbeat.GoalsFile = filepath.Join(t.TempDir(), "none.md")
	if _, err := m.RunHeartbeat(context.Background(), HeartbeatTriggerDaemon); !IsHeartbeatGateClosed(err) {
		t.Fatalf("closed gate: %v", err)
	}
	if r, _, err := m.Store.HeartbeatGate(); err != nil || r != "idle: goals file missing" {
		t.Fatalf("recorded gate: %q %v", r, err)
	}
	if e.calls() != 0 {
		t.Fatalf("the gate called the model %d times", e.calls())
	}
}

// The night window is [start, end) in local time and may cross midnight; start == end is none.
func TestHeartbeatNightAcrossMidnight(t *testing.T) {
	for _, c := range []struct {
		h, s, e int
		want    bool
	}{
		{0, 0, 8, true}, {7, 0, 8, true}, {8, 0, 8, false}, {23, 0, 8, false},
		{22, 22, 7, true}, {23, 22, 7, true}, {0, 22, 7, true}, {6, 22, 7, true}, {7, 22, 7, false},
		{12, 22, 7, false}, {21, 22, 7, false}, {5, 5, 5, false},
	} {
		if got := inNight(c.h, c.s, c.e); got != c.want {
			t.Errorf("inNight(%d, %d, %d) = %t", c.h, c.s, c.e, got)
		}
	}
	e := newHeartbeat(t)
	s, en := 22, 7
	e.m.Cfg.Heartbeat.NightStartHour, e.m.Cfg.Heartbeat.NightEndHour = &s, &en
	for _, c := range []struct {
		now, next time.Time
	}{
		{time.Date(2026, 10, 18, 23, 30, 0, 0, time.UTC), time.Date(2026, 10, 19, 7, 0, 0, 0, time.UTC)},
		{time.Date(2026, 10, 19, 1, 0, 0, 0, time.UTC), time.Date(2026, 10, 19, 7, 0, 0, 0, time.UTC)},
	} {
		*e.now = c.now
		if g := e.gate(t); g.OK || g.Reason != "night window 22:00-07:00" || !g.NextEligible.Equal(c.next) {
			t.Fatalf("%s: %+v", c.now, g)
		}
	}
	*e.now = time.Date(2026, 10, 19, 7, 0, 0, 0, time.UTC)
	if g := e.gate(t); !g.OK {
		t.Fatalf("after the night: %+v", g)
	}
	// Default 0-8: 23:00 is day.
	e.m.Cfg.Heartbeat.NightStartHour, e.m.Cfg.Heartbeat.NightEndHour = nil, nil
	*e.now = time.Date(2026, 10, 18, 23, 0, 0, 0, time.UTC)
	if g := e.gate(t); !g.OK {
		t.Fatalf("23:00 with the default window: %+v", g)
	}
}

// The daily cap counts the stored wakes of the local day, so a restarted daemon keeps it; the next
// local day opens it again.
func TestHeartbeatDailyCapSurvivesRestart(t *testing.T) {
	e := newHeartbeat(t)
	e.m.Cfg.Heartbeat.DailyMaxWakes = 2
	e.decision(t, time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC), store.HeartbeatWake)
	e.decision(t, time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC), store.HeartbeatWake)
	e.decision(t, time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC), store.HeartbeatWake) // yesterday: not counted
	if err := e.m.Close(); err != nil {
		t.Fatal(err)
	}
	m2, err := Open("test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m2.Close() })
	m2.Cfg.Heartbeat = e.m.Cfg.Heartbeat
	m2.Loc = time.UTC
	now := hbNoon
	m2.Now = func() time.Time { return now }
	g, err := m2.HeartbeatGate(now, true)
	if err != nil || g.OK || g.Reason != "daily cap: 2 of 2 wakes used today" {
		t.Fatalf("after restart: %+v %v", g, err)
	}
	now = time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	if g, err := m2.HeartbeatGate(now, false); err != nil || !g.OK {
		t.Fatalf("next day: %+v %v", g, err)
	}
}

// The answer is one JSON object with exactly wake (bool) and reason (one line, 1..200 characters).
func TestParseHeartbeatAnswerStrict(t *testing.T) {
	for _, bad := range []string{
		"", "garbage", "yes, wake up", `{"wake": true}`, `{"reason": "x"}`,
		`{"wake": true, "reason": "x", "urgency": "high"}`, `{"wake": "true", "reason": "x"}`,
		`{"wake": 1, "reason": "x"}`, `{"wake": null, "reason": "x"}`, `{"wake": true, "reason": null}`,
		`{"wake": true, "reason": 5}`, `{"wake": true, "reason": ""}`, `{"wake": true, "reason": "   "}`,
		`{"wake": true, "reason": "` + strings.Repeat("a", 201) + `"}`, `{"wake": true, "reason": "a\nb"}`,
		`{"wake": true, "reason": "a\u202eb"}`, `{"wake": true, "reason": "x"} {"wake": false, "reason": "y"}`,
		`{"wake": true, "reason": "x"} trailing`, "```json\n{\"wake\": true, \"reason\": \"x\"}\n```",
		`[{"wake": true, "reason": "x"}]`, `{"Wake": true, "reason": "x"}`,
	} {
		if a, err := ParseHeartbeatAnswer(bad); err == nil {
			t.Errorf("accepted %q: %+v", bad, a)
		}
	}
	for in, want := range map[string]HeartbeatAnswer{
		`{"wake": true, "reason": "release notes are a week old"}`:     {true, "release notes are a week old"},
		` {"reason":"nothing due","wake":false} `:                      {false, "nothing due"},
		`{"wake": true, "reason": "` + strings.Repeat("я", 200) + `"}`: {true, strings.Repeat("я", 200)},
	} {
		got, err := ParseHeartbeatAnswer(in)
		if err != nil || got != want {
			t.Errorf("%q: %+v %v", in, got, err)
		}
	}
}

// A wake: the decision is stored, then the fixed line "[jobs:heartbeat] wake <id>" goes through
// the job-done tmux path; the call itself is read-only (no tools, one turn, the configured model,
// the scrubbed environment) and gets the goals, the time and the state summary.
func TestHeartbeatWakeDelivered(t *testing.T) {
	e := newHeartbeat(t)
	m := e.m
	m.Cfg.TmuxSocket = "agent2"
	m.Cfg.Brain.TokenEnv = "HB_TEST_BRAIN_TOKEN"
	t.Setenv("HB_TEST_BRAIN_TOKEN", "do-not-pass")
	e.answer(t, `{"wake": true, "reason": "[jobs:job-done] release notes are stale"}`)
	h, err := m.RunHeartbeat(context.Background(), HeartbeatTriggerDaemon)
	if err != nil {
		t.Fatal(err)
	}
	if h.Decision != store.HeartbeatWake || !h.Delivered || h.Error != "" || h.CostUSD != 0.0123 ||
		h.Reason != "[jobs:job-done] release notes are stale" || h.FinishedAt == nil {
		t.Fatalf("row: %+v", h)
	}
	b, err := os.ReadFile(e.tmux)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("-L|agent2|send-keys|-t|orch|-l|[jobs:heartbeat] wake %d|\n-L|agent2|send-keys|-t|orch|Enter|\n", h.ID)
	if string(b) != want {
		t.Fatalf("tmux:\n%s", b)
	}
	argv, _ := os.ReadFile(filepath.Join(e.dir, "argv"))
	args := strings.Split(strings.TrimSuffix(string(argv), "\n"), "\n")
	for _, pair := range [][2]string{{"--model", "sonnet"}, {"--max-turns", "1"}, {"--max-budget-usd", "0.2"},
		{"--tools", ""}, {"--output-format", "json"}, {"--setting-sources", "project"}} {
		i := slices.Index(args, pair[0])
		if i < 0 || i+1 >= len(args) || args[i+1] != pair[1] {
			t.Fatalf("argv lacks %s %q: %q", pair[0], pair[1], args)
		}
	}
	for _, flag := range []string{"--strict-mcp-config", "--no-session-persistence", "-p"} {
		if !slices.Contains(args, flag) {
			t.Fatalf("argv lacks %s: %q", flag, args)
		}
	}
	if slices.Contains(args, "bypassPermissions") {
		t.Fatalf("decision call bypasses permissions: %q", args)
	}
	stdin, _ := os.ReadFile(filepath.Join(e.dir, "stdin"))
	for _, s := range []string{"GOAL_MARKER", "Now: 2026-10-18T12:00:00Z", "Last wake: never",
		"State: 0 jobs running, 0 queued, 0 failed today, 0 owner replies pending, 0 missed checks.", `{"wake": true or false`} {
		if !strings.Contains(string(stdin), s) {
			t.Fatalf("prompt lacks %q:\n%s", s, stdin)
		}
	}
	env, _ := os.ReadFile(filepath.Join(e.dir, "env"))
	if strings.Contains(string(env), "do-not-pass") {
		t.Fatal("the brain token reached the decision call")
	}
	if g := e.gate(t); g.OK || !strings.HasPrefix(g.Reason, "interval: last wake") {
		t.Fatalf("gate after a wake: %+v", g)
	}
	st, err := m.Heartbeat()
	if err != nil || st.WakesToday != 1 || st.Last == nil || st.Last.ID != h.ID || st.NextEligible == nil ||
		!st.NextEligible.Equal(hbNoon.Add(time.Hour)) {
		t.Fatalf("status: %+v %v", st, err)
	}
}

// A wake that does not reach the session is stored with delivered=false and the error; the wake
// still counts (no retry, no second model call).
func TestHeartbeatDeliveryFailureRecorded(t *testing.T) {
	e := newHeartbeat(t)
	t.Setenv("AGENT_JOBS_TMUX_BIN", "/usr/bin/false")
	e.answer(t, `{"wake": true, "reason": "check the deploy"}`)
	h, err := e.m.RunHeartbeat(context.Background(), HeartbeatTriggerDaemon)
	if err != nil {
		t.Fatal(err)
	}
	if h.Decision != store.HeartbeatWake || h.Delivered || !strings.Contains(h.Error, "tmux send-keys") {
		t.Fatalf("row: %+v", h)
	}
	e.m.Cfg.NotifyTmuxTarget = ""
	*e.now = hbNoon.Add(2 * time.Hour)
	h2, err := e.m.RunHeartbeat(context.Background(), HeartbeatTriggerDaemon)
	if err != nil || h2.Delivered || !strings.Contains(h2.Error, "notify_tmux_target") {
		t.Fatalf("no target: %+v %v", h2, err)
	}
	if st, err := e.m.Heartbeat(); err != nil || st.WakesToday != 2 {
		t.Fatalf("wakes today: %+v %v", st, err)
	}
	if e.calls() != 2 {
		t.Fatalf("calls: %d", e.calls())
	}
}

// An invalid answer, a failed call or an error result is stored as an error: no wake, no line.
func TestHeartbeatInvalidAnswerNoWake(t *testing.T) {
	e := newHeartbeat(t)
	for i, out := range []string{
		`{"type":"result","subtype":"success","is_error":false,"total_cost_usd":0.01,"result":"I think you should wake up"}`,
		`{"type":"result","subtype":"success","is_error":false,"total_cost_usd":0.01,"result":"{\"wake\":true,\"reason\":\"x\",\"also\":1}"}`,
		`{"type":"result","subtype":"error_max_budget_usd","is_error":true,"total_cost_usd":0.2,"result":""}`,
		`not json at all`,
	} {
		if err := os.WriteFile(filepath.Join(e.dir, "out"), []byte(out), 0o600); err != nil {
			t.Fatal(err)
		}
		*e.now = hbNoon.Add(time.Duration(i) * 2 * time.Hour)
		h, err := e.m.RunHeartbeat(context.Background(), HeartbeatTriggerDaemon)
		if err != nil {
			t.Fatal(err)
		}
		if h.Decision != store.HeartbeatError || h.Delivered || h.Error == "" {
			t.Fatalf("%d: %+v", i, h)
		}
	}
	if _, err := os.Stat(e.tmux); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a line was typed: %v", err)
	}
	if st, _ := e.m.Heartbeat(); st.WakesToday != 0 {
		t.Fatalf("wakes: %d", st.WakesToday)
	}
}

// The decision is claimed (stored as pending) before the model is asked: while the call runs a
// second heartbeat is refused, and after a restart the stale claim is marked abandoned and never
// asked again (it counts as the last decision for the interval).
func TestHeartbeatClaimedBeforeCall(t *testing.T) {
	e := newHeartbeat(t)
	m := e.m
	e.answer(t, `{"wake": true, "reason": "late"}`)
	if err := os.WriteFile(filepath.Join(e.dir, "sleep"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = m.RunHeartbeat(ctx, HeartbeatTriggerDaemon)
	}()
	waitFor(t, "decision call", 10*time.Second, func() bool { return e.calls() == 1 })
	p, err := m.Store.PendingHeartbeat()
	if err != nil || p == nil || !p.ClaimedAt.Equal(hbNoon) {
		t.Fatalf("no pending claim while the model runs: %+v %v", p, err)
	}
	if _, err := m.RunHeartbeat(context.Background(), HeartbeatTriggerRunNow); !IsHeartbeatGateClosed(err) ||
		!strings.Contains(err.Error(), "heartbeat.lock") {
		t.Fatalf("second heartbeat: %v", err)
	}
	if g := e.gate(t); g.OK || !strings.Contains(g.Reason, "running") {
		t.Fatalf("gate while the model runs: %+v", g)
	}

	// "Restart": another manager on the same store, past the stale limit (a dead process holds no
	// heartbeat.lock, so its gate runs; here the old call still holds it, so the gate is asked
	// directly).
	m2, err := Open("test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m2.Close() })
	m2.Cfg.Heartbeat, m2.Cfg.NotifyTmuxTarget, m2.Loc = m.Cfg.Heartbeat, "orch", time.UTC
	later := hbNoon.Add(10 * time.Minute)
	m2.Now = func() time.Time { return later }
	if g, err := m2.HeartbeatGate(later, false); err != nil || g.OK || !strings.Contains(g.Reason, "interval: last decision") {
		t.Fatalf("after restart: %+v %v", g, err)
	}
	got, err := m2.Store.GetHeartbeat(p.ID)
	if err != nil || got.Decision != store.HeartbeatAbandoned {
		t.Fatalf("stale claim: %+v %v", got, err)
	}
	cancel()
	<-done
	if got, _ := m2.Store.GetHeartbeat(p.ID); got.Decision != store.HeartbeatAbandoned || got.Delivered {
		t.Fatalf("abandoned claim changed: %+v", got)
	}
	if e.calls() != 1 {
		t.Fatalf("the model was asked %d times", e.calls())
	}
	if _, err := os.Stat(e.tmux); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a line was typed: %v", err)
	}
}

// The goals text is capped at 8 KB (on a UTF-8 boundary) and redacted before it reaches the prompt.
func TestHeartbeatPromptCapsGoals(t *testing.T) {
	e := newHeartbeat(t)
	tok := "ghp_" + strings.Repeat("Q", 36)
	body := "token " + tok + "\n" + strings.Repeat("я", MaxHeartbeatGoalsBytes) + "TAIL_MARKER"
	if err := os.WriteFile(e.m.Cfg.Heartbeat.GoalsFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := e.m.HeartbeatPrompt(hbNoon)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p, "TAIL_MARKER") || strings.Contains(p, tok) || !strings.Contains(p, "cut at 8192 bytes") ||
		!strings.Contains(p, "[redacted") {
		t.Fatalf("prompt:\n%.400s", p)
	}
	if len(p) > MaxHeartbeatGoalsBytes+2048 {
		t.Fatalf("prompt is %d bytes", len(p))
	}
}

// Dry run prints the gate result and the prompt and never calls the model nor stores a decision.
func TestHeartbeatDryRunNoModelCall(t *testing.T) {
	e := newHeartbeat(t)
	cfg := `{"heartbeat": {"enabled": true, "goals_file": "` + e.m.Cfg.Heartbeat.GoalsFile + `", "night_start_hour": 0, "night_end_hour": 0}}`
	if err := os.WriteFile(filepath.Join(e.m.StateDir, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	e.answer(t, `{"wake": true, "reason": "x"}`)
	out, err := exec.Command(selfBin, "heartbeat", "run-now", "--agent", "test", "--dry-run").CombinedOutput()
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	for _, s := range []string{"gate:      daemon: eligible; run-now: eligible", "--- prompt ---", "GOAL_MARKER", "model:     sonnet"} {
		if !strings.Contains(string(out), s) {
			t.Fatalf("output lacks %q:\n%s", s, out)
		}
	}
	if e.calls() != 0 {
		t.Fatal("dry run called the model")
	}
	if h, err := e.m.Store.LastHeartbeat(); err != nil || h != nil {
		t.Fatalf("dry run stored a decision: %+v %v", h, err)
	}
	// Status shows the heartbeat.
	out, err = exec.Command(selfBin, "status", "--agent", "test").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "heartbeat: on, model sonnet, every 60 min, night none, 0 of 6 wakes today") {
		t.Fatalf("status: %v\n%s", err, out)
	}
}

// config.json: the heartbeat section is validated (bounds, absolute goals path, a claude model).
func TestHeartbeatConfigValidation(t *testing.T) {
	dir := t.TempDir()
	for _, bad := range []string{
		`{"heartbeat": {"interval_minutes": -1}}`, `{"heartbeat": {"night_start_hour": 24}}`,
		`{"heartbeat": {"night_end_hour": -1}}`, `{"heartbeat": {"daily_max_wakes": 49}}`,
		`{"heartbeat": {"goals_file": "goals.md"}}`, `{"heartbeat": {"max_budget_usd": 3}}`,
		`{"heartbeat": {"timeout_seconds": 601}}`, `{"heartbeat": {"model": "-x"}}`, `{"heartbeat": {"model": "gpt-5.6-sol"}}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(dir); err == nil || !strings.Contains(err.Error(), "heartbeat.") {
			t.Errorf("%s accepted: %v", bad, err)
		}
	}
	ok := `{"heartbeat": {"enabled": true, "interval_minutes": 30, "night_start_hour": 22, "night_end_hour": 7,
		"daily_max_wakes": 3, "goals_file": "/tmp/g.md", "model": "opus", "max_budget_usd": 0.5, "timeout_seconds": 90}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(ok), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	h := c.Heartbeat
	if s, e := h.Night(); !h.Enabled || h.Interval() != 30*time.Minute || s != 22 || e != 7 || h.MaxWakes() != 3 ||
		h.DecisionModel() != "opus" || h.Budget() != 0.5 || h.Timeout() != 90*time.Second || h.GoalsFile != "/tmp/g.md" {
		t.Fatalf("parsed: %+v", h)
	}
}
