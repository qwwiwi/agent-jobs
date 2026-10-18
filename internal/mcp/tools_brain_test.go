package mcp

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qwwiwi/agent-jobs/internal/brain"
	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

var brainTools = []string{"memory_search", "memory_write", "memory_already_exists", "task_create", "task_list",
	"task_update", "task_get", "send_message", "inbox"}

func brainServer(t *testing.T, role Role, b brain.Brain, jobID string) *sdk.ClientSession {
	t.Helper()
	d := Deps{Brain: b, JobID: jobID, Author: "sa-test"}
	if role == RoleOrchestrator {
		d.Manager = newManager(t)
	}
	s, err := NewServer(role, d)
	if err != nil {
		t.Fatal(err)
	}
	return connect(t, s)
}

// Reference, "MCP tools": the orchestrator gets every memory, task and team tool; a job gets
// memory_search and memory_write only (plus notify_owner, tools_notify_test.go), and the job-role
// server refuses the rest.
func TestBrainToolsPerRole(t *testing.T) {
	orch := toolNames(t, brainServer(t, RoleOrchestrator, &brain.Fake{}, ""))
	for _, n := range brainTools {
		if !slices.Contains(orch, n) {
			t.Fatalf("orchestrator lacks %s: %v", n, orch)
		}
	}
	job := brainServer(t, RoleJob, &brain.Fake{}, "j1")
	if got := toolNames(t, job); !slices.Equal(got, []string{"memory_search", "memory_write", "notify_owner"}) {
		t.Fatalf("job role lists %v", got)
	}
	for _, n := range brainTools[2:] {
		if msg := callErr(t, job, n, map[string]any{}); !strings.Contains(msg, "not available to the job role") {
			t.Fatalf("job role %s: %q", n, msg)
		}
	}
}

// Without a brain every brain tool fails with "brain not configured", and the job tools still work.
func TestBrainToolsNotConfigured(t *testing.T) {
	cs := brainServer(t, RoleOrchestrator, nil, "")
	args := map[string]map[string]any{
		"memory_search":         {"query": "q"},
		"memory_write":          {"kind": "runbook", "title": "t", "body": "b", "tags": []string{"x"}},
		"memory_already_exists": {"text": "t"},
		"task_create":           {"title": "t"},
		"task_list":             {},
		"task_update":           {"task_id": 1, "status": "progress"},
		"task_get":              {"task_id": 1},
		"send_message":          {"to": "sa-a", "text": "hi"},
		"inbox":                 {},
	}
	for _, n := range brainTools {
		if msg := callErr(t, cs, n, args[n]); !strings.Contains(msg, "brain not configured") {
			t.Fatalf("%s without brain: %q", n, msg)
		}
	}
	var started JobView
	call(t, cs, "job_start", startArgs(nil), &started)
	if started.ID == "" {
		t.Fatal("job_start broken without a brain")
	}
}

