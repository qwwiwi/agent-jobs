package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// OpenRouter jobs (issue #136): a claude job (claude -p, the guard, the same prompt and report) whose
// model is served by OpenRouter's Anthropic-compatible endpoint instead of the Anthropic subscription.
// The job's model is "openrouter:<slug>" (e.g. openrouter:z-ai/glm-5.3), the slug one of
// providers.openrouter.models. Such a job is billed "api" (real money): its cost comes from the usage
// OpenRouter reports to the runner, its budget is enforced by the wrapper's loopback proxy, and the
// provider's daily cap refuses new jobs once their spend would pass it.
//
// Routing (R8). Claude Code sends no OpenRouter `provider` object, the `:floor` / `:nitro` model
// variants only pick a sort (price / throughput) and widen the pool with flex / priority tiers, and the
// account-wide preferences (privacy settings) cover data policy and ignored providers but no
// quantization and nothing per job. So the wrapper runs a small proxy on 127.0.0.1 for the job's whole
// life: claude talks to it with a per-job token, and the proxy puts the job's `provider` object (sort,
// quantizations, data_collection, zdr, ignore, order) into every /v1/messages request before it
// forwards the request with the real key, which never leaves the wrapper's memory.

// OpenRouterModelPrefix starts the model of an OpenRouter job.
const OpenRouterModelPrefix = "openrouter:"

// DefaultOpenRouterBaseURL is OpenRouter's Anthropic-compatible API root (claude appends /v1/messages).
const DefaultOpenRouterBaseURL = "https://openrouter.ai/api"

// OpenRouterCapWindow is the window of the provider's daily cap.
const OpenRouterCapWindow = 24 * time.Hour

// ProvidersConfig is the providers section of config.json.
type ProvidersConfig struct {
	// OpenRouter enables jobs on OpenRouter models; nil = not configured (openrouter:* models refused).
	OpenRouter *OpenRouterConfig `json:"openrouter"`
}

// OpenRouterConfig is providers.openrouter of config.json. The key itself is never in config.json:
// key_file names an env-style file holding OPENROUTER_API_KEY=..., read by the wrapper at the start of
// each job.
type OpenRouterConfig struct {
	// BaseURL is the Anthropic-compatible API root; default DefaultOpenRouterBaseURL. https only (plain
	// http only to a loopback host).
	BaseURL string `json:"base_url"`
	// KeyFile is the absolute path of the env file with OPENROUTER_API_KEY=... (required).
	KeyFile string `json:"key_file"`
	// Models is the allow-list of OpenRouter model slugs (e.g. "z-ai/glm-5.3"); a job's slug must be one.
	Models []string `json:"models"`
	// DailyCapUSD caps what this agent's OpenRouter jobs created in the last 24 hours spend or may still
	// spend (queued, running and stopped ones, and those ended without a known cost, count at their whole
	// budget); a job whose budget would pass it is refused. Required, > 0: a money cap is always stated.
	DailyCapUSD *float64 `json:"daily_cap_usd"`
	// DenyRepos are repositories OpenRouter jobs may not work on: an absolute path or glob is matched
	// against the job's repository path, anything else against its folder name.
	DenyRepos []string `json:"deny_repos"`
	// Routing is the data-center routing policy of every model (R8).
	Routing RoutingPolicy `json:"routing"`
	// ModelRouting overrides fields of Routing per model slug (a slug of Models).
	ModelRouting map[string]RoutingPolicy `json:"model_routing"`
	// CodexPolicy is what happens to a codex job on codex's native sandbox while the key sandbox is on
	// (keysandbox.go): that sandbox is bwrap, which cannot start inside any Landlock domain (Landlock
	// forbids mount(2) to a restricted process), so the job either runs with its codex process outside
	// the key sandbox, where codex and every command it runs can read key_file (allow_unprotected, the
	// default, warned about in stderr.log and system_doctor), or is refused (refuse). A codex job with
	// codex_sandbox worktree-full-access runs inside the key sandbox either way.
	CodexPolicy string `json:"codex_policy,omitempty"`
}

// Values of providers.openrouter.codex_policy.
const (
	CodexPolicyAllowUnprotected = "allow_unprotected"
	CodexPolicyRefuse           = "refuse"
)

// codexPolicy is the configured codex_policy (allow_unprotected when unset).
func (o *OpenRouterConfig) codexPolicy() string {
	if o.CodexPolicy == "" {
		return CodexPolicyAllowUnprotected
	}
	return o.CodexPolicy
}

