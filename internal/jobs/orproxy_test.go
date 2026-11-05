package jobs

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// proxyFixture is an orProxy in front of an httptest upstream; hits counts the upstream requests.
type proxyFixture struct {
	p    *orProxy
	hits atomic.Int64
}

type orProxyRoutingTransport func(*http.Request) (*http.Response, error)

func (f orProxyRoutingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestOpenRouterProxyDropsClientRouting(t *testing.T) {
	const body = `{"model":"client/model","models":["x/expensive"],"route":"fallback","provider":{"order":["client-provider"]},"plugins":[{"id":"web"}],"transforms":["middle-out"],"preset":"client-preset","usage":{"include":true},"session_id":"client-session","max_tokens":10,"messages":[{"role":"user","content":"hello"}],"stream":true}`
	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
		for _, sessionID := range []string{"job-session", ""} {
			t.Run(path+"/session="+sessionID, func(t *testing.T) {
				p := &orProxy{token: "tok", slug: orGLM, upstream: "https://upstream.invalid/api",
					provider: json.RawMessage(`{"order":["job-provider"]}`), sessionID: sessionID, budget: 5}
				hits := 0
				p.client = &http.Client{Transport: orProxyRoutingTransport(func(r *http.Request) (*http.Response, error) {
					hits++
					if r.URL.Path != "/api"+path {
						t.Errorf("upstream path = %q", r.URL.Path)
					}
					var got map[string]json.RawMessage
					if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
						t.Fatal(err)
					}
					for _, key := range []string{"models", "route", "plugins", "transforms", "preset", "usage"} {
						if _, ok := got[key]; ok {
							t.Errorf("client %s reached upstream", key)
						}
					}
					want := map[string]string{"model": `"` + orGLM + `"`, "max_tokens": "10",
						"messages": `[{"role":"user","content":"hello"}]`, "stream": "true"}
					if path == "/v1/messages" {
						want["provider"] = string(p.provider)
						if sessionID != "" {
							want["session_id"] = `"` + sessionID + `"`
						}
					}
					for _, key := range []string{"provider", "session_id"} {
						if _, ok := want[key]; !ok && got[key] != nil {
							t.Errorf("client %s reached upstream", key)
						}
					}
					for key, value := range want {
						if string(got[key]) != value {
							t.Errorf("%s = %s, want %s", key, got[key], value)
						}
					}
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
						Body: io.NopCloser(strings.NewReader(`{"usage":{"cost":0.01}}`))}, nil
				})}
				r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
				r.Header.Set("Authorization", "Bearer tok")
				w := httptest.NewRecorder()
				p.ServeHTTP(w, r)
				if w.Code != http.StatusOK || hits != 1 {
					t.Fatalf("status %d, upstream hits %d; want 200, 1", w.Code, hits)
				}
			})
		}
	}
}

func newProxyFixture(t *testing.T, budget float64, price ModelPrice, upstream http.HandlerFunc) *proxyFixture {
	t.Helper()
	f := &proxyFixture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		upstream(w, r)
	}))
	t.Cleanup(srv.Close)
	f.p = &orProxy{upstream: srv.URL + "/api", key: orFakeKey, token: "tok", slug: orGLM, provider: json.RawMessage(`{}`),
		budget: budget, price: price, client: &http.Client{Timeout: 10 * time.Second}}
	if err := startORProxy(f.p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.p.close)
	return f
}

// do sends one request with the job's token and returns its status and body.
func (f *proxyFixture) do(t *testing.T, method, path, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, f.p.URL()+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// sseOK answers a /v1/messages request with a complete stream that reports cost.
func sseOK(cost string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":1}}}\n\n"+
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":5,\"cost\":"+cost+"}}\n\n"+
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}
}

// B1 (#136 R3 review): the proxy forwards exactly POST /v1/messages and POST /v1/messages/count_tokens;
// any other path or method is refused without reaching OpenRouter (it would carry the real key past the
// model rewrite, the routing, the budget and the accounting).
func TestOpenRouterProxyAllowList(t *testing.T) {
	f := newProxyFixture(t, 5, ModelPrice{Input: 1, Output: 1}, sseOK("0.01"))
	body := `{"model":"x","max_tokens":10,"messages":[]}`
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/v1/chat/completions"},
		{http.MethodPost, "/v1/completions"},
		{http.MethodPost, "/v1/complete"},
		{http.MethodGet, "/v1/models"},
		{http.MethodGet, "/v1/messages"},
		{http.MethodPut, "/v1/messages"},
		{http.MethodPost, "/v1/messages/batches"},
		{http.MethodPost, "/v1/messages/"},
		{http.MethodPost, "/v1/messages/count_tokens/x"},
		{http.MethodGet, "/v1/credits"},
		{http.MethodPost, "/v1/keys"},
		{http.MethodPost, "/api/v1/chat/completions"},
	} {
		code, out := f.do(t, c.method, c.path, body)
		if code != http.StatusForbidden || !strings.Contains(out, "permission_error") {
			t.Errorf("%s %s: %d %s, want 403", c.method, c.path, code, out)
		}
	}
	if n := f.hits.Load(); n != 0 {
		t.Fatalf("%d refused request(s) reached the upstream", n)
	}
	if code, _ := f.do(t, http.MethodPost, "/v1/messages?beta=true", body); code != http.StatusOK {
		t.Fatalf("POST /v1/messages: %d", code)
	}
	if code, _ := f.do(t, http.MethodPost, "/v1/messages/count_tokens", body); code != http.StatusOK {
		t.Fatalf("POST /v1/messages/count_tokens: %d", code)
	}
	if n := f.hits.Load(); n != 2 {
		t.Fatalf("upstream hits %d, want 2", n)
	}
}

