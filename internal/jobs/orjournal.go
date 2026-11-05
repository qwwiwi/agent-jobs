package jobs

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// The OpenRouter cost journal: <state>/openrouter-costs.jsonl (the agent's state dir, next to jobs.db),
// one JSON line per attempt of an OpenRouter job, appended by the wrapper when the attempt ends (it
// succeeded, failed, hit its budget, or the daemon killed it). A line carries the proxy's own figures
// (requests, tokens, cost) side by side with OpenRouter's billed record of the same requests, read
// from its generation endpoint by their generation ids. The journal is append-only: when OpenRouter's
// record was not available at the end of the attempt, `agent-jobs openrouter costs --reconcile`
// appends a reconcile line for that attempt, which the reader merges into it. Neither the key, the
// proxy token, a prompt nor a response text is ever written.

// OpenRouterCostsFile is the journal's file name in the agent's state dir.
const OpenRouterCostsFile = "openrouter-costs.jsonl"

// The journal's line types.
const (
	ORCostRun       = "run"
	ORCostReconcile = "reconcile"
)

// ORStatusKilled is the journal status of an attempt the daemon ended (cancel, stop or timeout): the
// wrapper got SIGTERM. The CLI shows the job row's final status instead when it has one.
const ORStatusKilled = "killed"

// ORBilled is OpenRouter's own record of an attempt's requests: the sum of their generations.
type ORBilled struct {
	// CostUSD is the sum of the generations' total_cost; nil when one of them could not be read.
	CostUSD *float64 `json:"cost_usd"`
	// Reason says why CostUSD is nil (empty when it is known).
	Reason string `json:"reason,omitempty"`
	// Generations is how many generation ids were looked up; Found how many were read.
	Generations int `json:"generations"`
	Found       int `json:"found"`
	// Providers and Models are the distinct provider_name and model values OpenRouter reports.
	Providers []string `json:"providers,omitempty"`
	Models    []string `json:"models,omitempty"`
	// The native token counts (the provider's own tokenizer) of the generations read.
	NativeTokensPrompt     int64 `json:"native_tokens_prompt"`
	NativeTokensCompletion int64 `json:"native_tokens_completion"`
	NativeTokensCached     int64 `json:"native_tokens_cached"` // the record's native_tokens_cached
	// CheckedAt is when the lookup ran (UTC, RFC 3339); empty when it did not run.
	CheckedAt string `json:"checked_at,omitempty"`
}

// ORCostLine is one line of the journal. A run line has every field; a reconcile line only Type,
// ReconciledAt, JobID, Attempt and OpenRouter.
type ORCostLine struct {
	Type         string `json:"type"`
	TSEnd        string `json:"ts_end,omitempty"` // when the attempt ended (UTC, RFC 3339)
	ReconciledAt string `json:"ts,omitempty"`     // a reconcile line's time
	JobID        string `json:"job_id"`
	Attempt      int    `json:"attempt"`
	Title        string `json:"title,omitempty"`
	Kind         string `json:"kind,omitempty"`
	Repo         string `json:"repo,omitempty"`     // the repository's folder name
	Model        string `json:"model,omitempty"`    // the OpenRouter slug
	Endpoint     string `json:"endpoint,omitempty"` // the endpoint the routing policy picked
	// Providers are the providers the responses named (when they name one).
	Providers  []string `json:"providers,omitempty"`
	Requests   int      `json:"requests"`
	TokensIn   int64    `json:"tokens_in"`
	TokensOut  int64    `json:"tokens_out"`
	CacheRead  int64    `json:"cache_read"`
	CacheWrite int64    `json:"cache_write"`
	// CostUSD is the proxy's cost (OpenRouter's usage.cost of each response, else its tokens at the plan's
	// price); nil with CostUnknown when a request ended without its final usage (it is charged at its
	// reservation: CostReservedUSD).
	CostUSD         *float64 `json:"cost_usd"`
	CostUnknown     bool     `json:"cost_unknown"`
	CostReservedUSD *float64 `json:"cost_reserved_usd,omitempty"`
	MaxBudgetUSD    float64  `json:"max_budget_usd"`
	// Status is the wrapper's verdict (succeeded / failed, as the daemon's Verdict maps the result), or
	// ORStatusKilled; Error its reason.
	Status        string    `json:"status,omitempty"`
	Error         string    `json:"error,omitempty"`
	GenerationIDs []string  `json:"generation_ids"`
	OpenRouter    *ORBilled `json:"openrouter"`
}