// RoutingPolicy is how the data-center provider of an OpenRouter model is chosen. Every field is
// optional; the effective policy fills in the defaults (OpenRouterConfig.Policy).
type RoutingPolicy struct {
	// Sort is price (default), throughput or latency.
	Sort string `json:"sort,omitempty"`
	// MinQuantization is the lowest quantization accepted (default fp8): an endpoint below it, or one
	// whose quantization is unknown, is not used. "any" accepts every endpoint.
	MinQuantization string `json:"min_quantization,omitempty"`
	// DataCollection is deny (default: only providers that do not store or train on the data) or allow.
	DataCollection string `json:"data_collection,omitempty"`
	// ZDR restricts routing to zero-data-retention endpoints (default false).
	ZDR *bool `json:"zdr,omitempty"`
	// MinUptimePct drops endpoints whose recent uptime is below it (default 95; 0 = no uptime filter).
	MinUptimePct *float64 `json:"min_uptime_pct,omitempty"`
	// Ignore are provider slugs never used ("deepinfra", "baseten/fp4").
	Ignore []string `json:"ignore,omitempty"`
	// Order are provider slugs tried first, in this order, before the sorted rest.
	Order []string `json:"order,omitempty"`
}

// Routing defaults (R8).
const (
	DefaultRoutingSort            = "price"
	DefaultRoutingMinQuantization = "fp8"
	DefaultRoutingDataCollection  = "deny"
	DefaultRoutingMinUptimePct    = 95.0
	// QuantizationAny is the min_quantization that accepts every endpoint.
	QuantizationAny = "any"
)

// RoutingSorts are the accepted routing sorts.
var RoutingSorts = []string{"price", "throughput", "latency"}

// quantizationBits ranks OpenRouter's quantization levels (the Quantization enum of its API) by bits.
// "unknown" has no rank: it never satisfies a minimum.
var quantizationBits = map[string]int{
	"int4": 4, "fp4": 4, "mxfp4": 4, "nvfp4": 4, "fp6": 6, "int8": 8, "fp8": 8, "mxfp8": 8,
	"fp16": 16, "bf16": 16, "fp32": 32,
}

// quantizationOrder is the enum in OpenRouter's order, for a stable quantizations list.
var quantizationOrder = []string{"int4", "int8", "fp4", "mxfp4", "nvfp4", "fp6", "fp8", "mxfp8", "fp16", "bf16", "fp32"}

// EffectiveRouting is a policy with every default filled in.
type EffectiveRouting struct {
	Sort            string   `json:"sort"`
	MinQuantization string   `json:"min_quantization"`
	DataCollection  string   `json:"data_collection"`
	ZDR             bool     `json:"zdr"`
	MinUptimePct    float64  `json:"min_uptime_pct"`
	Ignore          []string `json:"ignore,omitempty"`
	Order           []string `json:"order,omitempty"`
}

// Policy returns the effective routing of slug: the model's override over the common routing over the
// defaults.
func (c *OpenRouterConfig) Policy(slug string) EffectiveRouting {
	p := EffectiveRouting{Sort: DefaultRoutingSort, MinQuantization: DefaultRoutingMinQuantization,
		DataCollection: DefaultRoutingDataCollection, MinUptimePct: DefaultRoutingMinUptimePct}
	if c == nil {
		return p
	}
	for _, r := range []RoutingPolicy{c.Routing, c.ModelRouting[slug]} {
		if r.Sort != "" {
			p.Sort = r.Sort
		}
		if r.MinQuantization != "" {
			p.MinQuantization = r.MinQuantization
		}
		if r.DataCollection != "" {
			p.DataCollection = r.DataCollection
		}
		if r.ZDR != nil {
			p.ZDR = *r.ZDR
		}
		if r.MinUptimePct != nil {
			p.MinUptimePct = *r.MinUptimePct
		}
		if r.Ignore != nil {
			p.Ignore = slices.Clone(r.Ignore)
		}
		if r.Order != nil {
			p.Order = slices.Clone(r.Order)
		}
	}
	return p
}

// Quantizations is the provider.quantizations filter of the policy: every level at or above
// MinQuantization (nil for "any": no filter).
func (p EffectiveRouting) Quantizations() []string {
	min, ok := quantizationBits[p.MinQuantization]
	if !ok {
		return nil
	}
	var out []string
	for _, q := range quantizationOrder {
		if quantizationBits[q] >= min {
			out = append(out, q)
		}
	}
	return out
}

