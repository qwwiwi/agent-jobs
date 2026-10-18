package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// body is one GET with the fixture's token; it fails the test unless the status is 200.
func (f *fixture) body(t *testing.T, path string) string {
	t.Helper()
	res, b := f.do(t, http.MethodGet, path, f.token)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, res.StatusCode, b)
	}
	return string(b)
}

// No title derived from a schedule prompt reaches the API: a new schedule without a title, an older
// row whose title is the prompt prefix, and the jobs they fire (new and older rows) are all shown as
// "schedule <id>".
func TestNoPromptDerivedTitles(t *testing.T) {
	f := newFixture(t)
	const prompt = "PROMPT-SECRET rotate the billing keys every night"
	sc, err := f.m.AddSchedule(jobs.ScheduleRequest{Cron: "0 3 * * *", Prompt: prompt, Kind: "review", Model: "sonnet"})
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := f.m.Store.AddSchedule(&store.Schedule{Cron: "0 4 * * *", Prompt: prompt, Enabled: true, Kind: "review",
		Model: sc.Model, Runner: sc.Runner, Title: "schedule: " + jobs.Printable(prompt, 60)}, f.now)
	if err != nil {
		t.Fatal(err)
	}
	old := f.insert(t, "261018-100000-aaaa", store.StatusSucceeded)
	// A job a legacy schedule fired before the change: its title was copied from the prompt.
	j := &store.Job{ID: "261018-100001-bbbb", Agent: "test", Title: "schedule: " + jobs.Printable(prompt, 60), Kind: "review",
		Brief: prompt, Status: store.StatusQueued, Model: "sonnet", MaxTurns: 1, MaxBudgetUSD: 1, TimeoutMinutes: 5,
		CreatedAt: time.Now(), ScheduleID: strconv.FormatInt(legacy.ID, 10)}
	if err := f.m.Store.Insert(j); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/v1/schedules", "/v1/jobs", "/v1/jobs/" + j.ID, "/v1/jobs/" + old.ID} {
		if b := f.body(t, p); strings.Contains(b, "PROMPT-SECRET") || strings.Contains(b, "rotate the billing") {
			t.Fatalf("%s carries the prompt: %s", p, b)
		}
	}
	var so SchedulesOutput
	if err := json.Unmarshal([]byte(f.body(t, "/v1/schedules")), &so); err != nil {
		t.Fatal(err)
	}
	if len(so.Schedules) != 2 || so.Schedules[0].Title != fmt.Sprintf("schedule %d", sc.ID) ||
		so.Schedules[1].Title != fmt.Sprintf("schedule %d", legacy.ID) {
		t.Fatalf("schedule titles: %+v", so.Schedules)
	}
	var jo JobOutput
	if err := json.Unmarshal([]byte(f.body(t, "/v1/jobs/"+j.ID)), &jo); err != nil {
		t.Fatal(err)
	}
	if jo.Title != fmt.Sprintf("schedule %d", legacy.ID) {
		t.Fatalf("legacy fired job title %q", jo.Title)
	}
}

// The report of a job is read only inside its job dir: a symlinked job dir or a hard link to an
// outside file is refused, and the answer names no path.
func TestJobReportContainment(t *testing.T) {
	f := newFixture(t)
	j := f.insert(t, "261018-100001-bbbb", store.StatusSucceeded)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "report.md"), []byte("OUTSIDE-SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := f.m.JobDir(j.ID)
	if err := os.Symlink(outside, dir); err != nil {
		t.Fatal(err)
	}
	check := func(what string) {
		t.Helper()
		out := f.get(t, "/v1/jobs/"+j.ID+"?report=1", http.StatusOK)
		if out["report"] != nil || out["report_error"] == nil {
			t.Fatalf("%s: %v", what, out)
		}
		if e := out["report_error"].(string); strings.Contains(e, outside) || strings.Contains(e, f.m.StateDir) {
			t.Fatalf("%s: report error names a path: %q", what, e)
		}
	}
	check("symlinked job dir")
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(outside, "report.md"), filepath.Join(dir, "report.md")); err != nil {
		t.Skipf("hard link: %v", err)
	}
	check("hard link")
}

