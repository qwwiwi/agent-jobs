// Package httpapi is the read-only HTTP API for a jobs dashboard (DEVIATIONS #8). It runs inside the
// daemon, off by default, on a loopback address or a unix socket only, and answers GET requests that
// carry the agent's bearer token (<state>/http-api.token). No endpoint changes state; briefs, prompts,
// env, config and paths outside the agent state are never returned.
package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/mcp"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// Limits of the API.
const (
	// MaxReportBytes caps report.md in /v1/jobs/{id}?report=1; the full file stays in the job dir.
	MaxReportBytes = 64 << 10
	DefaultLimit   = 20
	MaxLimit       = 200
	// RatePerSec and RateBurst are the token bucket of one client (remote IP, or the unix socket).
	RatePerSec = 10
	RateBurst  = 10
	// maxTokenBytes bounds the token file read on every request.
	maxTokenBytes = 4096
	maxErrorRunes = 500
)

// Server timeouts and header cap.
const (
	ReadHeaderTimeout = 5 * time.Second
	ReadTimeout       = 10 * time.Second
	WriteTimeout      = 30 * time.Second
	IdleTimeout       = 60 * time.Second
	MaxHeaderBytes    = 8 << 10
)

// ErrDisabled is returned by Serve when http_api.enabled is false.
var ErrDisabled = errors.New("http api: disabled (http_api.enabled is false)")

// TokenPath is the bearer token file of an agent: <state>/http-api.token.
func TokenPath(stateDir string) string { return filepath.Join(stateDir, "http-api.token") }

// RotateToken writes a new random token to <state>/http-api.token (mode 600, atomic rename) and
// returns it. The old token stops working on the next request.
func RotateToken(stateDir string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("http api token: %w", err)
	}
	tok := hex.EncodeToString(b)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return "", err
	}
	p := TokenPath(stateDir)
	tmp := p + ".new"
	_ = os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("http api token: %w", err)
	}
	if _, err := io.WriteString(f, tok+"\n"); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return "", fmt.Errorf("http api token: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("http api token: %w", err)
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("http api token: %w", err)
	}
	return tok, nil
}

// readToken reads the token file without following a symlink and refuses one that is not a regular
// file or that the group or others can access.
func readToken(path string) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", filepath.Base(path))
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s has mode %v; want 600 (agent-jobs http-api token --rotate)", filepath.Base(path), fi.Mode().Perm())
	}
	b, err := io.ReadAll(io.LimitReader(f, maxTokenBytes))
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", fmt.Errorf("%s is empty", filepath.Base(path))
	}
	return tok, nil
}

// API serves the endpoints for one agent's Manager.
type API struct {
	m       *jobs.Manager
	version string
	started time.Time
	limiter *limiter
}

// New returns the API of m; version is reported by /v1/health.
func New(m *jobs.Manager, version string) *API {
	return &API{m: m, version: version, started: m.Now(), limiter: newLimiter(RatePerSec, RateBurst, m.Now)}
}

// NewServer is the http.Server of the API with its timeouts and header cap.
func NewServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: ReadHeaderTimeout,
		ReadTimeout:       ReadTimeout,
		WriteTimeout:      WriteTimeout,
		IdleTimeout:       IdleTimeout,
		MaxHeaderBytes:    MaxHeaderBytes,
	}
}

// Serve runs the API of m until ctx ends: ErrDisabled when http_api is off; an error when the listen
// address is not loopback or a unix socket path, or the token file is missing or open to others.
func Serve(ctx context.Context, m *jobs.Manager, version string) error {
	cfg := m.Cfg.HTTPAPI
	if !cfg.Enabled {
		return ErrDisabled
	}
	listen := cfg.Listen
	if listen == "" {
		listen = jobs.DefaultHTTPAPIListen
	}
	if err := jobs.ValidateHTTPListen(listen); err != nil {
		return fmt.Errorf("http api: %w", err)
	}
	if _, err := readToken(TokenPath(m.StateDir)); err != nil {
		return fmt.Errorf("http api: token: %w (create one with agent-jobs http-api token --rotate)", err)
	}
	ln, err := listenOn(listen)
	if err != nil {
		return fmt.Errorf("http api: %w", err)
	}
	srv := NewServer(New(m, version))
	m.Log.Printf("http api: listening on %s (read-only, bearer token)", listen)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return fmt.Errorf("http api: %w", err)
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		_ = srv.Close()
	}
	<-errc
	return nil
}

