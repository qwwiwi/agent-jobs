package jobs

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// Each claude message counts once by its id (its largest usage); codex turn.completed usage counts
// cached input apart from the rest.
func TestParseStreamStatsTokens(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"assistant","message":{"id":"m1","content":[],"usage":{"input_tokens":10,"output_tokens":1,"cache_read_input_tokens":5,"cache_creation_input_tokens":2}}}`,
		`{"type":"assistant","message":{"id":"m1","content":[],"usage":{"input_tokens":10,"output_tokens":7,"cache_read_input_tokens":5,"cache_creation_input_tokens":2}}}`,
		`{"type":"assistant","message":{"id":"m2","content":[],"usage":{"input_tokens":3,"output_tokens":4}}}`,
		`{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":40,"output_tokens":9}}`,
	}, "\n")
	ss, err := ParseStreamStats(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	want := TokenUsage{Input: 10 + 3 + 60, Output: 7 + 4 + 9, CacheRead: 5 + 40, CacheWrite: 2}
	if ss.Tokens != want {
		t.Fatalf("tokens %+v, want %+v", ss.Tokens, want)
	}
}

func TestPriceFor(t *testing.T) {
	var c Config
	for _, m := range []string{"opus", "claude-opus-4-6", "claude-sonnet-4-6[1m]", "claude-opus-4-1-20250805", "fable"} {
		if _, ok := c.PriceFor(m); !ok {
			t.Errorf("%s: no price", m)
		}
	}
	if _, ok := c.PriceFor("gpt-6.1-sol"); ok {
		t.Fatal("codex models have no built-in price")
	}
	c.ModelPrices = map[string]ModelPrice{"gpt-6.1-sol": {Input: 1, Output: 8, CacheRead: 0.1}}
	p, ok := c.PriceFor("gpt-6.1-sol")
	if !ok || p.Output != 8 {
		t.Fatalf("config price: %+v %v", p, ok)
	}
	if got := p.Cost(TokenUsage{Input: 1e6, Output: 1e6, CacheRead: 1e6}); math.Abs(got-9.1) > 1e-9 {
		t.Fatalf("cost %v", got)
	}
}