// A stored error text never reaches the API: jobs and schedules answer with a code and its fixed
// message; paths and secret-looking markers in the text do not appear in any response.
func TestErrorsAreCodes(t *testing.T) {
	f := newFixture(t)
	const marker = "sk-ant-SECRETMARKER"
	path := "/home/someone/private/" + marker
	for i, c := range []struct{ status, text, code string }{
		{store.StatusFailed, "refused to start: worktree add " + path, ErrLaunchFailed},
		{store.StatusFailed, "refused to start: codex job not launched: config reload failed: " + path, ErrCodexConfigRefused},
		{store.StatusTimeout, "timeout after 5 minutes; " + path, ErrTimeout},
		{store.StatusCancelled, "cancelled; " + path, ErrCancelled},
		{store.StatusFailed, jobs.ReasonSurvivors + " " + path, ErrProcessesSurvived},
		{store.StatusFailed, "claude exited with code 1 " + path, ErrRunFailed},
		{store.StatusFailed, "acceptance failed (exit 2) " + path, ErrAcceptanceFailed},
		{store.StatusFailed, "something else " + path, ErrUnknown},
	} {
		now := time.Now()
		j := &store.Job{ID: fmt.Sprintf("261018-10000%d-eeee", i), Agent: "test", Title: "t", Kind: "fix", Brief: "b",
			Status: c.status, Model: "opus", MaxTurns: 1, MaxBudgetUSD: 1, TimeoutMinutes: 5, CreatedAt: now,
			StartedAt: &now, FinishedAt: &now, Error: c.text, AcceptanceCmd: "make test " + path}
		if err := f.m.Store.Insert(j); err != nil {
			t.Fatal(err)
		}
		var out JobOutput
		if err := json.Unmarshal([]byte(f.body(t, "/v1/jobs/"+j.ID)), &out); err != nil {
			t.Fatal(err)
		}
		if out.ErrorCode != c.code || out.ErrorMessage != errorMessages[c.code] {
			t.Errorf("%q: code %q message %q, want %q", c.text, out.ErrorCode, out.ErrorMessage, c.code)
		}
	}
	sess := &store.Schedule{Cron: "0 3 * * *", Prompt: "p", Enabled: true}
	job := &store.Schedule{Cron: "0 4 * * *", Prompt: "p", Enabled: true, Kind: "review", Title: "t"}
	for _, sc := range []*store.Schedule{sess, job} {
		added, err := f.m.Store.AddSchedule(sc, f.now)
		if err != nil {
			t.Fatal(err)
		}
		minute := f.now.Truncate(time.Minute)
		if ok, err := f.m.Store.ClaimScheduleFiring(added.ID, minute, minute.Format("2006-01-02 15:04")); err != nil || !ok {
			t.Fatalf("claim: %v %v", ok, err)
		}
		if err := f.m.Store.RecordScheduleFiring(added.ID, minute, "", "tmux send-keys failed: "+path); err != nil {
			t.Fatal(err)
		}
	}
	var so SchedulesOutput
	if err := json.Unmarshal([]byte(f.body(t, "/v1/schedules")), &so); err != nil {
		t.Fatal(err)
	}
	if len(so.Schedules) != 2 || so.Schedules[0].LastErrorCode != ErrDeliveryFailed || so.Schedules[1].LastErrorCode != ErrLaunchFailed {
		t.Fatalf("schedule codes: %+v", so.Schedules)
	}
	for _, p := range []string{"/v1/jobs?limit=200", "/v1/jobs/261018-100000-eeee", "/v1/schedules", "/v1/stats", "/v1/health"} {
		b := f.body(t, p)
		for _, bad := range []string{marker, "/home/someone", `"error":`, `"last_error":`} {
			if strings.Contains(b, bad) {
				t.Fatalf("%s carries %q: %s", p, bad, b)
			}
		}
	}
}

func TestLoopbackHost(t *testing.T) {
	for _, c := range []struct {
		host, port string
		ok         bool
	}{
		{"localhost", "7781", true}, {"localhost:7781", "7781", true}, {"LOCALHOST:7781", "7781", true},
		{"127.0.0.1", "7781", true}, {"127.0.0.1:7781", "7781", true}, {"127.9.9.9:7781", "7781", true},
		{"[::1]", "7781", true}, {"[::1]:7781", "7781", true}, {"localhost:1234", "", true},
		{"localhost:1234", "7781", false}, {"127.0.0.1:80", "7781", false}, {"[::1]:80", "7781", false},
		{"evil.example", "7781", false}, {"evil.example:7781", "7781", false}, {"localhost.evil.example", "", false},
		{"127.0.0.1.nip.io:7781", "7781", false}, {"192.168.1.10:7781", "7781", false}, {"0.0.0.0", "", false},
		{"::1", "", false}, {"::1:7781", "7781", false}, {"[::1", "", false}, {"[::1]x", "", false},
		{"[::1]:", "", false}, {"localhost:", "", false}, {"localhost:+1", "", false}, {"localhost:007781", "7781", false},
		{"[127.0.0.1]", "", false}, {"[::ffff:127.0.0.1]", "", false}, {"[fe80::1%25lo]", "", false}, {"", "", false},
		{"127.1", "", false}, {"localhost:99999", "", false},
	} {
		if got := loopbackHost(c.host, c.port); got != c.ok {
			t.Errorf("loopbackHost(%q, %q) = %v, want %v", c.host, c.port, got, c.ok)
		}
	}
}

