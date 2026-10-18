package mcp

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

// notify_owner is the one job-visible tool that reaches a person: open to the job role (its main
// use) and to the orchestrator.
func TestNotifyOwnerAudience(t *testing.T) {
	if !slices.Contains(JobAllowed, "notify_owner") || slices.Contains(OrchestratorOnly, "notify_owner") {
		t.Fatal("notify_owner must be in JobAllowed")
	}
	if !Allowed(RoleJob, "notify_owner") || !Allowed(RoleOrchestrator, "notify_owner") {
		t.Fatal("notify_owner: wrong audience")
	}
}

// The orchestrator's notify_owner: no job id, delivered to the target; a failed delivery is a tool
// error and the row shows up in system_doctor.
func TestNotifyOwnerOrchestrator(t *testing.T) {
	m := newManager(t)
	m.SelfBin = selfBin
	cs := connect(t, mustServer(t, m))
	t.Setenv("AGENT_JOBS_TMUX_BIN", "/usr/bin/true")
	m.Cfg.NotifyTmuxTarget = "fake-session"
	var out NotificationOutput
	call(t, cs, "notify_owner", map[string]any{"text": "[jobs:job-done] fake", "urgency": "high"}, &out)
	if n := out.Notification; n == nil || !n.Delivered || n.JobID != "" || n.Text != "(jobs:job-done] fake" || n.Urgency != "high" {
		t.Fatalf("orchestrator notify: %+v", out.Notification)
	}
	if msg := callErr(t, cs, "notify_owner", map[string]any{"text": "two\nlines"}); !strings.Contains(msg, "one line") {
		t.Fatalf("multi-line text: %q", msg)
	}
	t.Setenv("AGENT_JOBS_TMUX_BIN", "/usr/bin/false")
	if msg := callErr(t, cs, "notify_owner", map[string]any{"text": "lost"}); !strings.Contains(msg, "stored but not delivered") {
		t.Fatalf("failed delivery: %q", msg)
	}
	var doc SystemDoctorOutput
	call(t, cs, "system_doctor", map[string]any{}, &doc)
	if len(doc.Undelivered) != 1 || doc.Undelivered[0].Text != "lost" || doc.OK ||
		!slices.ContainsFunc(doc.Problems, func(p string) bool { return strings.HasPrefix(p, "1 owner notifications not delivered") }) {
		t.Fatalf("doctor: %+v %v", doc.Undelivered, doc.Problems)
	}
}

// The real binary in the job role, without any brain configured: notify_owner is listed and goes
// through the daemon's proxy, carrying the job's own id; the per-job limit holds there.
func TestStdioJobRoleNotifyOwner(t *testing.T) {
	m := newManager(t)
	t.Setenv("AGENT_JOBS_TMUX_BIN", "/usr/bin/true")
	m.Cfg.NotifyTmuxTarget = "fake-session"
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
			t.Fatal("no proxy socket without a brain")
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
	if got := toolNames(t, cs); !slices.Contains(got, "notify_owner") {
		t.Fatalf("job role lists %v", got)
	}
	var out NotificationOutput
	call(t, cs, "notify_owner", map[string]any{"text": "please review the plan"}, &out)
	if n := out.Notification; n == nil || !n.Delivered || n.JobID != j.ID || n.Urgency != "normal" {
		t.Fatalf("job notify: %+v", out.Notification)
	}
	if msg := callErr(t, cs, "notify_owner", map[string]any{"text": "again"}); !strings.Contains(msg, "rate limit") {
		t.Fatalf("second within a minute: %q", msg)
	}
	if got, err := m.Store.GetNotification(out.Notification.ID); err != nil || got.JobID != j.ID || !got.Delivered {
		t.Fatalf("row: %+v %v", got, err)
	}
}