// quantizationOK reports whether an endpoint's quantization satisfies the policy.
func (p EffectiveRouting) quantizationOK(q string) bool {
	if p.MinQuantization == QuantizationAny {
		return true
	}
	bits, ok := quantizationBits[strings.ToLower(q)]
	return ok && bits >= quantizationBits[p.MinQuantization]
}

// ProviderObject is the OpenRouter `provider` object the proxy injects: the policy's filters, its
// sort, and order = the policy's own order followed by ranked (the eligible endpoints ranked locally,
// when the endpoints list could be read). Fallbacks stay allowed: the filters still bind them.
func (p EffectiveRouting) ProviderObject(ranked []string, lowUptime []string) map[string]any {
	obj := map[string]any{
		"sort":            p.Sort,
		"data_collection": p.DataCollection,
		"allow_fallbacks": true,
	}
	if q := p.Quantizations(); q != nil {
		obj["quantizations"] = q
	}
	if p.ZDR {
		obj["zdr"] = true
	}
	ignore := slices.Clone(p.Ignore)
	for _, t := range lowUptime {
		if !slices.Contains(ignore, t) {
			ignore = append(ignore, t)
		}
	}
	if len(ignore) > 0 {
		obj["ignore"] = ignore
	}
	order := slices.Clone(p.Order)
	for _, t := range ranked {
		if !slices.Contains(order, t) {
			order = append(order, t)
		}
	}
	if len(order) > maxProviderOrder {
		order = order[:maxProviderOrder]
	}
	if len(order) > 0 {
		obj["order"] = order
	}
	return obj
}

// maxProviderOrder caps the order list sent to OpenRouter.
const maxProviderOrder = 8

// reOpenRouterSlug is an OpenRouter model slug: author/model, with an optional :variant.
var reOpenRouterSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}/[A-Za-z0-9][A-Za-z0-9._-]{0,95}(?::[a-z0-9_-]{1,32})?$`)

// reProviderSlug is an OpenRouter provider (endpoint) slug: provider[/variant][/region][/tier].
var reProviderSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}(?:/[a-z0-9][a-z0-9._-]{0,63}){0,3}$`)

// OpenRouterSlug returns the slug of an "openrouter:<slug>" model; ok is false for any other model.
func OpenRouterSlug(model string) (string, bool) {
	if !strings.HasPrefix(model, OpenRouterModelPrefix) {
		return "", false
	}
	return strings.TrimPrefix(model, OpenRouterModelPrefix), true
}

// IsOpenRouterJob reports whether j runs on an OpenRouter model.
func IsOpenRouterJob(j *store.Job) bool { return j.Provider == store.ProviderOpenRouter }

// JobProvider is who serves a job's model: openrouter, openai (codex) or anthropic.
func JobProvider(j *store.Job) string {
	switch {
	case j.Provider != "":
		return j.Provider
	case j.Runner == store.RunnerCodex:
		return "openai"
	}
	return "anthropic"
}

// openRouterKeyFieldNames would hold a key inside config.json: refused, the key goes in key_file.
var openRouterKeyFieldNames = []string{"api_key", "key", "token", "openrouter_api_key", "auth_token"}