func TestMemoryToolsWithFake(t *testing.T) {
	f := &brain.Fake{Notes: []brain.Note{
		{Path: "30-decisions/a.md", Score: 0.8, Snippet: "A"},
		{Path: "70-runbooks/b.md", Score: 0.3, Snippet: "B"},
	}}
	cs := brainServer(t, RoleOrchestrator, f, "")

	var found NotesOutput
	call(t, cs, "memory_search", map[string]any{"query": "deploy", "limit": 1}, &found)
	if len(found.Notes) != 1 || found.Notes[0].Path != "30-decisions/a.md" || f.Queries[0] != "deploy" {
		t.Fatalf("memory_search: %+v %v", found, f.Queries)
	}
	call(t, cs, "memory_search", map[string]any{"query": "deploy"}, &found)
	if len(found.Notes) != 2 {
		t.Fatalf("memory_search default limit: %+v", found)
	}
	for _, a := range []map[string]any{{"query": " "}, {"query": "q", "limit": 21}} {
		callErr(t, cs, "memory_search", a)
	}

	var ex MemoryAlreadyExistsOutput
	call(t, cs, "memory_already_exists", map[string]any{"text": "deploy"}, &ex)
	if !ex.Exists || ex.MinScore != brain.DefaultAlreadyExistsMinScore || len(ex.Matches) != 2 {
		t.Fatalf("memory_already_exists: %+v", ex)
	}
	call(t, cs, "memory_already_exists", map[string]any{"text": "deploy", "min_score": 0.9}, &ex)
	if ex.Exists || ex.MinScore != 0.9 {
		t.Fatalf("memory_already_exists 0.9: %+v", ex)
	}
	callErr(t, cs, "memory_already_exists", map[string]any{"text": ""})

	var w ObjectOutput
	// The orchestrator cannot forge a job tag either: caller tags with the prefix are dropped.
	call(t, cs, "memory_write", map[string]any{"kind": "decision", "title": "T", "body": "B",
		"tags": []string{"x", brain.JobTagPrefix + "261017-000000-dead"}}, &w)
	if w.Result["result"] == nil || len(f.Written) != 1 || !slices.Equal(f.Written[0].Tags, []string{"x"}) || f.Written[0].Author != "" {
		t.Fatalf("memory_write: %+v %+v", w, f.Written)
	}
	for _, a := range []map[string]any{
		{"kind": "essay", "title": "T", "body": "B", "tags": []string{"x"}},
		{"kind": "runbook", "title": "T", "body": "B"},
		{"kind": "error_pattern", "title": "T", "body": "B", "tags": []string{"x"}},
	} {
		callErr(t, cs, "memory_write", a)
	}
	if len(f.Written) != 1 {
		t.Fatal("an invalid note was written")
	}

	f.Err = errors.New("recall index offline")
	if msg := callErr(t, cs, "memory_search", map[string]any{"query": "q"}); !strings.Contains(msg, "recall index offline") {
		t.Fatalf("brain error: %q", msg)
	}
}

// A job's notes carry its provenance (job tag that cannot be forged or dropped, author
// <agent>/job:<id>, marker line); a job cannot write a decision; a job can read memory as well.
func TestJobRoleMemoryTools(t *testing.T) {
	f := &brain.Fake{Notes: []brain.Note{{Path: "p.md", Score: 1}}}
	cs := brainServer(t, RoleJob, f, "261017-120000-beef")
	var found NotesOutput
	call(t, cs, "memory_search", map[string]any{"query": "q"}, &found)
	if len(found.Notes) != 1 {
		t.Fatalf("job memory_search: %+v", found)
	}
	var w ObjectOutput
	call(t, cs, "memory_write", map[string]any{"kind": "runbook", "title": "T", "body": "B",
		"tags": []string{"x", brain.JobTagPrefix + "261017-000000-dead"}}, &w)
	n := f.Written[0]
	if !slices.Equal(n.Tags, []string{"x", brain.JobTagPrefix + "261017-120000-beef"}) || n.Author != "sa-test/job:261017-120000-beef" ||
		brain.NoteJob(brain.Note{Snippet: n.Body}) != "261017-120000-beef" {
		t.Fatalf("job note: %+v", n)
	}
	if msg := callErr(t, cs, "memory_write", map[string]any{"kind": "decision", "title": "T", "body": "B", "tags": []string{"x"}}); !strings.Contains(msg, "cannot write a decision") {
		t.Fatalf("job decision: %q", msg)
	}
}

