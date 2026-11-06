package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Low (fix loop 2): a run without OpenRouter's figure and without any generation id to look up is
// counted among those still without OpenRouter's record, in text and JSON.
func TestOpenRouterCostsReconcileCountsRunsWithoutGenerationIDs(t *testing.T) {
	key := "sk-or-" + "v1-" + strings.Repeat("0123fedc4567ba98", 4)
	end := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	run := func(job, extra string) string {
		return `{"type":"run","ts_end":"` + end + `","job_id":"` + job + `","attempt":1,"model":"z-ai/glm-5.3","requests":1,` + extra +
			`"cost_usd":0.2,"cost_unknown":false,"max_budget_usd":1,"status":"succeeded","generation_ids":[],` +
			`"openrouter":{"cost_usd":null,"reason":"not looked up","generations":0}}` + "\n"
	}
	// One run names its untracked request, one (an older line) carries no ids and no count.
	journal := run("job-untracked", `"requests_without_generation_id":1,`) + run("job-noids", "")
	asked := 0
	orCostsAgent(t, key, journal, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/api/v1/key":
			return orCostsResponse(r, http.StatusOK, `{"data":{"usage":1,"limit":null,"limit_remaining":null}}`), nil
		case "/api/v1/generation":
			asked++
		}
		return orCostsResponse(r, http.StatusNotFound, `{}`), nil
	})
	var out bytes.Buffer
	if err := cmdOpenRouter([]string{"costs", "--agent", "or", "--reconcile"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "reconciled 0 run(s); 2 still without OpenRouter's record") {
		t.Fatalf("reconcile output:\n%s", out.String())
	}
	out.Reset()
	if err := cmdOpenRouter([]string{"costs", "--agent", "or", "--reconcile", "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	var v struct {
		Reconciled *int `json:"reconciled"`
		Unresolved *int `json:"unresolved"`
	}
	if err := json.Unmarshal(out.Bytes(), &v); err != nil {
		t.Fatalf("json: %v\n%s", err, out.String())
	}
	if v.Reconciled == nil || *v.Reconciled != 0 || v.Unresolved == nil || *v.Unresolved != 2 {
		t.Fatalf("reconciled %v unresolved %v, want 0 and 2:\n%s", v.Reconciled, v.Unresolved, out.String())
	}
	if asked != 0 {
		t.Errorf("generations looked up for runs without ids: %d", asked)
	}
}
