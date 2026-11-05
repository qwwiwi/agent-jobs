package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// The OpenRouter cost journal: one line per attempt in <state>/openrouter-costs.jsonl.

// journalPath is the cost journal of m's agent.
func journalPath(m *Manager) string { return filepath.Join(m.StateDir, OpenRouterCostsFile) }

// readJournal returns the journal's raw lines and their decoded form; it checks the file's mode.
func readJournal(t *testing.T, m *Manager) ([]string, []ORCostLine) {
	t.Helper()
	path := journalPath(m)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("journal: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("journal mode %o, want 600", fi.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw []string
	var lines []ORCostLine
	for _, s := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		var l ORCostLine
		if err := json.Unmarshal([]byte(s), &l); err != nil {
			t.Fatalf("journal line %q: %v", s, err)
		}
		raw, lines = append(raw, s), append(lines, l)
	}
	return raw, lines
}

// assertNoSecretInLine fails when a journal line carries the key, a proxy token or the prompt.
func assertNoSecretInLine(t *testing.T, raw, prompt string) {
	t.Helper()
	for _, bad := range []string{orFakeKey, orFakeKey[9:], "agent-jobs-proxy-", prompt, `"hi"`} {
		if strings.Contains(raw, bad) {
			t.Errorf("journal line carries %q: %s", bad, raw)
		}
	}
}

func TestORCostJournalSuccess(t *testing.T) {
	f := newFakeOpenRouter(t, "0.0123")
	f.billed = "0.0125" // within 10% of the proxy's figure: no mismatch
	m, _ := orManager(t, f.srv.URL+"/api", writeKeyFile(t, t.TempDir()))
	j := enqueue(t, m, "journal this run", func(r *StartRequest) {
		r.Model = "openrouter:" + orGLM
		r.Title = "cheap model run"
	})
	j = runToEnd(t, m, j.ID)
	if j.Status != store.StatusSucceeded {
		t.Fatalf("status %s (%s)", j.Status, j.Error)
	}
	raw, lines := readJournal(t, m)
	if len(lines) != 1 {
		t.Fatalf("journal lines %d, want 1:\n%s", len(lines), strings.Join(raw, "\n"))
	}
	l := lines[0]
	if _, err := time.Parse(time.RFC3339, l.TSEnd); err != nil || !strings.HasSuffix(l.TSEnd, "Z") {
		t.Errorf("ts_end %q is not UTC RFC 3339", l.TSEnd)
	}
	if l.Type != ORCostRun || l.JobID != j.ID || l.Attempt != 1 || l.Title != "cheap model run" || l.Kind != "general" ||
		l.Model != orGLM || l.Endpoint != "baidu/fp8" || l.Requests != 1 || l.TokensIn != 1000 || l.TokensOut != 50 ||
		l.MaxBudgetUSD != j.MaxBudgetUSD || l.Status != store.StatusSucceeded || l.Error != "" {
		t.Errorf("line: %s", raw[0])
	}
	if l.CostUSD == nil || *l.CostUSD != 0.0123 || l.CostUnknown {
		t.Errorf("proxy cost: %s", raw[0])
	}
	if len(l.GenerationIDs) != 1 || l.GenerationIDs[0] != "gen-1" || len(l.Providers) != 1 || l.Providers[0] != "Baidu" {
		t.Errorf("generations / providers: %s", raw[0])
	}
	o := l.OpenRouter
	if o == nil || o.CostUSD == nil || *o.CostUSD != 0.0125 || o.Found != 1 || o.Generations != 1 || o.Reason != "" ||
		len(o.Providers) != 1 || o.Providers[0] != "Baidu" || len(o.Models) != 1 || o.Models[0] != orGLM ||
		o.NativeTokensPrompt != 990 || o.NativeTokensCompletion != 48 {
		t.Errorf("openrouter record: %s", raw[0])
	}
	f.mu.Lock()
	genAuth := append([]string{}, f.genAuth...)
	f.mu.Unlock()
	if len(genAuth) != 1 || genAuth[0] != "Bearer "+orFakeKey {
		t.Errorf("generation lookups %d (with the key: %v)", len(genAuth), len(genAuth) == 1 && genAuth[0] == "Bearer "+orFakeKey)
	}
	assertNoSecretInLine(t, raw[0], "journal this run")
	// The key is still in no artifact of the job, no log and no row.
	assertNoKey(t, m.JobDir(j.ID), "", j, m)
}

