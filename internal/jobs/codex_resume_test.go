package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// fakeCodexThread is the thread id the fake codex starts a new thread with.
const fakeCodexThread = "0199a213-81c0-7800-8aa1-bbab2a035a53"

// runCodexToRest ticks until the job is neither queued nor running (an automatic continuation is run
// through too).
func runCodexToRest(t *testing.T, m *Manager, id string) *store.Job {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if err := m.Tick(); err != nil {
			t.Fatal(err)
		}
		if j := get(t, m, id); j.Status != store.StatusQueued && j.Status != store.StatusRunning {
			return j
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("job %s did not come to rest", id)
	return nil
}

func codexArgvOf(t *testing.T, m *Manager, id string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(m.JobDir(id), "argv.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A codex job runs without --ephemeral and its thread id (thread.started) is recorded as the attempt's
// session; with codex_resumable off it runs ephemeral and records none.
func TestCodexThreadIDRecorded(t *testing.T) {
	m := newCodexManager(t)
	j := runToEnd(t, m, enqueue(t, m, "review the thing", codexReview).ID)
	if j.Status != store.StatusSucceeded {
		t.Fatalf("got %s %q", j.Status, j.Error)
	}
	if strings.Contains(codexArgvOf(t, m, j.ID), "--ephemeral") {
		t.Fatalf("a resumable codex job must not run --ephemeral:\n%s", codexArgvOf(t, m, j.ID))
	}
	if j.SessionID != fakeCodexThread {
		t.Fatalf("session id %q, want the codex thread id", j.SessionID)
	}

	off := false
	m.Cfg.CodexResumable = &off
	writeCodexConfig(t, m, `{"codex_resumable": false}`)
	j = runToEnd(t, m, enqueue(t, m, "review the thing", codexReview).ID)
	if j.Status != store.StatusSucceeded {
		t.Fatalf("got %s %q", j.Status, j.Error)
	}
	if !strings.Contains(codexArgvOf(t, m, j.ID), "\n--ephemeral\n") || j.SessionID != "" {
		t.Fatalf("codex_resumable off: session %q argv:\n%s", j.SessionID, codexArgvOf(t, m, j.ID))
	}
}

// job_resume of a codex job continues its thread with `codex exec resume <thread_id>` under the same
// isolation flags, with the update on stdin.
func TestCodexResumeContinuesThread(t *testing.T) {
	m := newCodexManager(t)
	j := runToEnd(t, m, enqueue(t, m, "FAKE_CODEX_ONCEFAIL", codexReview).ID)
	if j.Status != store.StatusFailed || j.SessionID != fakeCodexThread {
		t.Fatalf("first attempt: %s %q session %q", j.Status, j.Error, j.SessionID)
	}
	first := codexArgvOf(t, m, j.ID)
	if _, err := m.Resume(j.ID, "the model is back, go on", Limits{}); err != nil {
		t.Fatalf("resume of a codex job: %v", err)
	}
	j = runCodexToRest(t, m, j.ID)
	if j.Status != store.StatusSucceeded || j.Attempt != 2 || j.RunMode != store.RunResume || j.SessionID != fakeCodexThread {
		t.Fatalf("resumed: %s %q attempt %d mode %q session %q", j.Status, j.Error, j.Attempt, j.RunMode, j.SessionID)
	}
	want := strings.TrimSuffix(first, "-\n") + "resume\n" + fakeCodexThread + "\n-\n"
	if got := codexArgvOf(t, m, j.ID); got != want {
		t.Fatalf("resume argv:\n%s\nwant:\n%s", got, want)
	}
	stdin, _ := os.ReadFile(filepath.Join(m.JobDir(j.ID), "stdin.txt"))
	if !strings.Contains(string(stdin), "the model is back, go on") || strings.Contains(string(stdin), "FAKE_CODEX_ONCEFAIL") {
		t.Fatalf("the resumed thread gets the update only, got:\n%s", stdin)
	}
}

// A thread whose rollout file is gone cannot be resumed: the attempt starts a fresh thread with the
// brief, the previous attempt and the update.
func TestCodexResumeFallsBackWithoutRollout(t *testing.T) {
	m := newCodexManager(t)
	j := runToEnd(t, m, enqueue(t, m, "FAKE_CODEX_ONCEFAIL", codexReview).ID)
	if j.Status != store.StatusFailed {
		t.Fatalf("first attempt: %s %q", j.Status, j.Error)
	}
	if err := os.RemoveAll(filepath.Join(os.Getenv("CODEX_HOME"), "sessions")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Resume(j.ID, "go on", Limits{}); err != nil {
		t.Fatal(err)
	}
	j = runCodexToRest(t, m, j.ID)
	if j.Status != store.StatusSucceeded || j.RunMode != store.RunFallback {
		t.Fatalf("fallback: %s %q mode %q", j.Status, j.Error, j.RunMode)
	}
	if argv := codexArgvOf(t, m, j.ID); strings.Contains(argv, "resume") {
		t.Fatalf("fallback must start a new thread:\n%s", argv)
	}
	stdin, _ := os.ReadFile(filepath.Join(m.JobDir(j.ID), "stdin.txt"))
	for _, want := range []string{"FAKE_CODEX_ONCEFAIL", "## Previous attempt", "go on", "previous codex session could not be resumed"} {
		if !strings.Contains(string(stdin), want) {
			t.Fatalf("fallback prompt lacks %q:\n%s", want, stdin)
		}
	}
}

// `codex exec resume` starts a new thread when it does not find the id: the wrapper fails the attempt
// instead of letting a thread without the brief run on.
func TestCodexResumeOtherThreadFails(t *testing.T) {
	m := newCodexManager(t)
	j := runToEnd(t, m, enqueue(t, m, "FAKE_CODEX_ONCEFAIL", codexReview).ID)
	t.Setenv("FAKE_CODEX_LOSE_THREAD", "1")
	if _, err := m.Resume(j.ID, "go on", Limits{}); err != nil {
		t.Fatal(err)
	}
	j = runCodexToRest(t, m, j.ID)
	if j.Status != store.StatusFailed || !strings.Contains(j.Error, "did not resume thread "+fakeCodexThread) {
		t.Fatalf("got %s %q", j.Status, j.Error)
	}
}

// A codex attempt that timed out is continued once, in its thread; the continuation that ends is not
// continued again.
func TestCodexTimeoutAutoContinuedOnce(t *testing.T) {
	m := newCodexManager(t)
	j := enqueue(t, m, "FAKE_CODEX_SLEEP=60", func(r *StartRequest) { codexReview(r); r.TimeoutMinutes = 1 })
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "fake codex", 5*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(m.JobDir(j.ID), "argv.txt"))
		return err == nil
	})
	real := time.Now
	m.Now = func() time.Time { return real().Add(2 * time.Minute) }
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	m.Now = real
	j = get(t, m, j.ID)
	if _, ok := autoContinuedFrom(j.PendingContext); !ok || j.Attempt != 2 ||
		(j.Status != store.StatusQueued && j.Status != store.StatusRunning) {
		t.Fatalf("timed-out codex attempt not auto-continued: %s attempt %d %q ctx %q", j.Status, j.Attempt, j.Error, j.PendingContext)
	}
	j = runCodexToRest(t, m, j.ID)
	if j.Status != store.StatusSucceeded || j.RunMode != store.RunResume {
		t.Fatalf("continuation: %s %q mode %q", j.Status, j.Error, j.RunMode)
	}
	if argv := codexArgvOf(t, m, j.ID); !strings.Contains(argv, "resume\n"+fakeCodexThread+"\n-\n") {
		t.Fatalf("continuation argv:\n%s", argv)
	}
	if line := m.DoneLineFor(j); !strings.Contains(line, "auto-continued (attempt 2") {
		t.Fatalf("done line %q", line)
	}
}

// A codex attempt that ended its turn without a report is continued once; the resumed thread reports
// its total usage, of which the attempt counts only its own part.
func TestCodexNoReportAutoContinuedUsagePerAttempt(t *testing.T) {
	m := newCodexManager(t)
	j := runCodexToRest(t, m, enqueue(t, m, "FAKE_CODEX_ONCENOREPORT", codexReview).ID)
	if j.Status != store.StatusSucceeded || j.Attempt != 2 || j.RunMode != store.RunResume {
		t.Fatalf("got %s %q attempt %d mode %q", j.Status, j.Error, j.Attempt, j.RunMode)
	}
	res := m.endedResult(j)
	if res.InputTokens != 1200 || res.CachedInputTokens != 800 || res.OutputTokens != 90 || res.ReasoningTokens != 30 {
		t.Fatalf("attempt 2 usage %d/%d/%d/%d, want its own 1200/800/90/30", res.InputTokens, res.CachedInputTokens, res.OutputTokens, res.ReasoningTokens)
	}
	if want := (TokenUsage{Input: 400, Output: 90, CacheRead: 800}); res.Tokens != want {
		t.Fatalf("priced usage %+v, want %+v", res.Tokens, want)
	}
	// The done line sums the attempts.
	if line := m.DoneLineFor(j); !strings.Contains(line, "tokens in 2400 (cached 1600) · out 180 (reasoning 60)") {
		t.Fatalf("done line %q", line)
	}
}

// The done line of a codex job carries its token usage and marks the cost as an estimate.
func TestCodexDoneLineUsage(t *testing.T) {
	m := newCodexManager(t)
	m.Cfg.ModelPrices = map[string]ModelPrice{"gpt-6.1-sol": {Input: 2, Output: 10, CacheRead: 0.5}}
	j := runToEnd(t, m, enqueue(t, m, "review the thing", codexReview).ID)
	if j.Status != store.StatusSucceeded {
		t.Fatalf("got %s %q", j.Status, j.Error)
	}
	m.recordStatsOf(j.ID)
	line := m.DoneLineFor(j)
	if !strings.Contains(line, "· tokens in 1200 (cached 800) · out 90 (reasoning 30) ·") {
		t.Fatalf("done line lacks the usage: %q", line)
	}
	if !strings.Contains(line, " est.") || strings.Contains(line, "cost unknown") {
		t.Fatalf("done line must mark the cost as an estimate: %q", line)
	}
	if log, _ := os.ReadFile(filepath.Join(m.JobDir(j.ID), "stderr.log")); !strings.Contains(string(log),
		"codex usage: input 1200 (cached 800), output 90 (reasoning 30) tokens") {
		t.Fatalf("usage not logged: %s", log)
	}
}

// codex_max_tokens caps one attempt's tokens: the wrapper stops codex once the stream's usage is over
// it, and the attempt fails with a clear error, whatever codex wrote or however it exited.
func TestCodexTokenCap(t *testing.T) {
	m := newCodexManager(t)
	noAutoContinue(m)
	limit := int64(1000)
	m.Cfg.CodexMaxTokens = &limit
	writeCodexConfig(t, m, `{"codex_max_tokens": 1000, "auto_continue": false}`)
	start := time.Now()
	// The fake lingers 60 s after its usage event: the wrapper must not wait for it.
	j := runCodexToRest(t, m, enqueue(t, m, "FAKE_CODEX_LINGER", codexReview).ID)
	if j.Status != store.StatusFailed || !strings.Contains(j.Error, "codex token cap") ||
		!strings.Contains(j.Error, "1290 of 1000") {
		t.Fatalf("got %s %q", j.Status, j.Error)
	}
	if d := time.Since(start); d > 25*time.Second {
		t.Fatalf("the wrapper waited %s for codex after the cap", d)
	}
	// Under the cap nothing changes.
	limit = 5000
	writeCodexConfig(t, m, `{"codex_max_tokens": 5000}`)
	if j := runToEnd(t, m, enqueue(t, m, "review the thing", codexReview).ID); j.Status != store.StatusSucceeded {
		t.Fatalf("under the cap: %s %q", j.Status, j.Error)
	}
}

func TestCodexArgvResume(t *testing.T) {
	j := &store.Job{Kind: "review", Model: "gpt-6.1-sol"}
	base := codexArgs(j, "high", CodexSandboxReadOnly, t.TempDir(), false, codexSession{resume: fakeCodexThread})
	ca, err := codexArgvSandbox(append([]string{"codex"}, base...))
	if err != nil || ca.resume != fakeCodexThread || ca.ephemeral || ca.sandbox != CodexSandboxReadOnly {
		t.Fatalf("resume argv: %+v %v", ca, err)
	}
	ca, err = codexArgvSandbox(append([]string{"codex"}, CodexArgs(j, "high", CodexSandboxReadOnly, t.TempDir(), false)...))
	if err != nil || ca.resume != "" || !ca.ephemeral {
		t.Fatalf("ephemeral argv: %+v %v", ca, err)
	}
	n := len(base)
	for name, argv := range map[string][]string{
		"not a thread id":    append(append([]string{}, base[:n-2]...), "--last", "-"),
		"id is an option":    append(append([]string{}, base[:n-2]...), "--yolo", "-"),
		"resume not last":    append(append([]string{}, base...), "-s", "danger-full-access"),
		"resume with a tail": append(append([]string{}, base[:n-1]...), "extra", "-"),
		"fork":               append(append([]string{}, base[:n-3]...), "fork", fakeCodexThread, "-"),
	} {
		if _, err := codexArgvSandbox(append([]string{"codex"}, argv...)); err == nil {
			t.Fatalf("%s: admitted %v", name, argv[n-4:])
		}
	}
	for id, want := range map[string]bool{
		fakeCodexThread: true, "0199A213-81c0-7800-8aa1-bbab2a035a53": false, "--last": false, "": false,
		"0199a213-81c0-7800-8aa1-bbab2a035a53x": false,
	} {
		if ValidCodexThreadID(id) != want {
			t.Fatalf("ValidCodexThreadID(%q) != %v", id, want)
		}
	}
}

func TestParseCodexStream0172Usage(t *testing.T) {
	p := filepath.Join(t.TempDir(), "stream.jsonl")
	lines := []string{
		`{"type":"thread.started","thread_id":"` + fakeCodexThread + `"}`,
		`{"type":"turn.started"}`,
		`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"## Summary\nok"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":40,"cache_write_input_tokens":0,"output_tokens":10,"reasoning_output_tokens":7}}`,
	}
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := ParseCodexStream(p)
	if r.CodexThreadID != fakeCodexThread || r.ReasoningTokens != 7 || r.InputTokens != 100 {
		t.Fatalf("parsed %+v", r)
	}
}
