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
		"j4 job 4 title (finished 4h ago), j5 job 5 title (finished 5h ago), +2 more, see job_list\n"
	if out := run("bind", bindEvent("s1", orch)); out != want {
		t.Errorf("SessionStart: got %q, want %q", out, want)
	}
	// A burst of identical prompts: the state did not change, so nothing is injected again (#166).
	for range 3 {
		if out := run("brief-reminder", prompt("s1", "hello")); out != "" {
			t.Errorf("UserPromptSubmit with unchanged state: got %q, want nothing", out)
		}
	}
	if out := run("brief-reminder", prompt("s2", "hello")); out != "" {
		t.Errorf("another session: %q", out)
	}
	if out := run("brief-reminder", prompt("s1", "[jobs:job-done] job another succeeded")); out != "" {
		t.Errorf("daemon prompt with unchanged state: got %q, want nothing", out)
	}
	withBrief := fmt.Sprintf(`{"session_id":"s1","hook_event_name":"UserPromptSubmit","cwd":%q,"prompt":"hello"}`, draftRepo(t))
	if out := run("brief-reminder", withBrief); !strings.HasPrefix(out, jobs.BriefReminderPrefix+"Checklist: ") ||
		strings.Contains(out, "[jobs:pending-outcome]") || strings.Count(out, "\n") != 1 {
		t.Errorf("checklist only, the outcome reminder was already injected: %q", out)
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

// pendingOutcomeHarness is a bound orchestrator session with a movable clock for the dedupe tests.
type pendingOutcomeHarness struct {
	t    *testing.T
	orch string
	now  time.Time
	m    *jobs.Manager
}

func newPendingOutcomeHarness(t *testing.T) *pendingOutcomeHarness {
	t.Helper()
	h := &pendingOutcomeHarness{t: t, orch: bindableAgent(t, "test"), now: time.Now().Truncate(time.Second)}
	m, err := h.open("test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	h.m = m
	return h
}

func (h *pendingOutcomeHarness) open(agent string) (*jobs.Manager, error) {
	m, err := jobs.Open(agent)
	if err == nil {
		m.Now = func() time.Time { return h.now }
	}
	return m, err
}

func (h *pendingOutcomeHarness) finish(id, title string) {
	h.t.Helper()
	finished := h.now.Add(-time.Minute)
	if err := h.m.Store.Insert(&store.Job{ID: id, Agent: "test", Title: title, Kind: "fix", Brief: "b",
		Status: store.StatusSucceeded, Model: "sonnet", CreatedAt: finished, FinishedAt: &finished, NotifiedAt: &finished}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *pendingOutcomeHarness) run(sub, input string) string {
	h.t.Helper()
	var out, errs bytes.Buffer
	if code := runHook([]string{sub, "--agent", "test"}, strings.NewReader(input), &out, &errs, h.open); code != 0 || errs.Len() != 0 {
		h.t.Fatalf("%s: code %d, stderr %q", sub, code, errs.String())
	}
	return out.String()
}

func (h *pendingOutcomeHarness) prompt() string {
	return h.run("brief-reminder", `{"session_id":"s1","hook_event_name":"UserPromptSubmit","prompt":"hello"}`)
}

func (h *pendingOutcomeHarness) bind() string { return h.run("bind", bindEvent("s1", h.orch)) }

func TestHookPendingOutcomeBurstEmitsOnce(t *testing.T) {
	h := newPendingOutcomeHarness(t)
	h.finish("a1", "first")
	if out := h.bind(); !strings.Contains(out, "a1 first") {
		t.Fatalf("SessionStart: %q", out)
	}
	for i := range 3 {
		if out := h.prompt(); out != "" {
			t.Errorf("prompt %d of the burst: %q, want nothing", i+1, out)
		}
	}
}

func TestHookPendingOutcomeChangedStateEmitsAgain(t *testing.T) {
	h := newPendingOutcomeHarness(t)
	h.finish("a1", "first")
	h.bind()
	if out := h.prompt(); out != "" {
		t.Fatalf("unchanged: %q", out)
	}
	h.finish("a2", "second")
	if out := h.prompt(); !strings.Contains(out, "2 finished jobs") || !strings.Contains(out, "a2 second") {
		t.Errorf("a new finished job: %q", out)
	}
	if out := h.prompt(); out != "" {
		t.Errorf("repeated after the new snapshot: %q", out)
	}
	if err := h.m.SetOutcome("a1", jobs.OutcomeAccepted, ""); err != nil {
		t.Fatal(err)
	}
	if out := h.prompt(); !strings.Contains(out, "1 finished jobs") || strings.Contains(out, "a1 first") {
		t.Errorf("a recorded outcome: %q", out)
	}
	if err := h.m.SetOutcome("a2", jobs.OutcomeAccepted, ""); err != nil {
		t.Fatal(err)
	}
	if out := h.prompt(); out != "" {
		t.Errorf("nothing pending: %q", out)
	}
}

func TestHookPendingOutcomeRepeatsAfterInterval(t *testing.T) {
	h := newPendingOutcomeHarness(t)
	// The prompt hook opens its store through briefReminderOpener: give it the harness clock too, so
	// every step decides at an exact offset from the bind's emission.
	opener := briefReminderOpener
	briefReminderOpener = func(agent string) (*jobs.Manager, error) {
		m, err := opener(agent)
		if err == nil {
			m.Now = func() time.Time { return h.now }
		}
		return m, err
	}
	t.Cleanup(func() { briefReminderOpener = opener })
	h.finish("a1", "first")
	h.bind()
	h.now = h.now.Add(jobs.DefaultHookRepeatAfter - time.Second)
	if out := h.prompt(); out != "" {
		t.Errorf("just before the default repeat interval: %q", out)
	}
	h.now = h.now.Add(time.Second)
	if out := h.prompt(); !strings.Contains(out, "a1 first") {
		t.Errorf("at the default repeat interval: %q", out)
	}
	if out := h.prompt(); out != "" {
		t.Errorf("after the repeat: %q", out)
	}
	writeAgentConfig(t, "test", map[string]any{"notify_tmux_target": "orch-session", "orchestrator_dir": h.orch,
		"hooks": map[string]any{"repeat_after_seconds": 60}})
	h.now = h.now.Add(59 * time.Second)
	if out := h.prompt(); out != "" {
		t.Errorf("before the configured interval: %q", out)
	}
	h.now = h.now.Add(time.Second)
	if out := h.prompt(); !strings.Contains(out, "a1 first") {
		t.Errorf("at the configured interval: %q", out)
	}
}

func TestHookPendingOutcomeCompactionFreshSnapshot(t *testing.T) {
	h := newPendingOutcomeHarness(t)
	h.finish("a1", "first")
	first := h.bind()
	if first == "" || h.prompt() != "" {
		t.Fatalf("setup: bind %q", first)
	}
	// Compaction fires SessionStart again for the same session: a new context epoch.
	if out := h.bind(); out != first {
		t.Errorf("SessionStart after compaction: got %q, want the fresh snapshot %q", out, first)
	}
	if out := h.prompt(); out != "" {
		t.Errorf("prompt after the fresh snapshot: %q", out)
	}
}

func TestHookPendingOutcomeByteCap(t *testing.T) {
	h := newPendingOutcomeHarness(t)
	writeAgentConfig(t, "test", map[string]any{"notify_tmux_target": "orch-session", "orchestrator_dir": h.orch,
		"hooks": map[string]any{"max_bytes": 600}})
	for i := 1; i <= 5; i++ {
		h.finish(fmt.Sprintf("job-%d", i), strings.Repeat("long title ", 11))
	}
	out := h.bind()
	if len(strings.TrimSuffix(out, "\n")) > 600 {
		t.Errorf("block is %d bytes, cap 600: %q", len(out), out)
	}
	if !strings.HasPrefix(out, "[jobs:pending-outcome] 5 finished jobs") || !strings.Contains(out, "more, see job_list") {
		t.Errorf("truncation marker missing: %q", out)
	}
	if !strings.Contains(out, "job-5 ") {
		t.Errorf("first job dropped: %q", out)
	}
}
