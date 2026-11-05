package jobs

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// The OpenRouter proxy of a job (see openrouter.go): an HTTP server on 127.0.0.1, one per job, run by
// the wrapper and closed when claude has exited. claude reaches it with ANTHROPIC_BASE_URL and a per-job
// token as ANTHROPIC_AUTH_TOKEN; the proxy checks the token, sets the request's model to the job's slug
// and its `provider` object to the job's routing, and forwards it to OpenRouter with the real key in
// Authorization. The key is only in the proxy's memory: never in claude's environment, a file the job
// can read, a log line or an error. Responses are passed through unchanged, read on the way for their
// usage (and OpenRouter's usage.cost), which is the job's cost; once the job's budget is spent every
// further request is refused, and the wrapper fails the job with that reason.

// maxProxyBody caps a request body the proxy reads (claude sends the whole conversation each turn).
const maxProxyBody = 64 << 20

// maxProxyJSONResponse caps the non-streaming response body kept for its usage.
const maxProxyJSONResponse = 8 << 20

// orProxy is one job's OpenRouter proxy.
type orProxy struct {
	upstream  string // the API root, e.g. https://openrouter.ai/api
	key       string // the OpenRouter key: memory only
	token     string // what claude presents
	slug      string
	provider  json.RawMessage
	sessionID string
	price     ModelPrice // per million tokens, for a response without usage.cost
	budget    float64
	client    *http.Client
	log       io.Writer

	mu sync.Mutex
	// cost is what was spent: the settled cost of each finished request, and the reservation of each
	// that ended without its final usage (uncertain counts those).
	cost      float64
	reserved  float64 // the worst case of the requests in flight (reserve)
	pending   int     // how many are in flight: none = nothing reserved (no float residue)
	uncertain int
	usage     TokenUsage
	requests  int
	rewrites  int
	refused   int
	closed    bool           // close charged what was still in flight: later settles change nothing
	inflight  sync.WaitGroup // the /v1/messages requests between reserve and settle

	ln        net.Listener
	srv       *http.Server
	closeOnce sync.Once
}

// reserveBytesPerToken is the request bytes counted as one input token in a reservation: JSON
// conversations run at three or more bytes a token, so two over-reserves.
const reserveBytesPerToken = 2

// reserveDefaultMaxTokens is the output reserved for a request without a usable max_tokens.
const reserveDefaultMaxTokens = 64000

// closeSettleWait bounds how long close waits for the requests in flight to settle.
const closeSettleWait = 15 * time.Second

// newProxyToken is a random per-job token for claude's ANTHROPIC_AUTH_TOKEN.
func newProxyToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "agent-jobs-proxy-" + hex.EncodeToString(b), nil
}

// startORProxy listens on 127.0.0.1 (a port the kernel picks) and serves until close.
func startORProxy(p *orProxy) error {
	if p.client == nil {
		p.client = &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment,
			TLSHandshakeTimeout: 30 * time.Second, ResponseHeaderTimeout: 10 * time.Minute, ForceAttemptHTTP2: true}}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("openrouter proxy: listen: %w", err)
	}
	p.ln = ln
	logTo := io.Discard
	if p.log != nil {
		logTo = p.log
	}
	// The server's own log (accept errors, a handler panic) goes to stderr.log; it never holds a header.
	p.srv = &http.Server{Handler: p, ReadHeaderTimeout: 30 * time.Second,
		ErrorLog: log.New(logTo, "run-wrapper: openrouter proxy: ", 0)}
	go func() { _ = p.srv.Serve(ln) }()
	return nil
}

// URL is the proxy's base URL for ANTHROPIC_BASE_URL.
func (p *orProxy) URL() string { return "http://" + p.ln.Addr().String() }

// close stops the proxy (open streams are cut: claude has exited) and waits for the requests in flight
// to settle; one still unsettled after closeSettleWait is charged at its reservation, uncertain.
func (p *orProxy) close() {
	p.closeOnce.Do(func() {
		if p.srv != nil {
			_ = p.srv.Close()
		}
		done := make(chan struct{})
		go func() { p.inflight.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(closeSettleWait):
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.pending > 0 {
			p.cost += p.reserved
			p.uncertain += p.pending
			p.reserved, p.pending = 0, 0
		}
		p.closed = true
	})
}

// Cost is what the job's requests cost so far: what was spent, plus the reservations of the requests
// still in flight (the most they can still cost).
func (p *orProxy) Cost() float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cost + p.reserved
}