// listenOn opens a loopback TCP listener, or a unix socket (mode 600) for an absolute path; a stale
// socket is replaced, a live one or any other file is left alone.
func listenOn(listen string) (net.Listener, error) {
	if !strings.HasPrefix(listen, "/") {
		return net.Listen("tcp", listen)
	}
	if fi, err := os.Lstat(listen); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket; not replacing it", listen)
		}
		if c, err := net.DialTimeout("unix", listen, time.Second); err == nil {
			_ = c.Close()
			return nil, fmt.Errorf("another process serves %s", listen)
		}
		if err := os.Remove(listen); err != nil {
			return nil, fmt.Errorf("remove stale socket: %w", err)
		}
	}
	ln, err := net.Listen("unix", listen)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(listen, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("chmod socket: %w", err)
	}
	return ln, nil
}

// ServeHTTP checks, in order: the client's rate (429), the bearer token (401), the method (405), then
// routes (404 for an unknown path).
func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	if !a.limiter.allow(clientKey(r)) {
		h.Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, "rate limit: at most 10 requests per second")
		return
	}
	if !a.authorized(r) {
		h.Set("WWW-Authenticate", `Bearer realm="agent-jobs"`)
		writeError(w, http.StatusUnauthorized, "missing or wrong bearer token")
		return
	}
	if r.Method != http.MethodGet {
		h.Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "read-only API: GET only")
		return
	}
	switch p := r.URL.Path; {
	case p == "/v1/health":
		a.health(w)
	case p == "/v1/jobs":
		a.listJobs(w, r)
	case strings.HasPrefix(p, "/v1/jobs/"):
		a.getJob(w, r, strings.TrimPrefix(p, "/v1/jobs/"))
	case p == "/v1/stats":
		a.stats(w, r)
	case p == "/v1/schedules":
		a.schedules(w)
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

// authorized compares the Authorization bearer token with the token file in constant time. The file
// is read on every request, so a rotation takes effect at once; a missing or unsafe file fails closed.
func (a *API) authorized(r *http.Request) bool {
	want, err := readToken(TokenPath(a.m.StateDir))
	if err != nil {
		return false
	}
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(got)), []byte(want)) == 1
}

func clientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || host == "" {
		return "unix"
	}
	return host
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// ErrorOutput is the body of every error response.
type ErrorOutput struct {
	Error string `json:"error"`
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, ErrorOutput{Error: msg})
}

// internalError logs the cause and answers without it (the cause may hold paths).
func (a *API) internalError(w http.ResponseWriter, what string, err error) {
	a.m.Log.Printf("http api: %s: %v", what, err)
	writeError(w, http.StatusInternalServerError, what+" failed")
}

// HealthOutput is /v1/health.
type HealthOutput struct {
	Version   string `json:"version"`
	StartedAt string `json:"started_at"`
	UptimeSec int64  `json:"uptime_sec"`
	Running   int    `json:"running"`
	Queued    int    `json:"queued"`
}

func (a *API) health(w http.ResponseWriter) {
	running, err := a.m.Store.ListByStatus(store.StatusRunning)
	if err != nil {
		a.internalError(w, "health", err)
		return
	}
	queued, err := a.m.Store.ListByStatus(store.StatusQueued)
	if err != nil {
		a.internalError(w, "health", err)
		return
	}
	writeJSON(w, http.StatusOK, HealthOutput{Version: a.version, StartedAt: a.started.UTC().Format(time.RFC3339),
		UptimeSec: int64(a.m.Now().Sub(a.started) / time.Second), Running: len(running), Queued: len(queued)})
}

