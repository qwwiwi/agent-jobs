package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

var selfBin string // built agent-jobs binary

func TestMain(m *testing.M) {
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "GIT_") {
			_ = os.Unsetenv(k)
		}
	}
	_ = os.Unsetenv(jobs.JobEnvVar) // the tests set the job marker themselves where they need it
	dir, err := os.MkdirTemp("", "agent-jobs-mcp-test-")
	if err != nil {
		log.Fatal(err)
	}
	selfBin = filepath.Join(dir, "agent-jobs")
	if out, err := exec.Command("go", "build", "-o", selfBin, "../../cmd/agent-jobs").CombinedOutput(); err != nil {
		log.Fatalf("build: %v\n%s", err, out)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// newManager opens a real manager (real SQLite store) under a temp AGENT_JOBS_HOME.
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

// connect serves s in memory and returns a connected client session.
func connect(t *testing.T, s *sdk.Server) *sdk.ClientSession {
	t.Helper()
	ctx := context.Background()
	ct, st := sdk.NewInMemoryTransports()
	ss, err := s.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close(); _ = ss.Close() })
	return cs
}

func toolNames(t *testing.T, cs *sdk.ClientSession) []string {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	slices.Sort(names)
	return names
}

func text(res *sdk.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// call invokes a tool and decodes its structured result into out; it fails the test on a tool error.
func call(t *testing.T, cs *sdk.ClientSession, name string, args map[string]any, out any) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("%s: tool error: %s", name, text(res))
	}
	if err := json.Unmarshal([]byte(text(res)), out); err != nil {
		t.Fatalf("%s: decode %q: %v", name, text(res), err)
	}
}

// callErr invokes a tool that must fail and returns the error text (tool error or protocol error).
func callErr(t *testing.T, cs *sdk.ClientSession, name string, args map[string]any) string {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return err.Error()
	}
	if !res.IsError {
		t.Fatalf("%s %v succeeded: %s", name, args, text(res))
	}
	return text(res)
}

// Reference «MCP-тулы»: every tool belongs to exactly one list, every listed tool exists, and the
// lists have no duplicates.
func TestEveryToolInExactlyOneList(t *testing.T) {
	defined := map[string]bool{}
	for _, d := range definitions() {
		if defined[d.name] {
			t.Fatalf("tool %q defined twice", d.name)
		}
		defined[d.name] = true
		if _, err := audienceOf(d.name); err != nil {
			t.Fatal(err)
		}
	}
	for _, list := range [][]string{OrchestratorOnly, JobAllowed} {
		seen := map[string]bool{}
		for _, n := range list {
			if seen[n] {
				t.Fatalf("tool %q listed twice", n)
			}
			seen[n] = true
			if !defined[n] {
				t.Fatalf("tool %q is listed but has no definition", n)
			}
		}
	}
	if len(defined) != len(OrchestratorOnly)+len(JobAllowed) {
		t.Fatalf("%d definitions, %d listed", len(defined), len(OrchestratorOnly)+len(JobAllowed))
	}
	// A tool in neither list or in both is rejected by the server builder too.
	if _, err := audienceOf("not_a_tool"); err == nil {
		t.Fatal("unlisted tool accepted")
	}
	saved := JobAllowed
	JobAllowed = []string{"job_start"}
	_, err := audienceOf("job_start")
	JobAllowed = saved
	if err == nil {
		t.Fatal("tool in both lists accepted")
	}
}

// The job role sees only allow-listed tools, and the server refuses a hidden tool by itself: even
// with every orchestrator tool registered on the job server, tools/list hides them and tools/call
// is refused before any handler runs (the handlers here have no manager and would panic).
func TestJobRoleRefusesHiddenToolsServerSide(t *testing.T) {
	plain, err := NewServer(RoleJob, Deps{})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := newServer(RoleJob, Deps{}, true)
	if err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]*sdk.Server{"plain": plain, "all registered": loaded} {
		cs := connect(t, s)
		if got := toolNames(t, cs); len(got) != len(JobAllowed) {
			t.Fatalf("%s: job role lists %v, want %v", name, got, JobAllowed)
		}
		for _, tool := range OrchestratorOnly {
			msg := callErr(t, cs, tool, map[string]any{"id": "x"})
			if !strings.Contains(msg, `is not available to the job role`) {
				t.Fatalf("%s: %s refused with %q, want the role refusal", name, tool, msg)
			}
		}
	}
}

