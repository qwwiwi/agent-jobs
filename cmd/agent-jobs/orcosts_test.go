package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

// `agent-jobs openrouter costs`: the per-run lines with the proxy's and OpenRouter's cost side by side,
// a mismatch flagged, totals per day / per model / 30 days, malformed lines counted, the key's usage
// header, and --reconcile filling OpenRouter's record of a run that lacks it (appended, never rewritten).
func TestOpenRouterCostsCommand(t *testing.T) {
	key := "sk-or-" + "v1-" + strings.Repeat("fedcba9876543210", 4)
	var genAuth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/key":
			if r.Header.Get("Authorization") != "Bearer "+key {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, `{"data":{"usage":3.25,"limit":20,"limit_remaining":16.75}}`)
		case "/api/v1/generation":
			genAuth = append(genAuth, r.Header.Get("Authorization"))
			if r.URL.Query().Get("id") != "gen-late" {
				http.NotFound(w, r)
				return
			}
			_, _ = io.WriteString(w, `{"data":{"total_cost":0.5,"provider_name":"Novita","model":"z-ai/glm-5.3","native_tokens_prompt":10,"native_tokens_completion":5}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	home := t.TempDir()
	t.Setenv("AGENT_JOBS_HOME", home)
	state := filepath.Join(home, "or")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(t.TempDir(), "or.env")
	if err := os.WriteFile(keyFile, []byte("OPENROUTER_API_KEY="+key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := `{"providers":{"openrouter":{"base_url":"` + srv.URL + `/api","key_file":"` + keyFile + `","models":["z-ai/glm-5.3"],"daily_cap_usd":5}}}`
	if err := os.WriteFile(filepath.Join(state, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	line := func(job, gen string, proxy float64, billed *float64, ago time.Duration) string {
		l := jobs.ORCostLine{Type: jobs.ORCostRun, TSEnd: now.Add(-ago).Format(time.RFC3339), JobID: job, Attempt: 1,
			Title: "t " + job, Model: "z-ai/glm-5.3", Endpoint: "novita/fp8", Requests: 1, TokensIn: 100, TokensOut: 10,
			CostUSD: &proxy, Status: "succeeded", GenerationIDs: []string{gen}, OpenRouter: &jobs.ORBilled{CostUSD: billed}}
		if billed == nil {
			l.OpenRouter.Reason = "1 of 1 generation(s) not read"
		}
		b, _ := json.Marshal(l)
		return string(b)
	}
	billed := 0.1
	journal := filepath.Join(state, jobs.OpenRouterCostsFile)
	body := line("job-ok", "gen-ok", 0.1, &billed, time.Hour) + "\n" + line("job-late", "gen-late", 0.4, nil, 2*time.Hour) + "\n{broken\n"
	if err := os.WriteFile(journal, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := cmdOpenRouter([]string{"costs", "--agent", "or"}, &out); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{"openrouter key: usage $3.2500, limit $20.00, remaining $16.75", "job-ok", "job-late",
		"proxy $0.1000  openrouter $0.1000", "proxy $0.4000  openrouter unknown", "(1 without OpenRouter's figure)",
		"last 30 days: 2 run(s)  proxy $0.5000  openrouter $0.1000", "skipped 1 malformed line(s)"} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
	if len(genAuth) != 0 {
		t.Fatalf("generations looked up without --reconcile: %d", len(genAuth))
	}
	out.Reset()
	if err := cmdOpenRouter([]string{"costs", "--agent", "or", "--reconcile"}, &out); err != nil {
		t.Fatal(err)
	}
	text = out.String()
	if !strings.Contains(text, "reconciled 1 run(s); 0 still without OpenRouter's record") ||
		!strings.Contains(text, "proxy $0.4000  openrouter $0.5000  succeeded  MISMATCH >10%") ||
		!strings.Contains(text, "last 30 days: 2 run(s)  proxy $0.5000  openrouter $0.6000") {
		t.Fatalf("reconcile output:\n%s", text)
	}
	if len(genAuth) != 1 || genAuth[0] != "Bearer "+key {
		t.Fatalf("generation lookups %d", len(genAuth))
	}
	after, _ := os.ReadFile(journal)
	if !strings.HasPrefix(string(after), body) || strings.Count(string(after), `"type":"reconcile"`) != 1 {
		t.Fatalf("the journal was not appended to:\n%s", after)
	}
	// A second reconcile has nothing left to do.
	out.Reset()
	if err := cmdOpenRouter([]string{"costs", "--agent", "or", "--reconcile", "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Key        *jobs.ORKeyUsage `json:"key"`
		Reconciled *int             `json:"reconciled"`
		Runs       []struct {
			JobID    string `json:"job_id"`
			Mismatch bool   `json:"mismatch"`
		} `json:"runs"`
		PerModel []jobs.ORCostTotal `json:"per_model"`
		Total    jobs.ORCostTotal   `json:"total_30d"`
		Bad      int                `json:"malformed_lines"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("json: %v\n%s", err, out.String())
	}
	if doc.Key == nil || doc.Key.Usage == nil || *doc.Key.Usage != 3.25 || doc.Reconciled == nil || *doc.Reconciled != 0 ||
		len(doc.Runs) != 2 || doc.Runs[0].JobID != "job-late" || !doc.Runs[0].Mismatch || len(doc.PerModel) != 1 ||
		doc.Total.Runs != 2 || doc.Bad != 1 {
		t.Fatalf("json: %s", out.String())
	}
	if strings.Contains(out.String(), key) || strings.Contains(string(after), key) || strings.Contains(text, key) {
		t.Fatal("the key reached the output or the journal")
	}
	if err := cmdOpenRouter([]string{"costs"}, &out); err == nil {
		t.Fatal("costs without --agent accepted")
	}
}