// validateProviders checks the providers section; raw is providers.openrouter as written (nil when
// absent), read for key fields that must not be there.
func (c *Config) validateProviders(raw json.RawMessage) error {
	o := c.Providers.OpenRouter
	if o == nil {
		return nil
	}
	if raw != nil {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err == nil {
			for _, k := range openRouterKeyFieldNames {
				if _, ok := fields[k]; ok {
					return fmt.Errorf("providers.openrouter.%s: the key is never stored in config.json; put "+
						"OPENROUTER_API_KEY=... in the file named by key_file (mode 600)", k)
				}
			}
		}
	}
	if o.BaseURL == "" {
		o.BaseURL = DefaultOpenRouterBaseURL
	}
	if err := validateOpenRouterBaseURL(o.BaseURL); err != nil {
		return err
	}
	if o.KeyFile == "" || !filepath.IsAbs(o.KeyFile) {
		return fmt.Errorf("providers.openrouter.key_file %q must be an absolute path (an env file with OPENROUTER_API_KEY=...)", o.KeyFile)
	}
	if len(o.Models) == 0 {
		return errors.New("providers.openrouter.models must list the allowed model slugs (e.g. z-ai/glm-5.3)")
	}
	for _, m := range o.Models {
		if !reOpenRouterSlug.MatchString(m) {
			return fmt.Errorf("providers.openrouter.models: %q is not an OpenRouter model slug (author/model)", m)
		}
	}
	if o.DailyCapUSD == nil || !(*o.DailyCapUSD > 0) || math.IsInf(*o.DailyCapUSD, 0) {
		return errors.New("providers.openrouter.daily_cap_usd is required and must be > 0 (USD per 24 hours): " +
			"OpenRouter jobs spend real money")
	}
	for _, d := range o.DenyRepos {
		if strings.TrimSpace(d) == "" {
			return errors.New("providers.openrouter.deny_repos: an entry is empty")
		}
		if _, err := filepath.Match(d, ""); err != nil {
			return fmt.Errorf("providers.openrouter.deny_repos %q: %w", d, err)
		}
	}
	if p := o.CodexPolicy; p != "" && p != CodexPolicyAllowUnprotected && p != CodexPolicyRefuse {
		return fmt.Errorf("providers.openrouter.codex_policy %q must be %s or %s", p, CodexPolicyAllowUnprotected, CodexPolicyRefuse)
	}
	if err := o.Routing.validate("providers.openrouter.routing"); err != nil {
		return err
	}
	for slug, r := range o.ModelRouting {
		if !slices.Contains(o.Models, slug) {
			return fmt.Errorf("providers.openrouter.model_routing: %q is not one of providers.openrouter.models", slug)
		}
		if err := r.validate("providers.openrouter.model_routing." + slug); err != nil {
			return err
		}
	}
	return nil
}

func validateOpenRouterBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("providers.openrouter.base_url %q must be an https URL without credentials, query or fragment", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if h := u.Hostname(); h == "localhost" || isLoopbackIP(h) {
			return nil // a local test server or gateway
		}
	}
	return fmt.Errorf("providers.openrouter.base_url %q must be https (plain http only to a loopback host)", raw)
}