// B3 (#136 R3 review): requests in flight at once cannot all pass the same remaining budget: each
// reserves its worst case (max_tokens at the output price, plus its input) before it is sent, and a
// request is refused once what was spent plus what is reserved reaches the budget.
func TestOpenRouterProxyReservesConcurrentRequests(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	open := func() { once.Do(func() { close(release) }) }
	var arrived atomic.Int64
	// Output $40000/M: max_tokens 10 reserves $0.40; the budget $1 admits three ($0, $0.40, $0.80 < $1).
	f := newProxyFixture(t, 1, ModelPrice{Output: 40000}, func(w http.ResponseWriter, r *http.Request) {
		arrived.Add(1)
		<-release
		sseOK("0.1")(w, r)
	})
	t.Cleanup(open) // after the fixture's: runs before the upstream closes, should the test end early
	body := `{"model":"x","max_tokens":10,"stream":true,"messages":[]}`
	codes := make(chan int, 5)
	for range 5 {
		go func() {
			req, _ := http.NewRequest(http.MethodPost, f.p.URL()+"/v1/messages", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer tok")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				codes <- -1
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			codes <- resp.StatusCode
		}()
	}
	refused := 0
	deadline := time.After(10 * time.Second)
	for refused < 2 {
		select {
		case c := <-codes:
			if c != http.StatusBadRequest {
				t.Fatalf("a request ended with %d while the upstream holds the admitted ones", c)
			}
			refused++
		case <-deadline:
			t.Fatalf("refused %d of 5 concurrent requests, upstream reached by %d, want 2 refused and 3 sent", refused, arrived.Load())
		}
	}
	if n := arrived.Load(); n > 3 {
		t.Fatalf("upstream reached by %d concurrent requests, want 3", n)
	}
	open()
	for range 3 {
		if c := <-codes; c != http.StatusOK {
			t.Fatalf("admitted request: %d", c)
		}
	}
	if c := f.p.Cost(); c < 0.3-1e-9 || c > 0.3+1e-9 {
		t.Fatalf("cost %v, want the three reported $0.1", c)
	}
	if f.p.CostUncertain() {
		t.Fatalf("settled requests left the cost uncertain: uncertain %d reserved %v cost %v", f.p.uncertain, f.p.reserved, f.p.cost)
	}
}

// B4 (#136 R3 review): a request that ends without its final usage (the upstream stream cut, a transport
// error, the client gone) is charged at its reservation and makes the cost uncertain; it never counts as
// a known $0.
func TestOpenRouterProxyIncompleteKeepsReservation(t *testing.T) {
	body := `{"model":"x","max_tokens":10,"stream":true,"messages":[]}`
	t.Run("stream cut", func(t *testing.T) {
		f := newProxyFixture(t, 5, ModelPrice{Output: 40000}, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":1}}}\n\n")
			w.(http.Flusher).Flush()
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		})
		f.do(t, http.MethodPost, "/v1/messages", body)
		f.p.close()
		if c := f.p.Cost(); c < 0.4-1e-9 || !f.p.CostUncertain() {
			t.Fatalf("cost %v uncertain %t, want the $0.40 reservation, uncertain", c, f.p.CostUncertain())
		}
	})
	t.Run("transport error", func(t *testing.T) {
		f := newProxyFixture(t, 5, ModelPrice{Output: 40000}, sseOK("0.1"))
		f.p.upstream = "http://127.0.0.1:1"
		if code, _ := f.do(t, http.MethodPost, "/v1/messages", body); code != http.StatusBadGateway {
			t.Fatalf("upstream down: %d", code)
		}
		f.p.close()
		if c := f.p.Cost(); c < 0.4-1e-9 || !f.p.CostUncertain() {
			t.Fatalf("cost %v uncertain %t, want the $0.40 reservation, uncertain", c, f.p.CostUncertain())
		}
	})
	t.Run("client gone", func(t *testing.T) {
		hold := make(chan struct{})
		t.Cleanup(func() { close(hold) })
		f := newProxyFixture(t, 5, ModelPrice{Output: 40000}, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10}}}\n\n")
			w.(http.Flusher).Flush()
			select {
			case <-hold:
			case <-r.Context().Done():
			}
		})
		req, _ := http.NewRequest(http.MethodPost, f.p.URL()+"/v1/messages", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer tok")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 16)
		_, _ = resp.Body.Read(buf)
		_ = resp.Body.Close() // the client hangs up mid-stream
		f.p.close()
		if c := f.p.Cost(); c < 0.4-1e-9 || !f.p.CostUncertain() {
			t.Fatalf("cost %v uncertain %t, want the $0.40 reservation, uncertain", c, f.p.CostUncertain())
		}
	})
	t.Run("complete", func(t *testing.T) {
		f := newProxyFixture(t, 5, ModelPrice{Output: 40000}, sseOK("0.1"))
		f.do(t, http.MethodPost, "/v1/messages", body)
		f.p.close()
		if c := f.p.Cost(); c != 0.1 || f.p.CostUncertain() {
			t.Fatalf("cost %v uncertain %t, want the reported $0.1, known", c, f.p.CostUncertain())
		}
	})
	t.Run("rejected", func(t *testing.T) {
		f := newProxyFixture(t, 5, ModelPrice{Output: 40000}, func(w http.ResponseWriter, r *http.Request) {
			anthropicError(w, http.StatusTooManyRequests, "rate_limit_error", "slow down")
		})
		if code, _ := f.do(t, http.MethodPost, "/v1/messages", body); code != http.StatusTooManyRequests {
			t.Fatalf("code %d", code)
		}
		f.p.close()
		if c := f.p.Cost(); c != 0 || f.p.CostUncertain() {
			t.Fatalf("cost %v uncertain %t: a rejected request is not billed", c, f.p.CostUncertain())
		}
	})
}

