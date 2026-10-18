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
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

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
	// RatePerSec and RateBurst are the token bucket of one client (remote IP, or the unix socket);
	// authenticated requests and refused (401) ones are counted in separate buckets.
	RatePerSec = 10
	RateBurst  = 10
	// DefaultScheduleLimit and MaxScheduleLimit bound one page of /v1/schedules.
	DefaultScheduleLimit = 100
	MaxScheduleLimit     = 500
	// MaxStatsGroups caps the groups and skills of /v1/stats; DefaultStatsDays is its range without
	// since, MaxStatsDays the oldest since accepted.
	MaxStatsGroups   = 200
	DefaultStatsDays = 30
	MaxStatsDays     = 365
	// MaxFieldBytes caps every string field of a response (titles, keys, names).
	MaxFieldBytes = 200
	// maxTokenBytes bounds the token file read on every request.
	maxTokenBytes = 4096
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
// address is not loopback or a unix socket path, the state dir holding the default socket is not
// 700 and the agent user's, the token file is missing or open to others, or the bind fails. Whether
// it serves (and why not) is recorded in <state>/http-api.status for status and system_doctor.
func Serve(ctx context.Context, m *jobs.Manager, version string) error {
	if !m.Cfg.HTTPAPI.Enabled {
		return ErrDisabled
	}
	err := serve(ctx, m, version)
	reason := "daemon stopped"
	if err != nil {
		reason = err.Error()
	}
	m.RecordHTTPAPI(jobs.HTTPAPIStatus{Reason: reason})
	return err
}

func serve(ctx context.Context, m *jobs.Manager, version string) error {
	listen := m.Cfg.HTTPAPIListen(m.StateDir)
	if err := jobs.ValidateHTTPListen(listen); err != nil {
		return fmt.Errorf("http api: %w", err)
	}
	if listen == jobs.HTTPAPISocket(m.StateDir) {
		if err := checkStateDir(m.StateDir); err != nil {
			return fmt.Errorf("http api: %w", err)
		}
	}
	if _, err := readToken(TokenPath(m.StateDir)); err != nil {
		return fmt.Errorf("http api: token: %w (create one with agent-jobs http-api token --rotate)", err)
	}
	ln, err := listenOn(listen)
	if err != nil {
		return fmt.Errorf("http api: %w", err)
	}
	srv := NewServer(New(m, version))
	bound := ln.Addr().String()
	m.Log.Printf("http api: listening on %s (read-only, bearer token)", bound)
	m.RecordHTTPAPI(jobs.HTTPAPIStatus{Serving: true, Listen: bound})
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

// checkStateDir refuses a state dir that is a symlink, not a directory, open to the group or others,
// or owned by another user: the default socket lives there, and nobody else may create it first.
func checkStateDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("state dir: %w", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("state dir %s is not a directory (%s)", dir, fi.Mode().Type())
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("state dir %s has mode %v; want 700 for the default socket", dir, fi.Mode().Perm())
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || int64(st.Uid) != int64(os.Getuid()) {
		return fmt.Errorf("state dir %s is not owned by uid %d", dir, os.Getuid())
	}
	return nil
}

// listenOn opens a loopback TCP listener, or a unix socket (mode 600, created under umask 077 so it
// is never reachable by others, not even between bind and chmod) for an absolute path; a stale
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
	old := syscall.Umask(0o077)
	ln, err := net.Listen("unix", listen)
	syscall.Umask(old)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(listen, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("chmod socket: %w", err)
	}
	return ln, nil
}