// CostUncertain reports whether Cost is not a settled figure: a request ended without its final usage
// or price (it is charged at its reservation), or one is still in flight.
func (p *orProxy) CostUncertain() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.uncertain > 0 || p.pending > 0
}

// worstCase is the most one /v1/messages request can cost at the plan's price: its input (the body's
// bytes / reserveBytesPerToken at the higher of the input and cache-write price) plus max_tokens of output.
func (p *orProxy) worstCase(bodyBytes int, maxTokens int64) float64 {
	in := float64(bodyBytes) / reserveBytesPerToken * math.Max(p.price.Input, p.price.CacheWrite)
	return (in + float64(maxTokens)*p.price.Output) / 1e6
}

// reserve admits one /v1/messages request when what was spent plus what is reserved is still below the
// budget, and reserves hold for it; a refusal counts toward BudgetFailure. The check and the reservation
// are one step under the lock, so requests in flight at once cannot all pass the same remaining budget:
// the job's spend stays within its budget plus one request's worst case.
func (p *orProxy) reserve(hold float64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || (p.budget > 0 && p.cost+p.reserved >= p.budget) {
		p.refused++
		return false
	}
	p.reserved += hold
	p.pending++
	p.requests++
	p.inflight.Add(1)
	return true
}

// settleOutcome is how a reserved request ended.
type settleOutcome int

const (
	settleIncomplete settleOutcome = iota // no final usage: charged at its reservation, uncertain
	settleDone                            // a 200 with its final usage: charged its cost
	settleRejected                        // OpenRouter answered with an error, read whole: not billed
)

// settle releases a request's reservation and charges it by its outcome. A finished response without
// OpenRouter's usage.cost is priced by its tokens at the plan's price; when neither prices it (no cost,
// no tokens or no price) it is charged like an incomplete one.
func (p *orProxy) settle(hold float64, u responseUsage, how settleOutcome) {
	defer p.inflight.Done()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return // close charged the reservation
	}
	p.pending--
	p.reserved = math.Max(p.reserved-hold, 0)
	if p.pending == 0 {
		p.reserved = 0
	}
	if sum, ok := p.usage.Add(u.Tokens); ok {
		p.usage = sum
	}
	c := p.price.Cost(u.Tokens)
	if u.HasCost {
		c = u.Cost
	}
	if math.IsInf(c, 0) || math.IsNaN(c) || c < 0 {
		c, how = 0, settleIncomplete
	}
	priced := u.HasCost || (u.Tokens != (TokenUsage{}) && p.price != (ModelPrice{}))
	switch {
	case how == settleRejected:
	case how == settleDone && priced:
		p.cost += c
	default:
		p.cost += math.Max(c, hold)
		p.uncertain++
	}
}

// BudgetFailure is the job's failure reason once a request was refused for the budget ("" before).
func (p *orProxy) BudgetFailure() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.refused == 0 {
		return ""
	}
	return fmt.Sprintf("openrouter budget reached: the job's requests cost $%.4f (with what is reserved for those in "+
		"flight) of its max_budget_usd $%.2f; %d further request(s) refused", p.cost+p.reserved, p.budget, p.refused)
}

// summary is the proxy's line for stderr.log.
func (p *orProxy) summary() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return fmt.Sprintf("openrouter proxy: %d request(s), cost $%.4f, tokens in %d / out %d / cache read %d / cache write %d, "+
		"%d model rewrite(s), %d refused, %d charged at their reservation (no final usage)", p.requests, p.cost+p.reserved,
		p.usage.Input, p.usage.Output, p.usage.CacheRead, p.usage.CacheWrite, p.rewrites, p.refused, p.uncertain)
}

// anthropicError writes an error in the Anthropic API's shape.
func anthropicError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	b, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]string{"type": typ, "message": msg}})
	_, _ = w.Write(b)
}