func TestModelPricesValidated(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"model_prices":{"x":{"input":-1}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(dir); err == nil || !strings.Contains(err.Error(), "model_prices") {
		t.Fatalf("negative price accepted: %v", err)
	}
}

// A timed-out job with assistant usage in its stream gets an estimated cost, which the totals include
// and mark as estimated; the job-done line shows it as "est.".
func TestTimedOutJobEstimatedCost(t *testing.T) {
	m := newManager(t)
	j := enqueue(t, m, "FAKE_USAGE FAKE_SLEEP=60", func(r *StartRequest) { r.TimeoutMinutes = 1 })
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "usage in the stream", 10*time.Second, func() bool {
		b, _ := os.ReadFile(filepath.Join(m.JobDir(j.ID), "stream.jsonl"))
		return strings.Contains(string(b), `"m2"`)
	})
	real := time.Now
	m.Now = func() time.Time { return real().Add(2 * time.Minute) }
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	j = get(t, m, j.ID)
	if j.Status != store.StatusTimeout || !j.CostUnknown {
		t.Fatalf("status %s cost_unknown %v", j.Status, j.CostUnknown)
	}
	st, err := m.Store.GetStats(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	// sonnet: 1000 in × $2 + 100000 out × $10 + 1M cache read × $0.2, per MTok.
	want := (1000*2.0 + 100000*10.0 + 1e6*0.2) / 1e6
	if st.CostSource != store.CostSourceEstimated || math.Abs(st.CostEstimatedUSD-want) > 1e-4 ||
		st.InputTokens != 1000 || st.OutputTokens != 100000 || st.CacheReadTokens != 1e6 {
		t.Fatalf("stats: %+v (want estimate %v)", st, want)
	}
	rep, err := m.Stats(time.Time{}, "kind")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Totals.CostUnknownJobs != 0 || rep.Totals.EstimatedJobs != 1 || math.Abs(rep.Totals.CostTotalUSD-want) > 1e-4 ||
		math.Abs(rep.Totals.CostEstimatedUSD-want) > 1e-4 {
		t.Fatalf("totals: %+v", rep.Totals)
	}
	if line := m.DoneLineFor(j); !strings.Contains(line, "cost "+EstimatedCostText(want)) {
		t.Fatalf("done line: %s", line)
	}
}

// job_outcome sets and overwrites, refuses unknown ids, unfinished jobs and other agents' jobs; the
// stats count the shares, the unrecorded jobs and the cost per usable result, grouped by repo too.
func TestSetOutcomeAndStats(t *testing.T) {
	m := newManager(t)
	now := time.Now()
	ins := func(id, agent, status, repo string, cost float64) {
		t.Helper()
		if err := m.Store.Insert(&store.Job{ID: id, Agent: agent, Title: id, Kind: "fix", Brief: "b", Status: status,
			Model: "sonnet", MaxTurns: 1, MaxBudgetUSD: 1, TimeoutMinutes: 1, CreatedAt: now, FinishedAt: &now,
			CostUSD: cost, RepoPath: repo}); err != nil {
			t.Fatal(err)
		}
	}
	ins("a", m.Agent, store.StatusSucceeded, "/x/alpha", 3)
	ins("b", m.Agent, store.StatusSucceeded, "/x/alpha", 1)
	ins("c", m.Agent, store.StatusFailed, "/x/beta", 2)
	ins("q", m.Agent, store.StatusQueued, "", 0)
	ins("s", m.Agent, store.StatusStopped, "", 0)
	ins("o", "other", store.StatusSucceeded, "", 0)
	for _, id := range []string{"a", "b", "c"} {
		if err := os.MkdirAll(m.JobDir(id), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(m.JobDir(id), "report.md"), []byte("## Summary\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.BackfillStats(false); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ id, outcome, note, want string }{
		{"nope", OutcomeAccepted, "", "not found"},
		{"q", OutcomeAccepted, "", "is queued"},
		{"s", OutcomeAccepted, "", "is stopped"},
		{"o", OutcomeAccepted, "", "belongs to agent other"},
		{"a", "great", "", "must be one of"},
		{"a", OutcomeAccepted, strings.Repeat("x", 301), "at most 300"},
	} {
		if err := m.SetOutcome(c.id, c.outcome, c.note); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s %s: %v, want %q", c.id, c.outcome, err, c.want)
		}
	}
	if err := m.SetOutcome("a", OutcomeDiscarded, "first"); err != nil {
		t.Fatal(err)
	}
	if err := m.SetOutcome("a", OutcomeAccepted, "used as is"); err != nil {
		t.Fatal(err)
	}
	if err := m.SetOutcome("c", OutcomeExtended, ""); err != nil {
		t.Fatal(err)
	}
	if j := get(t, m, "a"); j.Outcome != OutcomeAccepted || j.OutcomeNote != "used as is" || j.OutcomeAt == nil {
		t.Fatalf("job row: %+v", j)
	}
	if st, err := m.Store.GetStats("a"); err != nil || st.Outcome != OutcomeAccepted || st.OutcomeNote != "used as is" {
		t.Fatalf("stats row: %+v %v", st, err)
	}
	// A rewrite of the stats row (backfill --all, the end of a later attempt) keeps the outcome.
	if _, err := m.BackfillStats(true); err != nil {
		t.Fatal(err)
	}
	rep, err := m.Stats(time.Time{}, "repo")
	if err != nil {
		t.Fatal(err)
	}
	tot := rep.Totals
	if tot.Outcomes[OutcomeAccepted] != 1 || tot.Outcomes[OutcomeExtended] != 1 || tot.Unrecorded != 1 ||
		tot.CostPerUsableResultUSD != 3 || tot.OutcomeShares[OutcomeUnrecorded] != ratio(1, tot.Jobs) {
		t.Fatalf("totals: %+v", tot)
	}
	groups := map[string]StatsGroup{}
	for _, g := range rep.Groups {
		groups[g.Key] = g
	}
	if a := groups["alpha"]; a.Jobs != 2 || a.Outcomes[OutcomeAccepted] != 1 || a.Unrecorded != 1 || a.CostPerUsableResultUSD != 4 {
		t.Fatalf("alpha: %+v", a)
	}
	if b := groups["beta"]; b.Jobs != 1 || b.CostPerUsableResultUSD != 2 {
		t.Fatalf("beta: %+v", b)
	}
}

// A timed-out attempt with no usage in its stream has nothing to price: its cost stays unknown, never
// "$0.00 est.".
func TestTimedOutJobWithoutUsageStaysUnknown(t *testing.T) {
	m := newManager(t)
	j := enqueue(t, m, "FAKE_SLEEP=60", func(r *StartRequest) { r.TimeoutMinutes = 1 })
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "fake claude", 10*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(m.JobDir(j.ID), "argv.txt"))
		return err == nil
	})
	real := time.Now
	m.Now = func() time.Time { return real().Add(2 * time.Minute) }
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	j = get(t, m, j.ID)
	st, err := m.Store.GetStats(j.ID)
	if err != nil || j.Status != store.StatusTimeout || st.CostSource != store.CostSourceUnknown || st.CostEstimatedUSD != 0 {
		t.Fatalf("job %s stats %+v %v", j.Status, st, err)
	}
	if line := m.DoneLineFor(j); !strings.Contains(line, "cost unknown") {
		t.Fatalf("done line: %s", line)
	}
}

// A resumed job's outcome was about the previous result: Requeue clears it.
func TestRequeueClearsOutcome(t *testing.T) {
	m := newManager(t)
	now := time.Now()
	if err := m.Store.Insert(&store.Job{ID: "f", Agent: m.Agent, Title: "f", Kind: "fix", Brief: "b", Status: store.StatusFailed,
		Model: "sonnet", MaxTurns: 1, MaxBudgetUSD: 1, TimeoutMinutes: 1, CreatedAt: now, FinishedAt: &now}); err != nil {
		t.Fatal(err)
	}
	if err := m.SetOutcome("f", OutcomeRerun, "try opus"); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.Requeue("f", store.ResumeLimits{MaxTurns: 1, MaxBudgetUSD: 1, TimeoutMinutes: 1}, "ctx", ""); err != nil {
		t.Fatal(err)
	}
	if j := get(t, m, "f"); j.Outcome != "" || j.OutcomeNote != "" || j.OutcomeAt != nil {
		t.Fatalf("outcome kept: %+v", j)
	}
}