// ServeHTTP checks, in order: the Host header on a TCP listener (421), the bearer token (401, or 429
// once the client's bucket of refused requests is empty), the client's rate (429), the method (405),
// then routes (404 for an unknown path).
func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	if !hostAllowed(r) {
		writeError(w, http.StatusMisdirectedRequest, "host must be localhost or a loopback IP")
		return
	}
	key := clientKey(r)
	if !a.authorized(r) {
		if !a.limiter.allow("unauth " + key) {
			tooMany(w)
			return
		}
		h.Set("WWW-Authenticate", `Bearer realm="agent-jobs"`)
		writeError(w, http.StatusUnauthorized, "missing or wrong bearer token")
		return
	}
	if !a.limiter.allow("auth " + key) {
		tooMany(w)
		return
	}
	if r.Method != http.MethodGet {
		h.Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "read-only API: GET only")
		return
	}
	switch p := r.URL.Path; {
	case p == "/v1/health":
		a.health(w, r)
	case p == "/v1/jobs":
		a.listJobs(w, r)
	case strings.HasPrefix(p, "/v1/jobs/"):
		a.getJob(w, r, strings.TrimPrefix(p, "/v1/jobs/"))
	case p == "/v1/stats":
		a.stats(w, r)
	case p == "/v1/schedules":
		a.schedules(w, r)
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func tooMany(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "1")
	writeError(w, http.StatusTooManyRequests, "rate limit: at most 10 requests per second")
}

// hostAllowed is the DNS-rebinding check: on a TCP listener the Host header must be localhost or a
// loopback IP literal (v4, or v6 in brackets), with no port or the listener's port; no name is
// resolved. A unix socket is not reachable from a browser and is exempt. A request without the
// listener in its context (a handler called directly) is treated as TCP with any port.
func hostAllowed(r *http.Request) bool {
	port := ""
	switch la := r.Context().Value(http.LocalAddrContextKey).(type) {
	case *net.UnixAddr:
		return true
	case *net.TCPAddr:
		port = strconv.Itoa(la.Port)
	}
	return loopbackHost(r.Host, port)
}

// loopbackHost reports whether host ("name", "name:port", "[v6]" or "[v6]:port") names localhost or
// a loopback IP literal, with no port or port (any port when port is "").
func loopbackHost(host, port string) bool {
	var p string
	if rest, ok := strings.CutPrefix(host, "["); ok {
		v6, after, ok := strings.Cut(rest, "]")
		if !ok {
			return false
		}
		if after != "" {
			if p, ok = strings.CutPrefix(after, ":"); !ok || p == "" {
				return false
			}
		}
		if ip := net.ParseIP(v6); ip == nil || ip.To4() != nil || !ip.IsLoopback() {
			return false
		}
	} else {
		name := host
		if i := strings.LastIndexByte(host, ':'); i >= 0 {
			if name, p = host[:i], host[i+1:]; p == "" {
				return false
			}
		}
		if !strings.EqualFold(name, "localhost") {
			// A bare IPv6 literal (a colon left in name) needs brackets.
			if ip := net.ParseIP(name); ip == nil || ip.To4() == nil || strings.Contains(name, ":") || !ip.IsLoopback() {
				return false
			}
		}
	}
	if p == "" {
		return true
	}
	if n, err := strconv.Atoi(p); err != nil || n < 0 || n > 65535 || strconv.Itoa(n) != p {
		return false
	}
	return port == "" || p == port
}

// authorized compares the Authorization bearer token with the token file in constant time. The file
// is read on every request, so a rotation takes effect at once; a missing or unsafe file fails closed.
// The scheme is case-insensitive (RFC 9110: Bearer, bearer).
func (a *API) authorized(r *http.Request) bool {
	want, err := readToken(TokenPath(a.m.StateDir))
	if err != nil {
		return false
	}
	scheme, got, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
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
	writeJSON(w, code, ErrorOutput{Error: capField(msg)})
}

// internalError logs the cause and answers without it (the cause may hold paths).
func (a *API) internalError(w http.ResponseWriter, what string, err error) {
	a.m.Log.Printf("http api: %s: %v", what, err)
	writeError(w, http.StatusInternalServerError, what+" failed")
}