// authorized reports whether the request carries the job's token (Bearer or x-api-key).
func (p *orProxy) authorized(r *http.Request) bool {
	got := ""
	if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
		got = strings.TrimPrefix(a, "Bearer ")
	} else {
		got = r.Header.Get("X-Api-Key")
	}
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(p.token)) == 1
}

// hopHeaders are not forwarded (hop-by-hop, or the credentials the proxy replaces).
var hopHeaders = []string{"Authorization", "X-Api-Key", "Connection", "Keep-Alive", "Proxy-Authorization",
	"Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade", "Host", "Content-Length", "Accept-Encoding"}

func (p *orProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !p.authorized(r) {
		anthropicError(w, http.StatusUnauthorized, "authentication_error", "agent-jobs openrouter proxy: wrong or missing job token")
		return
	}
	// An exact allow-list: anything else would carry the real key past the model rewrite, the routing,
	// the budget and the accounting (OpenRouter's own /v1/chat/completions, its key or credit endpoints).
	if !proxyAllowed(r.Method, r.URL.Path) {
		anthropicError(w, http.StatusForbidden, "permission_error",
			"agent-jobs openrouter proxy: only POST /v1/messages and POST /v1/messages/count_tokens are forwarded")
		return
	}
	messages := r.URL.Path == "/v1/messages"
	b, err := io.ReadAll(io.LimitReader(r.Body, maxProxyBody+1))
	if err != nil || len(b) > maxProxyBody {
		anthropicError(w, http.StatusBadRequest, "invalid_request_error", "agent-jobs openrouter proxy: request body unreadable or too large")
		return
	}
	out, maxTokens, err := p.rewrite(b, messages)
	if err != nil {
		anthropicError(w, http.StatusBadRequest, "invalid_request_error", "agent-jobs openrouter proxy: "+err.Error())
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, p.upstream+r.URL.Path, bytes.NewReader(out))
	if err != nil {
		anthropicError(w, http.StatusBadGateway, "api_error", "agent-jobs openrouter proxy: "+err.Error())
		return
	}
	if r.URL.RawQuery == "beta=true" {
		req.URL.RawQuery = r.URL.RawQuery // claude's only query; any other is dropped
	}
	for k, vs := range r.Header {
		if !hopHeaderKey(k) {
			req.Header[k] = vs
		}
	}
	req.Header.Set("Authorization", "Bearer "+p.key)
	// A /v1/messages request reserves its worst case before it is sent (count_tokens is not billed).
	how, u := settleIncomplete, &usageReader{}
	if messages {
		hold := p.worstCase(len(b), maxTokens)
		if !p.reserve(hold) {
			anthropicError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf(
				"agent-jobs: this job's openrouter budget ($%.2f) is spent or reserved by the requests in flight; "+
					"no further requests are sent", p.budget))
			return
		}
		// Whatever ends the request (an error, the client gone, a panic), it is settled exactly once.
		defer func() { p.settle(hold, u.result(), how) }()
	}
	resp, err := p.client.Do(req)
	if err != nil {
		// A transport error names the URL, never a header. The request may have reached OpenRouter.
		anthropicError(w, http.StatusBadGateway, "api_error", "agent-jobs openrouter proxy: upstream: "+scrubKey(err.Error(), p.key))
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		if !hopHeaderKey(k) {
			w.Header()[k] = vs
		}
	}
	w.WriteHeader(resp.StatusCode)
	u.stream = strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	var rerr, werr error
	for {
		var n int
		n, rerr = resp.Body.Read(buf)
		if n > 0 {
			u.write(buf[:n])
			if _, werr = w.Write(buf[:n]); werr != nil {
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			break
		}
	}
	whole := werr == nil && errors.Is(rerr, io.EOF)
	switch {
	case !whole:
	case resp.StatusCode == http.StatusOK && u.final():
		how = settleDone
	case resp.StatusCode != http.StatusOK && !u.stream:
		how = settleRejected
	}
}

// proxyAllowed reports whether a request is one the proxy forwards: exactly POST /v1/messages and POST
// /v1/messages/count_tokens (a query string, e.g. claude's ?beta=true, is kept).
func proxyAllowed(method, path string) bool {
	return method == http.MethodPost && (path == "/v1/messages" || path == "/v1/messages/count_tokens")
}