// orcostsMaxLine caps one journal line the reader accepts.
const orcostsMaxLine = 4 << 20

// appendORCostLine appends v as one JSON line to path (created mode 600), in one write on an O_APPEND
// file. A line that carries one of secrets is refused.
func appendORCostLine(path string, v any, secrets ...string) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	for _, s := range secrets {
		if s != "" && bytes.Contains(b, []byte(s)) {
			return errors.New("the line would carry a secret: not written")
		}
	}
	b = append(b, '\n')
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Mode().Perm() != 0o600 {
		if err := f.Chmod(0o600); err != nil {
			return err
		}
	}
	n, err := f.Write(b)
	if err == nil && n != len(b) {
		err = io.ErrShortWrite
	}
	return err
}

// orJournal is the wrapper's journal of one OpenRouter attempt: its fixed fields, and the one line it
// writes when the attempt ends (end runs at most once, whichever path ends the attempt first).
type orJournal struct {
	path string
	base ORCostLine
	log  io.Writer
	once sync.Once
	// caughtElsewhere: another handler of the wrapper owns SIGTERM (the acceptance step), so onKill's
	// handler writes the line without ending the wrapper.
	caughtElsewhere atomic.Bool
}

// stopsCaughtElsewhere marks (on) or clears the span in which another handler owns SIGTERM. A nil
// journal (not an OpenRouter job) does nothing.
func (jr *orJournal) stopsCaughtElsewhere(on bool) {
	if jr != nil {
		jr.caughtElsewhere.Store(on)
	}
}

// newORJournal is the journal of j's attempt in the agent's state dir (<state>/jobs/<id> = jobDir).
func newORJournal(jobDir string, j *store.Job, log io.Writer) *orJournal {
	abs, err := filepath.Abs(jobDir)
	if err != nil {
		abs = jobDir
	}
	slug, _ := OpenRouterSlug(j.Model)
	repo := ""
	if j.RepoPath != "" {
		repo = filepath.Base(j.RepoPath)
	}
	return &orJournal{path: filepath.Join(filepath.Dir(filepath.Dir(abs)), OpenRouterCostsFile), log: log,
		base: ORCostLine{Type: ORCostRun, JobID: j.ID, Attempt: j.Attempt, Title: Printable(j.Title, 200), Kind: j.Kind,
			Repo: Printable(repo, 200), Model: slug, MaxBudgetUSD: j.MaxBudgetUSD}}
}

// end writes the attempt's line once: the proxy's figures (orj nil: the job failed before its proxy
// started, nothing was sent), status and errText, and OpenRouter's record (billed; nil = not looked
// up). A failure is logged, never the job's.
func (jr *orJournal) end(orj *openRouterJob, status, errText string, billed *ORBilled) {
	if jr == nil {
		return // not an OpenRouter job
	}
	jr.once.Do(func() {
		l := jr.base
		l.TSEnd = time.Now().UTC().Format(time.RFC3339)
		l.Status, l.GenerationIDs = status, []string{}
		var secrets []string
		if orj != nil {
			secrets = []string{orj.proxy.key, orj.proxy.token}
			for _, s := range secrets {
				errText = scrubKey(errText, s)
			}
			l.Endpoint = orj.endpoint
			orj.proxy.fill(&l)
		} else {
			zero := 0.0
			l.CostUSD = &zero
		}
		l.Error = RedactPrintable(errText, 400)
		switch {
		case billed != nil:
			l.OpenRouter = billed
		case l.Requests == 0:
			zero := 0.0
			l.OpenRouter = &ORBilled{CostUSD: &zero}
		default:
			l.OpenRouter = &ORBilled{Generations: len(l.GenerationIDs),
				Reason: "not looked up; run `agent-jobs openrouter costs --reconcile`"}
		}
		if err := appendORCostLine(jr.path, l, secrets...); err != nil {
			fmt.Fprintln(jr.log, "run-wrapper: openrouter cost journal:", RedactPrintable(err.Error(), 400))
		}
	})
}