// JobView is one job as the API (and `agent-jobs list --json`) shows it: the job row without its brief,
// session, job dir or repository path (Repo is the repository's folder name only).
type JobView struct {
	ID              string  `json:"id"`
	Title           string  `json:"title"`
	Kind            string  `json:"kind"`
	Status          string  `json:"status"`
	Model           string  `json:"model"`
	Complexity      string  `json:"complexity,omitempty"`
	Runner          string  `json:"runner"`
	ReasoningEffort string  `json:"reasoning_effort,omitempty"`
	Billing         string  `json:"billing"`
	MaxTurns        int     `json:"max_turns"`
	MaxBudgetUSD    float64 `json:"max_budget_usd"`
	TimeoutMinutes  int     `json:"timeout_minutes"`
	Limits          string  `json:"limits,omitempty"`
	Turns           int     `json:"turns"`
	CostUSD         float64 `json:"cost_usd"`
	CostUnknown     bool    `json:"cost_unknown"`
	DurationSec     int     `json:"duration_sec"`
	Repo            string  `json:"repo,omitempty"`
	BaseCommit      string  `json:"base_commit,omitempty"`
	CreatedAt       string  `json:"created_at"`
	StartedAt       string  `json:"started_at,omitempty"`
	FinishedAt      string  `json:"finished_at,omitempty"`
	NotifiedAt      string  `json:"notified_at,omitempty"`
	CleanedAt       string  `json:"cleaned_at,omitempty"`
	OwnerWaits      bool    `json:"owner_waits"`
	ResumedFrom     string  `json:"resumed_from,omitempty"`
	ScheduleID      string  `json:"schedule_id,omitempty"`
	Attempt         int     `json:"attempt"`
	Containment     string  `json:"containment,omitempty"`
	// Error is the job's error with its repository path and the home directory masked.
	Error string `json:"error,omitempty"`
}

