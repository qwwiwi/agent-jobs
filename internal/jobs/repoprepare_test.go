package jobs

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// withPrepareConfig writes config.json with the given prepare steps (plus extra JSON members) and
// loads it into m.Cfg (the wrapper reads the file, the daemon m.Cfg).
func withPrepareConfig(t *testing.T, m *Manager, steps []PrepareStep, extra string) {
	t.Helper()
	b, err := json.Marshal(steps)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"prepare": ` + string(b) + extra + `}`
	if err := os.WriteFile(filepath.Join(m.StateDir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(m.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	m.Cfg = c
}

func inRepo(repo string) func(*StartRequest) {
	return func(r *StartRequest) { r.RepoPath = repo; r.Kind = "fix" }
}

// The prepare step runs in the worktree before the runner starts (stream.jsonl does not exist yet), in
// the dir it names, with the runner's environment (the real HOME), the job's git environment and
// marker; its output goes to prepare.log and its result into the trusted record job_status reads.
func TestPrepareRunsBeforeRunner(t *testing.T) {
	m := newManager(t)
	repo := newRepo(t)
	if err := os.Mkdir(filepath.Join(repo, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "sub", "keep.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "add", ".")
	gitT(t, repo, "commit", "-qm", "sub")
	t.Setenv("PREPARE_TEST_VAR", "seen")
	withPrepareConfig(t, m, []PrepareStep{
		{RepoGlob: "no-such-repo", Command: "touch ../../wrong-step"},
		{RepoGlob: filepath.Base(repo), Dir: "sub", Command: "echo PREP_OK; test ! -e ../../stream.jsonl && echo before > ../../prep-order.txt; " +
			"pwd > ../../prep-cwd.txt; env > ../../prep-env.txt; echo dep > prepared.txt"},
	}, "")
	j := runToEnd(t, m, enqueue(t, m, "do it", inRepo(repo)).ID)
	if j.Status != store.StatusSucceeded || j.Error != "" {
		t.Fatalf("got %s err=%q", j.Status, j.Error)
	}
	dir := m.JobDir(j.ID)
	if got := strings.TrimSpace(readFile(t, filepath.Join(dir, "prep-order.txt"))); got != "before" {
		t.Fatalf("prepare did not run before the runner: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "wrong-step")); err == nil {
		t.Fatal("a prepare step whose repo_glob does not match ran")
	}
	if cwd := strings.TrimSpace(readFile(t, filepath.Join(dir, "prep-cwd.txt"))); !samePath(cwd, filepath.Join(dir, "work", "sub")) {
		t.Fatalf("prepare cwd %s", cwd)
	}
	if _, err := os.Stat(filepath.Join(dir, "work", "sub", "prepared.txt")); err != nil {
		t.Fatalf("prepared file missing in the worktree: %v", err)
	}
	env := readFile(t, filepath.Join(dir, "prep-env.txt"))
	if !strings.Contains(env, JobEnvVar+"="+j.ID) || !strings.Contains(env, "GIT_CONFIG_NOSYSTEM=1") ||
		!envHas(env, "PREPARE_TEST_VAR", "seen") {
		t.Fatalf("prepare env lacks the job marker, the git env or the runner's variables:\n%s", env)
	}
	if home := os.Getenv("HOME"); home != "" && !envHas(env, "HOME", home) {
		t.Fatalf("prepare env does not keep the real HOME %s:\n%s", home, env)
	}
	if log := readFile(t, filepath.Join(dir, PrepareLog)); !strings.Contains(log, "PREP_OK") {
		t.Fatalf("prepare.log: %q", log)
	}
	p := m.PrepareResultOf(j)
	if p == nil || !p.Ran || p.Exit != 0 || p.TimedOut || p.Dir != "sub" || !strings.HasPrefix(p.Command, "echo PREP_OK") ||
		prepareFailure(*p) != "" {
		t.Fatalf("prepare result: %+v", p)
	}
}

// A failing prepare step fails the job before the runner starts, with a clear error.
func TestPrepareFailureFailsJobEarly(t *testing.T) {
	m := newManager(t)
	repo := newRepo(t)
	withPrepareConfig(t, m, []PrepareStep{{RepoGlob: filepath.Base(repo), Command: "echo PREP_BAD >&2; exit 4"}}, "")
	j := runToEnd(t, m, enqueue(t, m, "do it", inRepo(repo)).ID)
	if j.Status != store.StatusFailed || !strings.HasPrefix(j.Error, "prepare failed (exit 4 after ") ||
		!strings.Contains(j.Error, "see prepare.log") {
		t.Fatalf("got %s err=%q", j.Status, j.Error)
	}
	dir := m.JobDir(j.ID)
	if _, err := os.Stat(filepath.Join(dir, "stream.jsonl")); err == nil {
		t.Fatal("the runner started after a failed prepare step")
	}
	if log := readFile(t, filepath.Join(dir, PrepareLog)); !strings.Contains(log, "PREP_BAD") {
		t.Fatalf("prepare.log: %q", log)
	}
	if p := m.PrepareResultOf(j); p == nil || !p.Ran || p.Exit != 4 {
		t.Fatalf("prepare result: %+v", p)
	}
	if line := DoneLine(j, 0); !strings.Contains(line, "prepare failed (exit 4") {
		t.Fatalf("done line: %s", line)
	}
}

// A prepare dir that is missing in the worktree fails the job: the step did not run.
func TestPrepareMissingDirFailsJob(t *testing.T) {
	m := newManager(t)
	repo := newRepo(t)
	withPrepareConfig(t, m, []PrepareStep{{RepoGlob: filepath.Base(repo), Dir: "terminal", Command: "true"}}, "")
	j := runToEnd(t, m, enqueue(t, m, "do it", inRepo(repo)).ID)
	if j.Status != store.StatusFailed || !strings.HasPrefix(j.Error, "prepare did not run: dir \"terminal\"") {
		t.Fatalf("got %s err=%q", j.Status, j.Error)
	}
}

// No prepare configured (or none matching): no prepare step, no prepare.log, no result.
func TestPrepareNoneConfiguredNoChange(t *testing.T) {
	for name, steps := range map[string][]PrepareStep{
		"none":     nil,
		"no-match": {{RepoGlob: "/elsewhere/*", Command: "touch ../ran"}},
	} {
		t.Run(name, func(t *testing.T) {
			m := newManager(t)
			repo := newRepo(t)
			withPrepareConfig(t, m, steps, "")
			j := runToEnd(t, m, enqueue(t, m, "do it", inRepo(repo)).ID)
			if j.Status != store.StatusSucceeded {
				t.Fatalf("got %s %q", j.Status, j.Error)
			}
			dir := m.JobDir(j.ID)
			for _, f := range []string{PrepareLog, "ran"} {
				if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
					t.Fatalf("%s exists without a matching prepare step", f)
				}
			}
			if p := m.PrepareResultOf(j); p != nil {
				t.Fatalf("prepare result without a step: %+v", p)
			}
		})
	}
}

// A job without a repository never runs a prepare step, whatever the globs say.
func TestPrepareSkipsJobWithoutRepo(t *testing.T) {
	m := newManager(t)
	withPrepareConfig(t, m, []PrepareStep{{RepoGlob: "*", Command: "touch ../ran"}}, "")
	j := runToEnd(t, m, enqueue(t, m, "do it", nil).ID)
	if j.Status != store.StatusSucceeded {
		t.Fatalf("got %s %q", j.Status, j.Error)
	}
	if _, err := os.Stat(filepath.Join(m.JobDir(j.ID), "ran")); err == nil {
		t.Fatal("prepare ran for a job without a repository")
	}
}

// A resumed attempt keeps the prepared worktree: the step runs once, and its first-attempt result is
// still what job_status shows.
func TestPrepareNotRerunOnResume(t *testing.T) {
	m := newManager(t)
	sessionsHome(t, true)
	repo := newRepo(t)
	withPrepareConfig(t, m, []PrepareStep{{RepoGlob: filepath.Base(repo), Command: "echo run >> ../prep-runs.txt"}}, "")
	j := runToEnd(t, m, enqueue(t, m, "FAKE_EXIT3", inRepo(repo)).ID)
	if j.Status != store.StatusFailed {
		t.Fatalf("attempt 1: %s %q", j.Status, j.Error)
	}
	if _, err := m.Resume(j.ID, "go on", Limits{MaxBudgetUSD: 1}); err != nil {
		t.Fatal(err)
	}
	j = runToEnd(t, m, j.ID)
	if j.Status != store.StatusSucceeded || j.Attempt != 2 {
		t.Fatalf("attempt 2: %s %q (attempt %d)", j.Status, j.Error, j.Attempt)
	}
	if n := strings.Count(readFile(t, filepath.Join(m.JobDir(j.ID), "prep-runs.txt")), "run"); n != 1 {
		t.Fatalf("prepare ran %d times, want 1", n)
	}
	if p := m.PrepareResultOf(j); p == nil || !p.Ran || p.Exit != 0 {
		t.Fatalf("prepare result after resume: %+v", p)
	}
}

// A codex job gets the prepare step too, before codex starts.
func TestPrepareRunsForCodexJob(t *testing.T) {
	m := newCodexManager(t)
	repo := newRepo(t)
	withPrepareConfig(t, m, []PrepareStep{{RepoGlob: filepath.Base(repo),
		Command: "test ! -e ../stream.jsonl && echo before > ../prep-order.txt"}}, `, "codex_jobs_per_day": 0`)
	j := runToEnd(t, m, enqueue(t, m, "review the thing", func(r *StartRequest) { codexReview(r); r.RepoPath = repo }).ID)
	if j.Status != store.StatusSucceeded {
		t.Fatalf("got %s %q", j.Status, j.Error)
	}
	if got := strings.TrimSpace(readFile(t, filepath.Join(m.JobDir(j.ID), "prep-order.txt"))); got != "before" {
		t.Fatalf("prepare did not run before codex: %q", got)
	}
	if p := m.PrepareResultOf(j); p == nil || p.Exit != 0 {
		t.Fatalf("prepare result: %+v", p)
	}
}

func TestPrepareMatches(t *testing.T) {
	for _, c := range []struct {
		glob, repo string
		want       bool
	}{
		{"agent-jobs", "/home/u/agent-jobs", true},
		{"agent-*", "/home/u/agent-jobs", true},
		{"agent-jobs", "/home/u/other", false},
		{"/home/u/*", "/home/u/agent-jobs", true},
		{"/home/*", "/home/u/agent-jobs", false},
		{"*", "", false},
	} {
		if got := (PrepareStep{RepoGlob: c.glob}).Matches(c.repo); got != c.want {
			t.Errorf("%q matches %q = %v, want %v", c.glob, c.repo, got, c.want)
		}
	}
}

func TestPrepareConfigValidation(t *testing.T) {
	for _, bad := range []string{
		`{"prepare": [{"command": "true"}]}`,
		`{"prepare": [{"repo_glob": "[", "command": "true"}]}`,
		`{"prepare": [{"repo_glob": "x", "command": " "}]}`,
		`{"prepare": [{"repo_glob": "x", "command": "a\nb"}]}`,
		`{"prepare": [{"repo_glob": "x", "command": "true", "dir": "/abs"}]}`,
		`{"prepare": [{"repo_glob": "x", "command": "true", "dir": "../up"}]}`,
		`{"prepare": [{"repo_glob": "x", "command": "true", "timeout_minutes": 61}]}`,
		`{"prepare": [{"repo_glob": "x", "command": "true", "timeout_minutes": -1}]}`,
	} {
		if _, err := parseConfig(t.TempDir(), []byte(bad)); err == nil || !strings.Contains(err.Error(), "prepare[0]") {
			t.Errorf("%s: err = %v, want a prepare[0] error", bad, err)
		}
	}
	c, err := parseConfig(t.TempDir(), []byte(`{"prepare": [{"repo_glob": "x", "dir": "terminal", "command": "pnpm install --frozen-lockfile", "timeout_minutes": 20}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if s := c.PrepareFor("/r/x"); s == nil || s.Timeout().Minutes() != 20 || (PrepareStep{}).Timeout().Minutes() != DefaultPrepareTimeoutMinutes {
		t.Fatalf("PrepareFor: %+v", s)
	}
}

// The prepare dir must stay inside the worktree with its links resolved.
func TestPrepareDirStaysInWorktree(t *testing.T) {
	work := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(work, "out")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"out", "file", "missing"} {
		if _, err := prepareDir(work, rel); err == nil {
			t.Errorf("prepareDir(%q) accepted", rel)
		}
	}
	if d, err := prepareDir(work, ""); err != nil || d != work {
		t.Fatalf("prepareDir(\"\") = %q, %v", d, err)
	}
}

