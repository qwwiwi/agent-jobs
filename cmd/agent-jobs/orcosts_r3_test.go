package main

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

// orCostsRT is a socket-free http.RoundTripper.
type orCostsRT func(*http.Request) (*http.Response, error)

func (f orCostsRT) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func orCostsResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Status: http.StatusText(status), Header: http.Header{},
		Body: io.NopCloser(strings.NewReader(body)), Request: r, ContentLength: int64(len(body))}
}

// orCostsAgent writes agent "or"'s config (base_url https://openrouter.test/api, the key file with key)
// and the journal body, and routes the default transport (the command's client) through rt.
func orCostsAgent(t *testing.T, key, journal string, rt orCostsRT) string {
	t.Helper()
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
	cfg := `{"providers":{"openrouter":{"base_url":"https://openrouter.test/api","key_file":"` + keyFile +
		`","models":["z-ai/glm-5.3"],"daily_cap_usd":5}}}`
	if err := os.WriteFile(filepath.Join(state, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(state, jobs.OpenRouterCostsFile)
	if journal != "" {
		if err := os.WriteFile(path, []byte(journal), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	prev := http.DefaultTransport
	http.DefaultTransport = rt
	t.Cleanup(func() { http.DefaultTransport = prev })
	return path
}

// H1: the key's usage request answered with an error that echoes the key after padding: no fragment of
// the key reaches key_error (text or JSON).
func TestOpenRouterCostsKeyErrorScrubbed(t *testing.T) {
	key := "sk-or-" + "v1-" + strings.Repeat("0a1b2c3d4e5f6789", 4)
	pad := strings.Repeat("a", 130)
	orCostsAgent(t, key, "", func(r *http.Request) (*http.Response, error) {
		return orCostsResponse(r, http.StatusUnauthorized, `{"error":{"code":401,"message":"bad key `+pad+key+`"}}`), nil
	})
	for _, args := range [][]string{{"costs", "--agent", "or"}, {"costs", "--agent", "or", "--json"}} {
		var out bytes.Buffer
		if err := cmdOpenRouter(args, &out); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "HTTP 401") {
			t.Fatalf("%v: the key error is not shown:\n%s", args, out.String())
		}
		for i := 0; i+12 <= len(key); i++ {
			if frag := key[i : i+12]; strings.Contains(out.String(), frag) {
				t.Fatalf("%v: output carries the key fragment %q:\n%s", args, frag, out.String())
			}
		}
	}
}

// M2: a run with a billed request that has no generation id stays without OpenRouter's figure through
// --reconcile: the record of its other generations is not its cost.
func TestOpenRouterCostsReconcileKeepsUntrackedIncomplete(t *testing.T) {
	key := "sk-or-" + "v1-" + strings.Repeat("fedcba9876543210", 4)
	end := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	line := `{"type":"run","ts_end":"` + end + `","job_id":"job-mixed","attempt":1,"model":"z-ai/glm-5.3","requests":2,` +
		`"requests_without_generation_id":1,"cost_usd":0.2,"cost_unknown":false,"max_budget_usd":1,"status":"succeeded",` +
		`"generation_ids":["gen-1"],"openrouter":{"cost_usd":null,"reason":"1 billed request(s) without a generation id","generations":1,"found":1}}` + "\n"
	asked := 0
	path := orCostsAgent(t, key, line, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/api/v1/key":
			return orCostsResponse(r, http.StatusOK, `{"data":{"usage":1,"limit":null,"limit_remaining":null}}`), nil
		case "/api/v1/generation":
			asked++
			return orCostsResponse(r, http.StatusOK, `{"data":{"total_cost":0.1,"model":"z-ai/glm-5.3","provider_name":"P"}}`), nil
		}
		return orCostsResponse(r, http.StatusNotFound, `{}`), nil
	})
	var out bytes.Buffer
	if err := cmdOpenRouter([]string{"costs", "--agent", "or", "--reconcile"}, &out); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "reconciled 0 run(s); 1 still without OpenRouter's record") ||
		!strings.Contains(text, "proxy $0.2000  openrouter unknown") || !strings.Contains(text, "(1 without OpenRouter's figure)") {
		t.Fatalf("reconcile output:\n%s", text)
	}
	after, _ := os.ReadFile(path)
	if strings.Contains(string(after), `"type":"reconcile"`) {
		t.Fatalf("a reconcile line completed a run with an untracked request:\n%s", after)
	}
	if asked != 0 {
		t.Errorf("generations looked up for a run that cannot be completed: %d", asked)
	}
}