func TestOrchestratorRoleListsJobTools(t *testing.T) {
	s, err := NewServer(RoleOrchestrator, Deps{Manager: newManager(t)})
	if err != nil {
		t.Fatal(err)
	}
	want := slices.Sorted(slices.Values(append(slices.Clone(OrchestratorOnly), JobAllowed...)))
	if got := toolNames(t, connect(t, s)); !slices.Equal(got, want) {
		t.Fatalf("orchestrator tools %v, want %v", got, want)
	}
	if _, err := NewServer(RoleOrchestrator, Deps{}); err == nil {
		t.Fatal("orchestrator server without a manager")
	}
	if _, err := NewServer(Role("admin"), Deps{}); err == nil {
		t.Fatal("unknown role accepted")
	}
}

func startArgs(mod func(map[string]any)) map[string]any {
	a := map[string]any{"title": "t", "kind": "general", "brief": "do it", "model": "sonnet"}
	if mod != nil {
		mod(a)
	}
	return a
}

// Owner 2026-10-17: job_start takes an explicit model or a complexity, never guesses one; limits
// follow the defaults and ceilings.
func TestJobStartModelOrComplexity(t *testing.T) {
	m := newManager(t)
	cs := connect(t, mustServer(t, m))
	for _, c := range []struct {
		name string
		mod  func(map[string]any)
		want string
	}{
		{"neither", func(a map[string]any) { delete(a, "model") }, "model or complexity is required"},
		{"empty model", func(a map[string]any) { a["model"] = "" }, "model or complexity is required"},
		{"flag as model", func(a map[string]any) { a["model"] = "--dangerously-skip-permissions" }, "not a model name"},
		{"bad complexity", func(a map[string]any) { delete(a, "model"); a["complexity"] = "huge" }, "complexity"},
		{"turns above ceiling", func(a map[string]any) { a["max_turns"] = 201 }, "not clamped"},
		{"bad kind", func(a map[string]any) { a["kind"] = "deploy" }, "kind"},
	} {
		if msg := callErr(t, cs, "job_start", startArgs(c.mod)); !strings.Contains(msg, c.want) {
			t.Fatalf("%s: error %q does not mention %q", c.name, msg, c.want)
		}
	}
	if n, _ := m.Store.List(10); len(n) != 0 {
		t.Fatalf("rejected calls queued %d jobs", len(n))
	}
	var byCx JobView
	call(t, cs, "job_start", startArgs(func(a map[string]any) { delete(a, "model"); a["kind"] = "research"; a["complexity"] = "hard" }), &byCx)
	if byCx.Model != "opus" || byCx.Complexity != "hard" || byCx.MaxTurns != jobs.DefaultMaxTurns ||
		byCx.TimeoutMinutes != jobs.DefaultTimeoutMinutes || byCx.MaxBudgetUSD != jobs.DefaultMaxBudgetUSD {
		t.Fatalf("complexity + default limits: %+v", byCx)
	}
}