func TestPrepareFailureText(t *testing.T) {
	for _, c := range []struct {
		r    PrepareResult
		want string
	}{
		{PrepareResult{Ran: true}, ""},
		{PrepareResult{Ran: true, Exit: 2, DurationSec: 3}, "prepare failed (exit 2 after 3s); see prepare.log"},
		{PrepareResult{Ran: true, Exit: store.AcceptanceTimedOut, TimedOut: true, DurationSec: 600}, "prepare failed (timeout after 600s); see prepare.log"},
		{PrepareResult{Failure: "prepare did not run: x"}, "prepare did not run: x"},
	} {
		if got := prepareFailure(c.r); got != c.want {
			t.Errorf("prepareFailure(%+v) = %q, want %q", c.r, got, c.want)
		}
	}
}

// A resume after a prepare step that did not pass runs it again (the runner never started, so the
// worktree is still the base commit's); once it passed, a later resume does not.
func TestPrepareRerunOnResumeAfterFailure(t *testing.T) {
	m := newManager(t)
	sessionsHome(t, true)
	repo := newRepo(t)
	// Fails on attempt 1 (no attempts dir yet), passes once attempt 1 is archived.
	withPrepareConfig(t, m, []PrepareStep{{RepoGlob: filepath.Base(repo), Command: "echo run >> ../prep-runs.txt; test -d ../attempts"}}, "")
	j := runToEnd(t, m, enqueue(t, m, "FAKE_EXIT3", inRepo(repo)).ID)
	if j.Status != store.StatusFailed || !strings.HasPrefix(j.Error, "prepare failed (exit 1") {
		t.Fatalf("attempt 1: %s %q", j.Status, j.Error)
	}
	if _, err := m.Resume(j.ID, "go on", Limits{MaxBudgetUSD: 1}); err != nil {
		t.Fatal(err)
	}
	if p := m.PrepareResultOf(get(t, m, j.ID)); p == nil || p.Exit != 1 {
		t.Fatalf("prepare result of the queued resume: %+v", p)
	}
	j = runToEnd(t, m, j.ID)
	// The brief's FAKE_EXIT3 fails the runner of attempt 2: prepare passed, the runner ran.
	if j.Attempt != 2 || !strings.Contains(j.Error, "exited with code 3") {
		t.Fatalf("attempt 2: %s %q (attempt %d)", j.Status, j.Error, j.Attempt)
	}
	if p := m.PrepareResultOf(j); p == nil || p.Exit != 0 || prepareFailure(*p) != "" {
		t.Fatalf("prepare result of attempt 2: %+v", p)
	}
	if _, err := m.Resume(j.ID, "go on", Limits{MaxBudgetUSD: 1}); err != nil {
		t.Fatal(err)
	}
	j = runToEnd(t, m, j.ID)
	if j.Attempt != 3 || j.Status != store.StatusSucceeded {
		t.Fatalf("attempt 3: %s %q (attempt %d)", j.Status, j.Error, j.Attempt)
	}
	if n := strings.Count(readFile(t, filepath.Join(m.JobDir(j.ID), "prep-runs.txt")), "run"); n != 2 {
		t.Fatalf("prepare ran %d times, want 2 (attempts 1 and 2)", n)
	}
}