func ts(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// repoName is the folder name of a repository path ("" without one).
func repoName(p string) string {
	if p == "" {
		return ""
	}
	return filepath.Base(p)
}

// maskPaths replaces the repository path and the home directory in s, so an error text does not
// carry paths outside the agent state.
func maskPaths(s, repo string) string {
	if repo != "" {
		s = strings.ReplaceAll(s, repo, "<repo>")
	}
	if home, err := os.UserHomeDir(); err == nil && len(home) > 1 {
		s = strings.ReplaceAll(s, home, "~")
	}
	return jobs.Printable(s, maxErrorRunes)
}

// ViewOf is the API view of a job row.
func ViewOf(j *store.Job) JobView {
	v := JobView{
		ID: j.ID, Title: j.Title, Kind: j.Kind, Status: j.Status, Model: j.Model, Complexity: j.Complexity,
		Runner: j.Runner, ReasoningEffort: j.ReasoningEffort, Billing: j.Billing, MaxTurns: j.MaxTurns,
		MaxBudgetUSD: j.MaxBudgetUSD, TimeoutMinutes: j.TimeoutMinutes, Turns: j.Turns, CostUSD: j.CostUSD,
		CostUnknown: j.CostIsUnknown(), DurationSec: j.DurationSec, Repo: repoName(j.RepoPath),
		BaseCommit: j.BaseCommit, CreatedAt: ts(&j.CreatedAt), StartedAt: ts(j.StartedAt),
		FinishedAt: ts(j.FinishedAt), NotifiedAt: ts(j.NotifiedAt), CleanedAt: ts(j.CleanedAt),
		OwnerWaits: j.OwnerWaits, ResumedFrom: j.ResumedFrom, ScheduleID: j.ScheduleID, Attempt: j.Attempt,
		Containment: jobs.Containment(j),
	}
	if j.Runner == store.RunnerCodex {
		v.Limits = jobs.CodexLimitsNA
	}
	if j.Error != "" {
		v.Error = maskPaths(j.Error, j.RepoPath)
	}
	return v
}

// JobsOutput is /v1/jobs.
type JobsOutput struct {
	Jobs []JobView `json:"jobs"`
}

// statuses are the values of the status filter.
var statuses = []string{store.StatusQueued, store.StatusRunning, store.StatusSucceeded, store.StatusFailed,
	store.StatusTimeout, store.StatusCancelled, store.StatusStopped}

// errBadRequest marks an invalid filter (400, the message is shown).
var errBadRequest = errors.New("bad request")

// ListJobs is /v1/jobs and `agent-jobs list --json`: the newest jobs of this agent first, optionally
// of one status.
func ListJobs(m *jobs.Manager, status string, limit int) (JobsOutput, error) {
	if status != "" && !slices.Contains(statuses, status) {
		return JobsOutput{}, fmt.Errorf("%w: status %q must be one of %s", errBadRequest, status, strings.Join(statuses, "|"))
	}
	if limit < 1 || limit > MaxLimit {
		return JobsOutput{}, fmt.Errorf("%w: limit %d must be 1..%d", errBadRequest, limit, MaxLimit)
	}
	var list []*store.Job
	var err error
	if status == "" {
		list, err = m.Store.List(limit)
	} else {
		list, err = m.Store.ListRecentByStatus(status, limit)
	}
	if err != nil {
		return JobsOutput{}, err
	}
	out := JobsOutput{Jobs: make([]JobView, 0, len(list))}
	for _, j := range list {
		if j.Agent == m.Agent {
			out.Jobs = append(out.Jobs, ViewOf(j))
		}
	}
	return out, nil
}

func (a *API) listJobs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := DefaultLimit
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("limit must be 1..%d", MaxLimit))
			return
		}
		limit = n
	}
	out, err := ListJobs(a.m, q.Get("status"), limit)
	switch {
	case errors.Is(err, errBadRequest):
		writeError(w, http.StatusBadRequest, err.Error())
	case err != nil:
		a.internalError(w, "list jobs", err)
	default:
		writeJSON(w, http.StatusOK, out)
	}
}

// JobOutput is /v1/jobs/{id}; the report fields are set only with ?report=1.
type JobOutput struct {
	JobView
	Report          string `json:"report,omitempty"`
	ReportTruncated bool   `json:"report_truncated,omitempty"`
	ReportMissing   bool   `json:"report_missing,omitempty"`
	ReportError     string `json:"report_error,omitempty"`
}