// capField cuts s to MaxFieldBytes on a rune boundary and drops control and format characters.
func capField(s string) string {
	s = jobs.Printable(s, 0)
	if len(s) <= MaxFieldBytes {
		return s
	}
	cut := MaxFieldBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// Error codes of the API. A stored error text (a job's error, a schedule's last error) may carry
// paths, command output or secrets, so the API answers with one of these codes and its fixed message;
// the text itself stays in the daemon log and `agent-jobs status <id>`.
const (
	ErrLaunchFailed       = "launch_failed"
	ErrTimeout            = "timeout"
	ErrCancelled          = "cancelled"
	ErrGuardRefused       = "guard_refused"
	ErrCodexConfigRefused = "codex_config_refused"
	ErrDeliveryFailed     = "delivery_failed"
	ErrProcessesSurvived  = "processes_survived"
	ErrRunFailed          = "run_failed"
	ErrUnknown            = "unknown"
)

// errorMessages are the fixed messages of the error codes.
var errorMessages = map[string]string{
	ErrLaunchFailed:       "the job did not start",
	ErrTimeout:            "the job ran past its timeout",
	ErrCancelled:          "the job was cancelled",
	ErrGuardRefused:       "the guard refused an action of the job",
	ErrCodexConfigRefused: "the config does not allow this codex job",
	ErrDeliveryFailed:     "the schedule was not delivered to the session",
	ErrProcessesSurvived:  "processes outlived the job",
	ErrRunFailed:          "the runner failed or ended without a report",
	ErrUnknown:            "see agent-jobs status or the daemon log",
}

// codexConfigRefused reports a codex job refused by the config (checkCodexJob, reloadCodexConfig).
func codexConfigRefused(text string) bool {
	return strings.Contains(text, "codex_write_allowed") || strings.Contains(text, "codex job not launched")
}

// jobErrorCode classifies a job's stored error text ("" for none).
func jobErrorCode(status, text string) string {
	switch {
	case text == "":
		return ""
	case codexConfigRefused(text):
		return ErrCodexConfigRefused
	case strings.HasPrefix(text, "refused to start") || strings.HasPrefix(text, "launch interrupted"):
		return ErrLaunchFailed
	case status == store.StatusTimeout || strings.HasPrefix(text, "timeout"):
		return ErrTimeout
	case status == store.StatusCancelled || strings.HasPrefix(text, "cancelled"):
		return ErrCancelled
	case strings.Contains(text, jobs.ReasonSurvivors):
		return ErrProcessesSurvived
	case strings.Contains(text, "guard"):
		return ErrGuardRefused
	case strings.Contains(text, "exited with code") || strings.Contains(text, "result is an error") ||
		strings.Contains(text, "without a report"):
		return ErrRunFailed
	}
	return ErrUnknown
}

// scheduleErrorCode classifies a schedule's last error ("" for none): a session schedule's is a
// delivery failure, a job schedule's a job that did not start.
func scheduleErrorCode(sc *store.Schedule) string {
	switch {
	case sc.LastError == "":
		return ""
	case sc.Kind == "":
		return ErrDeliveryFailed
	case codexConfigRefused(sc.LastError):
		return ErrCodexConfigRefused
	}
	return ErrLaunchFailed
}

// HealthOutput is /v1/health.
type HealthOutput struct {
	Version   string `json:"version"`
	StartedAt string `json:"started_at"`
	UptimeSec int64  `json:"uptime_sec"`
	Running   int    `json:"running"`
	Queued    int    `json:"queued"`
}

func (a *API) health(w http.ResponseWriter, r *http.Request) {
	running, err := a.m.Store.CountByStatusContext(r.Context(), a.m.Agent, store.StatusRunning)
	if err != nil {
		a.internalError(w, "health", err)
		return
	}
	queued, err := a.m.Store.CountByStatusContext(r.Context(), a.m.Agent, store.StatusQueued)
	if err != nil {
		a.internalError(w, "health", err)
		return
	}
	writeJSON(w, http.StatusOK, HealthOutput{Version: capField(a.version), StartedAt: a.started.UTC().Format(time.RFC3339),
		UptimeSec: int64(a.m.Now().Sub(a.started) / time.Second), Running: running, Queued: queued})
}

// JobView is one job as the API (and `agent-jobs list --json`) shows it: the job row without its brief,
// session, job dir or repository path (Repo is the repository's folder name only), its error as a code.
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
	// ErrorCode is one of the Err* codes when the job has an error, ErrorMessage its fixed message.
	ErrorCode    string `json:"error_code,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
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
	return capField(filepath.Base(p))
}

// ViewOf is the API view of a job row.
func ViewOf(j *store.Job) JobView {
	v := JobView{
		ID: capField(j.ID), Title: capField(jobs.JobTitle(j)), Kind: capField(j.Kind), Status: capField(j.Status),
		Model: capField(j.Model), Complexity: capField(j.Complexity), Runner: capField(j.Runner),
		ReasoningEffort: capField(j.ReasoningEffort), Billing: capField(j.Billing), MaxTurns: j.MaxTurns,
		MaxBudgetUSD: j.MaxBudgetUSD, TimeoutMinutes: j.TimeoutMinutes, Turns: j.Turns, CostUSD: j.CostUSD,
		CostUnknown: j.CostIsUnknown(), DurationSec: j.DurationSec, Repo: repoName(j.RepoPath),
		BaseCommit: capField(j.BaseCommit), CreatedAt: ts(&j.CreatedAt), StartedAt: ts(j.StartedAt),
		FinishedAt: ts(j.FinishedAt), NotifiedAt: ts(j.NotifiedAt), CleanedAt: ts(j.CleanedAt),
		OwnerWaits: j.OwnerWaits, ResumedFrom: capField(j.ResumedFrom), ScheduleID: capField(j.ScheduleID),
		Attempt: j.Attempt, Containment: capField(jobs.Containment(j)),
	}
	if j.Runner == store.RunnerCodex {
		v.Limits = jobs.CodexLimitsNA
	}
	if code := jobErrorCode(j.Status, j.Error); code != "" {
		v.ErrorCode, v.ErrorMessage = code, errorMessages[code]
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
func ListJobs(ctx context.Context, m *jobs.Manager, status string, limit int) (JobsOutput, error) {
	if status != "" && !slices.Contains(statuses, status) {
		return JobsOutput{}, fmt.Errorf("%w: status must be one of %s", errBadRequest, strings.Join(statuses, "|"))
	}
	if limit < 1 || limit > MaxLimit {
		return JobsOutput{}, fmt.Errorf("%w: limit %d must be 1..%d", errBadRequest, limit, MaxLimit)
	}
	var list []*store.Job
	var err error
	if status == "" {
		list, err = m.Store.ListContext(ctx, limit)
	} else {
		list, err = m.Store.ListRecentByStatusContext(ctx, status, limit)
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

// intParam reads an integer query parameter (def when absent); ok is false when it is not a number.
func intParam(q url.Values, name string, def int) (int, bool) {
	s := q.Get(name)
	if s == "" {
		return def, true
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

func (a *API) listJobs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, ok := intParam(q, "limit", DefaultLimit)
	if !ok {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("limit must be 1..%d", MaxLimit))
		return
	}
	out, err := ListJobs(r.Context(), a.m, q.Get("status"), limit)
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
	j, err := a.m.Store.GetContext(r.Context(), id)
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
		text, truncated, err := mcp.ReadJobReport(a.m.StateDir, j.ID, MaxReportBytes)
		switch {
		case errors.Is(err, os.ErrNotExist):
			out.ReportMissing = true
		case err != nil:
			// The dashboard only needs to know the report is not readable; the cause goes to the log.
			a.m.Log.Printf("http api: job %s: report: %v", j.ID, err)
			out.ReportError = "report.md is not readable (not a regular file of this user, or no access)"
		default:
			out.Report, out.ReportTruncated = text, truncated
		}
	default:
		writeError(w, http.StatusBadRequest, "report must be 0 or 1")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// StatsOutput is /v1/stats: the aggregated report, its groups and skills capped at MaxStatsGroups.
type StatsOutput struct {
	jobs.StatsReport
	GroupsTruncated bool `json:"groups_truncated,omitempty"`
	SkillsTruncated bool `json:"skills_truncated,omitempty"`
}

func (a *API) stats(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	now := a.m.Now()
	s := q.Get("since")
	if s == "" {
		s = strconv.Itoa(DefaultStatsDays) + "d"
	}
	since, err := jobs.ParseSince(s, now)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if since.Before(now.Add(-MaxStatsDays * 24 * time.Hour)) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("since must be within the last %d days", MaxStatsDays))
		return
	}
	by := q.Get("by")
	if by != "" && !slices.Contains(jobs.StatsGroupBy, by) {
		writeError(w, http.StatusBadRequest, "by must be kind|model|skill")
		return
	}
	rep, err := a.m.StatsContext(r.Context(), since, by)
	if err != nil {
		a.internalError(w, "stats", err)
		return
	}
	out := StatsOutput{StatsReport: rep}
	if len(out.Groups) > MaxStatsGroups {
		out.Groups, out.GroupsTruncated = out.Groups[:MaxStatsGroups], true
	}
	if len(out.Skills) > MaxStatsGroups {
		out.Skills, out.SkillsTruncated = out.Skills[:MaxStatsGroups], true
	}
	for i := range out.Groups {
		out.Groups[i].Key = capField(out.Groups[i].Key)
	}
	for i := range out.Skills {
		out.Skills[i].Name = capField(out.Skills[i].Name)
	}
	for _, l := range [][]jobs.NameCount{out.GuardRules, out.Tools} {
		for i := range l {
			l[i].Name = capField(l[i].Name)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// ScheduleView is one schedule without its prompt (a schedule's prompt is a brief) or repository path,
// its last error as a code.
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
	// LastErrorCode is one of the Err* codes when the last firing was not delivered.
	LastErrorCode    string `json:"last_error_code,omitempty"`
	LastErrorMessage string `json:"last_error_message,omitempty"`
	NextFireAt       string `json:"next_fire_at,omitempty"`
}

// SchedulesOutput is one page of /v1/schedules, oldest first; NextCursor is the cursor of the next
// page ("" on the last one).
type SchedulesOutput struct {
	Schedules  []ScheduleView `json:"schedules"`
	NextCursor string         `json:"next_cursor,omitempty"`
}

// viewOfSchedule is the API view of a schedule row.
func (a *API) viewOfSchedule(sc *store.Schedule) ScheduleView {
	v := ScheduleView{
		ID: sc.ID, Cron: capField(sc.Cron), Enabled: sc.Enabled, Kind: capField(sc.Kind), Model: capField(sc.Model),
		Complexity: capField(sc.Complexity), Runner: capField(sc.Runner), ReasoningEffort: capField(sc.ReasoningEffort),
		MaxTurns: sc.MaxTurns, MaxBudgetUSD: sc.MaxBudgetUSD, TimeoutMinutes: sc.TimeoutMinutes,
		Repo: repoName(sc.RepoPath), CreatedAt: ts(&sc.CreatedAt), UpdatedAt: ts(&sc.UpdatedAt),
		LastFiredAt: ts(sc.LastFiredAt), LastJobID: capField(sc.LastJobID),
	}
	if sc.Kind != "" {
		v.Title = capField(jobs.ScheduleTitle(sc))
	}
	if code := scheduleErrorCode(sc); code != "" {
		v.LastErrorCode, v.LastErrorMessage = code, errorMessages[code]
	}
	if n := a.m.NextFire(sc); n != nil {
		v.NextFireAt = n.Format(time.RFC3339)
	}
	return v
}

func (a *API) schedules(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, ok := intParam(q, "limit", DefaultScheduleLimit)
	if !ok || limit < 1 || limit > MaxScheduleLimit {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("limit must be 1..%d", MaxScheduleLimit))
		return
	}
	var after int64
	if c := q.Get("cursor"); c != "" {
		n, err := strconv.ParseInt(c, 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "cursor must be the next_cursor of the previous page")
			return
		}
		after = n
	}
	list, err := a.m.Store.ListSchedulesPageContext(r.Context(), after, limit+1)
	if err != nil {
		a.internalError(w, "schedules", err)
		return
	}
	out := SchedulesOutput{Schedules: make([]ScheduleView, 0, min(len(list), limit))}
	if len(list) > limit {
		list = list[:limit]
		out.NextCursor = strconv.FormatInt(list[limit-1].ID, 10)
	}
	for _, sc := range list {
		out.Schedules = append(out.Schedules, a.viewOfSchedule(sc))
	}
	writeJSON(w, http.StatusOK, out)
}

// limiter is a token bucket per client key. When the map is full the bucket used longest ago is
// evicted, so a new client is never refused for want of room.
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
			var oldest string
			var at time.Time
			for k, o := range l.buckets {
				if oldest == "" || o.last.Before(at) {
					oldest, at = k, o.last
				}
			}
			delete(l.buckets, oldest)
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