func hopHeaderKey(k string) bool {
	for _, h := range hopHeaders {
		if strings.EqualFold(k, h) {
			return true
		}
	}
	return false
}

// scrubKey removes the key from a message (defence in depth: no error is known to carry it).
func scrubKey(msg, key string) string {
	if key == "" {
		return msg
	}
	return strings.ReplaceAll(msg, key, "[redacted]")
}

// rewrite sets the body's model to the job's slug and, for /v1/messages, its provider object and (when
// absent) its session_id (OpenRouter's sticky routing key: the provider keeps the prompt cache warm).
func (p *orProxy) rewrite(b []byte, messages bool) ([]byte, int64, error) {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(b, &body); err != nil || body == nil {
		return nil, 0, errors.New("request body is not a JSON object")
	}
	var maxTokens int64
	if json.Unmarshal(body["max_tokens"], &maxTokens) != nil || maxTokens <= 0 {
		maxTokens = reserveDefaultMaxTokens
	}
	var model string
	_ = json.Unmarshal(body["model"], &model)
	if model != p.slug {
		p.mu.Lock()
		p.rewrites++
		p.mu.Unlock()
		body["model"], _ = json.Marshal(p.slug)
	}
	if messages {
		body["provider"] = p.provider
		if _, ok := body["session_id"]; !ok && p.sessionID != "" {
			body["session_id"], _ = json.Marshal(p.sessionID)
		}
	}
	out, err := json.Marshal(body)
	return out, maxTokens, err
}

// responseUsage is the usage of one response: token counts (the largest seen in its events) and
// OpenRouter's usage.cost when it reported one.
type responseUsage struct {
	Tokens  TokenUsage
	Cost    float64
	HasCost bool
}

// usageReader scans a response body for its usage: SSE `data:` lines (message_start, message_delta)
// or one JSON body.
type usageReader struct {
	stream bool
	line   []byte
	body   []byte
	skip   bool
	res    responseUsage
	// stopped: the stream's message_stop event came (its final usage is in the message_delta before it).
	stopped bool
}

func (u *usageReader) write(b []byte) {
	if !u.stream {
		if len(u.body)+len(b) <= maxProxyJSONResponse {
			u.body = append(u.body, b...)
		}
		return
	}
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		chunk := b
		if i >= 0 {
			chunk = b[:i]
		}
		if !u.skip {
			if len(u.line)+len(chunk) > maxProxyJSONResponse {
				u.skip, u.line = true, nil
			} else {
				u.line = append(u.line, chunk...)
			}
		}
		if i < 0 {
			return
		}
		if !u.skip {
			u.event(u.line)
		}
		u.line, u.skip = u.line[:0], false
		b = b[i+1:]
	}
}

type usageJSON struct {
	InputTokens              *int64   `json:"input_tokens"`
	OutputTokens             *int64   `json:"output_tokens"`
	CacheReadInputTokens     *int64   `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int64   `json:"cache_creation_input_tokens"`
	Cost                     *float64 `json:"cost"`
}

func (u *usageReader) apply(x *usageJSON) {
	if x == nil {
		return
	}
	max := func(dst *int64, v *int64) {
		if v != nil && *v > *dst {
			*dst = *v
		}
	}
	max(&u.res.Tokens.Input, x.InputTokens)
	max(&u.res.Tokens.Output, x.OutputTokens)
	max(&u.res.Tokens.CacheRead, x.CacheReadInputTokens)
	max(&u.res.Tokens.CacheWrite, x.CacheCreationInputTokens)
	if x.Cost != nil && *x.Cost >= 0 && (!u.res.HasCost || *x.Cost > u.res.Cost) {
		u.res.Cost, u.res.HasCost = *x.Cost, true
	}
}

func (u *usageReader) event(line []byte) {
	data, ok := bytes.CutPrefix(bytes.TrimRight(line, "\r"), []byte("data:"))
	if !ok {
		return
	}
	var ev struct {
		Type    string     `json:"type"`
		Usage   *usageJSON `json:"usage"`
		Message struct {
			Usage *usageJSON `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(bytes.TrimSpace(data), &ev) != nil {
		return
	}
	if ev.Type == "message_stop" {
		u.stopped = true
	}
	u.apply(ev.Message.Usage)
	u.apply(ev.Usage)
}

