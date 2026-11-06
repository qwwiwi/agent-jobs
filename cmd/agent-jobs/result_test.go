package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// `result` reads report.md like job_result: a regular file only (a symlink is not followed, a FIFO
// does not hang the command), redacted.
func TestResultBoundedRegularReport(t *testing.T) {
	t.Setenv("AGENT_JOBS_HOME", t.TempDir())
	m, err := jobs.Open("test")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	j := &store.Job{ID: "261018-120000-feed", Agent: "test", Title: "t", Kind: "general", Brief: "b",
		Status: store.StatusSucceeded, Model: "sonnet", TimeoutMinutes: 45, CreatedAt: now, StartedAt: &now, FinishedAt: &now}
	if err := m.Store.Insert(j); err != nil {
		t.Fatal(err)
	}
	dir := m.JobDir(j.ID)
	_ = m.Close()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	report := filepath.Join(dir, "report.md")
	args := []string{"--agent", "test", j.ID}

	// A regular report: printed, redacted.
	tok := "ghp_" + strings.Repeat("Q", 36)
	if err := os.WriteFile(report, []byte("## Summary\nused "+tok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := captureStdout(t, func() error { return cmdResult(args) }); strings.Contains(out, tok) ||
		!strings.Contains(out, "[redacted:github]") {
		t.Fatalf("regular:\n%s", out)
	}

	// A symlink to a file outside: refused, the target is not printed.
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("OUTSIDE_SECRET_MARKER\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(report)
	if err := os.Symlink(outside, report); err != nil {
		t.Fatal(err)
	}
	var rerr error
	out := captureStdout(t, func() error { rerr = cmdResult(args); return nil })
	if rerr == nil || strings.Contains(out, "OUTSIDE_SECRET_MARKER") {
		t.Fatalf("symlink followed: err=%v\n%s", rerr, out)
	}

	// A FIFO: refused at once, never blocks.
	_ = os.Remove(report)
	if err := syscall.Mkfifo(report, 0o600); err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout, _ = os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	defer func() { os.Stdout = old }()
	done := make(chan error, 1)
	go func() { done <- cmdResult(args) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO report read without an error")
		}
	case <-time.After(5 * time.Second):
		// Unblock the reader so the test process can end.
		if w, err := os.OpenFile(report, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_, _ = io.WriteString(w, "x")
			_ = w.Close()
		}
		t.Fatal("result blocked on a FIFO report.md")
	}
}

// `result` of a job whose acceptance failure carries a token (cut short on the done line): no part
// of the token is printed.
func TestResultAcceptanceRedacted(t *testing.T) {
	t.Setenv("AGENT_JOBS_HOME", t.TempDir())
	m, err := jobs.Open("test")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tok := "ghp_" + strings.Repeat("Q", 36)
	j := &store.Job{ID: "261018-120000-acce", Agent: "test", Title: "t", Kind: "fix", Brief: "b",
		Status: store.StatusFailed, Model: "sonnet", TimeoutMinutes: 45, CreatedAt: now, StartedAt: &now, FinishedAt: &now,
		AcceptanceCmd: "make test", Error: "acceptance did not run " + strings.Repeat("x", 50) + " " + tok + " (see stderr.log)"}
	if err := m.Store.Insert(j); err != nil {
		t.Fatal(err)
	}
	dir := m.JobDir(j.ID)
	_ = m.Close()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte("## Summary\nok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() error { return cmdResult([]string{"--agent", "test", j.ID}) })
	if strings.Contains(out, "ghp_") || strings.Contains(out, "QQQQ") || !strings.Contains(out, "acceptance did not run") {
		t.Fatalf("result:\n%s", out)
	}
}

// `result` prints the compact view (Not done, Summary, Forks, section sizes) by default and all of
// report.md with --full; both redacted.
func TestResultCompactAndFull(t *testing.T) {
	t.Setenv("AGENT_JOBS_HOME", t.TempDir())
	m, err := jobs.Open("test")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	j := &store.Job{ID: "261106-150000-cafe", Agent: "test", Title: "t", Kind: "general", Brief: "b",
		Status: store.StatusSucceeded, Model: "sonnet", TimeoutMinutes: 45, CreatedAt: now, StartedAt: &now, FinishedAt: &now}
	if err := m.Store.Insert(j); err != nil {
		t.Fatal(err)
	}
	dir := m.JobDir(j.ID)
	_ = m.Close()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	tok := "ghp_" + strings.Repeat("Q", 36)
	report := "## Summary\nsummary text " + tok + "\n\n## Evidence\n" + strings.Repeat("evidence line\n", 500) +
		"\n## Not done / uncertain\nBLOCKED: blocker text\n\n## Forks\nnone\n\n## Progress\n- 10:00 UTC step\n"
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte(report), 0o600); err != nil {
		t.Fatal(err)
	}
	compact := captureStdout(t, func() error { return cmdResult([]string{"--agent", "test", j.ID}) })
	for _, want := range []string{"## Not done / uncertain\nBLOCKED: blocker text", "## Summary\nsummary text [redacted:github]", "## Forks\nnone",
		"Evidence 7013 B", "--full"} {
		if !strings.Contains(compact, want) {
			t.Fatalf("compact result lacks %q:\n%s", want, compact)
		}
	}
	if strings.Contains(compact, "evidence line") || strings.Contains(compact, "10:00 UTC step") || strings.Contains(compact, tok) {
		t.Fatalf("compact result is not compact or not redacted:\n%s", compact)
	}
	full := captureStdout(t, func() error { return cmdResult([]string{"--agent", "test", "--full", j.ID}) })
	if !strings.Contains(full, "evidence line") || !strings.Contains(full, "10:00 UTC step") || strings.Contains(full, tok) ||
		strings.Index(full, "BLOCKED: blocker text") > strings.Index(full, "summary text") {
		t.Fatalf("--full result:\n%s", full)
	}
}

// F1: the compact result of a report whose Evidence is bigger than the job_result response cap still
// shows "Not done / uncertain" and Forks (the CLI and job_result agree), and a report past the scan cap
// says so.
func TestResultCompactLateSections(t *testing.T) {
	t.Setenv("AGENT_JOBS_HOME", t.TempDir())
	m, err := jobs.Open("test")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	var ids []string
	for i, n := range []int{22000, jobs.MaxRedactBytes/14 + 10} {
		j := &store.Job{ID: fmt.Sprintf("261106-16000%d-cafe", i), Agent: "test", Title: "t", Kind: "general", Brief: "b",
			Status: store.StatusSucceeded, Model: "sonnet", TimeoutMinutes: 45, CreatedAt: now, StartedAt: &now, FinishedAt: &now}
		if err := m.Store.Insert(j); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(m.JobDir(j.ID), 0o700); err != nil {
			t.Fatal(err)
		}
		report := "## Summary\nfixed it\n\n## Evidence\n" + strings.Repeat("evidence line\n", n) +
			"\n## Not done / uncertain\nBLOCKED: late blocker\n\n## Forks\n- late / fork / reason\n"
		if err := os.WriteFile(filepath.Join(m.JobDir(j.ID), "report.md"), []byte(report), 0o600); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, j.ID)
	}
	_ = m.Close()
	out := captureStdout(t, func() error { return cmdResult([]string{"--agent", "test", ids[0]}) })
	if !strings.Contains(out, "## Not done / uncertain\nBLOCKED: late blocker") || !strings.Contains(out, "## Forks\n- late / fork / reason") ||
		strings.Contains(out, "report cut at") {
		t.Fatalf("late sections lost:\n%s", out)
	}
	out = captureStdout(t, func() error { return cmdResult([]string{"--agent", "test", ids[1]}) })
	if strings.Contains(out, "late blocker") || !strings.Contains(out, "INCOMPLETE SCAN") {
		t.Fatalf("report past the scan cap is not marked:\n%s", out)
	}
}