func isLoopbackIP(h string) bool {
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func (r RoutingPolicy) validate(where string) error {
	if r.Sort != "" && !slices.Contains(RoutingSorts, r.Sort) {
		return fmt.Errorf("%s.sort %q must be one of %s", where, r.Sort, strings.Join(RoutingSorts, "|"))
	}
	if q := r.MinQuantization; q != "" && q != QuantizationAny {
		if _, ok := quantizationBits[q]; !ok {
			return fmt.Errorf("%s.min_quantization %q must be one of %s|%s", where, q, strings.Join(quantizationOrder, "|"), QuantizationAny)
		}
	}
	if d := r.DataCollection; d != "" && d != "deny" && d != "allow" {
		return fmt.Errorf("%s.data_collection %q must be deny or allow", where, d)
	}
	if u := r.MinUptimePct; u != nil && (*u < 0 || *u > 100 || math.IsNaN(*u)) {
		return fmt.Errorf("%s.min_uptime_pct %g must be 0..100", where, *u)
	}
	for _, list := range [][]string{r.Ignore, r.Order} {
		for _, s := range list {
			if !reProviderSlug.MatchString(s) {
				return fmt.Errorf("%s: %q is not a provider slug (provider[/variant][/region])", where, s)
			}
		}
	}
	return nil
}

// errOpenRouterSources refuses sources on an OpenRouter job (#146): deny_repos covers only the job's
// repository, so a source file from a denied repository, or a job report about one, would reach the
// third-party provider. Until a source policy exists, an OpenRouter job takes none.
var errOpenRouterSources = errors.New("openrouter jobs take no sources (providers.openrouter.deny_repos covers only the job's repo; " +
	"a source file or job report would reach the third-party provider): start it without sources, or on a claude or codex model")

// checkOpenRouterJob refuses an OpenRouter job the config does not allow (R2, R5): not configured, a
// slug off the allow-list, a review job, a codex runner, or a denied repository (repo may be "").
func (c Config) checkOpenRouterJob(slug, kind, runner, repo string) error {
	o := c.Providers.OpenRouter
	if o == nil {
		return errors.New("openrouter models need providers.openrouter in config.json (base_url, key_file, models, daily_cap_usd)")
	}
	if !slices.Contains(o.Models, slug) {
		return fmt.Errorf("openrouter model %q is not allowed: providers.openrouter.models lists %s", slug, strings.Join(o.Models, ", "))
	}
	if kind == "review" {
		return errors.New("openrouter models do not run review jobs: a review runs on the subscription's claude or on codex")
	}
	if IsGateKind(kind) {
		return fmt.Errorf("openrouter models do not run %s jobs (gate kinds run on claude or codex)", kind)
	}
	if runner == store.RunnerCodex {
		return errors.New("an openrouter model runs on the claude runner, not codex")
	}
	if repo != "" {
		if d := o.deniedRepo(repo); d != "" {
			return fmt.Errorf("repository %s is denied to openrouter jobs (providers.openrouter.deny_repos %q)", repo, d)
		}
	}
	return nil
}

// deniedRepo returns the deny_repos entry that matches repo ("" for none): an absolute entry is a path
// or glob of the repository path, any other entry a name or glob of its folder name.
func (o *OpenRouterConfig) deniedRepo(repo string) string {
	clean := filepath.Clean(repo)
	for _, d := range o.DenyRepos {
		target := filepath.Base(clean)
		if filepath.IsAbs(d) {
			target = clean
			d = filepath.Clean(d)
		}
		if ok, err := filepath.Match(d, target); err == nil && ok {
			return d
		}
	}
	return ""
}

// DailyCap returns the provider's daily cap in USD (0 when not configured).
func (o *OpenRouterConfig) DailyCap() float64 {
	if o == nil || o.DailyCapUSD == nil {
		return 0
	}
	return *o.DailyCapUSD
}

// OpenRouterSpentToday is the daily cap's state: "$spent/$cap" (spent includes what running and queued
// jobs may still spend).
func (m *Manager) OpenRouterSpentToday() (string, error) {
	v, err := m.Store.ProviderSpendSince(m.Agent, store.ProviderOpenRouter, m.Now().Add(-OpenRouterCapWindow))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("$%.2f/$%.2f", v, m.Config().Providers.OpenRouter.DailyCap()), nil
}

// Endpoint is one data-center endpoint of an OpenRouter model, from /api/v1/models/<slug>/endpoints.
// Prices are USD per million tokens (the API gives USD per token).
type Endpoint struct {
	Provider     string   `json:"provider"`
	Tag          string   `json:"tag"`
	Quantization string   `json:"quantization"`
	Input        float64  `json:"input_per_m"`
	Output       float64  `json:"output_per_m"`
	CacheRead    *float64 `json:"cache_read_per_m,omitempty"`
	CacheWrite   *float64 `json:"cache_write_per_m,omitempty"`
	Context      int64    `json:"context_length"`
	Uptime30m    *float64 `json:"uptime_last_30m,omitempty"`
	Uptime1d     *float64 `json:"uptime_last_1d,omitempty"`
	Status       int      `json:"status"`
	Latency      *float64 `json:"latency_last_30m,omitempty"`
	Throughput   *float64 `json:"throughput_last_30m,omitempty"`
}

// Uptime is the endpoint's uptime for the policy: the lower of its 30-minute and 1-day figures; ok is
// false when neither is known.
func (e Endpoint) Uptime() (float64, bool) {
	switch {
	case e.Uptime30m != nil && e.Uptime1d != nil:
		return math.Min(*e.Uptime30m, *e.Uptime1d), true
	case e.Uptime30m != nil:
		return *e.Uptime30m, true
	case e.Uptime1d != nil:
		return *e.Uptime1d, true
	}
	return 0, false
}

// Blended is the endpoint's blended price per million tokens, 3 input : 1 output (the usual blend; an
// agent's requests are input-heavy).
func (e Endpoint) Blended() float64 { return (3*e.Input + e.Output) / 4 }

// Price is the endpoint's price as a ModelPrice (a missing cache price counts as the input price).
func (e Endpoint) Price() ModelPrice {
	p := ModelPrice{Input: e.Input, Output: e.Output, CacheRead: e.Input, CacheWrite: e.Input}
	if e.CacheRead != nil {
		p.CacheRead = *e.CacheRead
	}
	if e.CacheWrite != nil {
		p.CacheWrite = *e.CacheWrite
	}
	return p
}

type endpointsDoc struct {
	Data struct {
		ID        string `json:"id"`
		Endpoints []struct {
			ProviderName string   `json:"provider_name"`
			Tag          string   `json:"tag"`
			Quantization *string  `json:"quantization"`
			Context      int64    `json:"context_length"`
			Status       int      `json:"status"`
			Uptime30m    *float64 `json:"uptime_last_30m"`
			Uptime1d     *float64 `json:"uptime_last_1d"`
			Latency      *float64 `json:"latency_last_30m"`
			Throughput   *float64 `json:"throughput_last_30m"`
			Pricing      struct {
				Prompt     string `json:"prompt"`
				Completion string `json:"completion"`
				CacheRead  string `json:"input_cache_read"`
				CacheWrite string `json:"input_cache_write"`
			} `json:"pricing"`
		} `json:"endpoints"`
	} `json:"data"`
}

// ParseEndpoints reads the JSON of /api/v1/models/<slug>/endpoints.
func ParseEndpoints(b []byte) ([]Endpoint, error) {
	var doc endpointsDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("parse endpoints: %w", err)
	}
	perM := func(s string) (float64, bool, error) {
		if strings.TrimSpace(s) == "" {
			return 0, false, nil
		}
		v, err := strconv.ParseFloat(s, 64)
		if err != nil || v < 0 || math.IsInf(v, 0) || math.IsNaN(v) {
			return 0, false, fmt.Errorf("price %q is not a price", s)
		}
		return math.Round(v*1e6*1e6) / 1e6, true, nil // per token -> per million, rounded off float noise
	}
	out := make([]Endpoint, 0, len(doc.Data.Endpoints))
	for _, d := range doc.Data.Endpoints {
		e := Endpoint{Provider: d.ProviderName, Tag: d.Tag, Context: d.Context, Status: d.Status,
			Uptime30m: d.Uptime30m, Uptime1d: d.Uptime1d, Latency: d.Latency, Throughput: d.Throughput, Quantization: "unknown"}
		if d.Quantization != nil && *d.Quantization != "" {
			e.Quantization = strings.ToLower(*d.Quantization)
		}
		var ok bool
		var err error
		if e.Input, ok, err = perM(d.Pricing.Prompt); err != nil || !ok {
			return nil, fmt.Errorf("endpoint %s: input price: %v", d.Tag, errOr(err, "missing"))
		}
		if e.Output, ok, err = perM(d.Pricing.Completion); err != nil || !ok {
			return nil, fmt.Errorf("endpoint %s: output price: %v", d.Tag, errOr(err, "missing"))
		}
		if v, ok, err := perM(d.Pricing.CacheRead); err != nil {
			return nil, fmt.Errorf("endpoint %s: cache read price: %w", d.Tag, err)
		} else if ok {
			e.CacheRead = &v
		}
		if v, ok, err := perM(d.Pricing.CacheWrite); err != nil {
			return nil, fmt.Errorf("endpoint %s: cache write price: %w", d.Tag, err)
		} else if ok {
			e.CacheWrite = &v
		}
		out = append(out, e)
	}
	return out, nil
}