// While the command runs, its process group is in the job row (cancel, stop and the timeout end it
// without a scope too) and the record shows an unfinished step; afterwards the group is cleared.
func TestPrepareRecordsGroupAndProgress(t *testing.T) {
	m := newManager(t)
	repo := newRepo(t)
	withPrepareConfig(t, m, []PrepareStep{{RepoGlob: filepath.Base(repo), Command: "touch ../prep-started; sleep 2"}}, "")
	j := enqueue(t, m, "do it", inRepo(repo))
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "prepare start", 10*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(m.JobDir(j.ID), "prep-started"))
		return err == nil
	})
	running := get(t, m, j.ID)
	if running.AcceptancePGID <= 0 {
		t.Fatalf("prepare process group not recorded: %d", running.AcceptancePGID)
	}
	if p := m.PrepareResultOf(running); p == nil || p.Ran || p.Failure != prepareUnfinished {
		t.Fatalf("in-progress prepare result: %+v", p)
	}
	j = runToEnd(t, m, j.ID)
	if j.Status != store.StatusSucceeded || j.AcceptancePGID != 0 {
		t.Fatalf("got %s %q, acceptance pgid %d", j.Status, j.Error, j.AcceptancePGID)
	}
	if p := m.PrepareResultOf(j); p == nil || !p.Ran || p.Exit != 0 || p.DurationSec < 1 {
		t.Fatalf("prepare result: %+v", p)
	}
}

