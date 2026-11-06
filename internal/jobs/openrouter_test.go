package jobs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/guard"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// Issue #136: jobs on OpenRouter models through the claude runner.

const (
	orGLM  = "z-ai/glm-5.3"
	orKimi = "moonshotai/kimi-k3.1-pro"
)

// orFakeKey is a made-up key in OpenRouter's shape: the redaction test greps every artifact for it.
var orFakeKey = "sk-or-" + "v1-" + strings.Repeat("0123456789abcdef", 4)

func readFixture(t *testing.T, slug string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "openrouter", strings.ReplaceAll(slug, "/", "_")+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func fixtureEndpoints(t *testing.T, slug string) []Endpoint {
	t.Helper()
	eps, err := ParseEndpoints(readFixture(t, slug))
	if err != nil {
		t.Fatal(err)
	}
	return eps
}

func f64(v float64) *float64 { return &v }

// orConfig is a valid providers.openrouter for base and keyFile.
func orConfig(base, keyFile string) *OpenRouterConfig {
	return &OpenRouterConfig{BaseURL: base, KeyFile: keyFile, Models: []string{orGLM, orKimi}, DailyCapUSD: f64(20)}
}

func writeKeyFile(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "openrouter.env")
	if err := os.WriteFile(p, []byte("# OpenRouter\nexport OPENROUTER_API_KEY=\""+orFakeKey+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func loadConfigBody(t *testing.T, body string) (Config, error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return LoadConfig(dir)
}

// R1: providers.openrouter is validated; the key is never accepted inside config.json.
func TestOpenRouterConfigValidation(t *testing.T) {
	ok := `{"providers":{"openrouter":{"key_file":"/k/openrouter.env","models":["z-ai/glm-5.3"],"daily_cap_usd":5,
		"routing":{"sort":"price","min_quantization":"fp8","data_collection":"deny","zdr":false,"min_uptime_pct":95,
		"ignore":["deepinfra"],"order":["baidu/fp8"]},"model_routing":{"z-ai/glm-5.3":{"min_quantization":"fp4"}}}}}`
	c, err := loadConfigBody(t, ok)
	if err != nil {
		t.Fatal(err)
	}
	o := c.Providers.OpenRouter
	if o.BaseURL != DefaultOpenRouterBaseURL || o.DailyCap() != 5 || o.codexPolicy() != CodexPolicyAllowUnprotected {
		t.Fatalf("defaults: %+v", o)
	}
	if p := o.Policy(orGLM); p.MinQuantization != "fp4" || p.Sort != "price" || p.DataCollection != "deny" ||
		p.MinUptimePct != 95 || !slices.Equal(p.Ignore, []string{"deepinfra"}) || !slices.Equal(p.Order, []string{"baidu/fp8"}) {
		t.Fatalf("policy with override: %+v", p)
	}
	if c, _ := loadConfigBody(t, `{}`); c.Providers.OpenRouter != nil {
		t.Fatal("absent providers must stay nil")
	}
	bad := map[string]string{
		"key in config":    `{"providers":{"openrouter":{"api_key":"sk-or-v1-x","key_file":"/k","models":["a/b"],"daily_cap_usd":1}}}`,
		"no key_file":      `{"providers":{"openrouter":{"models":["a/b"],"daily_cap_usd":1}}}`,
		"relative key":     `{"providers":{"openrouter":{"key_file":"k.env","models":["a/b"],"daily_cap_usd":1}}}`,
		"no models":        `{"providers":{"openrouter":{"key_file":"/k","daily_cap_usd":1}}}`,
		"bad slug":         `{"providers":{"openrouter":{"key_file":"/k","models":["glm"],"daily_cap_usd":1}}}`,
		"no cap":           `{"providers":{"openrouter":{"key_file":"/k","models":["a/b"]}}}`,
		"zero cap":         `{"providers":{"openrouter":{"key_file":"/k","models":["a/b"],"daily_cap_usd":0}}}`,
		"http base":        `{"providers":{"openrouter":{"base_url":"http://openrouter.ai/api","key_file":"/k","models":["a/b"],"daily_cap_usd":1}}}`,
		"bad sort":         `{"providers":{"openrouter":{"key_file":"/k","models":["a/b"],"daily_cap_usd":1,"routing":{"sort":"cheap"}}}}`,
		"bad quant":        `{"providers":{"openrouter":{"key_file":"/k","models":["a/b"],"daily_cap_usd":1,"routing":{"min_quantization":"q4"}}}}`,
		"bad data":         `{"providers":{"openrouter":{"key_file":"/k","models":["a/b"],"daily_cap_usd":1,"routing":{"data_collection":"maybe"}}}}`,
		"bad uptime":       `{"providers":{"openrouter":{"key_file":"/k","models":["a/b"],"daily_cap_usd":1,"routing":{"min_uptime_pct":101}}}}`,
		"unknown override": `{"providers":{"openrouter":{"key_file":"/k","models":["a/b"],"daily_cap_usd":1,"model_routing":{"c/d":{}}}}}`,
		"bad codex_policy": `{"providers":{"openrouter":{"key_file":"/k","models":["a/b"],"daily_cap_usd":1,"codex_policy":"allow"}}}`,
	}
	for name, body := range bad {
		if _, err := loadConfigBody(t, body); err == nil {
			t.Errorf("%s: accepted", name)
		} else if name == "key in config" && !strings.Contains(err.Error(), "key_file") {
			t.Errorf("key in config: %v", err)
		}
	}
	for _, p := range []string{CodexPolicyAllowUnprotected, CodexPolicyRefuse} {
		body := `{"providers":{"openrouter":{"key_file":"/k","models":["a/b"],"daily_cap_usd":1,"codex_policy":"` + p + `"}}}`
		if c, err := loadConfigBody(t, body); err != nil || c.Providers.OpenRouter.codexPolicy() != p {
			t.Errorf("codex_policy %s: %v", p, err)
		}
	}
	// A loopback http base (a test server, a local gateway) is fine.
	if _, err := loadConfigBody(t, `{"providers":{"openrouter":{"base_url":"http://127.0.0.1:9/api","key_file":"/k","models":["a/b"],"daily_cap_usd":1}}}`); err != nil {
		t.Fatal(err)
	}
}

// R2, R5, R7: job_start takes openrouter:<slug> from the allow-list only, never for review jobs, codex,
// or denied repos; such a job is billing api, provider openrouter; claude and codex jobs are unchanged.
func TestOpenRouterJobStartRules(t *testing.T) {
	m := newManager(t)
	m.Cfg.Providers.OpenRouter = orConfig(DefaultOpenRouterBaseURL, "/nonexistent/openrouter.env")
	j := enqueue(t, m, "x", func(r *StartRequest) { r.Model = "openrouter:" + orGLM })
	if j.Provider != store.ProviderOpenRouter || j.Billing != store.BillingAPI || j.Runner != store.RunnerClaude ||
		j.Model != "openrouter:"+orGLM || JobProvider(j) != "openrouter" {
		t.Fatalf("openrouter job: %+v", j)
	}
	if got := get(t, m, j.ID); got.Provider != store.ProviderOpenRouter || got.Billing != store.BillingAPI {
		t.Fatalf("stored: provider %q billing %q", got.Provider, got.Billing)
	}
	if args := strings.Join(ClaudeArgs(j, m.JobDir(j.ID)), " "); !strings.Contains(args, "--model "+orGLM+" ") {
		t.Fatalf("claude gets the bare slug: %s", args)
	}
	c := enqueue(t, m, "x", nil)
	if c.Provider != "" || c.Billing != store.BillingSubscription || JobProvider(c) != "anthropic" {
		t.Fatalf("claude job changed: %+v", c)
	}
	x := enqueue(t, m, "x", func(r *StartRequest) { r.Kind = "review"; r.Model = "gpt-5.5" })
	if x.Provider != "" || x.Billing != store.BillingSubscription || JobProvider(x) != "openai" {
		t.Fatalf("codex job changed: %+v", x)
	}
	repo := newRepo(t)
	m.Cfg.Providers.OpenRouter.DenyRepos = []string{filepath.Base(repo)}
	refused := map[string]StartRequest{
		"not allowed": {Model: "openrouter:openai/gpt-oss-120b"},
		"review":      {Model: "openrouter:" + orGLM, Kind: "review"},
		"codex":       {Model: "openrouter:" + orGLM, Runner: store.RunnerCodex},
		"denied repo": {Model: "openrouter:" + orGLM, RepoPath: repo},
		"bad slug":    {Model: "openrouter:glm"},
	}
	want := map[string]string{"not allowed": "not allowed: providers.openrouter.models lists", "review": "do not run review jobs",
		"codex": "codex", "denied repo": "denied to openrouter jobs", "bad slug": "not an OpenRouter model slug"}
	for name, r := range refused {
		r.Title, r.Brief = "t", "b"
		if r.Kind == "" {
			r.Kind = "general"
		}
		if _, err := m.Enqueue(r); err == nil || !strings.Contains(err.Error(), want[name]) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Another name matches only its own folder; an absolute glob matches the path.
	m.Cfg.Providers.OpenRouter.DenyRepos = []string{"other-repo"}
	enqueue(t, m, "x", func(r *StartRequest) { r.Model = "openrouter:" + orGLM; r.RepoPath = repo })
	m.Cfg.Providers.OpenRouter.DenyRepos = []string{filepath.Dir(repo) + "/*"}
	if _, err := m.Enqueue(StartRequest{Title: "t", Kind: "fix", Brief: "b", Model: "openrouter:" + orGLM, RepoPath: repo}); err == nil {
		t.Fatal("an absolute glob must deny the repo")
	}
	m.Cfg.Providers.OpenRouter = nil
	if _, err := m.Enqueue(StartRequest{Title: "t", Kind: "general", Brief: "b", Model: "openrouter:" + orGLM}); err == nil ||
		!strings.Contains(err.Error(), "providers.openrouter") {
		t.Fatalf("not configured: %v", err)
	}
}

// R4: the daily cap refuses a job whose budget would pass it; queued jobs count at their budget,
// finished ones at their cost, older ones not at all; other providers do not count.
func TestOpenRouterDailyCap(t *testing.T) {
	m := newManager(t)
	m.Cfg.Providers.OpenRouter = orConfig(DefaultOpenRouterBaseURL, "/k")
	m.Cfg.Providers.OpenRouter.DailyCapUSD = f64(10)
	now := time.Now()
	for _, j := range []*store.Job{
		{ID: "old", CreatedAt: now.Add(-25 * time.Hour), Status: store.StatusSucceeded, CostUSD: 9, MaxBudgetUSD: 10},
		{ID: "done", CreatedAt: now.Add(-time.Hour), Status: store.StatusSucceeded, CostUSD: 1.5, MaxBudgetUSD: 10},
	} {
		j.Agent, j.Title, j.Kind, j.Brief, j.Model, j.TimeoutMinutes = m.Agent, j.ID, "general", "b", "openrouter:"+orGLM, 1
		j.Provider, j.Billing = store.ProviderOpenRouter, store.BillingAPI
		if err := m.Store.Insert(j); err != nil {
			t.Fatal(err)
		}
	}
	enqueue(t, m, "claude jobs do not count", func(r *StartRequest) { r.MaxBudgetUSD = 50 })
	enqueue(t, m, "a", func(r *StartRequest) { r.Model = "openrouter:" + orGLM; r.MaxBudgetUSD = 4 })
	if got, _ := m.OpenRouterSpentToday(); got != "$5.50/$10.00" {
		t.Fatalf("spent today: %q", got)
	}
	enqueue(t, m, "b", func(r *StartRequest) { r.Model = "openrouter:" + orGLM; r.MaxBudgetUSD = 4.5 })
	_, err := m.Enqueue(StartRequest{Title: "c", Kind: "general", Brief: "c", Model: "openrouter:" + orGLM, MaxBudgetUSD: 0.5})
	if err == nil || !strings.Contains(err.Error(), "openrouter daily cap reached") || !strings.Contains(err.Error(), "$10.00") ||
		!strings.Contains(err.Error(), "daily_cap_usd") {
		t.Fatalf("over the cap: %v", err)
	}
}

// R8: the policy ranks the live endpoints (fixture of 2026-11-04 21:54 UTC): by default (price, fp8 or
// better, uptime 95) GLM 5.3 goes to Baidu fp8, never to the cheaper fp4 endpoints; Kimi K3.1 Pro has no
// fp8 endpoint at all, so it needs a per-model override.
func TestOpenRouterRankEndpointsFixture(t *testing.T) {
	var o *OpenRouterConfig // defaults
	glm := RankEndpoints(fixtureEndpoints(t, orGLM), o.Policy(orGLM))
	if !glm[0].Picked || glm[0].Tag != "baidu/fp8" || glm[0].Input != 0.28 || glm[0].Output != 0.88 {
		t.Fatalf("glm pick: %+v", glm[0])
	}
	for _, r := range glm {
		if r.Eligible && r.Quantization != "fp8" {
			t.Errorf("eligible below fp8 or unknown: %s %s", r.Tag, r.Quantization)
		}
		if r.Tag == "wafer/us" && (r.Eligible || !strings.Contains(r.Why, "quantization fp4 below fp8")) {
			t.Errorf("wafer fp4: %+v", r)
		}
		if r.Tag == "morph/fp8" && (r.Eligible || !strings.Contains(r.Why, "uptime")) {
			t.Errorf("morph at 83%% uptime: %+v", r)
		}
	}
	kimi := RankEndpoints(fixtureEndpoints(t, orKimi), o.Policy(orKimi))
	if kimi[0].Eligible || kimi[0].Picked {
		t.Fatalf("kimi must have no fp8 endpoint: %+v", kimi[0])
	}
	o = &OpenRouterConfig{ModelRouting: map[string]RoutingPolicy{orKimi: {MinQuantization: "fp4"}}}
	kimi = RankEndpoints(fixtureEndpoints(t, orKimi), o.Policy(orKimi))
	if !kimi[0].Picked || kimi[0].Tag != "inference-net/mxfp4" {
		t.Fatalf("kimi fp4 pick: %+v", kimi[0])
	}
	// order puts a provider first; ignore drops it.
	o = &OpenRouterConfig{Routing: RoutingPolicy{Order: []string{"novita"}, Ignore: []string{"baidu"}}}
	glm = RankEndpoints(fixtureEndpoints(t, orGLM), o.Policy(orGLM))
	if glm[0].Tag != "novita/fp8" {
		t.Fatalf("order: %+v", glm[0])
	}
	for _, r := range glm {
		if r.Provider == "Baidu" && (r.Eligible || r.Why != "ignored") {
			t.Fatalf("ignore: %+v", r)
		}
	}
}

// R8: the routing plan's provider object carries the policy and the ranked order; the estimate price is
// the highest eligible one; no eligible endpoint, or no list and no price, fails.
func TestOpenRouterPlanRouting(t *testing.T) {
	c := Config{Providers: ProvidersConfig{OpenRouter: orConfig(DefaultOpenRouterBaseURL, "/k")}}
	plan, err := planRouting(c, orGLM, fixtureEndpoints(t, orGLM), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(plan.Provider)
	var obj struct {
		Sort           string   `json:"sort"`
		Quantizations  []string `json:"quantizations"`
		DataCollection string   `json:"data_collection"`
		Order          []string `json:"order"`
		Ignore         []string `json:"ignore"`
		Fallbacks      bool     `json:"allow_fallbacks"`
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		t.Fatal(err)
	}
	if obj.Sort != "price" || obj.DataCollection != "deny" || !obj.Fallbacks || slices.Contains(obj.Quantizations, "fp4") ||
		!slices.Contains(obj.Quantizations, "fp8") || slices.Contains(obj.Quantizations, "unknown") ||
		len(obj.Order) == 0 || obj.Order[0] != "baidu/fp8" || !slices.Contains(obj.Ignore, "morph/fp8") {
		t.Fatalf("provider object: %s", b)
	}
	if plan.Price.Input < 0.28 || plan.Price.Output < 0.88 {
		t.Fatalf("estimate price must be the highest eligible: %+v", plan.Price)
	}
	// sort throughput: OpenRouter's live sort decides, no price-ranked order is sent.
	c.Providers.OpenRouter.Routing.Sort = "throughput"
	if tp, err := planRouting(c, orGLM, fixtureEndpoints(t, orGLM), nil); err != nil || tp.Provider["order"] != nil ||
		tp.Provider["sort"] != "throughput" {
		t.Fatalf("throughput plan: %v %v", tp.Provider, err)
	}
	c.Providers.OpenRouter.Routing.Sort = ""
	if _, err := planRouting(c, orKimi, fixtureEndpoints(t, orKimi), nil); err == nil || !strings.Contains(err.Error(), "no endpoint of") {
		t.Fatalf("kimi under fp8: %v", err)
	}
	if _, err := planRouting(c, orGLM, nil, io.ErrUnexpectedEOF); err == nil || !strings.Contains(err.Error(), "model_prices") {
		t.Fatalf("no list, no price: %v", err)
	}
	c.ModelPrices = map[string]ModelPrice{"openrouter:" + orGLM: {Input: 1.4, Output: 4.4}}
	if p, err := planRouting(c, orGLM, nil, io.ErrUnexpectedEOF); err != nil || p.Price.Input != 1.4 {
		t.Fatalf("no list, model_prices: %+v %v", p, err)
	}
}

// R9: the endpoints table sorts by the policy and marks the pick.
func TestOpenRouterFormatEndpoints(t *testing.T) {
	var o *OpenRouterConfig
	var buf bytes.Buffer
	FormatEndpoints(&buf, orGLM, o.Policy(orGLM), RankEndpoints(fixtureEndpoints(t, orGLM), o.Policy(orGLM)))
	out := buf.String()
	lines := strings.Split(out, "\n")
	if !strings.HasPrefix(lines[2], "* baidu/fp8") || !strings.Contains(lines[2], "PICKED") || !strings.Contains(lines[2], "$0.28") ||
		!strings.Contains(lines[2], "$0.88") || !strings.Contains(lines[2], "1048576") || !strings.Contains(lines[2], "100.0%") {
		t.Fatalf("pick line: %q", lines[2])
	}
	if !strings.Contains(lines[1], "IN/1M") || !strings.Contains(lines[1], "CACHE/1M") || !strings.Contains(out, "quantization fp4 below fp8") {
		t.Fatalf("table:\n%s", out)
	}
	buf.Reset()
	FormatEndpoints(&buf, orKimi, o.Policy(orKimi), RankEndpoints(fixtureEndpoints(t, orKimi), o.Policy(orKimi)))
	if !strings.Contains(buf.String(), "no endpoint satisfies the policy") || strings.Contains(buf.String(), "PICKED") {
		t.Fatalf("kimi table:\n%s", buf.String())
	}
}

// R3: the key file is read strictly; errors never carry its content.
func TestReadOpenRouterKey(t *testing.T) {
	dir := t.TempDir()
	p := writeKeyFile(t, dir)
	if k, err := ReadOpenRouterKey(p); err != nil || k != orFakeKey {
		t.Fatalf("key: %q %v", k, err)
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadOpenRouterKey(p); err == nil || !strings.Contains(err.Error(), "chmod 600") || strings.Contains(err.Error(), orFakeKey) {
		t.Fatalf("world-readable: %v", err)
	}
	q := filepath.Join(dir, "other.env")
	if err := os.WriteFile(q, []byte("OTHER="+orFakeKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadOpenRouterKey(q); err == nil || strings.Contains(err.Error(), orFakeKey) {
		t.Fatalf("no key line: %v", err)
	}
	if _, err := ReadOpenRouterKey(filepath.Join(dir, "missing.env")); err == nil {
		t.Fatal("missing file accepted")
	}
}

// R3: claude's environment loses every Anthropic, OAuth and cloud credential and points at the proxy.
func TestOpenRouterEnv(t *testing.T) {
	in := []string{"PATH=/bin", "ANTHROPIC_API_KEY=sk-ant-decoy", "ANTHROPIC_BASE_URL=https://api.anthropic.com",
		"CLAUDE_CODE_OAUTH_TOKEN=decoy", "CLAUDE_CODE_USE_BEDROCK=1", "AWS_BEARER_TOKEN_BEDROCK=x",
		"OPENROUTER_API_KEY=" + orFakeKey, "ANTHROPIC_CUSTOM_HEADERS=x: y", "HOME=/h"}
	out := openRouterEnv(in, "http://127.0.0.1:1", "tok", orGLM)
	got := map[string]string{}
	for _, kv := range out {
		k, v, _ := strings.Cut(kv, "=")
		if _, dup := got[k]; dup {
			t.Fatalf("duplicate %s", k)
		}
		got[k] = v
	}
	for _, k := range []string{"CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_USE_BEDROCK", "AWS_BEARER_TOKEN_BEDROCK", "OPENROUTER_API_KEY", "ANTHROPIC_CUSTOM_HEADERS"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s kept", k)
		}
	}
	want := map[string]string{"PATH": "/bin", "HOME": "/h", "ANTHROPIC_API_KEY": "", "ANTHROPIC_BASE_URL": "http://127.0.0.1:1",
		"ANTHROPIC_AUTH_TOKEN": "tok", "ANTHROPIC_MODEL": orGLM, "ANTHROPIC_DEFAULT_OPUS_MODEL": orGLM,
		"ANTHROPIC_DEFAULT_SONNET_MODEL": orGLM, "ANTHROPIC_DEFAULT_HAIKU_MODEL": orGLM, "CLAUDE_CODE_SUBAGENT_MODEL": orGLM}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	work := t.TempDir()
	if err := os.MkdirAll(filepath.Join(work, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, ".claude", "settings.json"), []byte(`{"env":{"ANTHROPIC_BASE_URL":"https://x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, err := projectSettingsEnvConflict(work); err != nil || !strings.Contains(c, "ANTHROPIC_BASE_URL") {
		t.Fatalf("project settings env: %q %v", c, err)
	}
}

// fakeOpenRouter is an OpenRouter stand-in: the public endpoints list and /v1/messages answering with
// an SSE stream whose usage reports cost; it records what it was sent.
type fakeOpenRouter struct {
	srv  *httptest.Server
	cost string
	mu   sync.Mutex
	auth []string
	body []map[string]json.RawMessage
	// Like OpenRouter (live, 2026-11-05): each /v1/messages response carries its generation id gen-<n> in
	// the X-Generation-Id and request-id headers, its message_start the message id msg_<n>.
	// /v1/generation answers 400 to an id that is not a generation id and 404 to a generation's first
	// genNotYet lookups (its record lags the response); then its record (billed, a total_cost) with
	// genStatus 0, else genStatus. genAuth records that endpoint's auth, genAsked the ids it was asked.
	gens      int
	billed    string
	genStatus int
	genNotYet int
	genSeen   map[string]int
	genAuth   []string
	genAsked  []string
	// genBlock, when set, holds every /v1/generation answer until it is closed (or the request ends);
	// genHit is closed at the first such request.
	genBlock chan struct{}
	genHit   chan struct{}
	hitOnce  sync.Once
}

// orGLMVersion is the versioned model name OpenRouter's generation record reports for orGLM.
const orGLMVersion = orGLM + "-20260816"

func newFakeOpenRouter(t *testing.T, cost string) *fakeOpenRouter {
	f := &fakeOpenRouter{cost: cost, billed: cost, genNotYet: 1, genSeen: map[string]int{}}
	if cost == "cut" {
		f.billed = "0.002" // OpenRouter bills what the cut stream produced
	}
	glm := readFixture(t, orGLM)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/generation":
			id := r.URL.Query().Get("id")
			f.mu.Lock()
			f.genAuth, f.genAsked = append(f.genAuth, r.Header.Get("Authorization")), append(f.genAsked, id)
			f.genSeen[id]++
			status, billed, n, early := f.genStatus, f.billed, f.gens, f.genSeen[id] <= f.genNotYet
			block, hit := f.genBlock, f.genHit
			f.mu.Unlock()
			if hit != nil {
				f.hitOnce.Do(func() { close(hit) })
			}
			if block != nil {
				select {
				case <-block:
				case <-r.Context().Done():
					return
				}
			}
			if !strings.HasPrefix(id, "gen-") {
				http.Error(w, `{"error":{"code":400,"message":"Invalid generation id"}}`, http.StatusBadRequest)
				return
			}
			var k int
			if _, err := fmt.Sscanf(id, "gen-%d", &k); err != nil || k < 1 || k > n || early {
				status = http.StatusNotFound
			}
			if status != 0 {
				http.Error(w, `{"error":{"code":404,"message":"Generation not found"}}`, status)
				return
			}
			_, _ = io.WriteString(w, `{"data":{"id":"`+id+`","total_cost":`+billed+`,"model":"`+orGLMVersion+
				`","provider_name":"Baidu","native_tokens_prompt":990,"native_tokens_completion":48,"native_tokens_cached":512,`+
				`"native_tokens_reasoning":0,"tokens_prompt":1000,"tokens_completion":50,"cache_discount":0.0001}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/key":
			if r.Header.Get("Authorization") != "Bearer "+orFakeKey {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, `{"data":{"label":"sk-or-v1-abc...","usage":3.25,"limit":20,"limit_remaining":16.75}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/models/"+orGLM+"/endpoints":
			if r.Header.Get("Authorization") != "" {
				t.Errorf("the endpoints list must be read without a key")
			}
			_, _ = w.Write(glm)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/messages":
			var body map[string]json.RawMessage
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.auth, f.body = append(f.auth, r.Header.Get("Authorization")), append(f.body, body)
			f.gens++
			gen, msg := fmt.Sprintf("gen-%d", f.gens), fmt.Sprintf("msg_%d", f.gens)
			f.mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("X-Generation-Id", gen)
			w.Header().Set("Request-Id", gen)
			if f.cost == "cut" {
				// The stream breaks before its final usage (B4).
				_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\""+msg+"\",\"usage\":{\"input_tokens\":1000,\"output_tokens\":1}}}\n\n")
				w.(http.Flusher).Flush()
				if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
					_ = conn.Close()
				}
				return
			}
			_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\""+msg+"\",\"provider\":\"Baidu\",\"usage\":{\"input_tokens\":1000,\"output_tokens\":1}}}\n\n"+
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":50,\"cost\":"+f.cost+"}}\n\n"+
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// orFakeClaude stands in for claude: it records its environment and argv, sends one request to
// ANTHROPIC_BASE_URL with its token (two with FAKE_TWO in the prompt), one with a wrong token, and
// prints a successful result whose own cost figure the wrapper must not trust (with FAKE_MIDTASK, one
// without a report: a mid-task end, #126).
const orFakeClaude = `#!/bin/bash
prompt=$(cat)
env > ../claude-env.txt
printf '%s\n' "$@" > ../claude-args.txt
send() {
  curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $1" -H 'content-type: application/json' \
    -d '{"model":"claude-sonnet-4-6","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}' \
    "$ANTHROPIC_BASE_URL/v1/messages"
}
if [[ $prompt == *KEYPROBE=* ]]; then
  # B6: the key file is unreadable to the job by any route: directly, and through a script it wrote.
  key=${prompt#*KEYPROBE=}; key=${key%%[[:space:]]*}
  { cat "$key"; echo "cat rc=$?"; printf 'cat "%s"\n' "$key" > ../probe.sh; bash ../probe.sh; echo "script rc=$?"; } > ../key-probe.txt 2>&1
fi
echo "first $(send "$ANTHROPIC_AUTH_TOKEN")" >> ../claude-codes.txt
if [[ $prompt == *FAKE_HANG* ]]; then
  sleep 60 # until the daemon ends the attempt
fi
if [[ $prompt == *FAKE_TWO* ]]; then
  echo "second $(send "$ANTHROPIC_AUTH_TOKEN")" >> ../claude-codes.txt
fi
echo "wrong $(send wrong-token)" >> ../claude-codes.txt
echo '{"type":"assistant","message":{"id":"a1","usage":{"input_tokens":1000,"output_tokens":50}}}'
if [[ $prompt == *FAKE_MIDTASK* ]]; then
  echo '{"type":"result","subtype":"success","is_error":false,"num_turns":1,"total_cost_usd":3.5,"result":"Waiting for the subagent notifications."}'
  exit 0
fi
echo '{"type":"result","subtype":"success","is_error":false,"num_turns":1,"total_cost_usd":3.5,"result":"## Summary\nok\n\n## Findings / Changes\n-\n\n## Evidence\n-\n\n## Not done / uncertain\nnothing"}'
`

// orManager is a manager whose config.json (read by the wrapper) and in-memory config both carry
// providers.openrouter pointed at base, with the fake claude above.
func orManager(t *testing.T, base string, keyFile string) (*Manager, *syncBuffer) {
	t.Helper()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl is needed by the fake claude")
	}
	m := newManager(t)
	bin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(bin, []byte(orFakeClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_JOBS_CLAUDE_BIN", bin)
	// Decoys the wrapper must keep from claude.
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-decoy0123456789abcdefghij")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oauth-decoy")
	cfg := map[string]any{"providers": map[string]any{"openrouter": map[string]any{
		"base_url": base, "key_file": keyFile, "models": []string{orGLM}, "daily_cap_usd": 20}}}
	b, _ := json.Marshal(cfg)
	stateDir := filepath.Dir(filepath.Dir(m.JobDir("x")))
	if err := os.WriteFile(filepath.Join(stateDir, "config.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	c.OwnerRulesPath = m.Cfg.OwnerRulesPath
	m.Cfg.Providers = c.Providers
	logs := &syncBuffer{}
	m.Log = log.New(logs, "", 0)
	return m, logs
}

// R3, R4, R6, R8 end to end: an OpenRouter job runs claude against the proxy, which forwards with the
// real key, the slug and the provider object; the job's cost is OpenRouter's; the key appears in no
// artifact of the job, no daemon log line and no job row.
func TestOpenRouterJobEndToEndRedaction(t *testing.T) {
	f := newFakeOpenRouter(t, "0.0123")
	keyFile := writeKeyFile(t, t.TempDir())
	m, logs := orManager(t, f.srv.URL+"/api", keyFile)
	j := enqueue(t, m, "do the thing", func(r *StartRequest) { r.Model = "openrouter:" + orGLM })
	j = runToEnd(t, m, j.ID)
	dir := m.JobDir(j.ID)
	stderr, _ := os.ReadFile(filepath.Join(dir, "stderr.log"))
	if j.Status != store.StatusSucceeded {
		t.Fatalf("status %s (%s)\nstderr:\n%s", j.Status, j.Error, stderr)
	}
	if j.CostUSD != 0.0123 || j.Billing != store.BillingAPI {
		t.Fatalf("cost %v (OpenRouter reported 0.0123, claude claimed 3.5), billing %s", j.CostUSD, j.Billing)
	}
	codes, _ := os.ReadFile(filepath.Join(dir, "claude-codes.txt"))
	if string(codes) != "first 200\nwrong 401\n" {
		t.Fatalf("request codes:\n%s", codes)
	}
	f.mu.Lock()
	auth, bodies := slices.Clone(f.auth), slices.Clone(f.body)
	f.mu.Unlock()
	if len(auth) != 1 || auth[0] != "Bearer "+orFakeKey {
		t.Fatalf("upstream auth: %d requests", len(auth))
	}
	var model string
	_ = json.Unmarshal(bodies[0]["model"], &model)
	if model != orGLM {
		t.Fatalf("model sent upstream: %q", model)
	}
	var provider struct {
		Sort          string   `json:"sort"`
		Quantizations []string `json:"quantizations"`
		Data          string   `json:"data_collection"`
		Order         []string `json:"order"`
	}
	if err := json.Unmarshal(bodies[0]["provider"], &provider); err != nil || provider.Sort != "price" || provider.Data != "deny" ||
		slices.Contains(provider.Quantizations, "fp4") || len(provider.Order) == 0 || provider.Order[0] != "baidu/fp8" {
		t.Fatalf("provider object: %s (%v)", bodies[0]["provider"], err)
	}
	env, _ := os.ReadFile(filepath.Join(dir, "claude-env.txt"))
	for _, want := range []string{"ANTHROPIC_BASE_URL=http://127.0.0.1:", "ANTHROPIC_MODEL=" + orGLM + "\n",
		"ANTHROPIC_DEFAULT_OPUS_MODEL=" + orGLM, "ANTHROPIC_DEFAULT_SONNET_MODEL=" + orGLM, "ANTHROPIC_DEFAULT_HAIKU_MODEL=" + orGLM,
		"CLAUDE_CODE_SUBAGENT_MODEL=" + orGLM, "ANTHROPIC_AUTH_TOKEN=agent-jobs-proxy-", "\nANTHROPIC_API_KEY=\n"} {
		if !strings.Contains("\n"+string(env), want) {
			t.Errorf("claude env lacks %q", want)
		}
	}
	for _, bad := range []string{"sk-ant-decoy", "oauth-decoy", "CLAUDE_CODE_OAUTH_TOKEN", "OPENROUTER_API_KEY"} {
		if strings.Contains(string(env), bad) {
			t.Errorf("claude env has %q", bad)
		}
	}
	if args, _ := os.ReadFile(filepath.Join(dir, "claude-args.txt")); !strings.Contains(string(args), "--model\n"+orGLM+"\n") {
		t.Errorf("claude argv: %s", args)
	}
	if !strings.Contains(string(stderr), "policy picks baidu/fp8") || !strings.Contains(string(stderr), "openrouter proxy: 1 request(s), cost $0.0123") {
		t.Errorf("stderr.log lacks the routing note or the proxy summary:\n%s", stderr)
	}
	// The redaction check (R6): every file the job left, the daemon's log, the job and stats rows.
	assertNoKey(t, dir, logs.String(), j, m)
}

// assertNoKey fails when the fake key is in any file under dir, the log, the job row or its stats.
func assertNoKey(t *testing.T, dir, logs string, j *store.Job, m *Manager) {
	t.Helper()
	files := 0
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files++
		if bytes.Contains(b, []byte(orFakeKey)) || bytes.Contains(b, []byte(orFakeKey[9:])) {
			t.Errorf("key in %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"prompt.md", "report.md", "stream.jsonl", "stderr.log"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s missing: the redaction check would prove nothing", name)
		}
	}
	if files < 4 || strings.Contains(logs, orFakeKey[9:]) {
		t.Errorf("files %d; key in daemon log: %t", files, strings.Contains(logs, orFakeKey[9:]))
	}
	row, _ := json.Marshal(get(t, m, j.ID))
	if bytes.Contains(row, []byte(orFakeKey[9:])) {
		t.Error("key in the job row")
	}
	if raw, _, _ := m.Store.WrapperResult(j.ID, get(t, m, j.ID).LaunchToken); strings.Contains(raw, orFakeKey[9:]) {
		t.Error("key in the wrapper result")
	}
	if st, err := m.Store.GetStats(j.ID); err == nil {
		b, _ := json.Marshal(st)
		if bytes.Contains(b, []byte(orFakeKey[9:])) || st.Provider != "openrouter" {
			t.Errorf("stats: %s", b)
		}
	}
}

// R4: once the job's requests cost its budget, the proxy refuses the next one and the job fails with
// that reason.
func TestOpenRouterBudgetEnforced(t *testing.T) {
	f := newFakeOpenRouter(t, "2.5")
	m, _ := orManager(t, f.srv.URL+"/api", writeKeyFile(t, t.TempDir()))
	j := enqueue(t, m, "FAKE_TWO", func(r *StartRequest) { r.Model = "openrouter:" + orGLM; r.MaxBudgetUSD = 1 })
	j = runToEnd(t, m, j.ID)
	codes, _ := os.ReadFile(filepath.Join(m.JobDir(j.ID), "claude-codes.txt"))
	if string(codes) != "first 200\nsecond 400\nwrong 401\n" {
		t.Fatalf("request codes:\n%s", codes)
	}
	if j.Status != store.StatusFailed || !strings.Contains(j.Error, "openrouter budget reached") || j.CostUSD != 2.5 {
		t.Fatalf("status %s, error %q, cost %v", j.Status, j.Error, j.CostUSD)
	}
	f.mu.Lock()
	n := len(f.auth)
	f.mu.Unlock()
	if n != 1 {
		t.Fatalf("upstream requests %d, want 1 (the second refused by the proxy)", n)
	}
	if _, err := m.Resume(j.ID, "try again", Limits{}); err == nil || !strings.Contains(err.Error(), "not resumed") {
		t.Fatalf("resume of an openrouter job: %v", err)
	}
}

// R3: a missing key file fails the job before claude starts, with a clear reason.
func TestOpenRouterMissingKeyFailsFast(t *testing.T) {
	f := newFakeOpenRouter(t, "0.01")
	m, _ := orManager(t, f.srv.URL+"/api", filepath.Join(t.TempDir(), "missing.env"))
	j := enqueue(t, m, "x", func(r *StartRequest) { r.Model = "openrouter:" + orGLM })
	j = runToEnd(t, m, j.ID)
	if j.Status != store.StatusFailed || !strings.Contains(j.Error, "openrouter job not started: openrouter key_file") {
		t.Fatalf("status %s, error %q", j.Status, j.Error)
	}
	if _, err := os.Stat(filepath.Join(m.JobDir(j.ID), "claude-env.txt")); err == nil {
		t.Fatal("claude ran without a key")
	}
	// Nothing was sent: the job counts $0 against the daily cap, not its whole budget.
	if j.CostIsUnknown() || j.CostUSD != 0 {
		t.Fatalf("cost unknown %t, cost %v", j.CostIsUnknown(), j.CostUSD)
	}
	if got, _ := m.OpenRouterSpentToday(); got != "$0.00/$20.00" {
		t.Fatalf("spent today: %q", got)
	}
}

// The proxy refuses a wrong token and never echoes the key in an upstream error.
func TestOpenRouterProxyUnit(t *testing.T) {
	p := &orProxy{upstream: "http://127.0.0.1:1", key: orFakeKey, token: "tok", slug: orGLM, provider: json.RawMessage(`{}`), budget: 1,
		client: &http.Client{Timeout: 2 * time.Second}}
	if err := startORProxy(p); err != nil {
		t.Fatal(err)
	}
	defer p.close()
	do := func(token string) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, p.URL()+"/v1/messages", strings.NewReader(`{"model":"x"}`))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, _ := do("nope"); code != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", code)
	}
	code, body := do("tok")
	if code != http.StatusBadGateway || strings.Contains(body, orFakeKey) {
		t.Fatalf("upstream down: %d %s", code, body)
	}
	// usage from an SSE stream and from a JSON body
	u := &usageReader{stream: true}
	u.write([]byte("data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10,\"cache_read_input_tokens\":5}}}\n\ndata: {\"type\":\"message_del"))
	u.write([]byte("ta\",\"usage\":{\"output_tokens\":7}}\n"))
	if r := u.result(); r.Tokens != (TokenUsage{Input: 10, Output: 7, CacheRead: 5}) || r.HasCost {
		t.Fatalf("sse usage: %+v", r)
	}
	u = &usageReader{}
	u.write([]byte(`{"usage":{"input_tokens":3,"output_tokens":4,"cost":0.5}}`))
	if r := u.result(); r.Tokens.Input != 3 || !r.HasCost || r.Cost != 0.5 {
		t.Fatalf("json usage: %+v", r)
	}
	p.price = ModelPrice{Input: 1, Output: 2}
	if !p.reserve(0) {
		t.Fatal("reserve refused")
	}
	p.settle(0, responseUsage{Tokens: TokenUsage{Input: 1e6, Output: 1e6}}, settleDone)
	if p.Cost() != 3 {
		t.Fatalf("estimated cost %v", p.Cost())
	}
}

// The openrouter key shape is redacted like the other credentials.
func TestRedactOpenRouterKey(t *testing.T) {
	out := RedactText("key " + orFakeKey + " end")
	if strings.Contains(out, orFakeKey[9:]) || !strings.Contains(out, "[redacted:openrouter]") {
		t.Fatalf("redacted: %q", out)
	}
}

// R7: job_stats groups by provider; the model keeps the openrouter: prefix, so by model shows it too.
func TestStatsByProvider(t *testing.T) {
	rows := []*store.JobStats{
		{ID: "a", Kind: "fix", Model: "openrouter:" + orGLM, Provider: "openrouter", Status: store.StatusSucceeded, CostUSD: 0.5},
		{ID: "b", Kind: "fix", Model: "opus", Provider: "anthropic", Status: store.StatusSucceeded, CostUSD: 2},
		{ID: "c", Kind: "review", Model: "gpt-5.5", Provider: "openai", Status: store.StatusSucceeded, CostUnknown: true},
		{ID: "d", Kind: "fix", Model: "sonnet", Status: store.StatusSucceeded},
	}
	rep, err := Summarize(rows, "provider")
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]int{}
	for _, g := range rep.Groups {
		keys[g.Key] = g.Jobs
	}
	if keys["openrouter"] != 1 || keys["anthropic"] != 1 || keys["openai"] != 1 || keys["(none)"] != 1 {
		t.Fatalf("groups: %v", keys)
	}
	rep, _ = Summarize(rows, "model")
	found := false
	for _, g := range rep.Groups {
		found = found || g.Key == "openrouter:"+orGLM
	}
	if !found {
		t.Fatal("by model lost the openrouter model")
	}
}

// R6: no job reads the key file: the guard of every job (an openrouter one or not) refuses it for the
// read tools and as a Bash word.
func TestOpenRouterKeyFileNotReadable(t *testing.T) {
	m := newManager(t)
	keyFile := writeKeyFile(t, t.TempDir())
	m.Cfg.Providers.OpenRouter = orConfig(DefaultOpenRouterBaseURL, keyFile)
	j := protectJob(t, m, "keyread")
	cfg := jobHookConfig(t, m, j)
	hook := m.guardCommand(j.ID, "", nil, nil, nil)
	for _, g := range reNoRead.FindAllStringSubmatch(hook, -1) {
		cfg.NoRead = append(cfg.NoRead, g[1])
	}
	if !slices.Contains(cfg.NoRead, keyFile) {
		t.Fatalf("hook --no-read = %v, want %s", cfg.NoRead, keyFile)
	}
	q := shellQuote(keyFile)
	for _, c := range []string{"cat " + q, "grep OPENROUTER " + q, "cp " + q + " ./k", "base64 < " + q} {
		if r := guard.Check(c, cfg); r == "" {
			t.Errorf("Check %q allowed", c)
		}
	}
	if r := guard.CheckFileRead("Read", keyFile, "", cfg); r == "" {
		t.Error("Read of the key file allowed")
	}
}

// #126 meets #136: an openrouter job that ends its turn without a report fails like any coder job, but
// it is not auto-continued (each attempt spends real money against the daily cap, which counts a job
// once): the refusal is in its error, and no second attempt reaches OpenRouter.
func TestOpenRouterMidTaskEndNotContinued(t *testing.T) {
	f := newFakeOpenRouter(t, "0.01")
	m, _ := orManager(t, f.srv.URL+"/api", writeKeyFile(t, t.TempDir()))
	j := enqueue(t, m, "FAKE_MIDTASK", func(r *StartRequest) { r.Model = "openrouter:" + orGLM })
	j = runToEnd(t, m, j.ID)
	if j.Status != store.StatusFailed || j.Attempt != 1 || !strings.Contains(j.Error, NoteNoSummary) ||
		!strings.Contains(j.Error, "not auto-continued") || !strings.Contains(j.Error, "openrouter") {
		t.Fatalf("status %s attempt %d error %q", j.Status, j.Attempt, j.Error)
	}
	if j.CostUSD != 0.01 || j.CostUnknown {
		t.Fatalf("cost %v unknown %v, want OpenRouter's 0.01", j.CostUSD, j.CostUnknown)
	}
	f.mu.Lock()
	n := len(f.auth)
	f.mu.Unlock()
	if n != 1 {
		t.Fatalf("upstream requests %d, want 1 (no continuation)", n)
	}
}

// B4 (#136 R3 review) end to end: a request whose stream breaks before its final usage leaves the attempt's
// cost unknown, charged at least at the request's reservation; it never records a known $0.
func TestOpenRouterCutStreamCostUnknown(t *testing.T) {
	f := newFakeOpenRouter(t, "cut")
	m, _ := orManager(t, f.srv.URL+"/api", writeKeyFile(t, t.TempDir()))
	j := enqueue(t, m, "x", func(r *StartRequest) { r.Model = "openrouter:" + orGLM })
	j = runToEnd(t, m, j.ID)
	if !j.CostIsUnknown() || j.CostUSD <= 0 {
		stderr, _ := os.ReadFile(filepath.Join(m.JobDir(j.ID), "stderr.log"))
		t.Fatalf("status %s cost %v unknown %t, want a non-zero reservation and an unknown cost\nstderr:\n%s",
			j.Status, j.CostUSD, j.CostIsUnknown(), stderr)
	}
	// The daily cap counts the job at its whole budget.
	if got, _ := m.OpenRouterSpentToday(); got != fmt.Sprintf("$%.2f/$20.00", j.MaxBudgetUSD) {
		t.Fatalf("spent today: %q", got)
	}
}

// B2 (#136 R3 review) probe: an openrouter job stopped while still queued (attempt 0) is not resumed
// either: that path requeued it with new limits past the no-resume refusal and the daily cap, so its
// budget could be raised to the $50 ceiling over a $20 cap.
func TestR3ProbeResumeAttempt0RaisesBudgetPastCap(t *testing.T) {
	f := newFakeOpenRouter(t, "0.01")
	m, _ := orManager(t, f.srv.URL+"/api", writeKeyFile(t, t.TempDir()))
	j := enqueue(t, m, "x", func(r *StartRequest) { r.Model = "openrouter:" + orGLM; r.MaxBudgetUSD = 5 })
	if _, err := m.Stop(j.ID, "probe"); err != nil {
		t.Fatal(err)
	}
	if got := get(t, m, j.ID); got.Status != store.StatusStopped || got.Attempt != 0 {
		t.Fatalf("status %s attempt %d, want a stopped job that never ran", got.Status, got.Attempt)
	}
	_, err := m.Resume(j.ID, "more money", Limits{MaxBudgetUSD: 50})
	if err == nil || !strings.Contains(err.Error(), "not resumed") {
		t.Fatalf("resume of a queued-stopped openrouter job with $50: %v", err)
	}
	if got := get(t, m, j.ID); got.Status != store.StatusStopped || got.MaxBudgetUSD != 5 {
		t.Fatalf("row changed: status %s budget %v", got.Status, got.MaxBudgetUSD)
	}
	// Without new limits too: a stopped job's whole budget already counts against the cap.
	if _, err := m.Resume(j.ID, "same money", Limits{}); err == nil {
		t.Fatal("resume of a queued-stopped openrouter job")
	}
}

// B6 (#136 R3 review) end to end: with providers.openrouter configured, no job process reads the key
// file, by cat or by a script it wrote (the guard is not involved: the fake claude is a shell script), in
// an openrouter job and in a subscription job alike; the wrapper's proxy still has the key.
func TestOpenRouterKeyFileUnreadableToJobProcesses(t *testing.T) {
	if err := landlockAvailable(); err != nil {
		t.Skip(err)
	}
	f := newFakeOpenRouter(t, "0.01")
	keyFile := writeKeyFile(t, t.TempDir())
	m, _ := orManager(t, f.srv.URL+"/api", keyFile)
	for _, model := range []string{"openrouter:" + orGLM, ""} {
		j := enqueue(t, m, "KEYPROBE="+keyFile+" go", func(r *StartRequest) {
			if model != "" {
				r.Model = model
			}
		})
		j = runToEnd(t, m, j.ID)
		dir := m.JobDir(j.ID)
		probe, err := os.ReadFile(filepath.Join(dir, "key-probe.txt"))
		if err != nil {
			stderr, _ := os.ReadFile(filepath.Join(dir, "stderr.log"))
			t.Fatalf("model %q: no probe (%v); status %s %q\nstderr:\n%s", model, err, j.Status, j.Error, stderr)
		}
		if bytes.Contains(probe, []byte(orFakeKey[9:])) || !strings.Contains(string(probe), "Permission denied") ||
			strings.Contains(string(probe), "cat rc=0") || strings.Contains(string(probe), "script rc=0") {
			t.Fatalf("model %q: the job read the key file:\n%s", model, probe)
		}
		if model != "" && (j.Status != store.StatusSucceeded || j.CostUSD != 0.01) {
			t.Fatalf("openrouter job: status %s %q cost %v (the proxy must still have the key)", j.Status, j.Error, j.CostUSD)
		}
	}
}

// B6: a key placement the sandbox cannot protect fails every job before anything of it runs.
func TestKeySandboxRefusedPlacementFailsJobs(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	c := Config{}
	c.Providers.OpenRouter = orConfig(DefaultOpenRouterBaseURL, filepath.Join(home, ".secrets", "openrouter.env"))
	old := keyPlacementCheck
	keyPlacementCheck = true
	defer func() { keyPlacementCheck = old }()
	if _, err := wrapperKeySandbox(c, t.TempDir()); err == nil || !strings.Contains(err.Error(), "overlaps") {
		t.Fatalf("key under $HOME: %v", err)
	}
	if s, err := wrapperKeySandbox(Config{}, t.TempDir()); s != nil || err != nil {
		t.Fatalf("no openrouter: %v %v", s, err)
	}
}

// D2 (#136 fix loop 2): bwrap cannot start in any Landlock domain, so with providers.openrouter set a
// codex job on codex's native sandbox runs with its runner outside the key sandbox by default
// (codex_policy allow_unprotected) and says so loudly in stderr.log; codex_policy refuse refuses it.
func TestCodexNativeJobUnderOpenRouterPolicy(t *testing.T) {
	if err := landlockAvailable(); err != nil {
		t.Skip(err)
	}
	keyFile := writeKeyFile(t, t.TempDir())
	for _, policy := range []string{"", CodexPolicyAllowUnprotected, CodexPolicyRefuse} {
		m := newCodexManager(t)
		o := map[string]any{"base_url": "https://openrouter.example/api", "key_file": keyFile,
			"models": []string{orGLM}, "daily_cap_usd": 20}
		if policy != "" {
			o["codex_policy"] = policy
		}
		b, _ := json.Marshal(map[string]any{"providers": map[string]any{"openrouter": o}})
		writeCodexConfig(t, m, string(b))
		j := runToEnd(t, m, enqueue(t, m, "review the thing", codexReview).ID)
		stderr, _ := os.ReadFile(filepath.Join(m.JobDir(j.ID), "stderr.log"))
		if policy == CodexPolicyRefuse {
			if j.Status == store.StatusSucceeded || !strings.Contains(j.Error, "openrouter key protection") {
				t.Fatalf("refuse: %s %q\nstderr:\n%s", j.Status, j.Error, stderr)
			}
			continue
		}
		if j.Status != store.StatusSucceeded {
			t.Fatalf("policy %q: %s %q\nstderr:\n%s", policy, j.Status, j.Error, stderr)
		}
		if !strings.Contains(string(stderr), "WARNING: openrouter key protection is OFF for this codex job's runner") {
			t.Fatalf("policy %q: no warning in stderr.log:\n%s", policy, stderr)
		}
	}
}

// Issue #148: claude prices an OpenRouter slug by its fallback Claude table, so its own
// --max-budget-usd would stop the job long before the proxy's real-price budget. An openrouter job's
// argv (fresh and resumed) carries no --max-budget-usd; any other claude job keeps its budget unchanged.
func TestOpenRouterClaudeArgsNoOwnBudget(t *testing.T) {
	m := newManager(t)
	m.Cfg.Providers.OpenRouter = orConfig(DefaultOpenRouterBaseURL, "/nonexistent/openrouter.env")
	or := enqueue(t, m, "x", func(r *StartRequest) { r.Model = "openrouter:" + orGLM; r.MaxBudgetUSD = 0.5 })
	or.SessionID = "s"
	for name, args := range map[string][]string{"fresh": ClaudeArgs(or, "/jd"), "resume": ResumeClaudeArgs(or, "/jd")} {
		if slices.Contains(args, "--max-budget-usd") {
			t.Errorf("%s: openrouter job caps claude's own budget: %q", name, args)
		}
		if i := slices.Index(args, "--max-turns"); i < 0 || i+1 >= len(args) || args[i+1] != fmt.Sprint(or.MaxTurns) {
			t.Errorf("%s: --max-turns must stay: %q", name, args)
		}
	}
	c := enqueue(t, m, "x", func(r *StartRequest) { r.Model = "sonnet"; r.MaxBudgetUSD = 0.5 })
	c.SessionID = "s"
	for name, args := range map[string][]string{"fresh": ClaudeArgs(c, "/jd"), "resume": ResumeClaudeArgs(c, "/jd")} {
		i := slices.Index(args, "--max-budget-usd")
		if i < 0 || i+1 >= len(args) || args[i+1] != "0.5" {
			t.Errorf("%s: claude job lacks --max-budget-usd 0.5: %q", name, args)
		}
	}
}

// Issue #148: whatever claude's own result says (an error, its own budget subtype, a non-zero exit),
// a job whose request the proxy refused for the budget fails with the proxy's reason.
func TestOpenRouterBudgetFailureOutranksClaudeExit(t *testing.T) {
	res := StreamResult{Found: true, IsError: true, Subtype: "error_max_budget_usd",
		ProviderFailure: "openrouter budget reached: the job's requests cost $0.5000"}
	for _, code := range []int{0, 1} {
		if st, why := Verdict(code, res, t.TempDir()); st != store.StatusFailed || !strings.HasPrefix(why, "openrouter budget reached") {
			t.Errorf("exit %d: %s %q", code, st, why)
		}
	}
}

// Issue #146: deny_repos checks only the job's repository, so a source file (from a denied repository,
// or another job's report about one) would reach the third-party provider. Until a source policy exists,
// an OpenRouter job takes no sources at all; a subscription job keeps them.
func TestOpenRouterRefusesSources(t *testing.T) {
	m := newManager(t)
	m.Cfg.Providers.OpenRouter = orConfig(DefaultOpenRouterBaseURL, "/k")
	repo := newRepo(t)
	m.Cfg.Providers.OpenRouter.DenyRepos = []string{filepath.Base(repo)}
	denied := sourceFixture(t, repo, "secret.md", "private notes")
	outside := sourceFixture(t, t.TempDir(), "notes.md", "public notes")
	prior := enqueue(t, m, "x", nil)
	if err := os.MkdirAll(m.JobDir(prior.ID), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.JobDir(prior.ID), "report.md"), []byte("report on a private repo"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, sources := range map[string][]string{
		"file in a denied repo": {denied},
		"file outside any repo": {outside},
		"another job's report":  {"job:" + prior.ID},
	} {
		_, err := m.Enqueue(StartRequest{Title: "t", Kind: "general", Brief: "b", Model: "openrouter:" + orGLM,
			MaxBudgetUSD: 1, Sources: sources})
		if err == nil || !strings.Contains(err.Error(), "openrouter jobs take no sources") {
			t.Errorf("%s: %v, want the sources refusal", name, err)
		}
	}
	if spend, err := m.Store.ProviderSpendSince(m.Agent, store.ProviderOpenRouter, time.Now().Add(-time.Hour)); err != nil || spend != 0 {
		t.Fatalf("a refused job reserved $%.2f, %v", spend, err)
	}
	// Without sources the OpenRouter job starts; a subscription job keeps its sources.
	enqueue(t, m, "x", func(r *StartRequest) { r.Model = "openrouter:" + orGLM; r.MaxBudgetUSD = 1 })
	j := enqueue(t, m, "x", func(r *StartRequest) { r.Sources = []string{outside} })
	if b, err := os.ReadFile(filepath.Join(m.JobDir(j.ID), SourcesDir, "notes.md")); err != nil || string(b) != "public notes" {
		t.Fatalf("subscription job sources: %q, %v", b, err)
	}
}
