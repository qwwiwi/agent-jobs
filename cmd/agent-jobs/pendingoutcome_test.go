package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

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