func errOr(err error, s string) any {
	if err != nil {
		return err
	}
	return s
}

// RankedEndpoint is an endpoint with the policy's verdict on it.
type RankedEndpoint struct {
	Endpoint
	Eligible bool `json:"eligible"`
	// Why is the reason an endpoint is not eligible ("" when it is).
	Why string `json:"why,omitempty"`
	// Picked marks the endpoint the policy picks (the first eligible one).
	Picked bool `json:"picked,omitempty"`
}

// providerSlugMatch reports whether an OpenRouter provider slug (ignore, order) names the endpoint:
// the slug or a prefix of its segments ("baseten" matches baseten/fp4/us), or its provider name.
func providerSlugMatch(slug string, e Endpoint) bool {
	slug = strings.ToLower(slug)
	tag := strings.ToLower(e.Tag)
	if tag == slug || strings.HasPrefix(tag, slug+"/") {
		return true
	}
	return strings.EqualFold(strings.ReplaceAll(e.Provider, " ", ""), strings.ReplaceAll(slug, "-", ""))
}

// RankEndpoints applies the policy to the endpoints: eligible ones first (the policy's order first,
// then by its sort: blended price, throughput or latency), then the others; the first eligible one is
// Picked. The data policy (data_collection, zdr) is not in the endpoints list: OpenRouter enforces it.
func RankEndpoints(eps []Endpoint, p EffectiveRouting) []RankedEndpoint {
	out := make([]RankedEndpoint, 0, len(eps))
	for _, e := range eps {
		r := RankedEndpoint{Endpoint: e, Eligible: true}
		up, known := e.Uptime()
		switch {
		case slices.ContainsFunc(p.Ignore, func(s string) bool { return providerSlugMatch(s, e) }):
			r.Eligible, r.Why = false, "ignored"
		case !p.quantizationOK(e.Quantization):
			r.Eligible, r.Why = false, fmt.Sprintf("quantization %s below %s", e.Quantization, p.MinQuantization)
		case p.MinUptimePct > 0 && !known:
			r.Eligible, r.Why = false, "uptime unknown"
		case p.MinUptimePct > 0 && up < p.MinUptimePct:
			r.Eligible, r.Why = false, fmt.Sprintf("uptime %.1f%% below %g%%", up, p.MinUptimePct)
		}
		out = append(out, r)
	}
	orderIdx := func(e Endpoint) int {
		for i, s := range p.Order {
			if providerSlugMatch(s, e) {
				return i
			}
		}
		return len(p.Order)
	}
	less := func(a, b Endpoint) bool {
		switch p.Sort {
		case "throughput":
			if ta, tb := deref(a.Throughput, -1), deref(b.Throughput, -1); ta != tb {
				return ta > tb
			}
		case "latency":
			if la, lb := deref(a.Latency, math.MaxFloat64), deref(b.Latency, math.MaxFloat64); la != lb {
				return la < lb
			}
		}
		if a.Blended() != b.Blended() {
			return a.Blended() < b.Blended()
		}
		if a.Input != b.Input {
			return a.Input < b.Input
		}
		return a.Tag < b.Tag
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Eligible != b.Eligible {
			return a.Eligible
		}
		if oa, ob := orderIdx(a.Endpoint), orderIdx(b.Endpoint); a.Eligible && oa != ob {
			return oa < ob
		}
		return less(a.Endpoint, b.Endpoint)
	})
	if len(out) > 0 && out[0].Eligible {
		out[0].Picked = true
	}
	return out
}

