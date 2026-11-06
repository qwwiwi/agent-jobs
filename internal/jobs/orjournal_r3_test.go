package jobs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// orRoundTrip is a socket-free http.RoundTripper.
type orRoundTrip func(*http.Request) (*http.Response, error)

func (f orRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// orResponse is a response of status with body for r.
func orResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Status: http.StatusText(status), Header: http.Header{},
		Body: io.NopCloser(strings.NewReader(body)), Request: r, ContentLength: int64(len(body))}
}

// orOtherKey is a key-shaped value that is not the job's key (an echo of another OpenRouter key).
var orOtherKey = "sk-or-" + "v1-" + strings.Repeat("fedcba9876543210", 4)

// orPad is word characters before an echoed key: no word boundary in front of its sk-or- prefix, and
// enough of them that a cut to 200 characters keeps most of the key.
var orPad = strings.Repeat("a", 130)

// assertNoKeyFragment fails when s carries any 12-character piece of one of secrets.
func assertNoKeyFragment(t *testing.T, where, s string, secrets ...string) {
	t.Helper()
	for _, sec := range secrets {
		for i := 0; i+12 <= len(sec); i++ {
			if frag := sec[i : i+12]; strings.Contains(s, frag) {
				t.Errorf("%s carries the key fragment %q: %s", where, frag, s)
				return
			}
		}
	}
}

// H1: an OpenRouter error that echoes the key (or another sk-or- key) after padding, in its body, in a
// transport error or in the URL, reaches no error, journal line or CLI text, not even as a fragment.
func TestORErrorsScrubPaddedKey(t *testing.T) {
	echo := orPad + orFakeKey + " then " + orPad + orOtherKey + " and " + orPad + "sk-or-v1-0123\u0000" + orFakeKey[13:]
	bodies := map[string]func(r *http.Request) (*http.Response, error){
		"401 body": func(r *http.Request) (*http.Response, error) {
			return orResponse(r, http.StatusUnauthorized, `{"error":{"code":401,"message":"`+strings.ReplaceAll(echo, "\u0000", `\u0000`)+`"}}`), nil
		},
		"503 body": func(r *http.Request) (*http.Response, error) {
			return orResponse(r, http.StatusServiceUnavailable, `{"error":{"code":503,"message":"`+strings.ReplaceAll(echo, "\u0000", `\u0000`)+`"}}`), nil
		},
		"transport": func(r *http.Request) (*http.Response, error) {
			return nil, errors.New("upstream said " + echo)
		},
		"no data": func(r *http.Request) (*http.Response, error) {
			return orResponse(r, http.StatusOK, `{"nothing":1}`), nil
		},
		"bad data": func(r *http.Request) (*http.Response, error) {
			return orResponse(r, http.StatusOK, `{"data":{"usage":"`+orPad+orOtherKey+`"}}`), nil
		},
	}
	// The API root itself carries a key-shaped value: every error naming the URL must lose it.
	bases := []string{"https://openrouter.test/api", "https://openrouter.test/api/" + orPad + orOtherKey}
	for name, rt := range bodies {
		for _, base := range bases {
			client := &http.Client{Transport: orRoundTrip(rt)}
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			_, err := FetchORKeyUsage(ctx, client, base, orFakeKey)
			cancel()
			if err == nil {
				t.Fatalf("%s: no error", name)
			}
			assertNoKeyFragment(t, name+": key error", err.Error(), orFakeKey, orOtherKey)
			// What the CLI prints as key_error.
			assertNoKeyFragment(t, name+": CLI key_error", RedactPrintable(err.Error(), 300), orFakeKey, orOtherKey)

			ctx, cancel = context.WithTimeout(context.Background(), 150*time.Millisecond)
			b := LookupGenerations(ctx, client, base, orFakeKey, []string{"gen-1"}, 10*time.Millisecond)
			cancel()
			if b.CostUSD != nil || b.Reason == "" {
				t.Fatalf("%s: lookup %+v", name, b)
			}
			assertNoKeyFragment(t, name+": lookup reason", b.Reason, orFakeKey, orOtherKey)

			path := filepath.Join(t.TempDir(), OpenRouterCostsFile)
			p := &orProxy{key: orFakeKey, token: "agent-jobs-proxy-0123456789abcdef"}
			jr := &orJournal{path: path, log: &bytes.Buffer{}, base: ORCostLine{Type: ORCostRun, JobID: "j", Attempt: 1}}
			jr.end(&openRouterJob{proxy: p}, store.StatusFailed, "upstream: "+err.Error()+" / "+echo, &b)
			raw, rerr := os.ReadFile(path)
			if rerr != nil || len(raw) == 0 {
				t.Fatalf("%s: no journal line (%v)", name, rerr)
			}
			assertNoKeyFragment(t, name+": journal line", string(raw), orFakeKey, orOtherKey)
		}
	}
}