// onKill makes a SIGTERM to the wrapper (the daemon ending the attempt) write the attempt's line as
// killed before the wrapper dies of the signal as it would have without the handler. The returned
// function removes the handler.
func (jr *orJournal) onKill(orj *openRouterJob) func() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-ch:
				jr.end(orj, ORStatusKilled, "the wrapper got SIGTERM: the daemon ended the attempt (cancel, stop or timeout)",
					&ORBilled{Generations: len(orj.proxy.generationIDs()),
						Reason: "not looked up: the attempt was killed; run `agent-jobs openrouter costs --reconcile`"})
				if jr.caughtElsewhere.Load() {
					continue // the acceptance step ends the wrapper; a later SIGTERM still does
				}
				// Die of the signal, as the wrapper did without this handler.
				signal.Reset(syscall.SIGTERM)
				_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
				return
			case <-done:
				return
			}
		}
	}()
	var stop sync.Once
	return func() {
		stop.Do(func() {
			signal.Stop(ch)
			close(done)
		})
	}
}

// generationLookupWait bounds the lookup of an attempt's generations at its end: OpenRouter's record
// can lag the response by a few seconds; what is still missing then is left to --reconcile.
var generationLookupWait = 20 * time.Second

// generationRetryDelay is the first wait before a generation not found yet is asked for again; each
// further wait doubles, up to generationRetryGrowth times the first.
var generationRetryDelay = 2 * time.Second

// generationRetryGrowth caps the backoff of a generation lookup: OpenRouter's record usually lags the
// response by a few seconds, so the waits stay short enough for several tries within the window.
const generationRetryGrowth = 3

// generationLookupWorkers bounds the generation requests in flight at once.
const generationLookupWorkers = 4

// maxGenerationBytes caps a generation or key response read.
const maxGenerationBytes = 1 << 20

// errGenerationNotFound: OpenRouter has no record of the generation (yet).
var errGenerationNotFound = errors.New("not found (yet)")

// generationData is the part of OpenRouter's generation record the journal keeps.
type generationData struct {
	TotalCost              *float64 `json:"total_cost"`
	Model                  string   `json:"model"`
	ProviderName           string   `json:"provider_name"`
	NativeTokensPrompt     *int64   `json:"native_tokens_prompt"`
	NativeTokensCompletion *int64   `json:"native_tokens_completion"`
	NativeTokensCached     *int64   `json:"native_tokens_cached"`
}

// orStatusError is an OpenRouter answer other than 200 (and 404): its status and the message of its
// error object, when it has one.
type orStatusError struct {
	url     string
	status  int
	message string
}

func (e *orStatusError) Error() string {
	if e.message == "" {
		return fmt.Sprintf("get %s: HTTP %d", e.url, e.status)
	}
	return fmt.Sprintf("get %s: HTTP %d: %s", e.url, e.status, e.message)
}

// maxORErrorMessage caps the error message kept from an OpenRouter error body.
const maxORErrorMessage = 200

// orErrorMessage is the message of an OpenRouter error body ({"error":{"message":...}}), "" when it has
// none; printable and capped.
func orErrorMessage(b []byte) string {
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &body) != nil {
		return ""
	}
	return Printable(body.Error.Message, maxORErrorMessage)
}