func TestTaskToolsWithFake(t *testing.T) {
	f := &brain.Fake{}
	cs := brainServer(t, RoleOrchestrator, f, "")
	var c ObjectOutput
	call(t, cs, "task_create", map[string]any{"title": "write docs", "assignee": "sa-thrall"}, &c)
	if c.Result["id"] != float64(1) || c.Result["status"] != "new" || c.Result["priority"] != "medium" {
		t.Fatalf("task_create: %+v", c)
	}
	call(t, cs, "task_create", map[string]any{"title": "other", "assignee": "sa-claude"}, &c)
	callErr(t, cs, "task_create", map[string]any{"title": ""})

	var l TaskListOutput
	call(t, cs, "task_list", map[string]any{"assignee": "sa-thrall"}, &l)
	if len(l.Tasks) != 1 || l.Tasks[0]["title"] != "write docs" {
		t.Fatalf("task_list: %+v", l)
	}
	callErr(t, cs, "task_list", map[string]any{"limit": 201})

	var u ObjectOutput
	call(t, cs, "task_update", map[string]any{"task_id": 1, "priority": "high", "status": "progress"}, &u)
	if u.Result["status"] != "progress" || u.Result["priority"] != "high" {
		t.Fatalf("task_update: %+v", u)
	}
	if msg := callErr(t, cs, "task_update", map[string]any{"task_id": 1, "status": "done"}); !strings.Contains(msg, "invalid transition") {
		t.Fatalf("progress→done: %q", msg)
	}
	for _, a := range []map[string]any{{"task_id": 1}, {"task_id": 1, "status": "archived"}, {"task_id": 0, "status": "review"}} {
		callErr(t, cs, "task_update", a)
	}

	var g ObjectOutput
	call(t, cs, "task_get", map[string]any{"task_id": 1}, &g)
	if g.Result["status"] != "progress" {
		t.Fatalf("task_get: %+v", g)
	}
	if msg := callErr(t, cs, "task_get", map[string]any{"task_id": 7}); !strings.Contains(msg, "not found") {
		t.Fatalf("task_get unknown: %q", msg)
	}
	callErr(t, cs, "task_get", map[string]any{"task_id": -1})
}

func TestTeamToolsWithFake(t *testing.T) {
	f := &brain.Fake{Pending: []brain.Object{
		{"task_id": "d1", "payload": map[string]any{"text": "review PR"}},
		{"task_id": "d2", "payload": map[string]any{"text": "ping"}},
	}}
	cs := brainServer(t, RoleOrchestrator, f, "")
	var s ObjectOutput
	call(t, cs, "send_message", map[string]any{"to": "sa-silvana", "text": "done"}, &s)
	if s.Result["to_agent"] != "sa-silvana" || len(f.Sent) != 1 {
		t.Fatalf("send_message: %+v", s)
	}
	if p := f.Sent[0]["payload"].(brain.Object); p["text"] != "done" || p["via"] != ServerName {
		t.Fatalf("payload: %+v", p)
	}
	for _, a := range []map[string]any{{"to": "no such agent", "text": "x"}, {"to": "sa-a", "text": ""},
		{"to": "sa-a", "text": strings.Repeat("x", MaxMessageBytes+1)}} {
		callErr(t, cs, "send_message", a)
	}

	var in InboxOutput
	call(t, cs, "inbox", map[string]any{}, &in)
	if len(in.Messages) != 2 || len(in.Acked) != 0 {
		t.Fatalf("inbox: %+v", in)
	}
	call(t, cs, "inbox", map[string]any{"ack": []string{"d1"}}, &in)
	if len(in.Messages) != 1 || in.Messages[0]["task_id"] != "d2" || !slices.Equal(in.Acked, []string{"d1"}) {
		t.Fatalf("inbox after ack: %+v", in)
	}
	if msg := callErr(t, cs, "inbox", map[string]any{"ack": []string{"d9"}}); !strings.Contains(msg, `ack "d9"`) {
		t.Fatalf("ack unknown: %q", msg)
	}
	callErr(t, cs, "inbox", map[string]any{"limit": 101})
}