func deref(v *float64, d float64) float64 {
	if v == nil {
		return d
	}
	return *v
}

// routingPlan is what the wrapper decides for an OpenRouter job before claude starts: the provider
// object, the price used to estimate a response without a reported cost (the highest of each price
// among the eligible endpoints: a fallback may land on any of them), and a note for stderr.log.
type routingPlan struct {
	Provider map[string]any
	Price    ModelPrice
	Note     string
	Pick     string // the endpoint the policy picks ("" when the endpoints list was not read)
}

// planRouting builds the routing plan of slug from the endpoints list (eps; nil when it could not be
// read: fetchErr says why). No eligible endpoint fails; so does a list that could not be read when no
// model_prices row prices the model (the budget could not be enforced).
func planRouting(c Config, slug string, eps []Endpoint, fetchErr error) (routingPlan, error) {
	p := c.Providers.OpenRouter.Policy(slug)
	if fetchErr != nil {
		price, ok := c.PriceFor(OpenRouterModelPrefix + slug)
		if !ok {
			price, ok = c.PriceFor(slug)
		}
		if !ok {
			return routingPlan{}, fmt.Errorf("the endpoints of %s could not be read (%v) and model_prices has no row for it: "+
				"the job's budget could not be enforced", slug, fetchErr)
		}
		return routingPlan{Provider: p.ProviderObject(nil, nil), Price: price,
			Note: fmt.Sprintf("endpoints of %s not read (%v): routing by the policy's filters only, priced by model_prices", slug, fetchErr)}, nil
	}
	ranked := RankEndpoints(eps, p)
	var order, low []string
	var price ModelPrice
	for _, r := range ranked {
		if r.Eligible {
			order = append(order, r.Tag)
			ep := r.Price()
			price = ModelPrice{Input: math.Max(price.Input, ep.Input), Output: math.Max(price.Output, ep.Output),
				CacheRead: math.Max(price.CacheRead, ep.CacheRead), CacheWrite: math.Max(price.CacheWrite, ep.CacheWrite)}
		} else if strings.HasPrefix(r.Why, "uptime") {
			low = append(low, r.Tag)
		}
	}
	if len(order) == 0 {
		return routingPlan{}, fmt.Errorf("no endpoint of %s satisfies the routing policy (min_quantization %s, min_uptime_pct %g, "+
			"ignore %v): see `agent-jobs openrouter endpoints %s`, or relax it in providers.openrouter.model_routing",
			slug, p.MinQuantization, p.MinUptimePct, p.Ignore, slug)
	}
	pick := ranked[0]
	ranks := order
	if p.Sort != "price" {
		// The local ranking is by price; throughput and latency are OpenRouter's live figures, so its own
		// sort orders the eligible endpoints (only the policy's explicit order goes first).
		ranks = nil
	}
	return routingPlan{Provider: p.ProviderObject(ranks, low), Price: price, Pick: pick.Tag,
		Note: fmt.Sprintf("openrouter %s: policy picks %s (%s, $%g/$%g per 1M in/out), %d eligible endpoint(s), sort %s",
			slug, pick.Tag, pick.Quantization, pick.Input, pick.Output, len(order), p.Sort)}, nil
}