var reJobID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,63}$`)

func (a *API) getJob(w http.ResponseWriter, r *http.Request, id string) {
	if !reJobID.MatchString(id) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	j, err := a.m.Store.Get(id)
	if errors.Is(err, store.ErrNotFound) || err == nil && j.Agent != a.m.Agent {
		writeError(w, http.StatusNotFound, fmt.Sprintf("job %q not found", id))
		return
	}
	if err != nil {
		a.internalError(w, "get job", err)
		return
	}
	out := JobOutput{JobView: ViewOf(j)}
	switch rep := r.URL.Query().Get("report"); rep {
	case "", "0":
	case "1":
		text, truncated, err := mcp.ReadCapped(filepath.Join(a.m.JobDir(j.ID), "report.md"), MaxReportBytes)
		switch {
		case errors.Is(err, os.ErrNotExist):
			out.ReportMissing = true
		case err != nil:
			// The error names the job dir; the dashboard only needs to know the report is not readable.
			out.ReportError = "report.md is not readable (not a regular file or no access)"
		default:
			out.Report, out.ReportTruncated = text, truncated
		}
	default:
		writeError(w, http.StatusBadRequest, "report must be 0 or 1")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) stats(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	since, err := jobs.ParseSince(q.Get("since"), a.m.Now())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	by := q.Get("by")
	if by != "" && !slices.Contains([]string{"kind", "model", "skill"}, by) {
		writeError(w, http.StatusBadRequest, "by must be kind|model|skill")
		return
	}
	rep, err := a.m.Stats(since, by)
	if err != nil {
		a.internalError(w, "stats", err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// ScheduleView is one schedule without its prompt (a schedule's prompt is a brief) or repository path.
type ScheduleView struct {
	ID              int64   `json:"id"`
	Cron            string  `json:"cron"`
	Enabled         bool    `json:"enabled"`
	Kind            string  `json:"kind,omitempty"`
	Title           string  `json:"title,omitempty"`
	Model           string  `json:"model,omitempty"`
	Complexity      string  `json:"complexity,omitempty"`
	Runner          string  `json:"runner,omitempty"`
	ReasoningEffort string  `json:"reasoning_effort,omitempty"`
	MaxTurns        int     `json:"max_turns,omitempty"`
	MaxBudgetUSD    float64 `json:"max_budget_usd,omitempty"`
	TimeoutMinutes  int     `json:"timeout_minutes,omitempty"`
	Repo            string  `json:"repo,omitempty"`
	CreatedAt       string  `json:"created_at"`
	UpdatedAt       string  `json:"updated_at"`
	LastFiredAt     string  `json:"last_fired_at,omitempty"`
	LastJobID       string  `json:"last_job_id,omitempty"`
	LastError       string  `json:"last_error,omitempty"`
	NextFireAt      string  `json:"next_fire_at,omitempty"`
}

// SchedulesOutput is /v1/schedules.
type SchedulesOutput struct {
	Schedules []ScheduleView `json:"schedules"`
}

func (a *API) schedules(w http.ResponseWriter) {
	list, err := a.m.ListSchedules()
	if err != nil {
		a.internalError(w, "schedules", err)
		return
	}
	out := SchedulesOutput{Schedules: make([]ScheduleView, 0, len(list))}
	for _, sc := range list {
		v := ScheduleView{
			ID: sc.ID, Cron: sc.Cron, Enabled: sc.Enabled, Kind: sc.Kind, Title: sc.Title, Model: sc.Model,
			Complexity: sc.Complexity, Runner: sc.Runner, ReasoningEffort: sc.ReasoningEffort, MaxTurns: sc.MaxTurns,
			MaxBudgetUSD: sc.MaxBudgetUSD, TimeoutMinutes: sc.TimeoutMinutes, Repo: repoName(sc.RepoPath),
			CreatedAt: ts(&sc.CreatedAt), UpdatedAt: ts(&sc.UpdatedAt), LastFiredAt: ts(sc.LastFiredAt),
			LastJobID: sc.LastJobID,
		}
		if sc.LastError != "" {
			v.LastError = maskPaths(sc.LastError, sc.RepoPath)
		}
		if n := a.m.NextFire(sc); n != nil {
			v.NextFireAt = n.Format(time.RFC3339)
		}
		out.Schedules = append(out.Schedules, v)
	}
	writeJSON(w, http.StatusOK, out)
}

// limiter is a token bucket per client key. Idle buckets are dropped once the map grows.
type limiter struct {
	mu      sync.Mutex
	rate    float64
	burst   float64
	now     func() time.Time
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

const maxBuckets = 1024

func newLimiter(rate, burst int, now func() time.Time) *limiter {
	return &limiter{rate: float64(rate), burst: float64(burst), now: now, buckets: map[string]*bucket{}}
}

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= maxBuckets {
			for k, o := range l.buckets {
				if now.Sub(o.last) > time.Minute {
					delete(l.buckets, k)
				}
			}
			if len(l.buckets) >= maxBuckets {
				return false
			}
		}
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	if el := now.Sub(b.last).Seconds(); el > 0 {
		b.tokens = min(l.burst, b.tokens+el*l.rate)
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