// final reports whether the response carried its final usage: a stream's message_stop, or a JSON body
// (read whole, within maxProxyJSONResponse) with a usage object.
func (u *usageReader) final() bool {
	if u.stream {
		u.result() // a last event without a trailing newline
		return u.stopped
	}
	var body struct {
		Usage *usageJSON `json:"usage"`
	}
	return json.Unmarshal(u.body, &body) == nil && body.Usage != nil
}

func (u *usageReader) result() responseUsage {
	if !u.stream {
		var body struct {
			Usage *usageJSON `json:"usage"`
		}
		if json.Unmarshal(u.body, &body) == nil {
			u.apply(body.Usage)
		}
	} else if len(u.line) > 0 && !u.skip {
		u.event(u.line)
		u.line = u.line[:0]
	}
	return u.res
}

// ReadOpenRouterKey reads OPENROUTER_API_KEY from an env-style key file (KEY=value lines, # comments,
// optional "export " and quotes). The file must be a regular file not readable by others than its
// owner (mode 600 or stricter). Errors never carry the file's content.
func ReadOpenRouterKey(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("openrouter key_file %s: %w", path, errors.Unwrap(err))
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("openrouter key_file %s is not a regular file", path)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("openrouter key_file %s is readable by group or others (mode %o): chmod 600 it", path, fi.Mode().Perm())
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("openrouter key_file %s: %w", path, errors.Unwrap(err))
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, 64<<10))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) != "OPENROUTER_API_KEY" {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		if v == "" || strings.ContainsAny(v, " \t\r\n") {
			return "", fmt.Errorf("openrouter key_file %s: OPENROUTER_API_KEY is empty or malformed", path)
		}
		return v, nil
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("openrouter key_file %s: read failed", path)
	}
	return "", fmt.Errorf("openrouter key_file %s has no OPENROUTER_API_KEY=... line", path)
}

// openRouterDroppedEnv reports whether an inherited variable is removed from an OpenRouter job's claude:
// every ANTHROPIC_* variable (key, token, base URL, models, custom headers), the subscription's OAuth
// variables and the cloud-provider switches and credentials, and OPENROUTER_API_KEY itself. So a job can
// never authenticate against Anthropic or a cloud provider, nor see the key.
func openRouterDroppedEnv(k string) bool {
	switch {
	case strings.HasPrefix(k, "ANTHROPIC_"),
		strings.HasPrefix(k, "CLAUDE_CODE_OAUTH"),
		strings.HasPrefix(k, "CLAUDE_CODE_USE_"),
		strings.HasPrefix(k, "CLAUDE_CODE_SKIP_"),
		k == "CLAUDE_CODE_SUBAGENT_MODEL",
		k == "CLAUDE_CODE_API_KEY_HELPER_TTL_MS",
		k == "AWS_BEARER_TOKEN_BEDROCK",
		k == "OPENROUTER_API_KEY":
		return true
	}
	return false
}

// openRouterEnv returns env for an OpenRouter job's claude: the dropped variables removed, then claude
// pointed at the proxy (base URL, per-job token) with every model class set to the slug.
func openRouterEnv(env []string, baseURL, token, slug string) []string {
	out := make([]string, 0, len(env)+12)
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); !openRouterDroppedEnv(k) {
			out = append(out, kv)
		}
	}
	return append(out,
		"ANTHROPIC_BASE_URL="+baseURL,
		"ANTHROPIC_AUTH_TOKEN="+token,
		"ANTHROPIC_API_KEY=", // explicitly empty: no direct-Anthropic credential
		"ANTHROPIC_MODEL="+slug,
		"ANTHROPIC_DEFAULT_FABLE_MODEL="+slug,
		"ANTHROPIC_DEFAULT_OPUS_MODEL="+slug,
		"ANTHROPIC_DEFAULT_SONNET_MODEL="+slug,
		"ANTHROPIC_DEFAULT_HAIKU_MODEL="+slug,
		"ANTHROPIC_SMALL_FAST_MODEL="+slug,
		"CLAUDE_CODE_SUBAGENT_MODEL="+slug,
	)
}