func mustServer(t *testing.T, m *jobs.Manager) *sdk.Server {
	t.Helper()
	s, err := NewServer(RoleOrchestrator, Deps{Manager: m})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The five job tools over a real store: start → status → list → cancel → result, plus the result
// of a finished job with a report and the not-found / already-finished errors.
func TestJobToolsLifecycle(t *testing.T) {
	m := newManager(t)
	cs := connect(t, mustServer(t, m))

	var started JobView
	call(t, cs, "job_start", startArgs(func(a map[string]any) {
		a["complexity"], a["max_budget_usd"], a["max_turns"], a["timeout_minutes"] = "hard", 3, 20, 30
	}), &started)
	if started.ID == "" || started.Status != store.StatusQueued || started.Model != "sonnet" || started.MaxTurns != 20 ||
		started.TimeoutMinutes != 30 || started.MaxBudgetUSD != 3 || started.Complexity != "hard" ||
		started.Runner != "claude" || started.Billing != "subscription" || started.JobDir != m.JobDir(started.ID) {
		t.Fatalf("job_start: %+v", started)
	}
	row, err := m.Store.Get(started.ID)
	if err != nil || row.Brief != "do it" || row.Complexity != "hard" {
		t.Fatalf("stored row: %+v %v", row, err)
	}

	var st JobView
	call(t, cs, "job_status", map[string]any{"id": started.ID}, &st)
	if st.ID != started.ID || st.Status != store.StatusQueued || st.CreatedAt == "" {
		t.Fatalf("job_status: %+v", st)
	}

	var pending JobResultOutput
	call(t, cs, "job_result", map[string]any{"id": started.ID}, &pending)
	if pending.Finished || pending.Report != "" || pending.DoneLine != "" || !strings.Contains(pending.Note, "queued") {
		t.Fatalf("job_result of a queued job: %+v", pending)
	}

	// A finished job with a report, inserted as the daemon would leave it.
	now := time.Now()
	done := &store.Job{ID: "261017-120000-beef", Agent: "test", Title: "done job", Kind: "review", Brief: "b",
		Status: store.StatusSucceeded, Model: "opus", MaxTurns: 10, MaxBudgetUSD: 1, TimeoutMinutes: 5,
		CreatedAt: now.Add(-time.Hour), StartedAt: &now, FinishedAt: &now, Turns: 7, CostUSD: 0.5, DurationSec: 600}
	if err := m.Store.Insert(done); err != nil {
		t.Fatal(err)
	}
	dir := m.JobDir(done.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	report := "## Summary\nok\n\n## Not done / uncertain\nnothing\n"
	for f, body := range map[string]string{"report.md": report, "commits.patch": "p"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var res JobResultOutput
	call(t, cs, "job_result", map[string]any{"id": done.ID}, &res)
	if !res.Finished || res.Report != report || res.ReportMissing || res.DoneLine != jobs.DoneLine(done) ||
		!slices.Contains(res.Artifacts, filepath.Join(dir, "commits.patch")) || !slices.Contains(res.Artifacts, filepath.Join(dir, "report.md")) {
		t.Fatalf("job_result: %+v", res)
	}

	var list JobListOutput
	call(t, cs, "job_list", map[string]any{}, &list)
	if len(list.Jobs) != 2 || list.Jobs[0].ID != started.ID { // newest first
		t.Fatalf("job_list: %+v", list)
	}
	call(t, cs, "job_list", map[string]any{"status": "succeeded"}, &list)
	if len(list.Jobs) != 1 || list.Jobs[0].ID != done.ID {
		t.Fatalf("job_list succeeded: %+v", list)
	}
	call(t, cs, "job_list", map[string]any{"limit": 1}, &list)
	if len(list.Jobs) != 1 {
		t.Fatalf("job_list limit 1: %+v", list)
	}
	for _, args := range []map[string]any{{"limit": 201}, {"status": "lost"}} {
		callErr(t, cs, "job_list", args)
	}

	var cancelled JobCancelOutput
	call(t, cs, "job_cancel", map[string]any{"id": started.ID}, &cancelled)
	if cancelled.Status != store.StatusCancelled {
		t.Fatalf("job_cancel: %+v", cancelled)
	}
	if msg := callErr(t, cs, "job_cancel", map[string]any{"id": started.ID}); !strings.Contains(msg, "already finished (cancelled)") {
		t.Fatalf("second cancel: %q", msg)
	}
	var after JobResultOutput
	call(t, cs, "job_result", map[string]any{"id": started.ID}, &after)
	if !after.Finished || after.Status != store.StatusCancelled || !after.ReportMissing || len(after.Artifacts) != 0 {
		t.Fatalf("job_result of a cancelled job: %+v", after)
	}

	for _, tool := range []string{"job_status", "job_result", "job_cancel"} {
		if msg := callErr(t, cs, tool, map[string]any{"id": "nope"}); !strings.Contains(msg, `job "nope" not found`) {
			t.Fatalf("%s unknown id: %q", tool, msg)
		}
	}
}

// The real subcommand over stdio: `agent-jobs mcp --role job` exposes no orchestrator tool and
// refuses one; `--role orchestrator` exposes the job tools and queues a job in the agent's store.
func TestStdioSubcommand(t *testing.T) {
	home := t.TempDir()
	run := func(role string) *sdk.ClientSession {
		cmd := exec.Command(selfBin, "mcp", "--role", role, "--agent", "test")
		cmd.Env = append(os.Environ(), "AGENT_JOBS_HOME="+home)
		cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil).
			Connect(context.Background(), &sdk.CommandTransport{Command: cmd}, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cs.Close() })
		return cs
	}
	job := run("job")
	if got := toolNames(t, job); len(got) != len(JobAllowed) {
		t.Fatalf("job role over stdio lists %v", got)
	}
	if msg := callErr(t, job, "job_start", startArgs(nil)); !strings.Contains(msg, "not available to the job role") {
		t.Fatalf("job role job_start: %q", msg)
	}
	orch := run("orchestrator")
	if got := toolNames(t, orch); len(got) != len(OrchestratorOnly)+len(JobAllowed) {
		t.Fatalf("orchestrator over stdio lists %v", got)
	}
	var started JobView
	call(t, orch, "job_start", startArgs(nil), &started)
	st, err := store.Open(filepath.Join(home, "test", "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if j, err := st.Get(started.ID); err != nil || j.Status != store.StatusQueued {
		t.Fatalf("queued via stdio: %+v %v", j, err)
	}
	if out, err := exec.Command(selfBin, "mcp", "--role", "admin", "--agent", "test").CombinedOutput(); err == nil ||
		!strings.Contains(string(out), "role") {
		t.Fatalf("unknown role: %v %s", err, out)
	}
}

// No nested jobs, layer 1: under the job marker the real binary refuses every control command; only
// the job's own MCP server, the guard hook and help run.
func TestBinaryRefusesInsideJob(t *testing.T) {
	home := t.TempDir()
	env := append(os.Environ(), "AGENT_JOBS_HOME="+home, jobs.JobEnvVar+"=261017-000000-abcd")
	for _, args := range [][]string{
		{"start", "--agent", "test", "--title", "t", "--model", "opus", "--brief", "b"},
		{"cancel", "--agent", "test", "x"},
		{"tick", "--agent", "test"},
		{"daemon", "--agent", "test"},
		{"init", "--agent", "test"},
		{"status", "--agent", "test", "x"},
		{"list", "--agent", "test"},
		{"result", "--agent", "test", "x"},
		{"prune", "--agent", "test"},
		{"mcp", "--role", "orchestrator", "--agent", "test"},
		{"mcp", "--agent", "test", "--role=orchestrator"},
		{"mcp", "--agent", "test"},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cmd := exec.CommandContext(ctx, selfBin, args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		cancel()
		if err == nil || !strings.Contains(string(out), "refused: running inside job") {
			t.Fatalf("%v under the marker: err=%v out=%s", args, err, out)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "test")); err == nil {
		t.Fatal("a refused command created the agent's state dir")
	}
	// The guard hook still runs under the marker.
	cmd := exec.Command(selfBin, "guard")
	cmd.Env = env
	cmd.Stdin = strings.NewReader(`{"tool_name":"Bash","tool_input":{"command":"ls"}}`)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("guard under the marker: %v %s", err, out)
	}
	// The job's own MCP server still runs under the marker and refuses orchestrator tools.
	mc := exec.Command(selfBin, "mcp", "--role", "job", "--agent", "test")
	mc.Env = env
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil).
		Connect(context.Background(), &sdk.CommandTransport{Command: mc}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	if msg := callErr(t, cs, "job_start", startArgs(nil)); !strings.Contains(msg, "not available to the job role") {
		t.Fatalf("job role under the marker: %q", msg)
	}
	// Serve refuses the orchestrator role under the marker even when called directly.
	t.Setenv(jobs.JobEnvVar, "x")
	if err := Serve(context.Background(), RoleOrchestrator, "test"); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("Serve orchestrator under the marker: %v", err)
	}
}

// job_result never blocks on, follows or reads a non-regular report.md (the job dir is the job's to
// write): a FIFO or a symlink is reported as missing, and non-regular artifacts are not listed.
func TestJobResultRefusesNonRegularFiles(t *testing.T) {
	m := newManager(t)
	cs := connect(t, mustServer(t, m))
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i, mk := range []func(string) error{
		func(p string) error { return syscall.Mkfifo(p, 0o600) },
		func(p string) error { return os.Symlink(secret, p) },
		func(p string) error { return os.Mkdir(p, 0o700) },
	} {
		j := &store.Job{ID: "261017-12000" + string(rune('0'+i)) + "-f1f0", Agent: "test", Title: "t", Kind: "general",
			Brief: "b", Status: store.StatusSucceeded, Model: "opus", MaxTurns: 1, MaxBudgetUSD: 1, TimeoutMinutes: 1,
			CreatedAt: now, FinishedAt: &now}
		if err := m.Store.Insert(j); err != nil {
			t.Fatal(err)
		}
		dir := m.JobDir(j.ID)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := mk(filepath.Join(dir, "report.md")); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(filepath.Join(dir, "commits.patch"), 0o600); err != nil {
			t.Fatal(err)
		}
		done := make(chan JobResultOutput, 1)
		go func() {
			var out JobResultOutput
			call(t, cs, "job_result", map[string]any{"id": j.ID}, &out)
			done <- out
		}()
		select {
		case out := <-done:
			if !out.ReportMissing || out.Report != "" || !strings.Contains(out.Note, "not a regular file") ||
				len(out.Artifacts) != 0 || strings.Contains(out.Report, "SECRET") {
				t.Fatalf("case %d: %+v", i, out)
			}
		case <-time.After(10 * time.Second):
			// Unblock the stuck open (a writer arrives) so the test fails instead of hanging.
			if w, err := os.OpenFile(filepath.Join(dir, "report.md"), os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
				_ = w.Close()
			}
			t.Fatalf("case %d: job_result blocked on a non-regular report.md", i)
		}
	}
}

// No nested jobs, layer 2 (process-based): with the env marker gone, a control command started under
// a (fake, script) job wrapper is still refused by the ppid walk, while the guard hook still runs.
func TestBinaryRefusesUnderWrapperAncestor(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("ancestry check is Linux-only (/proc)")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "agent-jobs")
	body := "#!/bin/bash\n\"$REAL_BIN\" $CHILD_ARGS\necho \"exit=$?\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(childArgs string, stdin string) string {
		cmd := exec.Command(script, "run-wrapper", filepath.Join(dir, "jobs", "x"), "--token", "t", "--", "claude")
		cmd.Env = append(os.Environ(), "AGENT_JOBS_HOME="+dir, "REAL_BIN="+selfBin, "CHILD_ARGS="+childArgs)
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("fake wrapper: %v %s", err, out)
		}
		return string(out)
	}
	for _, args := range []string{"list --agent test", "tick --agent test", "start --agent test --title t --model opus --brief b",
		"mcp --role orchestrator --agent test"} {
		if out := run(args, ""); !strings.Contains(out, "refused: running under the job wrapper") || strings.Contains(out, "exit=0") {
			t.Fatalf("%s under a wrapper ancestor: %s", args, out)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "test")); err == nil {
		t.Fatal("a refused command created the agent's state dir")
	}
	if out := run("guard", `{"tool_name":"Bash","tool_input":{"command":"ls"}}`); !strings.Contains(out, "exit=0") {
		t.Fatalf("guard under a wrapper ancestor: %s", out)
	}
	// Without the wrapper in the chain the same command runs.
	cmd := exec.Command(selfBin, "list", "--agent", "test")
	cmd.Env = append(os.Environ(), "AGENT_JOBS_HOME="+dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("list without a wrapper: %v %s", err, out)
	}
}