func TestORCostJournalBudgetRefusal(t *testing.T) {
	f := newFakeOpenRouter(t, "2.5")
	m, _ := orManager(t, f.srv.URL+"/api", writeKeyFile(t, t.TempDir()))
	j := enqueue(t, m, "FAKE_TWO", func(r *StartRequest) { r.Model = "openrouter:" + orGLM; r.MaxBudgetUSD = 1 })
	j = runToEnd(t, m, j.ID)
	if j.Status != store.StatusFailed || !strings.Contains(j.Error, "openrouter budget reached") {
		t.Fatalf("status %s (%s)", j.Status, j.Error)
	}
	raw, lines := readJournal(t, m)
	if len(lines) != 1 {
		t.Fatalf("journal lines %d, want 1", len(lines))
	}
	l := lines[0]
	if l.Status != store.StatusFailed || !strings.Contains(l.Error, "openrouter budget reached") || l.Requests != 1 ||
		l.CostUSD == nil || *l.CostUSD != 2.5 || l.MaxBudgetUSD != 1 || l.OpenRouter == nil || l.OpenRouter.CostUSD == nil ||
		*l.OpenRouter.CostUSD != 2.5 {
		t.Errorf("line: %s", raw[0])
	}
	assertNoSecretInLine(t, raw[0], "FAKE_TWO")
}

func TestORCostJournalStartFailure(t *testing.T) {
	f := newFakeOpenRouter(t, "0.01")
	m, _ := orManager(t, f.srv.URL+"/api", filepath.Join(t.TempDir(), "missing.env"))
	j := enqueue(t, m, "x", func(r *StartRequest) { r.Model = "openrouter:" + orGLM })
	j = runToEnd(t, m, j.ID)
	if j.Status != store.StatusFailed {
		t.Fatalf("status %s", j.Status)
	}
	raw, lines := readJournal(t, m)
	if len(lines) != 1 {
		t.Fatalf("journal lines %d, want 1", len(lines))
	}
	l := lines[0]
	if l.Status != store.StatusFailed || !strings.Contains(l.Error, "openrouter job not started") || l.Requests != 0 ||
		l.CostUSD == nil || *l.CostUSD != 0 || l.CostUnknown || l.Model != orGLM || l.OpenRouter == nil ||
		l.OpenRouter.CostUSD == nil || *l.OpenRouter.CostUSD != 0 {
		t.Errorf("line: %s", raw[0])
	}
}

// A request that ended without its final usage: the proxy's cost is unknown (null, with the
// reservation it was charged), OpenRouter's record still says what was billed.
func TestORCostJournalCutStream(t *testing.T) {
	f := newFakeOpenRouter(t, "cut")
	m, _ := orManager(t, f.srv.URL+"/api", writeKeyFile(t, t.TempDir()))
	j := enqueue(t, m, "x", func(r *StartRequest) { r.Model = "openrouter:" + orGLM })
	runToEnd(t, m, j.ID)
	raw, lines := readJournal(t, m)
	if len(lines) != 1 {
		t.Fatalf("journal lines %d, want 1", len(lines))
	}
	l := lines[0]
	if l.CostUSD != nil || !l.CostUnknown || l.CostReservedUSD == nil || *l.CostReservedUSD <= 0 ||
		l.OpenRouter == nil || l.OpenRouter.CostUSD == nil || *l.OpenRouter.CostUSD != 0.002 || !strings.Contains(raw[0], `"cost_usd":null`) {
		t.Errorf("line: %s", raw[0])
	}
}