// orGet reads base+path with the key and decodes its {"data": ...} into out. HTTP 404 is
// errGenerationNotFound, another status an *orStatusError; an error never carries the key.
func orGet(ctx context.Context, client *http.Client, base, key, path string, out any) (retry bool, err error) {
	u := strings.TrimRight(base, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, errors.New(scrubKey(err.Error(), key))
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return true, errors.New(scrubKey(err.Error(), key))
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxGenerationBytes+1))
	switch {
	case err != nil:
		return true, fmt.Errorf("read %s: %s", u, scrubKey(err.Error(), key))
	case resp.StatusCode == http.StatusNotFound:
		return true, errGenerationNotFound
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
		return true, &orStatusError{u, resp.StatusCode, scrubKey(orErrorMessage(b), key)}
	case resp.StatusCode != http.StatusOK:
		return false, &orStatusError{u, resp.StatusCode, scrubKey(orErrorMessage(b), key)}
	case len(b) > maxGenerationBytes:
		return false, fmt.Errorf("get %s: more than %d bytes", u, maxGenerationBytes)
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(b, &env); err != nil || len(env.Data) == 0 || string(env.Data) == "null" {
		return false, fmt.Errorf("get %s: no data object", u)
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return false, fmt.Errorf("get %s: %v", u, err)
	}
	return false, nil
}

// fetchGeneration reads one generation's record, asking again while it is not found yet (404: its
// record lags the response) or OpenRouter answers 5xx / 429, first after retry, each wait doubling up to
// generationRetryGrowth times retry, until ctx ends. A 400 is permanent: the id is not a generation id.
func fetchGeneration(ctx context.Context, client *http.Client, base, key, id string, retry time.Duration) (generationData, error) {
	delay, maxDelay := retry, retry*generationRetryGrowth
	for {
		var d generationData
		again, err := orGet(ctx, client, base, key, "/v1/generation?id="+url.QueryEscape(id), &d)
		if err == nil {
			if d.TotalCost == nil || math.IsNaN(*d.TotalCost) || math.IsInf(*d.TotalCost, 0) || *d.TotalCost < 0 {
				return d, errors.New("the record has no usable total_cost")
			}
			return d, nil
		}
		var se *orStatusError
		if errors.As(err, &se) && se.status == http.StatusBadRequest {
			return d, fmt.Errorf("%w (not a generation id: OpenRouter refuses it)", err)
		}
		if !again {
			return d, err
		}
		select {
		case <-ctx.Done():
			return d, err
		case <-time.After(delay):
		}
		delay = min(delay*2, maxDelay)
	}
}

// LookupGenerations reads OpenRouter's record of each generation id (in base, with key; within ctx)
// and sums them. The sum is known only when every generation was read.
func LookupGenerations(ctx context.Context, client *http.Client, base, key string, ids []string, retry time.Duration) ORBilled {
	out := ORBilled{Generations: len(ids), CheckedAt: time.Now().UTC().Format(time.RFC3339)}
	if len(ids) == 0 {
		out.Reason = "no generation id in the responses"
		return out
	}
	type res struct {
		d   generationData
		err error
	}
	results := make([]res, len(ids))
	sem := make(chan struct{}, generationLookupWorkers)
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			d, err := fetchGeneration(ctx, client, base, key, id, retry)
			results[i] = res{d, err}
		}(i, id)
	}
	wg.Wait()
	var sum float64
	providers, models := map[string]bool{}, map[string]bool{}
	var firstErr error
	for _, r := range results {
		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
			}
			continue
		}
		out.Found++
		sum += *r.d.TotalCost
		if r.d.ProviderName != "" {
			providers[r.d.ProviderName] = true
		}
		if r.d.Model != "" {
			models[r.d.Model] = true
		}
		if r.d.NativeTokensPrompt != nil {
			out.NativeTokensPrompt += *r.d.NativeTokensPrompt
		}
		if r.d.NativeTokensCompletion != nil {
			out.NativeTokensCompletion += *r.d.NativeTokensCompletion
		}
		if r.d.NativeTokensCached != nil {
			out.NativeTokensCached += *r.d.NativeTokensCached
		}
	}
	out.Providers, out.Models = sortedKeys(providers), sortedKeys(models)
	if out.Found == len(ids) {
		out.CostUSD = &sum
		return out
	}
	out.Reason = RedactPrintable(fmt.Sprintf("%d of %d generation(s) not read (%v); run `agent-jobs openrouter costs --reconcile`",
		len(ids)-out.Found, len(ids), firstErr), 400)
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// lookup reads OpenRouter's record of the attempt's generations, bounded by generationLookupWait.
func (o *openRouterJob) lookup() *ORBilled {
	ids := o.proxy.generationIDs()
	if len(ids) == 0 && o.proxy.requestCount() == 0 {
		zero := 0.0
		return &ORBilled{CostUSD: &zero, CheckedAt: time.Now().UTC().Format(time.RFC3339)}
	}
	ctx, cancel := context.WithTimeout(context.Background(), generationLookupWait)
	defer cancel()
	b := LookupGenerations(ctx, o.proxy.client, o.proxy.upstream, o.proxy.key, ids, generationRetryDelay)
	return &b
}

