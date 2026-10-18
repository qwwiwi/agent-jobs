package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

func newManager(t *testing.T) *jobs.Manager {
	t.Helper()
	t.Setenv("AGENT_JOBS_HOME", t.TempDir())
	m, err := jobs.Open("test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	m.Log = log.New(io.Discard, "", 0)
	return m
}

// fixture is an API over a manager with a token and a few jobs.
type fixture struct {
	m     *jobs.Manager
	api   *API
	token string
	now   time.Time
}

const (
	secretBrief = "BRIEF-SECRET-do-not-show"
	secretRepo  = "/srv/private/secret-repo-path"
)

func newFixture(t *testing.T) *fixture {
	t.Helper()
	m := newManager(t)
	tok, err := RotateToken(m.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{m: m, token: tok, now: time.Now()}
	// A clock that only the test moves keeps the rate limiter deterministic.
	m.Now = func() time.Time { return f.now }
	f.api = New(m, "v-test")
	return f
}

func (f *fixture) insert(t *testing.T, id, status string) *store.Job {
	t.Helper()
	now := time.Now()
	j := &store.Job{ID: id, Agent: "test", Title: "title " + id, Kind: "fix", Brief: secretBrief, Status: status,
		Model: "opus", MaxTurns: 10, MaxBudgetUSD: 1, TimeoutMinutes: 5, CreatedAt: now, RepoPath: secretRepo,
		SessionID: "11111111-2222-3333-4444-555555555555"}
	if status != store.StatusQueued {
		j.StartedAt = &now
	}
	if status != store.StatusQueued && status != store.StatusRunning {
		j.FinishedAt = &now
		j.Error = "worktree add failed in " + secretRepo
	}
	if err := f.m.Store.Insert(j); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Second) // never hit the rate limit in shape tests
	return j
}

// newRequest is httptest.NewRequest addressed to localhost (its default Host is example.com).
func newRequest(method, path string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	r.Host = "localhost"
	return r
}

// do sends one request with the given bearer token ("" = no Authorization header).
func (f *fixture) do(t *testing.T, method, path, token string) (*http.Response, []byte) {
	t.Helper()
	f.now = f.now.Add(time.Second)
	r := newRequest(method, path)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	f.api.ServeHTTP(w, r)
	res := w.Result()
	b, _ := io.ReadAll(res.Body)
	return res, b
}

func (f *fixture) get(t *testing.T, path string, want int) map[string]any {
	t.Helper()
	res, b := f.do(t, http.MethodGet, path, f.token)
	if res.StatusCode != want {
		t.Fatalf("GET %s: status %d, want %d: %s", path, res.StatusCode, want, b)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("GET %s: content type %q", path, ct)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("GET %s: %v: %s", path, err, b)
	}
	return out
}

func TestOffByDefault(t *testing.T) {
	m := newManager(t)
	if m.Cfg.HTTPAPI.Enabled {
		t.Fatal("http_api enabled without config")
	}
	if want := filepath.Join(m.StateDir, "http-api.sock"); m.Cfg.HTTPAPI.Listen != "" || m.Cfg.HTTPAPIListen(m.StateDir) != want {
		t.Fatalf("listen %q (%q), want the default socket %q", m.Cfg.HTTPAPI.Listen, m.Cfg.HTTPAPIListen(m.StateDir), want)
	}
	if err := Serve(context.Background(), m, "v"); !errors.Is(err, ErrDisabled) {
		t.Fatalf("Serve with the API off: %v, want ErrDisabled", err)
	}
}

func TestNonLoopbackRefused(t *testing.T) {
	for _, l := range []string{"0.0.0.0:7781", ":7781", "[::]:7781", "192.168.1.10:7781", "10.0.0.1:80",
		"localhost:7781", "example.com:7781", "127.0.0.1", "127.0.0.1:99999", "relative/sock", "/tmp/../x.sock"} {
		if err := jobs.ValidateHTTPListen(l); err == nil {
			t.Errorf("listen %q accepted", l)
		}
	}
	for _, l := range []string{"127.0.0.1:7781", "127.1.2.3:0", "[::1]:7781", "/run/agent-jobs/api.sock"} {
		if err := jobs.ValidateHTTPListen(l); err != nil {
			t.Errorf("listen %q refused: %v", l, err)
		}
	}
	// The config refuses it at load, so neither the daemon nor any command starts with it.
	dir := t.TempDir()
	cfg := `{"http_api": {"enabled": true, "listen": "0.0.0.0:7781"}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.LoadConfig(dir); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("LoadConfig with a wildcard listen: %v", err)
	}
	// Serve checks again (a Manager built by hand).
	m := newManager(t)
	m.Cfg.HTTPAPI = jobs.HTTPAPIConfig{Enabled: true, Listen: "0.0.0.0:7781"}
	if err := Serve(context.Background(), m, "v"); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("Serve on a wildcard: %v", err)
	}
}

func TestUnauthorized(t *testing.T) {
	f := newFixture(t)
	for _, p := range []string{"/v1/health", "/v1/jobs", "/v1/jobs/x", "/v1/stats", "/v1/schedules", "/nope"} {
		for _, tok := range []string{"", "wrong", f.token + "x", f.token[:len(f.token)-1]} {
			res, b := f.do(t, http.MethodGet, p, tok)
			if res.StatusCode != http.StatusUnauthorized {
				t.Fatalf("GET %s token %q: %d, want 401: %s", p, tok, res.StatusCode, b)
			}
		}
		// POST without a token is 401 too: auth comes before the method.
		if res, _ := f.do(t, http.MethodPost, p, ""); res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("POST %s without token: %d", p, res.StatusCode)
		}
	}
	// Another scheme with the right value is refused.
	r := newRequest(http.MethodGet, "/v1/health")
	r.Header.Set("Authorization", "Basic "+f.token)
	w := httptest.NewRecorder()
	f.api.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("Basic scheme: %d", w.Code)
	}
	// A token file open to others fails closed.
	if err := os.Chmod(TokenPath(f.m.StateDir), 0o644); err != nil {
		t.Fatal(err)
	}
	if res, _ := f.do(t, http.MethodGet, "/v1/health", f.token); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("token file 644: %d, want 401", res.StatusCode)
	}
	// No token file: every request is 401.
	if err := os.Remove(TokenPath(f.m.StateDir)); err != nil {
		t.Fatal(err)
	}
	if res, _ := f.do(t, http.MethodGet, "/v1/health", f.token); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token file: %d, want 401", res.StatusCode)
	}
}

func TestRotateToken(t *testing.T) {
	f := newFixture(t)
	fi, err := os.Stat(TokenPath(f.m.StateDir))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("token mode %v", fi.Mode().Perm())
	}
	f.get(t, "/v1/health", http.StatusOK)
	old := f.token
	tok, err := RotateToken(f.m.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if tok == old || len(tok) != 64 {
		t.Fatalf("rotated token %q (old %q)", tok, old)
	}
	if res, _ := f.do(t, http.MethodGet, "/v1/health", old); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old token after rotate: %d", res.StatusCode)
	}
	f.token = tok
	f.get(t, "/v1/health", http.StatusOK)
}

func TestMethodNotAllowed(t *testing.T) {
	f := newFixture(t)
	f.insert(t, "261018-100000-aaaa", store.StatusQueued)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete,
		http.MethodOptions, http.MethodHead} {
		for _, p := range []string{"/v1/jobs", "/v1/jobs/261018-100000-aaaa", "/v1/stats", "/v1/schedules", "/v1/health"} {
			res, _ := f.do(t, method, p, f.token)
			if res.StatusCode != http.StatusMethodNotAllowed {
				t.Fatalf("%s %s: %d, want 405", method, p, res.StatusCode)
			}
			if res.Header.Get("Allow") != http.MethodGet {
				t.Fatalf("%s %s: Allow %q", method, p, res.Header.Get("Allow"))
			}
			if res.Header.Get("Access-Control-Allow-Origin") != "" {
				t.Fatalf("%s %s: CORS header set", method, p)
			}
		}
	}
	// Nothing changed: the queued job is still queued.
	j, err := f.m.Store.Get("261018-100000-aaaa")
	if err != nil || j.Status != store.StatusQueued {
		t.Fatalf("job after writes: %+v %v", j, err)
	}
}

func TestHealth(t *testing.T) {
	f := newFixture(t)
	f.insert(t, "261018-100000-aaaa", store.StatusQueued)
	f.insert(t, "261018-100001-bbbb", store.StatusRunning)
	f.insert(t, "261018-100002-cccc", store.StatusRunning)
	out := f.get(t, "/v1/health", http.StatusOK)
	if out["version"] != "v-test" || out["running"] != 2.0 || out["queued"] != 1.0 {
		t.Fatalf("health %v", out)
	}
	if _, ok := out["uptime_sec"].(float64); !ok {
		t.Fatalf("health without uptime: %v", out)
	}
}

func TestJobsList(t *testing.T) {
	f := newFixture(t)
	f.insert(t, "261018-100000-aaaa", store.StatusQueued)
	f.insert(t, "261018-100001-bbbb", store.StatusSucceeded)
	f.insert(t, "261018-100002-cccc", store.StatusFailed)
	out := f.get(t, "/v1/jobs", http.StatusOK)
	list := out["jobs"].([]any)
	if len(list) != 3 {
		t.Fatalf("jobs %d, want 3", len(list))
	}
	first := list[0].(map[string]any)
	for _, k := range []string{"id", "title", "kind", "status", "model", "runner", "turns", "cost_usd",
		"created_at", "attempt", "repo"} {
		if _, ok := first[k]; !ok {
			t.Fatalf("job view without %q: %v", k, first)
		}
	}
	if first["repo"] != "secret-repo-path" {
		t.Fatalf("repo %v, want the folder name only", first["repo"])
	}
	out = f.get(t, "/v1/jobs?status=failed", http.StatusOK)
	if l := out["jobs"].([]any); len(l) != 1 || l[0].(map[string]any)["id"] != "261018-100002-cccc" {
		t.Fatalf("status filter: %v", l)
	}
	out = f.get(t, "/v1/jobs?limit=2", http.StatusOK)
	if l := out["jobs"].([]any); len(l) != 2 {
		t.Fatalf("limit 2: %d jobs", len(l))
	}
	for _, p := range []string{"/v1/jobs?status=bogus", "/v1/jobs?limit=0", "/v1/jobs?limit=201", "/v1/jobs?limit=x"} {
		f.get(t, p, http.StatusBadRequest)
	}
	_, b := f.do(t, http.MethodGet, "/v1/jobs", f.token)
	assertNoSecrets(t, f, b)
}

func assertNoSecrets(t *testing.T, f *fixture, body []byte) {
	t.Helper()
	s := string(body)
	for _, bad := range []string{secretBrief, secretRepo, f.token, f.m.StateDir, "session_id", "brief", "job_dir",
		"11111111-2222"} {
		if strings.Contains(s, bad) {
			t.Fatalf("response carries %q: %s", bad, s)
		}
	}
}

func TestJobGet(t *testing.T) {
	f := newFixture(t)
	j := f.insert(t, "261018-100001-bbbb", store.StatusFailed)
	res, b := f.do(t, http.MethodGet, "/v1/jobs/"+j.ID, f.token)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", res.StatusCode, b)
	}
	assertNoSecrets(t, f, b)
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out["id"] != j.ID || out["status"] != store.StatusFailed {
		t.Fatalf("job %v", out)
	}
	if out["error_code"] != ErrUnknown || out["error_message"] != errorMessages[ErrUnknown] || out["error"] != nil {
		t.Fatalf("error %v %v %v: want a code and its fixed message only", out["error_code"], out["error_message"], out["error"])
	}
	if _, ok := out["report"]; ok {
		t.Fatal("report returned without ?report=1")
	}
	// No report file yet.
	out = f.get(t, "/v1/jobs/"+j.ID+"?report=1", http.StatusOK)
	if out["report_missing"] != true {
		t.Fatalf("missing report: %v", out)
	}
	f.get(t, "/v1/jobs/"+j.ID+"?report=yes", http.StatusBadRequest)
	f.get(t, "/v1/jobs/261018-999999-zzzz", http.StatusNotFound)
	f.get(t, "/v1/jobs/..%2F..%2Fconfig.json", http.StatusNotFound)
	f.get(t, "/v1/jobs/", http.StatusNotFound)
	f.get(t, "/v1/jobs/"+j.ID+"/x", http.StatusNotFound)
}

func TestJobReportCap(t *testing.T) {
	f := newFixture(t)
	j := f.insert(t, "261018-100001-bbbb", store.StatusSucceeded)
	dir := f.m.JobDir(j.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte("## Summary\nshort\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := f.get(t, "/v1/jobs/"+j.ID+"?report=1", http.StatusOK)
	if out["report"] != "## Summary\nshort\n" || out["report_truncated"] != nil {
		t.Fatalf("short report: %v", out)
	}
	// Cut at the cap, the report loses its whole incomplete last line (all of it without a line break).
	line := strings.Repeat("x", 99) + "\n"
	big := strings.Repeat(line, MaxReportBytes/len(line)+10)
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte(big), 0o600); err != nil {
		t.Fatal(err)
	}
	out = f.get(t, "/v1/jobs/"+j.ID+"?report=1", http.StatusOK)
	if r, _ := out["report"].(string); len(r) != MaxReportBytes/len(line)*len(line) || out["report_truncated"] != true {
		t.Fatalf("big report: %d bytes, truncated %v", len(r), out["report_truncated"])
	}
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte(strings.Repeat("x", MaxReportBytes+1000)), 0o600); err != nil {
		t.Fatal(err)
	}
	out = f.get(t, "/v1/jobs/"+j.ID+"?report=1", http.StatusOK)
	if r, _ := out["report"].(string); r != "" || out["report_truncated"] != true {
		t.Fatalf("one-line big report: %d bytes, truncated %v", len(r), out["report_truncated"])
	}
	// A symlinked report (the job dir is the job's to write) is not followed.
	if err := os.Remove(filepath.Join(dir, "report.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(f.m.StateDir, "config.json"), filepath.Join(dir, "report.md")); err != nil {
		t.Fatal(err)
	}
	out = f.get(t, "/v1/jobs/"+j.ID+"?report=1", http.StatusOK)
	if out["report"] != nil || out["report_error"] == nil {
		t.Fatalf("symlinked report: %v", out)
	}
	if strings.Contains(out["report_error"].(string), f.m.StateDir) {
		t.Fatalf("report error names a path: %v", out["report_error"])
	}
}

func TestStats(t *testing.T) {
	f := newFixture(t)
	out := f.get(t, "/v1/stats", http.StatusOK)
	for _, k := range []string{"by", "totals", "groups", "guard_rules", "skills", "tools"} {
		if _, ok := out[k]; !ok {
			t.Fatalf("stats without %q: %v", k, out)
		}
	}
	if out["by"] != "kind" {
		t.Fatalf("stats by %v", out["by"])
	}
	if out = f.get(t, "/v1/stats?by=model&since=7d", http.StatusOK); out["by"] != "model" {
		t.Fatalf("stats by model: %v", out)
	}
	f.get(t, "/v1/stats?by=brief", http.StatusBadRequest)
	f.get(t, "/v1/stats?since=yesterday", http.StatusBadRequest)
}

func TestSchedules(t *testing.T) {
	f := newFixture(t)
	out := f.get(t, "/v1/schedules", http.StatusOK)
	if l, ok := out["schedules"].([]any); !ok || len(l) != 0 {
		t.Fatalf("empty schedules: %v", out)
	}
	const prompt = "SCHEDULE-PROMPT-secret nightly review"
	if _, err := f.m.AddSchedule(jobs.ScheduleRequest{Cron: "0 3 * * *", Prompt: prompt, Kind: "review",
		Title: "nightly", Model: "sonnet"}); err != nil {
		t.Fatal(err)
	}
	res, b := f.do(t, http.MethodGet, "/v1/schedules", f.token)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", res.StatusCode, b)
	}
	if strings.Contains(string(b), "SCHEDULE-PROMPT") || strings.Contains(string(b), `"prompt"`) {
		t.Fatalf("schedules carry the prompt: %s", b)
	}
	var so SchedulesOutput
	if err := json.Unmarshal(b, &so); err != nil {
		t.Fatal(err)
	}
	if len(so.Schedules) != 1 || so.Schedules[0].Cron != "0 3 * * *" || so.Schedules[0].NextFireAt == "" ||
		so.Schedules[0].Kind != "review" {
		t.Fatalf("schedules %+v", so.Schedules)
	}
}

func TestRateLimit(t *testing.T) {
	f := newFixture(t)
	f.now = f.now.Add(time.Minute)
	got429 := 0
	for range RateBurst + 5 {
		r := newRequest(http.MethodGet, "/v1/health")
		r.Header.Set("Authorization", "Bearer "+f.token)
		w := httptest.NewRecorder()
		f.api.ServeHTTP(w, r)
		if w.Code == http.StatusTooManyRequests {
			got429++
		}
	}
	if got429 != 5 {
		t.Fatalf("429s within one instant: %d, want 5", got429)
	}
	// Another client has its own bucket.
	r := newRequest(http.MethodGet, "/v1/health")
	r.RemoteAddr = "127.0.0.2:5555"
	r.Header.Set("Authorization", "Bearer "+f.token)
	w := httptest.NewRecorder()
	f.api.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("second client: %d", w.Code)
	}
	// A second later the first client may send again.
	f.now = f.now.Add(time.Second)
	f.get(t, "/v1/health", http.StatusOK)
}

func TestServerLimits(t *testing.T) {
	s := NewServer(http.NotFoundHandler())
	if s.ReadHeaderTimeout <= 0 || s.ReadTimeout <= 0 || s.WriteTimeout <= 0 || s.IdleTimeout <= 0 ||
		s.MaxHeaderBytes <= 0 || s.MaxHeaderBytes > 64<<10 {
		t.Fatalf("server limits %+v", s)
	}
}

// TestServeUnixSocket runs the real server on a unix socket: mode 600, auth, shutdown on ctx.
func TestServeUnixSocket(t *testing.T) {
	f := newFixture(t)
	f.m.Now = time.Now
	// Unix socket paths are short (about 104 bytes): use a short temp dir.
	dir, err := os.MkdirTemp("", "aj")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "api.sock")
	f.m.Cfg.HTTPAPI = jobs.HTTPAPIConfig{Enabled: true, Listen: sock}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, f.m, "v-sock") }()
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}}}
	var res *http.Response
	for i := 0; ; i++ {
		req, _ := http.NewRequest(http.MethodGet, "http://agent-jobs/v1/health", nil)
		req.Header.Set("Authorization", "Bearer "+f.token)
		res, err = client.Do(req)
		if err == nil {
			break
		}
		if i > 100 {
			t.Fatalf("server did not come up: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	b, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.Contains(string(b), "v-sock") {
		t.Fatalf("health over the socket: %d %s", res.StatusCode, b)
	}
	fi, err := os.Lstat(sock)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode: %v %v", fi, err)
	}
	res, err = client.Get("http://agent-jobs/v1/jobs")
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token over the socket: %d", res.StatusCode)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve after cancel: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not stop")
	}
}

func TestServeRefusesWithoutSafeToken(t *testing.T) {
	m := newManager(t)
	m.Cfg.HTTPAPI = jobs.HTTPAPIConfig{Enabled: true, Listen: "127.0.0.1:0"}
	if err := Serve(context.Background(), m, "v"); err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("Serve without a token file: %v", err)
	}
	if err := os.WriteFile(TokenPath(m.StateDir), []byte("abc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Serve(context.Background(), m, "v"); err == nil || !strings.Contains(err.Error(), "mode") {
		t.Fatalf("Serve with a 644 token file: %v", err)
	}
}

// The report is redacted on the fly: a report.md whose in-place redaction is still pending (or of a
// running job) never leaves the API with a raw token.
func TestJobReportRedacted(t *testing.T) {
	f := newFixture(t)
	j := f.insert(t, "261018-100001-cccc", store.StatusSucceeded)
	dir := f.m.JobDir(j.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	tok := "ghp_" + strings.Repeat("Q", 36)
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte("## Summary\nused "+tok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := f.get(t, "/v1/jobs/"+j.ID+"?report=1", http.StatusOK)
	if r, _ := out["report"].(string); strings.Contains(r, tok) || !strings.Contains(r, "[redacted:github]") {
		t.Fatalf("report: %q", r)
	}
}
