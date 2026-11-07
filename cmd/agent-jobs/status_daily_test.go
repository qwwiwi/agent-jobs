package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

// captureStdout runs f and returns what it printed to stdout.
func captureStdout(t *testing.T, f func() error) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	ferr := f()
	os.Stdout = old
	_ = w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if ferr != nil {
		t.Fatal(ferr)
	}
	return string(out)
}

// `status` without a job id shows the daily automation and its last run.
func TestStatusShowsDaily(t *testing.T) {
	t.Setenv("AGENT_JOBS_HOME", t.TempDir())
	m, err := jobs.Open("test")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	out := captureStdout(t, func() error { return printDaily(m) })
	if !strings.Contains(out, "after 04:00 local time") || !strings.Contains(out, "last run:  never") {
		t.Fatalf("never ran:\n%s", out)
	}
	now := time.Now()
	if ok, err := m.Store.ClaimDaily("2026-10-18", now); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	res := `{"jobs_pruned":2,"bytes_freed":10,"ghosts":1,"suspects":1,"procs_error":"x"}`
	if err := m.Store.FinishDaily("2026-10-18", now, res); err != nil {
		t.Fatal(err)
	}
	out = captureStdout(t, func() error { return printDaily(m) })
	for _, want := range []string{"last run:  2026-10-18", "prune:     2 jobs, 10 bytes freed",
		"procs:     1 ghosts (0 job, 0 leaders, 1 suspects)", "error:     procs: x"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
	m.Cfg.DailyOff = true
	if out = captureStdout(t, func() error { return printDaily(m) }); !strings.Contains(out, "daily:     off") {
		t.Fatalf("off:\n%s", out)
	}
}

// `status` without a job id shows the queue and the configured max_running (config, default 2).
func TestStatusShowsQueue(t *testing.T) {
	t.Setenv("AGENT_JOBS_HOME", t.TempDir())
	m, err := jobs.Open("test")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if out := captureStdout(t, func() error { return printQueue(m) }); !strings.Contains(out, "queue:     0 running, 0 queued (configured max_running 2)") {
		t.Fatalf("default:\n%s", out)
	}
	five := 5
	m.Cfg.MaxRunning = &five
	if out := captureStdout(t, func() error { return printQueue(m) }); !strings.Contains(out, "(configured max_running 5)") {
		t.Fatalf("configured:\n%s", out)
	}
}

// `status` without a job id ends with the undelivered owner notifications of the last 24 h.
func TestStatusShowsUndeliveredNotifications(t *testing.T) {
	t.Setenv("AGENT_JOBS_HOME", t.TempDir())
	m, err := jobs.Open("test")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	out := captureStdout(t, func() error { return printUndelivered(m) })
	if !strings.Contains(out, "no undelivered owner notifications") {
		t.Fatalf("none:\n%s", out)
	}
	if _, err := m.NotifyOwner("job-7", "need a decision", "high"); err == nil { // no notify target configured
		t.Fatal("delivered without a target")
	}
	out = captureStdout(t, func() error { return printUndelivered(m) })
	for _, want := range []string{"1 owner notifications not delivered", "job job-7 (high): need a decision", "notify_tmux_target"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
}

// status shows the box-wide gate lock and a running job waiting for it (#208).
func TestStatusShowsGateLockWait(t *testing.T) {
	t.Setenv("AGENT_JOBS_HOME", t.TempDir())
	m, err := jobs.Open("test")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	j, err := m.Enqueue(jobs.StartRequest{Title: "t", Kind: "general", Brief: "do it", Model: "sonnet"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Store.Claim(j.ID, "tok", time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(m.JobDir(j.ID), 0o700); err != nil {
		t.Fatal(err)
	}
	st := jobs.GateWaitState{Waits: []jobs.GateWaitEntry{{PID: os.Getpid(), Since: time.Now().Add(-30 * time.Second).UTC(),
		Holder: "pid 1 job other since x: scripts/check.sh"}}}
	b, _ := json.Marshal(st)
	if err := os.WriteFile(filepath.Join(m.JobDir(j.ID), jobs.GateWaitFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() error { return printQueue(m) })
	for _, want := range []string{"gate lock: ", "gate wait: " + j.ID + " waiting for gate lock held by pid 1 job other since x: scripts/check.sh (waited 3"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}