// The Host check runs before auth (421 even with the right token) on TCP; a unix socket is exempt.
func TestHostHeader(t *testing.T) {
	f := newFixture(t)
	send := func(host string, local net.Addr) int {
		f.now = f.now.Add(time.Second)
		r := newRequest(http.MethodGet, "/v1/health")
		r.Host = host
		if local != nil {
			r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, local))
		}
		r.Header.Set("Authorization", "Bearer "+f.token)
		w := httptest.NewRecorder()
		f.api.ServeHTTP(w, r)
		return w.Code
	}
	tcp := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 7781}
	for host, want := range map[string]int{
		"evil.example":   http.StatusMisdirectedRequest,
		"evil.example:1": http.StatusMisdirectedRequest,
		"localhost:80":   http.StatusMisdirectedRequest,
		"[::1":           http.StatusMisdirectedRequest,
		"%zz":            http.StatusMisdirectedRequest,
		"":               http.StatusMisdirectedRequest,
		"localhost":      http.StatusOK,
		"127.0.0.1:7781": http.StatusOK,
		"[::1]:7781":     http.StatusOK,
	} {
		if got := send(host, tcp); got != want {
			t.Errorf("TCP Host %q: %d, want %d", host, got, want)
		}
	}
	if got := send("evil.example", &net.UnixAddr{Name: "/x.sock", Net: "unix"}); got != http.StatusOK {
		t.Errorf("unix socket with any Host: %d, want 200", got)
	}
	if got := send("evil.example", nil); got != http.StatusMisdirectedRequest {
		t.Errorf("no listener in the context: %d, want 421", got)
	}
}

func TestBearerSchemeCaseInsensitive(t *testing.T) {
	f := newFixture(t)
	for _, scheme := range []string{"Bearer", "bearer", "BEARER"} {
		r := newRequest(http.MethodGet, "/v1/health")
		r.Header.Set("Authorization", scheme+" "+f.token)
		w := httptest.NewRecorder()
		f.now = f.now.Add(time.Second)
		f.api.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("scheme %q: %d", scheme, w.Code)
		}
	}
}

func TestSchedulesPagination(t *testing.T) {
	f := newFixture(t)
	for i := range 5 {
		if _, err := f.m.Store.AddSchedule(&store.Schedule{Cron: "0 3 * * *", Prompt: "p", Enabled: true, Kind: "review",
			Title: strings.Repeat("t", 300) + strconv.Itoa(i)}, f.now); err != nil {
			t.Fatal(err)
		}
	}
	var ids []int64
	cursor := ""
	for page := 0; ; page++ {
		var so SchedulesOutput
		p := "/v1/schedules?limit=2"
		if cursor != "" {
			p += "&cursor=" + cursor
		}
		if err := json.Unmarshal([]byte(f.body(t, p)), &so); err != nil {
			t.Fatal(err)
		}
		if len(so.Schedules) > 2 {
			t.Fatalf("page of %d", len(so.Schedules))
		}
		for _, v := range so.Schedules {
			if len(v.Title) > MaxFieldBytes {
				t.Fatalf("title of %d bytes", len(v.Title))
			}
			ids = append(ids, v.ID)
		}
		if so.NextCursor == "" {
			break
		}
		cursor = so.NextCursor
		if page > 5 {
			t.Fatal("pagination does not end")
		}
	}
	if len(ids) != 5 || ids[0] != 1 || ids[4] != 5 {
		t.Fatalf("ids %v", ids)
	}
	for _, p := range []string{"/v1/schedules?limit=0", "/v1/schedules?limit=501", "/v1/schedules?limit=x",
		"/v1/schedules?cursor=-1", "/v1/schedules?cursor=x"} {
		f.get(t, p, http.StatusBadRequest)
	}
	f.get(t, "/v1/schedules?limit=500", http.StatusOK)
}