// The proxy keeps a response's generation id from its X-Generation-Id header (else request-id when it is
// a gen- id), streamed or not; the body's id is a message id and never a generation id. Provider comes
// from the body (a JSON body's top level, a stream's message_start).
func TestOpenRouterProxyGenerationIDFromHeader(t *testing.T) {
	const body = `{"model":"x","max_tokens":10,"messages":[]}`
	jsonBody := `{"id":"msg_fdw7PJ7PWY6MaqVYHTkK","type":"message","role":"assistant","model":"z-ai/glm-5.3","provider":"Baidu",` +
		`"content":[],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5,"cost":0.01}}`
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_s1\",\"provider\":\"Novita\",\"usage\":{\"input_tokens\":10,\"output_tokens\":1}}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":5,\"cost\":0.01}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	for _, c := range []struct {
		name, genHeader, requestID string
		stream                     bool
		wantGen, wantProvider      string
	}{
		{"json, X-Generation-Id", "gen-1793920043-Vi07TN7aIT7NesZYYZHD", "gen-1793920043-Vi07TN7aIT7NesZYYZHD", false, "gen-1793920043-Vi07TN7aIT7NesZYYZHD", "Baidu"},
		{"stream, X-Generation-Id", "gen-2", "", true, "gen-2", "Novita"},
		{"json, request-id fallback", "", "gen-3", false, "gen-3", "Baidu"},
		{"stream, request-id fallback", "", "gen-4", true, "gen-4", "Novita"},
		{"request-id not a generation", "", "req_5", false, "", "Baidu"},
		{"no header", "", "", true, "", "Novita"},
		{"header not a token", "gen 6", "", false, "", "Baidu"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newProxyFixture(t, 5, ModelPrice{Input: 1, Output: 1}, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if c.genHeader != "" {
					w.Header().Set("X-Generation-Id", c.genHeader)
				}
				if c.requestID != "" {
					w.Header().Set("Request-Id", c.requestID)
				}
				if c.stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, sse)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, jsonBody)
			})
			if code, b := f.do(t, http.MethodPost, "/v1/messages", body); code != http.StatusOK {
				t.Fatalf("status %d: %s", code, b)
			}
			f.p.inflight.Wait() // the request is settled
			ids := f.p.generationIDs()
			if c.wantGen == "" && len(ids) != 0 || c.wantGen != "" && (len(ids) != 1 || ids[0] != c.wantGen) {
				t.Errorf("generation ids %v, want %q", ids, c.wantGen)
			}
			var l ORCostLine
			f.p.fill(&l)
			if l := l; len(l.Providers) != 1 || l.Providers[0] != c.wantProvider {
				t.Errorf("providers %v, want %q", l.Providers, c.wantProvider)
			}
		})
	}
	// A non-200 response's header id is not kept.
	f := newProxyFixture(t, 5, ModelPrice{Input: 1, Output: 1}, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("X-Generation-Id", "gen-err")
		http.Error(w, `{"error":{"message":"upstream"}}`, http.StatusBadGateway)
	})
	f.do(t, http.MethodPost, "/v1/messages", body)
	f.p.inflight.Wait()
	if ids := f.p.generationIDs(); len(ids) != 0 {
		t.Errorf("a 502's id kept: %v", ids)
	}
}