// The prepare state is recorded before its command runs and before the runner starts, or the job
// stops there (fail closed): a failed "unfinished" record runs no command, a failed "passed" record
// starts no runner, so a durable "unfinished" never stands for a runner that ran.
func TestPrepareRecordFailureFailsClosed(t *testing.T) {
	for _, c := range []struct{ fault, errPrefix string }{
		{"unfinished", "prepare did not run: record the step's start: "},
		{"passed", "prepare failed: its passed result could not be recorded: "},
	} {
		t.Run(c.fault, func(t *testing.T) {
			m := newManager(t)
			repo := newRepo(t)
			withPrepareConfig(t, m, []PrepareStep{{RepoGlob: filepath.Base(repo), Command: "touch ../prep-ran"}}, "")
			t.Setenv("AGENT_JOBS_TEST_PREPARE_RECORD_FAULT", c.fault)
			j := runToEnd(t, m, enqueue(t, m, "do it", inRepo(repo)).ID)
			if j.Status != store.StatusFailed || !strings.HasPrefix(j.Error, c.errPrefix) || !strings.Contains(j.Error, "injected record fault") {
				t.Fatalf("got %s %q, want failed with %q", j.Status, j.Error, c.errPrefix)
			}
			dir := m.JobDir(j.ID)
			if _, err := os.Stat(filepath.Join(dir, "stream.jsonl")); err == nil {
				t.Fatal("the runner started although the prepare state was not recorded")
			}
			_, err := os.Stat(filepath.Join(dir, "prep-ran"))
			if ran := err == nil; ran != (c.fault == "passed") {
				t.Fatalf("prepare command ran = %v with the %s record failing", ran, c.fault)
			}
			if p := m.PrepareResultOf(j); p == nil || prepareFailure(*p) == "" && c.fault == "unfinished" {
				t.Fatalf("prepare result: %+v", p)
			}
		})
	}
}