// projectSettingsEnvConflict names a variable that the work dir's project settings (.claude/settings.json,
// .claude/settings.local.json, loaded with --setting-sources project) would set in claude's environment
// and that an OpenRouter job's routing or credentials depend on ("" for none): claude applies settings
// env over the process environment, so such a repository could point the job elsewhere.
func projectSettingsEnvConflict(work string) (string, error) {
	for _, name := range []string{"settings.json", "settings.local.json"} {
		path := work + "/.claude/" + name
		b, err := readSmall(path, 4<<20)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("read %s: %w", path, err)
		}
		var s struct {
			Env          map[string]json.RawMessage `json:"env"`
			APIKeyHelper json.RawMessage            `json:"apiKeyHelper"`
		}
		if json.Unmarshal([]byte(b), &s) != nil {
			continue // claude would not load it either
		}
		if len(s.APIKeyHelper) > 0 && string(s.APIKeyHelper) != "null" {
			return ".claude/" + name + " apiKeyHelper", nil
		}
		for k := range s.Env {
			if openRouterDroppedEnv(k) {
				return ".claude/" + name + " env " + k, nil
			}
		}
	}
	return "", nil
}

// openRouterJob is what the wrapper prepares for an OpenRouter job before claude starts.
type openRouterJob struct {
	proxy *orProxy
	slug  string
	note  string
}

// fetchEndpointsTimeout bounds the endpoints read at a job's start.
var fetchEndpointsTimeout = 20 * time.Second

// setupOpenRouter prepares an OpenRouter job's launch (R3, R8): the config still allows the job, the
// key file is readable (fail fast), the routing plan is made from the live endpoints list, and the
// proxy is started. jobID keys OpenRouter's sticky routing. A failure is the job's failure reason.
func setupOpenRouter(c Config, model, kind, repo, jobDir, jobID string, budget float64, logTo io.Writer) (*openRouterJob, error) {
	slug, ok := OpenRouterSlug(model)
	if !ok {
		return nil, fmt.Errorf("model %q is not an openrouter model", model)
	}
	if err := c.checkOpenRouterJob(slug, kind, "", repo); err != nil {
		return nil, err
	}
	o := c.Providers.OpenRouter
	if conflict, err := projectSettingsEnvConflict(jobDir + "/work"); err != nil {
		return nil, err
	} else if conflict != "" {
		return nil, fmt.Errorf("the repository's %s would override the openrouter routing or credentials: an openrouter job "+
			"does not run on it", conflict)
	}
	key, err := ReadOpenRouterKey(o.KeyFile)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), fetchEndpointsTimeout)
	defer cancel()
	eps, fetchErr := FetchEndpoints(ctx, &http.Client{Timeout: fetchEndpointsTimeout}, o.BaseURL, slug)
	plan, err := planRouting(c, slug, eps, fetchErr)
	if err != nil {
		return nil, err
	}
	provider, err := json.Marshal(plan.Provider)
	if err != nil {
		return nil, err
	}
	token, err := newProxyToken()
	if err != nil {
		return nil, fmt.Errorf("openrouter proxy token: %w", err)
	}
	p := &orProxy{upstream: strings.TrimRight(o.BaseURL, "/"), key: key, token: token, slug: slug, provider: provider,
		sessionID: "agent-jobs-" + jobID, price: plan.Price, budget: budget, log: logTo}
	if err := startORProxy(p); err != nil {
		return nil, err
	}
	return &openRouterJob{proxy: p, slug: slug, note: plan.Note + "; provider " + string(provider)}, nil
}

// env is openRouterEnv for this job's proxy.
func (o *openRouterJob) env(env []string) []string {
	return openRouterEnv(env, o.proxy.URL(), o.proxy.token, o.slug)
}

// apply puts the job's cost and budget failure into a result the wrapper records: the proxy's cost
// (what OpenRouter reported, else tokens at the plan's price) replaces claude's own figure, which prices
// the model by Anthropic's list.
func (o *openRouterJob) apply(res *StreamResult) {
	res.CostUSD = o.proxy.Cost()
	res.ProviderCostUncertain = o.proxy.CostUncertain()
	if f := o.proxy.BudgetFailure(); f != "" {
		res.ProviderFailure = f
	}
}