// M1: a printable X-Generation-Id that is no generation id (msg_...) does not hide the request-id's gen-.
func TestGenerationIDFromHeaderNeedsGenPrefix(t *testing.T) {
	for _, c := range []struct {
		xgen, reqID, want string
	}{
		{"msg_bad", "gen-good", "gen-good"},
		{"gen-x", "gen-y", "gen-x"},
		{"msg_bad", "", ""},
		{"", "req_123", ""},
		{"gen-", "gen-ok", "gen-ok"},
	} {
		h := http.Header{}
		if c.xgen != "" {
			h.Set("X-Generation-Id", c.xgen)
		}
		if c.reqID != "" {
			h.Set("Request-Id", c.reqID)
		}
		if got := generationIDFromHeader(h); got != c.want {
			t.Errorf("X-Generation-Id %q, request-id %q: %q, want %q", c.xgen, c.reqID, got, c.want)
		}
	}
}

// M2: one billed response with a generation id and one without: OpenRouter's record of the first is not
// the attempt's cost, so the line stays incomplete, and so does it after a reconcile line.
func TestORCostJournalUntrackedRequestIncomplete(t *testing.T) {
	p := &orProxy{key: orFakeKey, token: "agent-jobs-proxy-0123456789abcdef"}
	for _, gen := range []string{"gen-1", ""} {
		if !p.reserve(0.01) {
			t.Fatal("reserve refused")
		}
		p.settle(0.01, responseUsage{GenID: gen, HasCost: true, Cost: 0.01, Tokens: TokenUsage{Input: 10, Output: 1}}, settleDone)
	}
	// A rejected request (an error answer read whole) is not billed: it leaves nothing untracked.
	if !p.reserve(0.01) {
		t.Fatal("reserve refused")
	}
	p.settle(0.01, responseUsage{}, settleRejected)
	client := &http.Client{Transport: orRoundTrip(func(r *http.Request) (*http.Response, error) {
		return orResponse(r, http.StatusOK, `{"data":{"total_cost":0.01,"model":"m","provider_name":"P"}}`), nil
	})}
	b := LookupGenerations(context.Background(), client, "https://openrouter.test/api", orFakeKey, p.generationIDs(), time.Millisecond)
	if b.CostUSD == nil {
		t.Fatalf("lookup of gen-1: %+v", b)
	}
	path := filepath.Join(t.TempDir(), OpenRouterCostsFile)
	jr := &orJournal{path: path, log: &bytes.Buffer{}, base: ORCostLine{Type: ORCostRun, JobID: "j", Attempt: 1}}
	jr.end(&openRouterJob{proxy: p}, store.StatusSucceeded, "", &b)
	raw, _ := os.ReadFile(path)
	if !bytes.Contains(raw, []byte(`"requests_without_generation_id":1`)) {
		t.Errorf("the line does not count the request without a generation id: %s", raw)
	}
	if err := AppendORReconcile(path, "j", 1, b, orFakeKey); err != nil {
		t.Fatal(err)
	}
	runs, malformed, err := ReadORCosts(path)
	if err != nil || malformed != 0 || len(runs) != 1 {
		t.Fatalf("runs %d malformed %d err %v", len(runs), malformed, err)
	}
	if o := runs[0].OpenRouter; o == nil || o.CostUSD != nil || !strings.Contains(o.Reason, "without a generation id") {
		t.Fatalf("OpenRouter's record of a run with an untracked request is complete: %+v\n%s", o, raw)
	}
	if s := SummarizeORCosts(runs, 0, time.Now(), 30); s.Total30d.OpenRouterMissing != 1 {
		t.Errorf("30-day total: %+v", s.Total30d)
	}
}