// An attempt that left no wrapper record (refused to start) between a failed prepare step and the
// next resume does not hide the failure: the newest attempt with a record decides, so the step runs
// again.
func TestPrepareRerunSkipsAttemptsWithoutRecord(t *testing.T) {
	m := newManager(t)
	sessionsHome(t, true)
	repo := newRepo(t)
	withPrepareConfig(t, m, []PrepareStep{{RepoGlob: filepath.Base(repo), Command: "echo run >> ../prep-runs.txt; test -d ../attempts"}}, "")
	j := runToEnd(t, m, enqueue(t, m, "do it", inRepo(repo)).ID)
	if j.Status != store.StatusFailed || !strings.HasPrefix(j.Error, "prepare failed (exit 1") {
		t.Fatalf("attempt 1: %s %q", j.Status, j.Error)
	}
	if _, err := m.Resume(j.ID, "go on", Limits{MaxBudgetUSD: 1}); err != nil {
		t.Fatal(err)
	}
	// Attempt 2 is refused at launch, as launchQueued does it: claimed, failed, no wrapper record.
	token := newToken()
	if err := m.Store.Claim(j.ID, token, m.Now()); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.Finish(j.ID, token, store.StatusFailed, 0, 0, 0, "refused to start: test", m.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Resume(j.ID, "go on", Limits{MaxBudgetUSD: 1}); err != nil {
		t.Fatal(err)
	}
	j = runToEnd(t, m, j.ID)
	if j.Attempt != 3 || j.Status != store.StatusSucceeded {
		t.Fatalf("attempt 3: %s %q (attempt %d)", j.Status, j.Error, j.Attempt)
	}
	if n := strings.Count(readFile(t, filepath.Join(m.JobDir(j.ID), "prep-runs.txt")), "run"); n != 2 {
		t.Fatalf("prepare ran %d times, want 2 (attempts 1 and 3)", n)
	}
}

// A prepare step that runs past its time limit is ended with its process group and fails the job
// before the runner starts ("prepare failed (timeout after Ns)").
func TestPrepareTimeoutFailsJob(t *testing.T) {
	m := newManager(t)
	repo := newRepo(t)
	withPrepareConfig(t, m, []PrepareStep{{RepoGlob: filepath.Base(repo), Command: "echo PREP_STARTED; sleep 30"}}, "")
	t.Setenv("AGENT_JOBS_TEST_PREPARE_TIMEOUT_MS", "300")
	start := time.Now()
	j := runToEnd(t, m, enqueue(t, m, "do it", inRepo(repo)).ID)
	if j.Status != store.StatusFailed || !strings.HasPrefix(j.Error, "prepare failed (timeout after ") {
		t.Fatalf("got %s %q", j.Status, j.Error)
	}
	if took := time.Since(start); took > 12*time.Second {
		t.Fatalf("the timed-out step took %s", took)
	}
	dir := m.JobDir(j.ID)
	if _, err := os.Stat(filepath.Join(dir, "stream.jsonl")); err == nil {
		t.Fatal("the runner started after a timed-out prepare step")
	}
	if log := readFile(t, filepath.Join(dir, PrepareLog)); !strings.Contains(log, "PREP_STARTED") {
		t.Fatalf("prepare.log: %q", log)
	}
	if p := m.PrepareResultOf(j); p == nil || !p.Ran || !p.TimedOut {
		t.Fatalf("prepare result: %+v", p)
	}
}

// A stop during the prepare step ends its command; the runner never starts, the step counts as not
// passed, and the resume runs it again.
func TestPrepareStoppedThenRerun(t *testing.T) {
	m := newManager(t)
	sessionsHome(t, true)
	repo := newRepo(t)
	withPrepareConfig(t, m, []PrepareStep{{RepoGlob: filepath.Base(repo),
		Command: "echo run >> ../prep-runs.txt; test -d ../attempts || sleep 30"}}, "")
	j := enqueue(t, m, "do it", inRepo(repo))
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "prepare start", 10*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(m.JobDir(j.ID), "prep-runs.txt"))
		return err == nil
	})
	if _, err := m.Stop(j.ID, "test"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "stopped", 15*time.Second, func() bool {
		_ = m.Tick()
		return get(t, m, j.ID).Status == store.StatusStopped
	})
	j = get(t, m, j.ID)
	if _, err := os.Stat(filepath.Join(m.JobDir(j.ID), "stream.jsonl")); err == nil {
		t.Fatal("the runner started after a stopped prepare step")
	}
	if p := m.PrepareResultOf(j); p == nil || prepareFailure(*p) == "" {
		t.Fatalf("prepare result of the stopped attempt: %+v", p)
	}
	if _, err := m.Resume(j.ID, "go on", Limits{MaxBudgetUSD: 1}); err != nil {
		t.Fatal(err)
	}
	j = runToEnd(t, m, j.ID)
	if j.Status != store.StatusSucceeded || j.Attempt != 2 {
		t.Fatalf("attempt 2: %s %q (attempt %d)", j.Status, j.Error, j.Attempt)
	}
	if n := strings.Count(readFile(t, filepath.Join(m.JobDir(j.ID), "prep-runs.txt")), "run"); n != 2 {
		t.Fatalf("prepare ran %d times, want 2", n)
	}
}