// No nested jobs, layer 3: a process in the session of a running job (the wrapper's setsid session,
// recorded as the job's pgid) is refused even without the marker and outside the wrapper's subtree.
func TestBinaryRefusesInJobSession(t *testing.T) {
	m := newManager(t)
	home := os.Getenv("AGENT_JOBS_HOME")
	cmd := exec.Command("bash", "-c", `read x; exec "$REAL_BIN" list --agent test`)
	cmd.Env = append(os.Environ(), "REAL_BIN="+selfBin)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := m.Store.Insert(&store.Job{ID: "261017-130000-5e55", Agent: "test", Title: "t", Kind: "general", Brief: "b",
		Status: store.StatusRunning, Model: "opus", MaxTurns: 1, MaxBudgetUSD: 1, TimeoutMinutes: 1, CreatedAt: now,
		StartedAt: &now, PID: cmd.Process.Pid, PGID: cmd.Process.Pid}); err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(stdin, "\n")
	_ = stdin.Close()
	err = cmd.Wait()
	if err == nil || !strings.Contains(out.String(), "refused: running inside a job's session") {
		t.Fatalf("list in a job's session: %v %s (home %s)", err, out.String(), home)
	}
}

func TestIsWrapperCmdline(t *testing.T) {
	for _, c := range []struct {
		argv []string
		want bool
	}{
		{[]string{"/home/u/.local/bin/agent-jobs", "run-wrapper", "/s/jobs/1", "--token", "t", "--", "claude"}, true},
		{[]string{"/bin/bash", "/tmp/agent-jobs", "run-wrapper", "/s/jobs/1", "--token", "t"}, true},
		{[]string{"agent-jobs", "run-wrapper"}, false},
		{[]string{"agent-jobs", "list", "run-wrapper", "x", "--token"}, false},
		{[]string{"vim", "x", "y", "run-wrapper", "a", "--token"}, false},
	} {
		if got := jobs.IsWrapperCmdline(c.argv); got != c.want {
			t.Fatalf("%v = %v", c.argv, got)
		}
	}
}