func TestStatsBounds(t *testing.T) {
	f := newFixture(t)
	out := f.get(t, "/v1/stats", http.StatusOK)
	since, err := time.Parse(time.RFC3339, fmt.Sprint(out["since"]))
	if err != nil || f.now.Sub(since).Round(time.Hour) != DefaultStatsDays*24*time.Hour {
		t.Fatalf("default since %v (%v), want %d days back", out["since"], err, DefaultStatsDays)
	}
	f.get(t, "/v1/stats?since=365d", http.StatusOK)
	f.get(t, "/v1/stats?since=366d", http.StatusBadRequest)
	f.get(t, "/v1/stats?since=2000-01-01", http.StatusBadRequest)
	// More groups than the cap: one model per job.
	fin := f.now.Add(-time.Hour)
	for i := range MaxStatsGroups + 5 {
		if err := f.m.Store.PutStats(&store.JobStats{ID: fmt.Sprintf("s%d", i), Kind: "fix",
			Model: fmt.Sprintf("model-%03d-%s", i, strings.Repeat("m", 300)), Status: store.StatusSucceeded,
			CreatedAt: fin, FinishedAt: &fin}); err != nil {
			t.Fatal(err)
		}
	}
	var so StatsOutput
	if err := json.Unmarshal([]byte(f.body(t, "/v1/stats?by=model")), &so); err != nil {
		t.Fatal(err)
	}
	if len(so.Groups) != MaxStatsGroups || !so.GroupsTruncated || so.Totals.Jobs != MaxStatsGroups+5 {
		t.Fatalf("groups %d truncated %v jobs %d", len(so.Groups), so.GroupsTruncated, so.Totals.Jobs)
	}
	for _, g := range so.Groups {
		if len(g.Key) > MaxFieldBytes {
			t.Fatalf("group key of %d bytes", len(g.Key))
		}
	}
}