// What the prepare command leaves running outside its process group (a job-control background job,
// as a package manager's daemon) is ended by the session sweep before the runner starts; a leftover
// the sweep cannot end (made unkillable through the test-only hook) fails the job before the runner.
func TestPrepareLeftoverProcesses(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("session sweep reads /proc (Linux)")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash missing")
	}
	cmd := `bash -c 'set -m; sleep 30 > /dev/null 2>&1 & echo $! > ../leftover.pid'`
	t.Run("ended", func(t *testing.T) {
		m := newManager(t)
		repo := newRepo(t)
		withPrepareConfig(t, m, []PrepareStep{{RepoGlob: filepath.Base(repo), Command: cmd}}, "")
		j := runToEnd(t, m, enqueue(t, m, "do it", inRepo(repo)).ID)
		if j.Status != store.StatusSucceeded {
			t.Fatalf("got %s %q", j.Status, j.Error)
		}
		var pid int
		if _, err := fmt.Sscan(readFile(t, filepath.Join(m.JobDir(j.ID), "leftover.pid")), &pid); err != nil || pid <= 1 {
			t.Fatalf("leftover pid: %d %v", pid, err)
		}
		t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
		waitFor(t, "leftover ended", 5*time.Second, func() bool { return !procAlive(pid) })
		if log := readFile(t, filepath.Join(m.JobDir(j.ID), "stderr.log")); !strings.Contains(log, "prepare: ended 1 leftover process") {
			t.Fatalf("the prepare sweep did not end the leftover:\n%s", log)
		}
	})
	t.Run("survives", func(t *testing.T) {
		t.Setenv("AGENT_JOBS_TEST_UNKILLABLE", "1")
		t.Setenv("AGENT_JOBS_TEST_SWEEP_GRACE_MS", "20")
		m := newManager(t)
		repo := newRepo(t)
		withPrepareConfig(t, m, []PrepareStep{{RepoGlob: filepath.Base(repo), Command: cmd}}, "")
		id := enqueue(t, m, "do it", inRepo(repo)).ID
		t.Cleanup(func() {
			var pid int
			if _, err := fmt.Sscan(readFile(t, filepath.Join(m.JobDir(id), "leftover.pid")), &pid); err == nil && pid > 1 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		})
		j := runToEnd(t, m, id)
		if j.Status != store.StatusFailed || !strings.HasPrefix(j.Error, "prepare failed: ") {
			t.Fatalf("got %s %q", j.Status, j.Error)
		}
		if _, err := os.Stat(filepath.Join(m.JobDir(id), "stream.jsonl")); err == nil {
			t.Fatal("the runner started after a prepare step whose leftovers survived")
		}
	})
}