// A tool made job-visible that needs the manager fails as a tool error in the job role (no manager),
// not with a nil dereference that would kill the job's MCP server.
func TestJobRoleToolWithoutManagerFailsCleanly(t *testing.T) {
	savedDefs, savedAllowed := toolDefs, JobAllowed
	t.Cleanup(func() { toolDefs, JobAllowed = savedDefs, savedAllowed })
	toolDefs = append(slices.Clone(savedDefs), toolDef{"dummy_count", func(s *sdk.Server, d Deps) {
		addManagerTool(s, d, &sdk.Tool{Name: "dummy_count", Description: "test"},
			func(_ context.Context, _ *sdk.CallToolRequest, _ JobListInput) (*sdk.CallToolResult, JobCancelOutput, error) {
				n, err := d.Manager.Store.CountRunning()
				return nil, JobCancelOutput{ID: fmt.Sprint(n)}, err
			})
	}})
	JobAllowed = []string{"dummy_count"}
	s, err := NewServer(RoleJob, Deps{})
	if err != nil {
		t.Fatal(err)
	}
	cs := connect(t, s)
	if got := toolNames(t, cs); !slices.Equal(got, []string{"dummy_count"}) {
		t.Fatalf("job tools %v", got)
	}
	if msg := callErr(t, cs, "dummy_count", map[string]any{}); !strings.Contains(msg, "needs the job database") {
		t.Fatalf("dummy tool without a manager: %q", msg)
	}
	if got := toolNames(t, cs); len(got) != 1 { // the server survived the call
		t.Fatalf("after the call: %v", got)
	}
}