// H2: OpenRouter's generation record is read after the trusted completion is recorded. A lookup still
// waiting when the daemon's deadline passes leaves the job succeeded with its settled cost.
func TestORCostJournalLookupAfterCompletion(t *testing.T) {
	t.Setenv("AGENT_JOBS_TEST_GENERATION_WAIT", "20s")
	f := newFakeOpenRouter(t, "0.0123")
	block, hit := make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	release := func() { unblock.Do(func() { close(block) }) }
	f.mu.Lock()
	f.genBlock, f.genHit, f.genNotYet = block, hit, 0
	f.mu.Unlock()
	t.Cleanup(release)
	m, _ := orManager(t, f.srv.URL+"/api", writeKeyFile(t, t.TempDir()))
	m.KillGrace = 5 * time.Second
	j := enqueue(t, m, "lookup after completion", func(r *StartRequest) { r.Model = "openrouter:" + orGLM })
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-hit:
	case <-time.After(30 * time.Second):
		t.Fatal("the generation lookup never started")
	}
	// The daemon's clock passes the job's deadline while the lookup still waits.
	late := time.Now().Add(time.Duration(j.TimeoutMinutes+5) * time.Minute)
	m.Now = func() time.Time { return late }
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	got := get(t, m, j.ID)
	if got.Status != store.StatusSucceeded || got.CostUSD != 0.0123 || got.CostIsUnknown() {
		t.Fatalf("status %s (%s), cost %v unknown %v: want succeeded at the settled $0.0123",
			got.Status, got.Error, got.CostUSD, got.CostIsUnknown())
	}
	release()
	waitFor(t, "the journal line", 15*time.Second, func() bool {
		_, err := os.Stat(journalPath(m))
		return err == nil
	})
	raw, lines := readJournal(t, m)
	if len(lines) != 1 || lines[0].Status != store.StatusSucceeded || lines[0].CostUSD == nil || *lines[0].CostUSD != 0.0123 {
		t.Fatalf("journal: %s", strings.Join(raw, "\n"))
	}
	if got := get(t, m, j.ID); got.Status != store.StatusSucceeded || got.CostUSD != 0.0123 {
		t.Fatalf("the lookup changed the job: %s %v", got.Status, got.CostUSD)
	}
}

// H2: the lookup after the completion is bounded by what is left of the job's deadline; none left, it
// does not run (and the line says so).
func TestGenerationLookupWindowBoundedByDeadline(t *testing.T) {
	now := time.Now()
	if w := generationLookupWindow(time.Time{}, now); w != generationLookupWait {
		t.Errorf("no deadline: %v, want %v", w, generationLookupWait)
	}
	if w := generationLookupWindow(now.Add(3*time.Second), now); w != 3*time.Second {
		t.Errorf("3s left: %v", w)
	}
	if w := generationLookupWindow(now.Add(-time.Minute), now); w != 0 {
		t.Errorf("past the deadline: %v", w)
	}
	o := &openRouterJob{proxy: &orProxy{key: orFakeKey, requests: 1, genIDs: []string{"gen-1"},
		client: &http.Client{Transport: orRoundTrip(func(r *http.Request) (*http.Response, error) {
			t.Error("looked up past the deadline")
			return nil, errors.New("no")
		})}}}
	if b := o.lookup(now.Add(-time.Minute)); b.CostUSD != nil || !strings.Contains(b.Reason, "deadline has passed") {
		t.Errorf("lookup past the deadline: %+v", b)
	}
}