// ORKeyUsage is OpenRouter's figures of the key (GET /api/v1/key): what it spent and its limit.
type ORKeyUsage struct {
	Usage          *float64 `json:"usage"`
	Limit          *float64 `json:"limit"`
	LimitRemaining *float64 `json:"limit_remaining"`
}

// FetchORKeyUsage reads the key's usage and limit from base. An error never carries the key.
func FetchORKeyUsage(ctx context.Context, client *http.Client, base, key string) (ORKeyUsage, error) {
	var k ORKeyUsage
	_, err := orGet(ctx, client, base, key, "/v1/key", &k)
	return k, err
}

// ORCostRunView is a run line with its reconcile lines merged (the latest complete OpenRouter record).
type ORCostRunView struct {
	ORCostLine
	End time.Time `json:"-"`
	// FinalStatus is the job row's status when it is terminal and of this attempt, else the line's.
	FinalStatus string `json:"final_status"`
	// Mismatch: the proxy's and OpenRouter's costs differ by more than ORCostMismatch of OpenRouter's.
	Mismatch bool `json:"mismatch"`
}

// ORCostMismatch is the relative difference between the proxy's and OpenRouter's cost of a run that
// is flagged.
const ORCostMismatch = 0.10

// ReadORCosts reads the journal at path: its run lines, oldest first, each with the latest reconcile
// line of the same job and attempt merged into it, and how many lines were malformed (skipped). A
// missing journal is empty.
func ReadORCosts(path string) ([]ORCostRunView, int, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	var runs []ORCostRunView
	reconciled := map[string]*ORBilled{}
	malformed := 0
	rd := bufio.NewReaderSize(f, 64<<10)
	for {
		line, tooLong, err := readJournalLine(rd)
		if tooLong || len(bytes.TrimSpace(line)) > 0 {
			var l ORCostLine
			if tooLong || json.Unmarshal(line, &l) != nil || l.JobID == "" {
				malformed++
			} else {
				switch l.Type {
				case ORCostRun:
					end, perr := time.Parse(time.RFC3339, l.TSEnd)
					if perr != nil {
						malformed++
						break
					}
					runs = append(runs, ORCostRunView{ORCostLine: l, End: end})
				case ORCostReconcile:
					if l.OpenRouter != nil && l.OpenRouter.CostUSD != nil {
						reconciled[orRunKey(l.JobID, l.Attempt)] = l.OpenRouter
					}
				default:
					malformed++
				}
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, malformed, err
		}
	}
	for i := range runs {
		r := &runs[i]
		if b := reconciled[orRunKey(r.JobID, r.Attempt)]; b != nil && (r.OpenRouter == nil || r.OpenRouter.CostUSD == nil) {
			r.OpenRouter = b
		}
		r.FinalStatus = r.Status
		r.Mismatch = orMismatch(r.CostUSD, r.OpenRouter)
	}
	sort.SliceStable(runs, func(a, b int) bool { return runs[a].End.Before(runs[b].End) })
	return runs, malformed, nil
}

// readJournalLine reads one line; one longer than orcostsMaxLine is consumed and reported tooLong.
func readJournalLine(rd *bufio.Reader) (line []byte, tooLong bool, err error) {
	var buf []byte
	over := false
	for {
		var chunk []byte
		chunk, err = rd.ReadSlice('\n')
		if !over {
			if len(buf)+len(chunk) > orcostsMaxLine {
				over, buf = true, nil
			} else {
				buf = append(buf, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return buf, over, err
	}
}

func orRunKey(job string, attempt int) string { return fmt.Sprintf("%s#%d", job, attempt) }

// orMismatch reports whether the proxy's cost and OpenRouter's differ by more than ORCostMismatch.
func orMismatch(proxy *float64, billed *ORBilled) bool {
	if proxy == nil || billed == nil || billed.CostUSD == nil {
		return false
	}
	p, o := *proxy, *billed.CostUSD
	if o == 0 {
		return p > 1e-9
	}
	return math.Abs(p-o) > ORCostMismatch*o
}

// ORReconcile is a reconcile line for an attempt.
type ORReconcile struct {
	Type       string    `json:"type"`
	TS         string    `json:"ts"`
	JobID      string    `json:"job_id"`
	Attempt    int       `json:"attempt"`
	OpenRouter *ORBilled `json:"openrouter"`
}

// AppendORReconcile appends OpenRouter's record of a run to the journal at path.
func AppendORReconcile(path, jobID string, attempt int, billed ORBilled, key string) error {
	return appendORCostLine(path, ORReconcile{Type: ORCostReconcile, TS: time.Now().UTC().Format(time.RFC3339),
		JobID: jobID, Attempt: attempt, OpenRouter: &billed}, key)
}

// ORCostTotal is the sum of a group of runs.
type ORCostTotal struct {
	Key  string `json:"key,omitempty"`
	Runs int    `json:"runs"`
	// ProxyUSD sums the runs' known proxy costs; ProxyUnknown counts the runs without one.
	ProxyUSD     float64 `json:"proxy_usd"`
	ProxyUnknown int     `json:"proxy_unknown"`
	// OpenRouterUSD sums OpenRouter's billed costs; OpenRouterMissing counts the runs without one.
	OpenRouterUSD     float64 `json:"openrouter_usd"`
	OpenRouterMissing int     `json:"openrouter_missing"`
}

func (t *ORCostTotal) add(r ORCostRunView) {
	t.Runs++
	if r.CostUSD != nil {
		t.ProxyUSD += *r.CostUSD
	} else {
		t.ProxyUnknown++
	}
	if r.OpenRouter != nil && r.OpenRouter.CostUSD != nil {
		t.OpenRouterUSD += *r.OpenRouter.CostUSD
	} else {
		t.OpenRouterMissing++
	}
}

// ORCostSummary is what `agent-jobs openrouter costs` shows.
type ORCostSummary struct {
	Days     int             `json:"days"`
	Runs     []ORCostRunView `json:"runs"`
	PerDay   []ORCostTotal   `json:"per_day"`
	PerModel []ORCostTotal   `json:"per_model"`
	// Total30d is the last 30 days whatever Days is.
	Total30d  ORCostTotal `json:"total_30d"`
	Malformed int         `json:"malformed_lines"`
}

// SummarizeORCosts groups the runs of the last days days (UTC days) and totals the last 30.
func SummarizeORCosts(runs []ORCostRunView, malformed int, now time.Time, days int) ORCostSummary {
	s := ORCostSummary{Days: days, Runs: []ORCostRunView{}, PerDay: []ORCostTotal{}, PerModel: []ORCostTotal{}, Malformed: malformed}
	since := now.Add(-time.Duration(days) * 24 * time.Hour)
	since30 := now.Add(-30 * 24 * time.Hour)
	perDay, perModel := map[string]*ORCostTotal{}, map[string]*ORCostTotal{}
	for _, r := range runs {
		if !r.End.Before(since30) {
			s.Total30d.add(r)
		}
		if r.End.Before(since) {
			continue
		}
		s.Runs = append(s.Runs, r)
		day := r.End.UTC().Format("2006-01-02")
		if perDay[day] == nil {
			perDay[day] = &ORCostTotal{Key: day}
		}
		perDay[day].add(r)
		if perModel[r.Model] == nil {
			perModel[r.Model] = &ORCostTotal{Key: r.Model}
		}
		perModel[r.Model].add(r)
	}
	for _, k := range sortedKeys(boolKeys(perDay)) {
		s.PerDay = append(s.PerDay, *perDay[k])
	}
	for _, k := range sortedKeys(boolKeys(perModel)) {
		s.PerModel = append(s.PerModel, *perModel[k])
	}
	return s
}

func boolKeys(m map[string]*ORCostTotal) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

// usdText is a cost for the table: "unknown" for nil.
func usdText(v *float64) string {
	if v == nil {
		return "unknown"
	}
	return fmt.Sprintf("$%.4f", *v)
}

// FormatORCosts writes the summary for people.
func FormatORCosts(w io.Writer, s ORCostSummary) {
	fmt.Fprintf(w, "runs of the last %d day(s):\n", s.Days)
	if len(s.Runs) == 0 {
		fmt.Fprintln(w, "  none")
	}
	for _, r := range s.Runs {
		billed := "unknown"
		if r.OpenRouter != nil && r.OpenRouter.CostUSD != nil {
			billed = usdText(r.OpenRouter.CostUSD)
		}
		flag := ""
		if r.Mismatch {
			flag = fmt.Sprintf("  MISMATCH >%d%%", int(ORCostMismatch*100))
		}
		endpoint := r.Endpoint
		if endpoint == "" {
			endpoint = "-"
		}
		fmt.Fprintf(w, "  %s  %s  %q  %s  endpoint %s  tokens in %d / out %d / cache read %d / cache write %d  proxy %s  openrouter %s  %s%s\n",
			r.End.UTC().Format("2006-01-02 15:04"), r.JobID, Printable(r.Title, 60), r.Model, endpoint,
			r.TokensIn, r.TokensOut, r.CacheRead, r.CacheWrite, usdText(r.CostUSD), billed, r.FinalStatus, flag)
	}
	total := func(t ORCostTotal) string {
		out := fmt.Sprintf("%d run(s)  proxy $%.4f  openrouter $%.4f", t.Runs, t.ProxyUSD, t.OpenRouterUSD)
		if t.ProxyUnknown > 0 {
			out += fmt.Sprintf("  (%d without a known proxy cost)", t.ProxyUnknown)
		}
		if t.OpenRouterMissing > 0 {
			out += fmt.Sprintf("  (%d without OpenRouter's figure)", t.OpenRouterMissing)
		}
		return out
	}
	fmt.Fprintln(w, "per day (UTC):")
	for _, t := range s.PerDay {
		fmt.Fprintf(w, "  %s  %s\n", t.Key, total(t))
	}
	fmt.Fprintln(w, "per model:")
	for _, t := range s.PerModel {
		fmt.Fprintf(w, "  %s  %s\n", t.Key, total(t))
	}
	fmt.Fprintf(w, "last 30 days: %s\n", total(s.Total30d))
	if s.Malformed > 0 {
		fmt.Fprintf(w, "skipped %d malformed line(s)\n", s.Malformed)
	}
}

// FormatORKeyUsage is the header line of the key's own figures.
func FormatORKeyUsage(k ORKeyUsage) string {
	limit, remaining := "none", "n/a"
	if k.Limit != nil {
		limit = fmt.Sprintf("$%.2f", *k.Limit)
	}
	if k.LimitRemaining != nil {
		remaining = fmt.Sprintf("$%.2f", *k.LimitRemaining)
	}
	return fmt.Sprintf("openrouter key: usage %s, limit %s, remaining %s", usdText(k.Usage), limit, remaining)
}