// maxEndpointsBytes caps the endpoints JSON read.
const maxEndpointsBytes = 4 << 20

// FetchEndpoints reads the public endpoints list of slug from base (the API root, e.g.
// https://openrouter.ai/api). No key is sent: the list is public.
func FetchEndpoints(ctx context.Context, client *http.Client, base, slug string) ([]Endpoint, error) {
	if !reOpenRouterSlug.MatchString(slug) {
		return nil, fmt.Errorf("%q is not an OpenRouter model slug", slug)
	}
	model, _, _ := strings.Cut(slug, ":") // a variant shares its base model's endpoints
	u := strings.TrimRight(base, "/") + "/v1/models/" + model + "/endpoints"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", u, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxEndpointsBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", u, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get %s: HTTP %d", u, resp.StatusCode)
	}
	if len(b) > maxEndpointsBytes {
		return nil, fmt.Errorf("get %s: more than %d bytes", u, maxEndpointsBytes)
	}
	return ParseEndpoints(b)
}

// FormatEndpoints is the table of `agent-jobs openrouter endpoints`: ranked endpoints with prices per
// million tokens, quantization, context and uptime; "*" marks the one the policy picks.
func FormatEndpoints(w io.Writer, slug string, p EffectiveRouting, ranked []RankedEndpoint) {
	fmt.Fprintf(w, "%s: policy sort=%s min_quantization=%s data_collection=%s zdr=%t min_uptime_pct=%g",
		slug, p.Sort, p.MinQuantization, p.DataCollection, p.ZDR, p.MinUptimePct)
	if len(p.Ignore) > 0 {
		fmt.Fprintf(w, " ignore=%s", strings.Join(p.Ignore, ","))
	}
	if len(p.Order) > 0 {
		fmt.Fprintf(w, " order=%s", strings.Join(p.Order, ","))
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  %-28s %-14s %-7s %9s %9s %9s %9s %9s %8s  %s\n",
		"ENDPOINT", "PROVIDER", "QUANT", "IN/1M", "OUT/1M", "CACHE/1M", "BLEND/1M", "CONTEXT", "UPTIME", "VERDICT")
	for _, r := range ranked {
		mark := " "
		if r.Picked {
			mark = "*"
		}
		cache := "-"
		if r.CacheRead != nil {
			cache = fmt.Sprintf("$%.4g", *r.CacheRead)
		}
		uptime := "-"
		if u, ok := r.Uptime(); ok {
			uptime = fmt.Sprintf("%.1f%%", u)
		}
		verdict := "eligible"
		if r.Picked {
			verdict = "PICKED"
		} else if !r.Eligible {
			verdict = r.Why
		}
		fmt.Fprintf(w, "%s %-28s %-14s %-7s %9s %9s %9s %9s %9d %8s  %s\n", mark, cut(r.Tag, 28), cut(r.Provider, 14),
			r.Quantization, fmt.Sprintf("$%.4g", r.Input), fmt.Sprintf("$%.4g", r.Output), cache,
			fmt.Sprintf("$%.4g", r.Blended()), r.Context, uptime, verdict)
	}
	if len(ranked) == 0 || !ranked[0].Eligible {
		fmt.Fprintf(w, "no endpoint satisfies the policy: an OpenRouter job on %s would be refused at start\n", slug)
	}
	fmt.Fprintln(w, "data_collection and zdr are not in the endpoints list: OpenRouter enforces them per request.")
}

func cut(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
