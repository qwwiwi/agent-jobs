package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/guard"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// fakeAdvisorClaude stands in for the advisor's claude -p: it records each call (argv one per line,
// stdin, cwd, environment) in $FAKE_ADV_DIR as argv.N / stdin.N / cwd.N / env.N and prints
// $FAKE_ADV_DIR/out; with $FAKE_ADV_DIR/slow present it sleeps first (a call that runs long).
const fakeAdvisorClaude = `#!/bin/bash
d="$FAKE_ADV_DIR"
echo call >> "$d/calls"
n=$(wc -l < "$d/calls" | tr -d ' ')
printf '%s\n' "$@" > "$d/argv.$n"
cat > "$d/stdin.$n"
pwd > "$d/cwd.$n"
env > "$d/env.$n"
if [ -f "$d/slow" ]; then sleep 30; fi
cat "$d/out" 2>/dev/null
`

type advEnv struct {
	m   *Manager
	dir string
}

// newAdvisor is a manager whose claude is the fake advisor.
func newAdvisor(t *testing.T) *advEnv {
	t.Helper()
	m := newManager(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte(fakeAdvisorClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_JOBS_CLAUDE_BIN", bin)
	t.Setenv("FAKE_ADV_DIR", dir)
	e := &advEnv{m: m, dir: dir}
	e.answer(t, "success", "use the interface", 2, 0.1)
	return e
}

// answer makes the fake advisor print a claude JSON result.
func (e *advEnv) answer(t *testing.T, subtype, result string, turns int, cost float64) {
	t.Helper()
	b, err := json.Marshal(map[string]any{"type": "result", "subtype": subtype, "is_error": subtype != "success",
		"num_turns": turns, "total_cost_usd": cost, "result": result})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.dir, "out"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (e *advEnv) calls() int {
	b, _ := os.ReadFile(filepath.Join(e.dir, "calls"))
	return strings.Count(string(b), "call")
}

func (e *advEnv) argv(t *testing.T, n int) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(e.dir, "argv."+string(rune('0'+n))))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// crewJob enqueues a crew job with the given caps, claims it (running) and gives it a work dir and a
// stream with three assistant turns.
func (e *advEnv) crewJob(t *testing.T, calls int, budget float64) *store.Job {
	t.Helper()
	j := enqueue(t, e.m, "crew", func(r *StartRequest) {
		r.Crew = CrewRequest{AdvisorModel: "opus", AdvisorMaxCalls: calls, AdvisorMaxBudgetUSD: budget}
	})
	if err := e.m.Store.Claim(j.ID, "tok", e.m.Now()); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(e.m.JobDir(j.ID), "work"), 0o700); err != nil {
		t.Fatal(err)
	}
	stream := `{"type":"system","subtype":"init"}
{"type":"assistant","message":{"id":"m1","content":[]}}
{"type":"assistant","message":{"id":"m1","content":[]}}
{"type":"user","message":{"content":[]}}
{"type":"assistant","message":{"id":"m2","content":[]}}
{"type":"assistant","message":{"id":"m3","content":[]}}
`
	if err := os.WriteFile(filepath.Join(e.m.JobDir(j.ID), "stream.jsonl"), []byte(stream), 0o600); err != nil {
		t.Fatal(err)
	}
	j, err := e.m.Store.Get(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func argAfter(argv []string, flag string) (string, bool) {
	i := slices.Index(argv, flag)
	if i < 0 || i+1 >= len(argv) {
		return "", false
	}
	return argv[i+1], true
}

// advisor_max_calls holds: the calls past it are refused before any model runs, and every call is
// recorded with the main loop's turn, the question, the outcome, the turns and the cost.
func TestAdvisorCapEnforcedAndRecorded(t *testing.T) {
	e := newAdvisor(t)
	j := e.crewJob(t, 2, 0)
	if j.Crew == nil || j.Crew.AdvisorMaxCalls != 2 || j.Crew.AdvisorMaxBudgetUSD != DefaultAdvisorBudgetUSD {
		t.Fatalf("crew stored: %+v", j.Crew)
	}
	ctx := context.Background()
	for i := 1; i <= 2; i++ {
		a, err := e.m.AskAdvisor(ctx, j, "Option A or B for the cache?", "A: map, B: LRU")
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if a.Advice != "use the interface" || a.Call.N != i || a.CallsLeft != 2-i {
			t.Fatalf("call %d answer: %+v", i, a)
		}
	}
	_, err := e.m.AskAdvisor(ctx, j, "third question", "")
	if !errors.Is(err, store.ErrAdvisorLimit) || !strings.Contains(err.Error(), "advisor_max_calls") {
		t.Fatalf("third call: %v", err)
	}
	if n := e.calls(); n != 2 {
		t.Fatalf("the model ran %d times, want 2", n)
	}
	calls, err := e.m.Store.AdvisorCalls(j.ID, e.m.Now(), advisorStaleAfter)
	if err != nil || len(calls) != 2 {
		t.Fatalf("recorded calls: %+v %v", calls, err)
	}
	for _, c := range calls {
		if c.Turn != 3 || c.UntrustedWhy != "Option A or B for the cache?" || c.Outcome != store.AdvisorAnswered ||
			c.Turns != 2 || c.CostUSD != 0.1 || c.CostUnknown || c.Model != "opus" || c.EndedAt == nil {
			t.Fatalf("recorded call: %+v", c)
		}
	}
	v, err := e.m.Crew(j)
	if err != nil || v == nil || len(v.Calls) != 2 || v.CostAdvisorUSD < 0.199 || v.CostAdvisorUSD > 0.201 ||
		v.CostTotalUSD != v.CostMainUSD+v.CostAdvisorUSD {
		t.Fatalf("crew view: %+v %v", v, err)
	}
	if tl := CrewTimeline(v); len(tl) != 3 || !strings.Contains(tl[0], "2/2 calls") || !strings.Contains(tl[1], "#1 turn 3") {
		t.Fatalf("timeline: %q", tl)
	}
	// The question reaches the advisor as data, with the context.
	in, _ := os.ReadFile(filepath.Join(e.dir, "stdin.1"))
	if !strings.Contains(string(in), "Option A or B for the cache?") || !strings.Contains(string(in), "A: map, B: LRU") {
		t.Fatalf("advisor prompt: %s", in)
	}
}

// advisor_max_budget_usd holds: each call gets its share of the budget left (--max-budget-usd), and
// once the spend reaches the cap (claude's budget is checked between turns, so a call can overshoot
// its share) the next call is refused even with calls left.
func TestAdvisorBudgetEnforced(t *testing.T) {
	e := newAdvisor(t)
	j := e.crewJob(t, 4, 1.0)
	e.answer(t, "success", "ok", 1, 0.6)
	ctx := context.Background()
	if _, err := e.m.AskAdvisor(ctx, j, "q1", ""); err != nil {
		t.Fatal(err)
	}
	if b, _ := argAfter(e.argv(t, 1), "--max-budget-usd"); b != "0.25" {
		t.Fatalf("first call budget %q, want 0.25 (1.0 over 4 calls)", b)
	}
	a, err := e.m.AskAdvisor(ctx, j, "q2", "")
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := argAfter(e.argv(t, 2), "--max-budget-usd"); b != "0.133333" {
		t.Fatalf("second call budget %q, want 0.133333 (0.4 left over 3 calls)", b)
	}
	if a.BudgetLeftUSD != 0 || a.CallsLeft != 2 {
		t.Fatalf("left after the second call: %+v", a)
	}
	_, err = e.m.AskAdvisor(ctx, j, "q3", "")
	if !errors.Is(err, store.ErrAdvisorLimit) || !strings.Contains(err.Error(), "advisor_max_budget_usd") {
		t.Fatalf("third call: %v", err)
	}
	if e.calls() != 2 {
		t.Fatalf("the model ran %d times, want 2", e.calls())
	}
}

// A failed call is recorded as failed and still counts: a call without a reported cost counts at its
// whole budget (its share of the crew's), so failures cannot spend past the cap.
func TestAdvisorFailureRecordedAtBudget(t *testing.T) {
	e := newAdvisor(t)
	j := e.crewJob(t, 2, 1.0)
	if err := os.WriteFile(filepath.Join(e.dir, "out"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.AskAdvisor(context.Background(), j, "q", ""); err == nil || !strings.Contains(err.Error(), "advisor call 1 failed") {
		t.Fatalf("failed call: %v", err)
	}
	calls, err := e.m.Store.AdvisorCalls(j.ID, e.m.Now(), advisorStaleAfter)
	if err != nil || len(calls) != 1 || calls[0].Outcome != store.AdvisorFailed || !calls[0].CostUnknown || calls[0].Error == "" {
		t.Fatalf("recorded: %+v %v", calls, err)
	}
	if s := store.AdvisorSpend(calls); s != 0.5 {
		t.Fatalf("spend %v, want the call's whole budget 0.5", s)
	}
	// The second call gets what is left, 0.5, never more: 0.5 + 0.5 stays within the cap.
	e.answer(t, "success", "ok", 1, 0.2)
	if _, err := e.m.AskAdvisor(context.Background(), j, "q", ""); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if b, _ := argAfter(e.argv(t, 2), "--max-budget-usd"); b != "0.5" {
		t.Fatalf("second call budget %q, want 0.5", b)
	}
}

// One call at a time; a row the daemon never finished (stopped mid-call) stops blocking once stale.
func TestAdvisorOneAtATimeAndStale(t *testing.T) {
	e := newAdvisor(t)
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	e.m.Now = func() time.Time { return now }
	j := e.crewJob(t, 2, 2.0)
	claim := store.AdvisorClaim{Model: "opus", MaxCalls: 2, MaxBudget: 2.0, StaleAfter: advisorStaleAfter}
	if c, err := e.m.Store.ClaimAdvisorCall(j.ID, now, claim); err != nil || c.BudgetUSD != 1.0 {
		t.Fatalf("claim: %+v %v (want its share 1.0)", c, err)
	}
	if _, err := e.m.AskAdvisor(context.Background(), j, "q", ""); !errors.Is(err, store.ErrAdvisorLimit) ||
		!strings.Contains(err.Error(), "one at a time") {
		t.Fatalf("concurrent call: %v", err)
	}
	if e.calls() != 0 {
		t.Fatal("the model ran beside a call in flight")
	}
	// Once stale the row no longer blocks as in flight, but it counts at its whole budget (the cost of a
	// call the daemon lost is unknown): its share 1.0, so the next call gets the other 1.0, no more.
	now = now.Add(advisorStaleAfter + time.Second)
	if _, err := e.m.AskAdvisor(context.Background(), j, "q", ""); err != nil {
		t.Fatalf("after the stale row: %v", err)
	}
	if b, _ := argAfter(e.argv(t, 1), "--max-budget-usd"); b != "1" {
		t.Fatalf("call after the stale row: budget %q, want 1", b)
	}
	calls, err := e.m.Store.AdvisorCalls(j.ID, now, advisorStaleAfter)
	if err != nil || len(calls) != 2 || calls[0].Outcome != store.AdvisorStale || !calls[0].CostUnknown {
		t.Fatalf("stale row: %+v %v", calls, err)
	}
	if s := store.AdvisorSpend(calls); s != 1.1 {
		t.Fatalf("spend %v, want 1.0 (stale, at its budget) + 0.1", s)
	}
}

// The advisor cannot write: it runs with the read tools only, the daemon's own settings (deny rules for
// every writing tool, the guard hook read-only on the work dir with a read allowlist), no project
// settings of the work tree, no MCP servers and no bypassPermissions, in the job's work dir.
func TestAdvisorCannotWrite(t *testing.T) {
	e := newAdvisor(t)
	j := e.crewJob(t, 2, 1.0)
	if _, err := e.m.AskAdvisor(context.Background(), j, "q", ""); err != nil {
		t.Fatal(err)
	}
	argv := e.argv(t, 1)
	if v, _ := argAfter(argv, "--tools"); v != "Read,Glob,Grep" {
		t.Fatalf("--tools %q", v)
	}
	if v, ok := argAfter(argv, "--setting-sources"); !ok || v != "" {
		t.Fatalf("--setting-sources %q %v, want empty", v, ok)
	}
	if v, _ := argAfter(argv, "--permission-mode"); v != "default" {
		t.Fatalf("--permission-mode %q", v)
	}
	if slices.Contains(argv, "bypassPermissions") || slices.Contains(argv, "--add-dir") || !slices.Contains(argv, "--no-session-persistence") {
		t.Fatalf("argv: %q", argv)
	}
	if v, _ := argAfter(argv, "--model"); v != "opus" {
		t.Fatalf("--model %q", v)
	}
	work := filepath.Join(e.m.JobDir(j.ID), "work")
	if cwd, _ := os.ReadFile(filepath.Join(e.dir, "cwd.1")); strings.TrimSpace(string(cwd)) != work {
		if r, _ := filepath.EvalSymlinks(work); strings.TrimSpace(string(cwd)) != r {
			t.Fatalf("cwd %q, want %q", cwd, work)
		}
	}
	sp, _ := argAfter(argv, "--settings")
	if !strings.HasPrefix(sp, e.m.AdvisorDir(j.ID)) || strings.HasPrefix(sp, e.m.JobDir(j.ID)) {
		t.Fatalf("settings %s must be the daemon's, outside the job dir", sp)
	}
	b, err := os.ReadFile(sp)
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		DisableAllHooks bool `json:"disableAllHooks"`
		Permissions     struct {
			Deny []string `json:"deny"`
		} `json:"permissions"`
		Hooks struct {
			PreToolUse []struct {
				Hooks []struct {
					Command string `json:"command"`
				} `json:"hooks"`
			} `json:"PreToolUse"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"Bash", "Edit", "Write", "MultiEdit", "NotebookEdit", "Task", "Agent", "WebFetch"} {
		if !slices.Contains(s.Permissions.Deny, w) {
			t.Errorf("deny lacks %s", w)
		}
	}
	if s.DisableAllHooks || len(s.Hooks.PreToolUse) != 1 || len(s.Hooks.PreToolUse[0].Hooks) != 1 {
		t.Fatalf("hooks: %s", b)
	}
	hook := s.Hooks.PreToolUse[0].Hooks[0].Command
	if !strings.Contains(hook, "--read-only "+shellQuote(work)) || !strings.Contains(hook, "--read-allow "+shellQuote(work)) {
		t.Fatalf("guard hook not read-only on the work dir: %s", hook)
	}
	// The guard itself refuses a write inside a read-only work dir (what --read-only turns on).
	if msg := guard.CheckFileWrite("Write", filepath.Join(work, "x.go"), guard.Config{ReadOnly: work}); msg == "" {
		t.Fatal("guard allowed a write in the read-only work dir")
	}
	if mcpb, err := os.ReadFile(filepath.Join(e.m.AdvisorDir(j.ID), "mcp.json")); err != nil || !strings.Contains(string(mcpb), `"mcpServers": {}`) {
		t.Fatalf("advisor mcp.json: %s %v", mcpb, err)
	}
}

// Codex jobs have no advisor: the start is refused, and a codex row never runs one.
func TestAdvisorRefusedInCodexJobs(t *testing.T) {
	e := newAdvisor(t)
	_, err := e.m.Enqueue(StartRequest{Title: "t", Kind: "review", Brief: "b", Model: "gpt-6.1-sol",
		Crew: CrewRequest{AdvisorModel: "opus"}})
	if err == nil || !strings.Contains(err.Error(), "codex") {
		t.Fatalf("codex crew start: %v", err)
	}
	j := &store.Job{ID: "261106-000000-cdx0", Kind: "review", Runner: store.RunnerCodex,
		Crew: &store.Crew{AdvisorModel: "opus", AdvisorMaxCalls: 2, AdvisorMaxBudgetUSD: 1}}
	if _, err := e.m.AskAdvisor(context.Background(), j, "q", ""); err == nil || !strings.Contains(err.Error(), "codex") {
		t.Fatalf("codex advisor call: %v", err)
	}
	if e.calls() != 0 {
		t.Fatal("the advisor ran for a codex job")
	}
	if c := e.m.jobMCPConfigFor(j); strings.Contains(mustJSON(t, c), JobMCPAdvisorFlag) {
		t.Fatalf("codex job mcp config offers the advisor: %v", c)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Only a crew job's MCP server is told to offer advisor_ask; a plain job's call is refused anyway.
func TestAdvisorOnlyForCrewJobs(t *testing.T) {
	e := newAdvisor(t)
	crew := e.crewJob(t, 2, 1)
	if !strings.Contains(mustJSON(t, e.m.jobMCPConfigFor(crew)), `"`+JobMCPAdvisorFlag+`"`) {
		t.Fatalf("crew job mcp config: %v", e.m.jobMCPConfigFor(crew))
	}
	plain := running(t, e.m)
	if strings.Contains(mustJSON(t, e.m.jobMCPConfigFor(plain)), JobMCPAdvisorFlag) {
		t.Fatal("plain job mcp config offers the advisor")
	}
	if _, err := e.m.AskAdvisor(context.Background(), plain, "q", ""); !errors.Is(err, errNoAdvisor) {
		t.Fatalf("plain job: %v", err)
	}
	if e.calls() != 0 {
		t.Fatal("the advisor ran for a plain job")
	}
}

// The crew fields are validated at the start, never clamped.
func TestCrewValidation(t *testing.T) {
	m := newManager(t)
	for _, c := range []struct {
		crew CrewRequest
		want string
	}{
		{CrewRequest{AdvisorMaxCalls: 2}, "need advisor_model"},
		{CrewRequest{AdvisorModel: "opus", AdvisorMaxCalls: MaxAdvisorCalls + 1}, "advisor_max_calls"},
		{CrewRequest{AdvisorModel: "opus", AdvisorMaxCalls: -1}, "advisor_max_calls"},
		{CrewRequest{AdvisorModel: "opus", AdvisorMaxBudgetUSD: MaxAdvisorBudgetUSD + 1}, "advisor_max_budget_usd"},
		{CrewRequest{AdvisorModel: "opus", AdvisorMaxBudgetUSD: -1}, "advisor_max_budget_usd"},
		{CrewRequest{AdvisorModel: "gpt-6.1-sol"}, "claude model"},
		{CrewRequest{AdvisorModel: "openrouter:z-ai/glm-5.3"}, "openrouter"},
		{CrewRequest{AdvisorModel: "--dangerously-skip"}, "not a model name"},
		{CrewRequest{PairedWith: "261106-000000-none"}, "paired_with"},
	} {
		_, err := m.Enqueue(StartRequest{Title: "t", Kind: "general", Brief: "b", Model: "sonnet", Crew: c.crew})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: %v, want %q", c.crew, err, c.want)
		}
	}
	j := enqueue(t, m, "b", func(r *StartRequest) { r.Crew = CrewRequest{AdvisorModel: "opus"} })
	if j.Crew.AdvisorMaxCalls != DefaultAdvisorMaxCalls || j.Crew.AdvisorMaxBudgetUSD != DefaultAdvisorBudgetUSD {
		t.Fatalf("defaults: %+v", j.Crew)
	}
	plain := enqueue(t, m, "b", nil)
	if plain.Crew != nil {
		t.Fatalf("plain job has a crew: %+v", plain.Crew)
	}
}

// job_stats lists a crew job next to the plain job it names: status, outcome and the cost split.
func TestCrewPairsInStats(t *testing.T) {
	e := newAdvisor(t)
	plain := running(t, e.m)
	j := enqueue(t, e.m, "crew", func(r *StartRequest) {
		r.Crew = CrewRequest{AdvisorModel: "opus", PairedWith: plain.ID}
	})
	if err := e.m.Store.Claim(j.ID, "tok", e.m.Now()); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(e.m.JobDir(j.ID), "work"), 0o700); err != nil {
		t.Fatal(err)
	}
	j, _ = e.m.Store.Get(j.ID)
	if _, err := e.m.AskAdvisor(context.Background(), j, "q", ""); err != nil {
		t.Fatal(err)
	}
	rows := []*store.JobStats{{ID: j.ID}, {ID: plain.ID}}
	pairs, err := e.m.crewPairs(context.Background(), rows)
	if err != nil || len(pairs) != 1 {
		t.Fatalf("pairs: %+v %v", pairs, err)
	}
	p := pairs[0]
	if p.Job.ID != j.ID || p.Paired.ID != plain.ID || p.Job.AdvisorCalls != 1 || p.Job.AdvisorModel != "opus" ||
		p.Job.CostAdvisorUSD != 0.1 || p.Paired.AdvisorCalls != 0 || p.Paired.CostAdvisorUSD != 0 {
		t.Fatalf("pair: %+v", p)
	}
	// Outside the window (the crew job's row not in it): no pair.
	if pairs, err := e.m.crewPairs(context.Background(), []*store.JobStats{{ID: plain.ID}}); err != nil || len(pairs) != 0 {
		t.Fatalf("pairs outside the window: %+v %v", pairs, err)
	}
}

// advisor_ask through the daemon's proxy: the calling process must be the job's, the answer carries the
// advice and the call's record; another job's id is refused before the model runs.
func TestAdvisorThroughProxy(t *testing.T) {
	e := newAdvisor(t)
	sock := serveProxy(t, e.m)
	j := e.crewJob(t, 2, 1)
	c := jobPeer(t, e.m, sock, j)
	o, err := c.AdvisorAsk(context.Background(), "which API shape?", "")
	if err != nil {
		t.Fatal(err)
	}
	call, _ := o["call"].(map[string]any)
	if o["advice"] != "use the interface" || call["n"] != float64(1) || call["outcome"] != store.AdvisorAnswered {
		t.Fatalf("proxy answer: %v", o)
	}
	other := running(t, e.m)
	if _, err := c.as(other.ID).AdvisorAsk(context.Background(), "q", ""); err == nil {
		t.Fatal("a call as another job was answered")
	}
	if e.calls() != 1 {
		t.Fatalf("the model ran %d times, want 1", e.calls())
	}
}