// The real binary in the job role, with no brain in config.json: memory_search answers "brain not
// configured"; a broken brain block refuses to start the server.
func TestStdioJobRoleWithoutBrain(t *testing.T) {
	home := t.TempDir()
	cmd := exec.Command(selfBin, "mcp", "--role", "job", "--agent", "test")
	cmd.Env = append(os.Environ(), "AGENT_JOBS_HOME="+home, jobs.JobEnvVar+"=261017-120000-beef")
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil).
		Connect(context.Background(), &sdk.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	if msg := callErr(t, cs, "memory_search", map[string]any{"query": "q"}); !strings.Contains(msg, "brain not configured") {
		t.Fatalf("stdio job memory_search: %q", msg)
	}
	if err := os.MkdirAll(home+"/test", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(home+"/test/config.json", []byte(`{"brain":{"recall_url":"http://brain.example/mcp","token_env":"X"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	bad := exec.Command(selfBin, "mcp", "--role", "job", "--agent", "test")
	bad.Env = append(os.Environ(), "AGENT_JOBS_HOME="+home, jobs.JobEnvVar+"=261017-120000-beef")
	if out, err := bad.CombinedOutput(); err == nil || !strings.Contains(string(out), "loopback") {
		t.Fatalf("bad brain config: %v %s", err, out)
	}
	// Without the job marker the job role refuses to start: its notes would carry no job.
	noJob := exec.Command(selfBin, "mcp", "--role", "job", "--agent", "test")
	noJob.Env = append(os.Environ(), "AGENT_JOBS_HOME="+t.TempDir())
	if out, err := noJob.CombinedOutput(); err == nil || !strings.Contains(string(out), jobs.JobEnvVar+" is not set") {
		t.Fatalf("job role without job id: %v %s", err, out)
	}
}

// M1: the real binary in the job role never reads the brain token (the variable is unset and the
// call still works): memory_search and memory_write go through the daemon's proxy, which gives the
// note the job's provenance.
func TestStdioJobRoleThroughDaemonProxy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_JOBS_HOME", home)
	if err := os.MkdirAll(home+"/test", 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := `{"brain":{"recall_url":"http://127.0.0.1:9/mcp","memory_url":"http://127.0.0.1:9/mcp","token_env":"AJ_TEST_UNSET_BRAIN_TOKEN"}}`
	if err := os.WriteFile(home+"/test/config.json", []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := jobs.Open("test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	m.Log = log.New(io.Discard, "", 0)
	f := &brain.Fake{Notes: []brain.Note{{Path: "70-runbooks/a.md", Score: 0.7, Snippet: "A"}}}
	m.Brain = f
	j, err := m.Enqueue(jobs.StartRequest{Title: "t", Kind: "general", Brief: "b", Model: "sonnet"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Store.Claim(j.ID, "tok", time.Now()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.ServeBrain(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.Now().Add(5 * time.Second)
	for _, err := os.Stat(jobs.BrainSocketPath(m.StateDir)); err != nil; _, err = os.Stat(jobs.BrainSocketPath(m.StateDir)) {
		if time.Now().After(deadline) {
			t.Fatal("no brain socket")
		}
		time.Sleep(20 * time.Millisecond)
	}

	cmd := exec.Command(selfBin, "mcp", "--role", "job", "--agent", "test")
	cmd.Env = append(os.Environ(), jobs.JobEnvVar+"="+j.ID)
	// The proxy binds a call to the caller's session: the server leads one, recorded as the job's.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil).
		Connect(context.Background(), &sdk.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	if err := m.Store.SetPID(j.ID, "tok", cmd.Process.Pid, cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	var found NotesOutput
	call(t, cs, "memory_search", map[string]any{"query": "q"}, &found)
	if len(found.Notes) != 1 || found.Notes[0].Path != "70-runbooks/a.md" {
		t.Fatalf("job memory_search via proxy: %+v", found)
	}
	var w ObjectOutput
	call(t, cs, "memory_write", map[string]any{"kind": "runbook", "title": "T", "body": "B", "tags": []string{"x"}}, &w)
	if len(f.Written) != 1 || f.Written[0].Author != "test/job:"+j.ID || !slices.Contains(f.Written[0].Tags, brain.JobTagPrefix+j.ID) {
		t.Fatalf("job note via proxy: %+v", f.Written)
	}
}