// OpenRouter's record not available (yet): the line keeps the generation ids, openrouter.cost_usd null
// with a reason; --reconcile (ReadORCosts merges its line) fills it later.
func TestORCostJournalGenerationMissingThenReconciled(t *testing.T) {
	t.Setenv("AGENT_JOBS_TEST_GENERATION_WAIT", "400ms")
	f := newFakeOpenRouter(t, "0.04")
	f.genStatus = http.StatusNotFound
	m, _ := orManager(t, f.srv.URL+"/api", writeKeyFile(t, t.TempDir()))
	j := enqueue(t, m, "x", func(r *StartRequest) { r.Model = "openrouter:" + orGLM })
	runToEnd(t, m, j.ID)
	raw, lines := readJournal(t, m)
	if len(lines) != 1 {
		t.Fatalf("journal lines %d, want 1", len(lines))
	}
	l := lines[0]
	if l.OpenRouter == nil || l.OpenRouter.CostUSD != nil || !strings.Contains(l.OpenRouter.Reason, "--reconcile") ||
		len(l.GenerationIDs) != 1 || l.CostUSD == nil || *l.CostUSD != 0.04 {
		t.Fatalf("line: %s", raw[0])
	}
	f.mu.Lock()
	f.genStatus, f.billed = 0, "0.05"
	f.mu.Unlock()
	b := LookupGenerations(context.Background(), http.DefaultClient, f.srv.URL+"/api", orFakeKey, l.GenerationIDs, time.Millisecond)
	if b.CostUSD == nil {
		t.Fatalf("lookup: %+v", b)
	}
	if err := AppendORReconcile(journalPath(m), l.JobID, l.Attempt, b, orFakeKey); err != nil {
		t.Fatal(err)
	}
	runs, malformed, err := ReadORCosts(journalPath(m))
	if err != nil || malformed != 0 || len(runs) != 1 {
		t.Fatalf("runs %d, malformed %d, err %v", len(runs), malformed, err)
	}
	if r := runs[0]; r.OpenRouter == nil || r.OpenRouter.CostUSD == nil || *r.OpenRouter.CostUSD != 0.05 || !r.Mismatch {
		t.Fatalf("reconciled run: %+v (0.04 vs 0.05 is a mismatch over 10%%)", r.OpenRouter)
	}
}

// The daemon ends the attempt (cancel): the wrapper's SIGTERM handler writes the line as killed.
func TestORCostJournalCancelled(t *testing.T) {
	f := newFakeOpenRouter(t, "0.01")
	m, _ := orManager(t, f.srv.URL+"/api", writeKeyFile(t, t.TempDir()))
	m.KillGrace = 5 * time.Second
	j := enqueue(t, m, "FAKE_HANG", func(r *StartRequest) { r.Model = "openrouter:" + orGLM })
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	codes := filepath.Join(m.JobDir(j.ID), "claude-codes.txt")
	waitFor(t, "the first request", 15*time.Second, func() bool {
		b, _ := os.ReadFile(codes)
		return bytes.HasPrefix(b, []byte("first 200"))
	})
	if err := m.Cancel(j.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the journal line", 10*time.Second, func() bool {
		_, err := os.Stat(journalPath(m))
		return err == nil
	})
	raw, lines := readJournal(t, m)
	if len(lines) != 1 {
		t.Fatalf("journal lines %d, want 1", len(lines))
	}
	l := lines[0]
	if l.Status != ORStatusKilled || l.Requests != 1 || len(l.GenerationIDs) != 1 || l.OpenRouter == nil ||
		l.OpenRouter.CostUSD != nil || !strings.Contains(l.OpenRouter.Reason, "killed") {
		t.Errorf("line: %s", raw[0])
	}
	if got := get(t, m, j.ID); got.Status != store.StatusCancelled {
		t.Errorf("job status %s", got.Status)
	}
}

// A write failure is logged and never fails the job.
func TestORCostJournalWriteFailureLogged(t *testing.T) {
	dir := t.TempDir()
	jobDir := filepath.Join(dir, "jobs", "j1")
	// The journal path is a directory: the append fails.
	if err := os.MkdirAll(filepath.Join(dir, OpenRouterCostsFile), 0o700); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	jr := newORJournal(jobDir, &store.Job{ID: "j1", Model: "openrouter:" + orGLM, Attempt: 1}, &log)
	jr.end(nil, store.StatusFailed, "x", nil)
	if !strings.Contains(log.String(), "openrouter cost journal:") {
		t.Fatalf("log: %q", log.String())
	}
}