// No nested jobs, session leader: a job script clears the marker and orphans a child (no setsid), so
// the ppid walk no longer reaches the wrapper, and the child targets ANOTHER agent's store (no matching
// pgid there). The session leader is still the wrapper, and the child is refused.
func TestOrphanInWrapperSessionRefusedForOtherAgent(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("session-leader check is Linux-only (/proc)")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "agent-jobs")
	out := filepath.Join(dir, "child.out")
	// The fake wrapper (agent A's job) leads its own session; the subshell exits at once, so the
	// child is re-parented; the wrapper waits for the child's output file to be complete.
	// `( bash -c … & )`: the outer subshell exits at once, so the inner bash (its own cmdline, not the
	// wrapper's) is re-parented and the child's ppid chain never meets the wrapper.
	body := "#!/bin/bash\n" +
		"( bash -c 'env -u AGENT_JOBS_JOB \"$REAL_BIN\" start --agent agentb --title t --model opus --brief b > \"$OUT.tmp\" 2>&1; " +
		"echo \"ppid-chain-parent=$PPID exit=$?\" >> \"$OUT.tmp\"; mv \"$OUT.tmp\" \"$OUT\"' & )\n" +
		"for _ in $(seq 200); do [ -e \"$OUT\" ] && exit 0; sleep 0.05; done\nexit 1\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(script, "run-wrapper", filepath.Join(dir, "agenta", "jobs", "x"), "--token", "t", "--", "claude")
	cmd.Env = append(os.Environ(), "AGENT_JOBS_HOME="+dir, "REAL_BIN="+selfBin, "OUT="+out, "AGENT_JOBS_JOB=x")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fake wrapper: %v %s", err, b)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "refused: running in the session of a job wrapper") || strings.Contains(string(b), "exit=0") {
		t.Fatalf("orphan in a wrapper session, other agent: %s", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "agentb")); err == nil {
		t.Fatal("the refused command created agent B's state dir")
	}
}