func TestFieldCap(t *testing.T) {
	f := newFixture(t)
	long := strings.Repeat("я", 300) // 600 bytes
	now := time.Now()
	if err := f.m.Store.Insert(&store.Job{ID: "261018-100002-cccc", Agent: "test", Title: long, Kind: "fix", Brief: "b",
		Status: store.StatusQueued, Model: "opus", MaxTurns: 1, MaxBudgetUSD: 1, TimeoutMinutes: 5, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	var out JobOutput
	if err := json.Unmarshal([]byte(f.body(t, "/v1/jobs/261018-100002-cccc")), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Title) > MaxFieldBytes || len(out.Title) < MaxFieldBytes-1 || !strings.HasPrefix(long, out.Title) {
		t.Fatalf("title of %d bytes: %q", len(out.Title), out.Title)
	}
}

// Refused (401) requests and authenticated ones have separate buckets: a client flooding wrong
// tokens is throttled without throttling the dashboard, and the other way round.
func TestRateLimitBuckets(t *testing.T) {
	f := newFixture(t)
	f.now = f.now.Add(time.Minute)
	send := func(tok string) int {
		r := newRequest(http.MethodGet, "/v1/health")
		r.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		f.api.ServeHTTP(w, r)
		return w.Code
	}
	for range RateBurst {
		if c := send("wrong"); c != http.StatusUnauthorized {
			t.Fatalf("wrong token: %d, want 401", c)
		}
	}
	if c := send("wrong"); c != http.StatusTooManyRequests {
		t.Fatalf("wrong token past the burst: %d, want 429", c)
	}
	for range RateBurst {
		if c := send(f.token); c != http.StatusOK {
			t.Fatalf("right token after a 401 flood: %d, want 200", c)
		}
	}
	if c := send(f.token); c != http.StatusTooManyRequests {
		t.Fatalf("right token past the burst: %d, want 429", c)
	}
}

// A full bucket map evicts the bucket used longest ago instead of refusing a new client.
func TestLimiterEvictsOldest(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	l := newLimiter(1, 1, func() time.Time { return now })
	for i := range maxBuckets {
		now = now.Add(time.Millisecond)
		if !l.allow(fmt.Sprint("k", i)) {
			t.Fatalf("key %d refused", i)
		}
	}
	now = now.Add(time.Millisecond)
	if !l.allow("new") {
		t.Fatal("a new key is refused when the map is full")
	}
	if len(l.buckets) != maxBuckets {
		t.Fatalf("%d buckets, want %d", len(l.buckets), maxBuckets)
	}
	if _, ok := l.buckets["k0"]; ok {
		t.Fatal("the oldest bucket was not evicted")
	}
	if _, ok := l.buckets["k1"]; !ok {
		t.Fatal("a newer bucket was evicted")
	}
}

// waitStatus polls http-api.status until the API serves (or fails the test).
func waitStatus(t *testing.T, m *jobs.Manager, serving bool) jobs.HTTPAPIStatus {
	t.Helper()
	for range 250 {
		var st jobs.HTTPAPIStatus
		if b, err := os.ReadFile(jobs.HTTPAPIStatusPath(m.StateDir)); err == nil && json.Unmarshal(b, &st) == nil && st.Serving == serving {
			return st
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("http-api.status never showed serving=%v", serving)
	return jobs.HTTPAPIStatus{}
}

// Without http_api.listen the API serves on <state>/http-api.sock (mode 600) and records it; the
// status check shows where it listens, and after the stop that it does not serve.
func TestServeDefaultSocket(t *testing.T) {
	f := newFixture(t)
	f.m.Now = time.Now
	f.m.Cfg.HTTPAPI = jobs.HTTPAPIConfig{Enabled: true}
	sock := jobs.HTTPAPISocket(f.m.StateDir)
	if len(sock) > 100 {
		t.Skipf("socket path too long for this temp dir: %s", sock)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, f.m, "v") }()
	st := waitStatus(t, f.m, true)
	if st.Listen != sock {
		t.Fatalf("recorded listen %q, want %q", st.Listen, sock)
	}
	fi, err := os.Lstat(sock)
	if err != nil || fi.Mode().Type() != os.ModeSocket || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket: %v %v", fi, err)
	}
	if c, ok := f.m.HTTPAPICheck(); !ok || !c.OK || !strings.Contains(c.Detail, sock) {
		t.Fatalf("check while serving: %+v %v", c, ok)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Serve after cancel: %v", err)
	}
	if c, _ := f.m.HTTPAPICheck(); c.OK || !strings.HasPrefix(c.Detail, "http api not serving: daemon stopped") {
		t.Fatalf("check after stop: %+v", c)
	}
}

// A state dir open to others cannot hold the default socket: Serve refuses, and records why.
func TestServeDefaultSocketRefusesOpenStateDir(t *testing.T) {
	f := newFixture(t)
	f.m.Cfg.HTTPAPI = jobs.HTTPAPIConfig{Enabled: true}
	if err := os.Chmod(f.m.StateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(f.m.StateDir, 0o700) })
	err := Serve(context.Background(), f.m, "v")
	if err == nil || !strings.Contains(err.Error(), "want 700") {
		t.Fatalf("Serve with a 755 state dir: %v", err)
	}
	if _, err := os.Lstat(jobs.HTTPAPISocket(f.m.StateDir)); err == nil {
		t.Fatal("socket created in an open state dir")
	}
	c, ok := f.m.HTTPAPICheck()
	if !ok || c.OK || !strings.HasPrefix(c.Detail, "http api not serving: ") || !strings.Contains(c.Detail, "want 700") {
		t.Fatalf("check: %+v", c)
	}
}

// A TCP bind that fails (the port is taken) is recorded for status and system_doctor; port 0 is
// recorded and logged as the real port.
func TestServeTCPBindFailureRecorded(t *testing.T) {
	f := newFixture(t)
	f.m.Now = time.Now
	var logs bytes.Buffer
	f.m.Log = log.New(&logs, "", 0) // read after Serve returned
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	f.m.Cfg.HTTPAPI = jobs.HTTPAPIConfig{Enabled: true, Listen: taken.Addr().String()}
	if err := Serve(context.Background(), f.m, "v"); err == nil {
		t.Fatal("Serve on a taken port")
	}
	if c, _ := f.m.HTTPAPICheck(); c.OK || !strings.HasPrefix(c.Detail, "http api not serving: ") || !strings.Contains(c.Detail, "address already in use") {
		t.Fatalf("check after a failed bind: %+v", c)
	}
	f.m.Cfg.HTTPAPI.Listen = "127.0.0.1:0"
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, f.m, "v") }()
	st := waitStatus(t, f.m, true)
	cancel()
	<-done
	if strings.HasSuffix(st.Listen, ":0") || !strings.HasPrefix(st.Listen, "127.0.0.1:") {
		t.Fatalf("recorded listen %q, want the bound port", st.Listen)
	}
	if !strings.Contains(logs.String(), "listening on "+st.Listen) {
		t.Fatalf("log does not name the bound port %s: %s", st.Listen, logs.String())
	}
}