// A line that would carry the key or the proxy token is not written.
func TestORCostJournalRefusesSecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), OpenRouterCostsFile)
	if err := appendORCostLine(path, ORCostLine{Type: ORCostRun, JobID: "j", Error: "boom " + orFakeKey}, orFakeKey); err == nil {
		t.Fatal("a line with the key was written")
	}
	if _, err := os.Stat(path); err == nil {
		b, _ := os.ReadFile(path)
		if bytes.Contains(b, []byte(orFakeKey)) {
			t.Fatal("key in the journal")
		}
	}
	// The wrapper scrubs the key from the error it records.
	p := &orProxy{key: orFakeKey, token: "agent-jobs-proxy-tok"}
	jr := &orJournal{path: path, log: &bytes.Buffer{}, base: ORCostLine{Type: ORCostRun, JobID: "j", Attempt: 1}}
	jr.end(&openRouterJob{proxy: p}, store.StatusFailed, "upstream said "+orFakeKey+" and agent-jobs-proxy-tok", nil)
	b, _ := os.ReadFile(path)
	if len(b) == 0 || bytes.Contains(b, []byte(orFakeKey)) || bytes.Contains(b, []byte("agent-jobs-proxy-tok")) {
		t.Fatalf("journal: %s", b)
	}
}

func orLine(job string, end time.Time, model string, proxy, billed *float64) string {
	l := ORCostLine{Type: ORCostRun, TSEnd: end.UTC().Format(time.RFC3339), JobID: job, Attempt: 1, Model: model,
		CostUSD: proxy, CostUnknown: proxy == nil, Status: store.StatusSucceeded, GenerationIDs: []string{"gen-" + job},
		OpenRouter: &ORBilled{CostUSD: billed}}
	b, _ := json.Marshal(l)
	return string(b)
}

func TestORCostsAggregation(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), OpenRouterCostsFile)
	lines := []string{
		orLine("a", now.Add(-1*time.Hour), orGLM, f64(0.10), f64(0.10)),
		orLine("b", now.Add(-2*time.Hour), orKimi, f64(0.20), f64(0.30)), // mismatch
		`{"type":"run","job_id":"broken"`,                                // malformed
		"not json",                                                       // malformed
		orLine("c", now.Add(-26*time.Hour), orGLM, nil, nil),             // unknown both
		orLine("d", now.Add(-40*24*time.Hour), orGLM, f64(5), f64(5)),    // outside 30 days
		`{"type":"run","job_id":"e","ts_end":"yesterday"}`,               // malformed time
		"",
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AppendORReconcile(path, "c", 1, ORBilled{CostUSD: f64(0.07), Generations: 1, Found: 1}, orFakeKey); err != nil {
		t.Fatal(err)
	}
	runs, malformed, err := ReadORCosts(path)
	if err != nil {
		t.Fatal(err)
	}
	if malformed != 3 || len(runs) != 4 {
		t.Fatalf("malformed %d, runs %d; want 3, 4", malformed, len(runs))
	}
	s := SummarizeORCosts(runs, malformed, now, 7)
	if len(s.Runs) != 3 || s.Runs[0].JobID != "c" || !s.Runs[1].Mismatch || s.Runs[2].Mismatch {
		t.Fatalf("runs: %+v", s.Runs)
	}
	if len(s.PerDay) != 2 || s.PerDay[0].Key != "2026-11-04" || s.PerDay[1].Key != "2026-11-05" ||
		s.PerDay[1].Runs != 2 || !near(s.PerDay[1].ProxyUSD, 0.30) || !near(s.PerDay[1].OpenRouterUSD, 0.40) ||
		s.PerDay[0].ProxyUnknown != 1 || !near(s.PerDay[0].OpenRouterUSD, 0.07) {
		t.Fatalf("per day: %+v", s.PerDay)
	}
	if len(s.PerModel) != 2 || s.PerModel[0].Key != orKimi || s.PerModel[1].Key != orGLM || s.PerModel[1].Runs != 2 {
		t.Fatalf("per model: %+v", s.PerModel)
	}
	if s.Total30d.Runs != 3 || !near(s.Total30d.OpenRouterUSD, 0.47) || s.Total30d.OpenRouterMissing != 0 {
		t.Fatalf("30 days: %+v", s.Total30d)
	}
	var out bytes.Buffer
	FormatORCosts(&out, s)
	for _, want := range []string{"MISMATCH >10%", "per day (UTC):", "per model:", "last 30 days: 3 run(s)",
		"skipped 3 malformed line(s)", "proxy unknown", "openrouter $0.0700"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func near(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }

// A missing journal is empty, not an error.
func TestORCostsMissingJournal(t *testing.T) {
	runs, malformed, err := ReadORCosts(filepath.Join(t.TempDir(), "none.jsonl"))
	if err != nil || malformed != 0 || len(runs) != 0 {
		t.Fatalf("runs %d malformed %d err %v", len(runs), malformed, err)
	}
}
