package main

import (
	"bytes"
	"database/sql"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

type hookDiagnosticWriter struct {
	bytes.Buffer
	goroutine string
}

func (w *hookDiagnosticWriter) Write(p []byte) (int, error) {
	var stack [64]byte
	w.goroutine = string(bytes.Fields(stack[:runtime.Stack(stack[:], false)])[1])
	return w.Buffer.Write(p)
}

func TestHookBriefReminderPendingOutcomeError(t *testing.T) {
	orch := bindableAgent(t, "test")
	m, err := jobs.Open("test")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	var out bytes.Buffer
	var errs hookDiagnosticWriter
	if code := runHook([]string{"bind", "--agent", "test"}, strings.NewReader(bindEvent("s1", orch)), &out, &errs, open); code != 0 || errs.Len() != 0 {
		t.Fatalf("bind: exit %d, stderr %q", code, errs.String())
	}
	input := fmt.Sprintf(`{"session_id":"s1","hook_event_name":"UserPromptSubmit","cwd":%q,"prompt":"hello"}`, draftRepo(t))
	run := func() int {
		t.Helper()
		out.Reset()
		errs.Reset()
		return runHook([]string{"brief-reminder", "--agent", "test"}, strings.NewReader(input), &out, &errs, open)
	}
	if code := run(); code != 0 || errs.Len() != 0 || !strings.HasPrefix(out.String(), jobs.BriefReminderPrefix+"Checklist: ") {
		t.Fatalf("checklist reminder: exit %d, stdout %q, stderr %q", code, out.String(), errs.String())
	}
	want := out.String()
	finished := time.Now().Add(-time.Hour)
	if err := m.Store.Insert(&store.Job{ID: "bad-gate", Agent: "test", Title: "finished job", Kind: "fix", Brief: "b",
		Status: store.StatusSucceeded, Model: "sonnet", CreatedAt: finished, FinishedAt: &finished}); err != nil {
		t.Fatal(err)
	}
	dir, err := jobs.StateDir("test")
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE jobs SET gate_input = '{' WHERE id = 'bad-gate'`); err != nil {
		t.Fatal(err)
	}
	if code := run(); code != 0 || out.String() != want {
		t.Errorf("pending-outcome error: exit %d, stdout %q, want %q", code, out.String(), want)
	}
	if !strings.HasPrefix(errs.String(), "agent-jobs hook brief-reminder: ") ||
		!strings.Contains(errs.String(), "job bad-gate: gate_input: unexpected end of JSON input") ||
		!strings.HasSuffix(errs.String(), "(no pending-outcome reminder)\n") {
		t.Errorf("pending-outcome diagnostic: %q", errs.String())
	}
	t.Run("stderr_on_hook_goroutine", func(t *testing.T) {
		var stack [64]byte
		caller := string(bytes.Fields(stack[:runtime.Stack(stack[:], false)])[1])
		if code := run(); code != 0 {
			t.Fatalf("brief-reminder: exit %d", code)
		}
		if errs.goroutine != caller {
			t.Errorf("stderr written by goroutine %s, want hook caller %s", errs.goroutine, caller)
		}
	})
}

func TestHookPendingOutcome(t *testing.T) {
	orch := bindableAgent(t, "test")
	now := time.Now().Truncate(time.Second)
	opener := func(agent string) (*jobs.Manager, error) {
		m, err := jobs.Open(agent)
		if err == nil {
			m.Now = func() time.Time { return now }
		}
		return m, err
	}
	m, err := opener("test")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	for i := 1; i <= 7; i++ {
		finished := now.Add(-time.Duration(i) * time.Hour)
		if err := m.Store.Insert(&store.Job{ID: fmt.Sprintf("j%d", i), Agent: "test", Title: fmt.Sprintf("job %d\n title", i),
			Kind: "fix", Brief: "b", Status: store.StatusSucceeded, Model: "sonnet", CreatedAt: finished,
			FinishedAt: &finished, NotifiedAt: &finished}); err != nil {
			t.Fatal(err)
		}
	}
	run := func(sub, input string) string {
		t.Helper()
		var out, errs bytes.Buffer
		code := runHook([]string{sub, "--agent", "test"}, strings.NewReader(input), &out, &errs, opener)
		if code != 0 || errs.Len() != 0 {
			t.Fatalf("%s: code %d, stderr %q", sub, code, errs.String())
		}
		return out.String()
	}
	prompt := func(session, text string) string {
		return fmt.Sprintf(`{"session_id":%q,"hook_event_name":"UserPromptSubmit","prompt":%q}`, session, text)
	}
	if out := run("brief-reminder", prompt("s1", "hello")); out != "" {
		t.Fatalf("unbound session: %q", out)
	}
	want := "[jobs:pending-outcome] 7 finished jobs without job_outcome: j1 job 1 title (finished 1h ago), " +
		"j2 job 2 title (finished 2h ago), j3 job 3 title (finished 3h ago), " +
		"j4 job 4 title (finished 4h ago), j5 job 5 title (finished 5h ago), and 2 more\n"
	if out := run("bind", bindEvent("s1", orch)); out != want {
		t.Errorf("SessionStart: got %q, want %q", out, want)
	}
	for range 2 {
		if out := run("brief-reminder", prompt("s1", "hello")); out != want {
			t.Errorf("UserPromptSubmit: got %q, want %q", out, want)
		}
	}
	if out := run("brief-reminder", prompt("s2", "hello")); out != "" {
		t.Errorf("another session: %q", out)
	}
	if out := run("brief-reminder", prompt("s1", "[jobs:job-done] job another succeeded")); out != want {
		t.Errorf("daemon prompt: got %q, want %q", out, want)
	}
	withBrief := fmt.Sprintf(`{"session_id":"s1","hook_event_name":"UserPromptSubmit","cwd":%q,"prompt":"hello"}`, draftRepo(t))
	if out := run("brief-reminder", withBrief); !strings.HasPrefix(out, jobs.BriefReminderPrefix+"Checklist: ") ||
		!strings.HasSuffix(out, want) || strings.Count(out, "[jobs:pending-outcome]") != 1 || strings.Count(out, "\n") != 2 {
		t.Errorf("checklist and one outcome reminder: %q", out)
	}
	if out := run("bind", bindEvent("restarted", orch)); out != want {
		t.Errorf("SessionStart after restart: got %q, want %q", out, want)
	}
	if out := run("brief-reminder", prompt("s1", "hello")); out != "" {
		t.Errorf("previous session after restart: %q", out)
	}
	if out := run("bind", bindEvent("s1", orch)); out != want {
		t.Errorf("rebind: got %q, want %q", out, want)
	}
	for i := 1; i <= 7; i++ {
		if err := m.SetOutcome(fmt.Sprintf("j%d", i), jobs.OutcomeAccepted, ""); err != nil {
			t.Fatal(err)
		}
		out := run("brief-reminder", prompt("s1", "hello"))
		if strings.Contains(out, fmt.Sprintf("j%d job", i)) || (i == 7 && out != "") {
			t.Fatalf("after outcome for j%d: %q", i, out)
		}
	}
	if out := run("bind", bindEvent("restarted", orch)); out != "" {
		t.Errorf("SessionStart after outcomes: %q", out)
	}
}
