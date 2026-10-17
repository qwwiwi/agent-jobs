package jobs

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// sessionsHome points claude's config dir at a temp dir; with fake set, the fake claude keeps its
// session transcripts there (else --resume finds nothing, as with a pruned session).
func sessionsHome(t *testing.T, fake bool) string {
	t.Helper()
	d := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", d)
	if fake {
		t.Setenv("FAKE_SESSIONS", d)
	} else {
		t.Setenv("FAKE_SESSIONS", "")
	}
	return d
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// startAndStop launches a job that writes wip.txt (uncommitted) and an assistant message, then
// sleeps; the test stops it while it sleeps.
func startAndStop(t *testing.T, m *Manager, mod func(*StartRequest)) *store.Job {
	t.Helper()
	j := enqueue(t, m, "FAKE_WIP FAKE_SLEEP_ONCE=30", mod)
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(m.JobDir(j.ID), "work")
	waitFor(t, "work in progress", 15*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(work, "wip.txt"))
		return err == nil && strings.Contains(readFile(t, filepath.Join(m.JobDir(j.ID), "stream.jsonl")), "WIP_PROGRESS_NOTE")
	})
	st, err := m.Stop(j.ID, "plan changed")
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// E2: job_stop ends a running job as stopped (resumable) and keeps its worktree, uncommitted work and
// session; job_resume continues the same session (--resume <same id>) in the same work dir with the
// orchestrator update as the new message, within what the first attempt left of its limits.
func TestStopRunningThenResumeSameSession(t *testing.T) {
	m := newManager(t)
	sessionsHome(t, true)
	repo := newRepo(t)
	st := startAndStop(t, m, func(r *StartRequest) { r.RepoPath = repo; r.Kind = "fix" })
	dir := m.JobDir(st.ID)
	work := filepath.Join(dir, "work")
	if st.Status != store.StatusStopped || st.Turns != 1 || !strings.Contains(st.Error, "plan changed") ||
		!ValidSessionID(st.SessionID) || st.Attempt != 1 || st.RunMode != store.RunNew {
		t.Fatalf("stopped row: %+v", st)
	}
	if !sessionExists(st.SessionID) {
		t.Fatal("the session transcript must be on disk")
	}
	if _, err := os.Stat(filepath.Join(work, "wip.txt")); err != nil {
		t.Fatalf("uncommitted work lost: %v", err)
	}
	if !strings.Contains(gitT(t, repo, "worktree", "list", "--porcelain"), work) {
		t.Fatal("worktree unregistered")
	}
	// Ticks neither notify nor clean up a stopped job; stopping it again is refused.
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if j := get(t, m, st.ID); j.Status != store.StatusStopped || j.NotifiedAt != nil || j.CleanedAt != nil {
		t.Fatalf("after tick: %+v", j)
	}
	if _, err := m.Stop(st.ID, ""); !errors.Is(err, store.ErrImmutable) {
		t.Fatalf("second stop: %v", err)
	}

	r, err := m.Resume(st.ID, "NEW_FACT_42: use the other API", Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != store.StatusQueued || r.MaxTurns != DefaultMaxTurns-1 || r.TimeoutMinutes != DefaultTimeoutMinutes-(st.DurationSec+59)/60 ||
		r.MaxBudgetUSD != DefaultMaxBudgetUSD || r.PendingContext != "NEW_FACT_42: use the other API" {
		t.Fatalf("resumed row: %+v", r)
	}
	j := runToEnd(t, m, st.ID)
	if j.Status != store.StatusSucceeded || j.Attempt != 2 || j.RunMode != store.RunResume || j.SessionID != st.SessionID {
		t.Fatalf("after resume: %+v", j)
	}
	argv := readFile(t, filepath.Join(dir, "argv.txt"))
	if !strings.Contains(argv, "--resume\n"+st.SessionID+"\n") || strings.Contains(argv, "--session-id") ||
		strings.Contains(argv, "--no-session-persistence") {
		t.Fatalf("resume argv:\n%s", argv)
	}
	stdin := readFile(t, filepath.Join(dir, "stdin.txt"))
	for _, s := range []string{"## Orchestrator update", "NEW_FACT_42: use the other API", "Continue the task with this update; keep the report contract"} {
		if !strings.Contains(stdin, s) {
			t.Fatalf("update message lacks %q:\n%s", s, stdin)
		}
	}
	if strings.Contains(stdin, "OWNER_RULE_MARKER") {
		t.Fatal("a resumed session gets only the update, not the brief again")
	}
	if _, err := os.Stat(filepath.Join(work, "wip.txt")); err != nil {
		t.Fatal("the resumed attempt must run in the same work dir")
	}
	if line := DoneLine(j); !strings.Contains(line, "resumed (attempt 2)") || strings.Contains(line, "fresh session") {
		t.Fatalf("done line: %s", line)
	}
	atts, err := m.Store.Attempts(j.ID)
	if err != nil || len(atts) != 1 || atts[0].N != 1 || atts[0].Status != store.StatusStopped || atts[0].Turns != 1 ||
		atts[0].Mode != store.RunNew || atts[0].SessionID != st.SessionID {
		t.Fatalf("attempts: %+v %v", atts, err)
	}
	if _, err := os.Stat(filepath.Join(dir, attemptsDir, "1", "stream.jsonl")); err != nil {
		t.Fatalf("attempt 1 stream not archived: %v", err)
	}
	if c := strings.Count(readFile(t, filepath.Join(dir, "launches.txt")), "launch"); c != 2 {
		t.Fatalf("launched %d times, want 2", c)
	}
}

// Cancelled and succeeded jobs cannot be resumed; resume needs context.
func TestResumeRefusedForCancelledAndSucceeded(t *testing.T) {
	m := newManager(t)
	sessionsHome(t, true)
	done := runToEnd(t, m, enqueue(t, m, "ok", nil).ID)
	if done.Status != store.StatusSucceeded {
		t.Fatalf("precondition: %s", done.Status)
	}
	cancelled := enqueue(t, m, "x", nil)
	if err := m.Cancel(cancelled.ID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{done.ID, cancelled.ID} {
		if _, err := m.Resume(id, "more", Limits{}); !errors.Is(err, store.ErrImmutable) {
			t.Fatalf("resume of %s: %v", get(t, m, id).Status, err)
		}
	}
	queued := enqueue(t, m, "y", nil)
	if _, err := m.Resume(queued.ID, "more", Limits{}); !errors.Is(err, store.ErrImmutable) {
		t.Fatalf("resume of a queued job: %v", err)
	}
	if _, err := m.Stop(queued.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Resume(queued.ID, "  ", Limits{}); err == nil {
		t.Fatal("resume without context accepted")
	}
	if _, err := m.Resume(queued.ID, "z", Limits{MaxTurns: CeilingMaxTurns + 1}); err == nil || !strings.Contains(err.Error(), "not clamped") {
		t.Fatalf("limits above the ceiling: %v", err)
	}
}

// job_amend appends to a queued job's brief and keeps its queue position; anything not queued is refused.
func TestAmendQueuedKeepsPosition(t *testing.T) {
	m := newManager(t)
	var ids []string
	for _, b := range []string{"first", "second", "third"} {
		ids = append(ids, enqueue(t, m, b, nil).ID)
		time.Sleep(10 * time.Millisecond)
	}
	j, err := m.Amend(ids[1], "AMENDED_FACT: the API moved")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(j.Brief, "second") || !strings.Contains(j.Brief, "## Orchestrator update\n\nAMENDED_FACT: the API moved") ||
		j.Status != store.StatusQueued {
		t.Fatalf("amended: %+v", j)
	}
	queued, err := m.Store.ListByStatus(store.StatusQueued)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, q := range queued {
		order = append(order, q.ID)
	}
	if !slices.Equal(order, ids) {
		t.Fatalf("queue order %v, want %v", order, ids)
	}
	if _, err := m.Amend(ids[0], " "); err == nil {
		t.Fatal("empty amend accepted")
	}
	if err := m.Cancel(ids[2]); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Amend(ids[2], "late"); !errors.Is(err, store.ErrImmutable) {
		t.Fatalf("amend of a cancelled job: %v", err)
	}
	// The amended brief is what the job gets.
	runToEnd(t, m, ids[1])
	if !strings.Contains(readFile(t, filepath.Join(m.JobDir(ids[1]), "prompt.md")), "AMENDED_FACT") {
		t.Fatal("prompt.md lacks the amendment")
	}
}

// A job stopped while queued has no session yet: resume appends the update to its brief and it
// starts as a first attempt.
func TestStopQueuedThenResumeStartsWithUpdate(t *testing.T) {
	m := newManager(t)
	sessionsHome(t, true)
	j := enqueue(t, m, "original brief", nil)
	st, err := m.Stop(j.ID, "wait")
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != store.StatusStopped || st.Attempt != 0 {
		t.Fatalf("stopped queued job: %+v", st)
	}
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if get(t, m, j.ID).Status != store.StatusStopped {
		t.Fatal("a stopped job must not launch")
	}
	if _, err := m.Resume(j.ID, "QUEUED_UPDATE", Limits{MaxTurns: 7}); err != nil {
		t.Fatal(err)
	}
	done := runToEnd(t, m, j.ID)
	if done.Status != store.StatusSucceeded || done.Attempt != 1 || done.MaxTurns != 7 || strings.Contains(DoneLine(done), "resumed") {
		t.Fatalf("after resume: %+v", done)
	}
	prompt := readFile(t, filepath.Join(m.JobDir(j.ID), "prompt.md"))
	if !strings.Contains(prompt, "original brief") || !strings.Contains(prompt, "QUEUED_UPDATE") {
		t.Fatalf("prompt:\n%s", prompt)
	}
	if !strings.Contains(readFile(t, filepath.Join(m.JobDir(j.ID), "argv.txt")), "--session-id\n"+done.SessionID+"\n") {
		t.Fatal("first attempt must pass its pre-assigned --session-id")
	}
	if atts, _ := m.Store.Attempts(j.ID); len(atts) != 0 {
		t.Fatalf("a job that never ran has no archived attempt: %+v", atts)
	}
}

// When the session transcript is gone, the next attempt starts a fresh session in the same work dir
// with the original brief, a summary of the previous attempt and the update.
func TestResumeMissingSessionFallsBack(t *testing.T) {
	m := newManager(t)
	sessionsHome(t, false) // the fake claude keeps no transcripts
	repo := newRepo(t)
	st := startAndStop(t, m, func(r *StartRequest) { r.RepoPath = repo; r.Kind = "fix" })
	if _, err := m.Resume(st.ID, "FALLBACK_UPDATE", Limits{}); err != nil {
		t.Fatal(err)
	}
	j := runToEnd(t, m, st.ID)
	if j.Status != store.StatusSucceeded || j.RunMode != store.RunFallback || j.SessionID == st.SessionID || !ValidSessionID(j.SessionID) {
		t.Fatalf("fallback row: %+v", j)
	}
	dir := m.JobDir(j.ID)
	argv := readFile(t, filepath.Join(dir, "argv.txt"))
	if strings.Contains(argv, "--resume") || !strings.Contains(argv, "--session-id\n"+j.SessionID+"\n") {
		t.Fatalf("fallback argv:\n%s", argv)
	}
	stdin := readFile(t, filepath.Join(dir, "stdin.txt"))
	for _, s := range []string{"OWNER_RULE_MARKER", "FAKE_WIP", "## Previous attempt", "ended stopped after 1 turns",
		"WIP_PROGRESS_NOTE", "FALLBACK_UPDATE", "could not be resumed", "git log " + j.BaseCommit + "..HEAD"} {
		if !strings.Contains(stdin, s) {
			t.Fatalf("fallback prompt lacks %q:\n%s", s, stdin)
		}
	}
	if line := DoneLine(j); !strings.Contains(line, "resumed (attempt 2, fresh session)") {
		t.Fatalf("done line: %s", line)
	}
}

// claude --resume failing at run time (the session exists on disk but cannot be resumed) falls back
// inside the wrapper to a fresh session; the daemon records the new session id.
func TestResumeFailureFallsBackInWrapper(t *testing.T) {
	m := newManager(t)
	sessionsHome(t, true)
	st := startAndStop(t, m, nil)
	if _, err := m.Resume(st.ID, "FAKE_RESUME_FAIL please", Limits{}); err != nil {
		t.Fatal(err)
	}
	j := runToEnd(t, m, st.ID)
	dir := m.JobDir(j.ID)
	if j.Status != store.StatusSucceeded || j.RunMode != store.RunFallback || j.SessionID == st.SessionID {
		t.Fatalf("row: %+v (stderr %s)", j, readFile(t, filepath.Join(dir, "stderr.log")))
	}
	all := strings.Split(readFile(t, filepath.Join(dir, "argv-all.txt")), "---\n")
	if len(all) != 4 || !strings.Contains(all[1], "--resume\n"+st.SessionID+"\n") || !strings.Contains(all[2], "--session-id\n"+j.SessionID+"\n") {
		t.Fatalf("launches:\n%q", all)
	}
	if !strings.Contains(readFile(t, filepath.Join(dir, "stdin.txt")), "could not be resumed") {
		t.Fatal("the fallback run must get the fallback prompt")
	}
	if _, err := os.Stat(filepath.Join(dir, "stream-resume-failed.jsonl")); err != nil {
		t.Fatal("the failed resume stream must be kept")
	}
	if !strings.Contains(readFile(t, filepath.Join(dir, "stderr.log")), "falling back to a fresh session") {
		t.Fatal("stderr.log must say why")
	}
}

// A failed job whose worktree cleanup already removed is resumed in a restored worktree: the saved
// ref, staged and unstaged changes and untracked files come back at the same path.
func TestResumeRestoresCleanedWorktree(t *testing.T) {
	m := newManager(t)
	sessionsHome(t, true)
	repo := newRepo(t)
	j := runToEnd(t, m, enqueue(t, m, "FAKE_COMMIT FAKE_EXIT3", func(r *StartRequest) { r.RepoPath = repo; r.Kind = "fix" }).ID)
	if j.Status != store.StatusFailed {
		t.Fatalf("precondition: %+v", j)
	}
	if err := m.Cleanup(j); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.MarkCleaned(j.ID, m.Now()); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(m.JobDir(j.ID), "work")
	if _, err := os.Stat(work); !os.IsNotExist(err) {
		t.Fatal("precondition: worktree removed")
	}
	r, err := m.Resume(j.ID, "fix the exit code", Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if r.NotifiedAt != nil || r.CleanedAt != nil || r.FinishedAt != nil {
		t.Fatalf("resumed row keeps old run fields: %+v", r)
	}
	done := runToEnd(t, m, j.ID)
	if done.Status != store.StatusSucceeded || done.RunMode != store.RunResume {
		t.Fatalf("after resume: %+v", done)
	}
	for f, want := range map[string]string{"committed.txt": "new\n", "tracked.txt": "v1\ndirty\n", "new-untracked.txt": "untracked\n"} {
		if got := readFile(t, filepath.Join(work, f)); got != want {
			t.Fatalf("%s = %q, want %q", f, got, want)
		}
	}
	if err := m.verifyWorktree(done, work); err != nil {
		t.Fatalf("restored worktree is not the job's: %v", err)
	}
}

// Default limits are what the previous attempt left; nothing left needs explicit limits.
func TestResumeLimits(t *testing.T) {
	j := &store.Job{MaxTurns: 80, MaxBudgetUSD: 10, TimeoutMinutes: 45, Turns: 30, CostUSD: 2.505, DurationSec: 61}
	l, err := resumeLimits(j, Limits{})
	if err != nil || l.MaxTurns != 50 || l.MaxBudgetUSD != 7.49 || l.TimeoutMinutes != 43 {
		t.Fatalf("remaining: %+v %v", l, err)
	}
	timedOut := &store.Job{MaxTurns: 80, MaxBudgetUSD: 10, TimeoutMinutes: 45, Turns: 30, CostUSD: 1, DurationSec: 45*60 + 20}
	if _, err := resumeLimits(timedOut, Limits{}); err == nil || !strings.Contains(err.Error(), "pass timeout_minutes") {
		t.Fatalf("timed out without new time: %v", err)
	}
	if l, err := resumeLimits(timedOut, Limits{TimeoutMinutes: 30}); err != nil || l.TimeoutMinutes != 30 || l.MaxTurns != 50 {
		t.Fatalf("explicit timeout: %+v %v", l, err)
	}
	if _, err := resumeLimits(j, Limits{MaxBudgetUSD: CeilingMaxBudgetUSD + 1}); err == nil {
		t.Fatal("budget above the ceiling accepted")
	}
}

// Session paths are found by the job's id only, and prune removes them with the job folder.
func TestPruneRemovesJobSessions(t *testing.T) {
	m := newManager(t)
	cfg := sessionsHome(t, true)
	j := runToEnd(t, m, enqueue(t, m, "ok", nil).ID)
	paths, err := SessionPaths(j.SessionID)
	if err != nil || len(paths) != 1 || filepath.Dir(filepath.Dir(paths[0])) != filepath.Join(cfg, "projects") {
		t.Fatalf("session paths %v %v", paths, err)
	}
	other := filepath.Join(cfg, "projects", "fake", NewSessionID()+".jsonl")
	if err := os.WriteFile(other, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	m.Now = func() time.Time { return time.Now().Add(PruneAge + time.Hour) }
	if _, err := m.Prune(false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(paths[0]); !os.IsNotExist(err) {
		t.Fatal("the job's session transcript must be pruned")
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal("prune removed another session")
	}
	if _, err := SessionPaths("../x"); err == nil {
		t.Fatal("invalid session id accepted")
	}
}

// The CLI: stop, amend, resume and result. The binary refuses its control commands under a job
// wrapper, so this test cannot run inside a job (skipped there).
func TestStopResumeAmendCLI(t *testing.T) {
	if err := CheckNotInJob(); err != nil {
		t.Skipf("CLI refuses under a job wrapper: %v", err)
	}
	m := newManager(t)
	run := func(args ...string) string {
		t.Helper()
		out, err := execSelf(args...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return out
	}
	j := enqueue(t, m, "cli brief", nil)
	run("amend", "--agent", "test", j.ID, "--context", "CLI_AMEND")
	run("stop", "--agent", "test", j.ID, "--reason", "cli stop")
	if got := get(t, m, j.ID); got.Status != store.StatusStopped || !strings.Contains(got.Brief, "CLI_AMEND") {
		t.Fatalf("after stop: %+v", got)
	}
	if out := run("resume", "--agent", "test", j.ID, "--context", "CLI_RESUME", "--max-turns", "9"); !strings.Contains(out, "resume queued") {
		t.Fatalf("resume output: %s", out)
	}
	if got := get(t, m, j.ID); got.Status != store.StatusQueued || got.MaxTurns != 9 || !strings.Contains(got.Brief, "CLI_RESUME") {
		t.Fatalf("after resume: %+v", got)
	}
	if out, err := execSelf("resume", "--agent", "test", j.ID, "--context", "again"); err == nil {
		t.Fatalf("resume of a queued job succeeded: %s", out)
	}
}
