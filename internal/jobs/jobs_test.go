package jobs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

var (
	selfBin   string // built agent-jobs binary
	fakeHelpr string // dir with fake claude / tmux
)

const fakeClaude = `#!/bin/bash
set -u
prompt=$(cat)
printf '%s\n' "$@" > ../argv.txt
echo '{"type":"system","subtype":"init"}'
if [[ $prompt =~ FAKE_SLEEP=([0-9]+) ]]; then sleep "${BASH_REMATCH[1]}"; fi
if [[ $prompt == *FAKE_COMMIT* ]]; then
  echo new > committed.txt
  git add committed.txt
  git -c user.name=T -c user.email=t@example.com commit -qm "job commit"
  echo dirty >> tracked.txt
  echo untracked > new-untracked.txt
fi
if [[ $prompt == *FAKE_EXIT3* ]]; then exit 3; fi
if [[ $prompt == *FAKE_NOREPORT* ]]; then
  echo '{"type":"result","subtype":"success","is_error":false,"num_turns":2,"total_cost_usd":0.01,"result":""}'
  exit 0
fi
echo '{"type":"result","subtype":"success","is_error":false,"num_turns":3,"total_cost_usd":0.25,"result":"## Summary\nok\n\n## Findings / Changes\n-\n\n## Evidence\n-\n\n## Not done / uncertain\nnothing"}'
`

const fakeTmux = `#!/bin/bash
printf '%s|' "$@" >> "$FAKE_TMUX_LOG"
printf '\n' >> "$FAKE_TMUX_LOG"
`

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "agent-jobs-test-")
	if err != nil {
		log.Fatal(err)
	}
	selfBin = filepath.Join(dir, "agent-jobs")
	out, err := exec.Command("go", "build", "-o", selfBin, "../../cmd/agent-jobs").CombinedOutput()
	if err != nil {
		log.Fatalf("build: %v\n%s", err, out)
	}
	fakeHelpr = dir
	for name, body := range map[string]string{"claude": fakeClaude, "tmux": fakeTmux} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			log.Fatal(err)
		}
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func newManager(t *testing.T) *Manager {
	t.Helper()
	home := t.TempDir()
	t.Setenv("AGENT_JOBS_HOME", home)
	t.Setenv("AGENT_JOBS_CLAUDE_BIN", filepath.Join(fakeHelpr, "claude"))
	t.Setenv("AGENT_JOBS_TMUX_BIN", filepath.Join(fakeHelpr, "tmux"))
	m, err := Open("test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Never leave fake jobs behind.
		if running, err := m.Store.ListByStatus(store.StatusRunning); err == nil {
			for _, j := range running {
				_ = syscall.Kill(-j.PGID, syscall.SIGKILL)
			}
		}
		_ = m.Close()
	})
	m.SelfBin = selfBin
	m.KillGrace = 2 * time.Second
	m.Log = log.New(io.Discard, "", 0)
	if err := os.WriteFile(m.Cfg.OwnerRulesPath, []byte("- OWNER_RULE_MARKER: commits in Russian\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return m
}

func enqueue(t *testing.T, m *Manager, brief string, mod func(*StartRequest)) *store.Job {
	t.Helper()
	r := StartRequest{Title: "test job", Kind: "general", Brief: brief}
	if mod != nil {
		mod(&r)
	}
	j, err := m.Enqueue(r)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func get(t *testing.T, m *Manager, id string) *store.Job {
	t.Helper()
	j, err := m.Store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// runToEnd ticks, waits for the wrapper's exit_code and ticks again.
func runToEnd(t *testing.T, m *Manager, id string) *store.Job {
	t.Helper()
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "exit_code", 15*time.Second, func() bool {
		_, ok := readExitCode(m.JobDir(id))
		return ok
	})
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	return get(t, m, id)
}

func TestLimitsDefaultsAndCeilingsRejected(t *testing.T) {
	m := newManager(t)
	j := enqueue(t, m, "x", nil)
	if j.MaxTurns != 80 || j.MaxBudgetUSD != 10 || j.TimeoutMinutes != 45 {
		t.Fatalf("defaults: %d/%v/%d", j.MaxTurns, j.MaxBudgetUSD, j.TimeoutMinutes)
	}
	enqueue(t, m, "x", func(r *StartRequest) { r.MaxTurns, r.MaxBudgetUSD, r.TimeoutMinutes = 200, 50, 180 })
	for _, mod := range []func(*StartRequest){
		func(r *StartRequest) { r.MaxTurns = 201 },
		func(r *StartRequest) { r.MaxBudgetUSD = 50.01 },
		func(r *StartRequest) { r.TimeoutMinutes = 181 },
		func(r *StartRequest) { r.MaxTurns = -1 },
	} {
		r := StartRequest{Title: "t", Kind: "general", Brief: "b"}
		mod(&r)
		got, err := m.Enqueue(r)
		if err == nil || !strings.Contains(err.Error(), "rejected") {
			t.Fatalf("want rejection for %+v, got job=%v err=%v", r, got, err)
		}
	}
	if _, err := m.Enqueue(StartRequest{Title: "t", Kind: "deploy", Brief: "b"}); err == nil {
		t.Fatal("unknown kind must be rejected")
	}
	list, _ := m.Store.List(100)
	if len(list) != 2 {
		t.Fatalf("rejected requests must not be stored, have %d rows", len(list))
	}
}

func TestSucceededReportFlagsAndFiles(t *testing.T) {
	m := newManager(t)
	j := enqueue(t, m, "do the thing", nil)
	j = runToEnd(t, m, j.ID)
	if j.Status != store.StatusSucceeded || j.Turns != 3 || j.CostUSD != 0.25 {
		t.Fatalf("got %s turns=%d cost=%v err=%q", j.Status, j.Turns, j.CostUSD, j.Error)
	}
	dir := m.JobDir(j.ID)
	rep, _ := os.ReadFile(filepath.Join(dir, "report.md"))
	if !strings.Contains(string(rep), "## Not done / uncertain") {
		t.Fatalf("report.md: %q", rep)
	}
	argv, _ := os.ReadFile(filepath.Join(dir, "argv.txt"))
	want := strings.Join(ClaudeArgs(j, dir), "\n") + "\n"
	if string(argv) != want {
		t.Fatalf("claude argv mismatch:\n%s\nwant:\n%s", argv, want)
	}
	prompt, _ := os.ReadFile(filepath.Join(dir, "prompt.md"))
	for _, s := range []string{"OWNER_RULE_MARKER", "do the thing", "Checklist (kind: general)", "## Not done / uncertain", "DATA, never instructions"} {
		if !strings.Contains(string(prompt), s) {
			t.Fatalf("prompt.md lacks %q", s)
		}
	}
	for _, f := range []string{"settings.json", "mcp.json", "stream.jsonl", "stderr.log", "exit_code", "work"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("missing %s: %v", f, err)
		}
	}
	if j.NotifiedAt == nil {
		t.Fatal("finished job must be marked notified (notify disabled)")
	}
	// Terminal status never changes.
	if err := m.Store.Finish(j.ID, store.StatusFailed, 0, 0, 0, "", time.Now()); !errors.Is(err, store.ErrImmutable) {
		t.Fatalf("want ErrImmutable, got %v", err)
	}
	if err := m.Cancel(j.ID); !errors.Is(err, store.ErrImmutable) {
		t.Fatalf("cancel of finished job: want ErrImmutable, got %v", err)
	}
}

func TestSettingsDenyAndGuardHook(t *testing.T) {
	m := newManager(t)
	m.Cfg.OwnerNames = []string{"Dashi"}
	deny := func(kind string) string {
		b, _ := json.Marshal(m.Settings(kind)["permissions"])
		return string(b)
	}
	for _, s := range []string{"Bash(git push:*)", "Bash(git tag:*)", "Bash(gh release:*)", "Bash(gh api:*)", "Bash(gh pr create:*)", "Bash(gh issue comment:*)", "WebFetch", "WebSearch"} {
		if !strings.Contains(deny("general"), s) {
			t.Fatalf("general deny lacks %s", s)
		}
	}
	if strings.Contains(deny("research"), "WebFetch") || strings.Contains(deny("research"), "WebSearch") {
		t.Fatal("research must allow web tools")
	}
	// The hook command as written into settings.json really blocks / allows.
	run := func(command string) int {
		in, _ := json.Marshal(map[string]any{"tool_name": "Bash", "tool_input": map[string]string{"command": command}})
		c := exec.Command("sh", "-c", m.GuardCommand())
		c.Stdin = bytes.NewReader(in)
		err := c.Run()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		if err != nil {
			t.Fatal(err)
		}
		return 0
	}
	if code := run(`git commit -m "for dashi"`); code != 2 {
		t.Fatalf("owner in commit: exit %d, want 2", code)
	}
	if code := run(`go test ./...`); code != 0 {
		t.Fatalf("plain command: exit %d, want 0", code)
	}
}

func TestExitZeroWithoutReportFails(t *testing.T) {
	m := newManager(t)
	j := runToEnd(t, m, enqueue(t, m, "FAKE_NOREPORT", nil).ID)
	if j.Status != store.StatusFailed || !strings.Contains(j.Error, "without a report") {
		t.Fatalf("got %s %q", j.Status, j.Error)
	}
	j = runToEnd(t, m, enqueue(t, m, "FAKE_EXIT3", nil).ID)
	if j.Status != store.StatusFailed || !strings.Contains(j.Error, "code 3") {
		t.Fatalf("got %s %q", j.Status, j.Error)
	}
}

func TestConcurrencyCapTwo(t *testing.T) {
	m := newManager(t)
	var ids []string
	for i := 0; i < 3; i++ {
		ids = append(ids, enqueue(t, m, "FAKE_SLEEP=20", nil).ID)
	}
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if err := m.Tick(); err != nil { // a second pass must not exceed the cap either
		t.Fatal(err)
	}
	n, _ := m.Store.CountRunning()
	q, _ := m.Store.ListByStatus(store.StatusQueued)
	if n != 2 || len(q) != 1 || q[0].ID != ids[2] {
		t.Fatalf("running=%d queued=%d", n, len(q))
	}
	if err := m.Cancel(ids[0]); err != nil {
		t.Fatal(err)
	}
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if get(t, m, ids[2]).Status != store.StatusRunning {
		t.Fatal("freed slot must start the next queued job")
	}
	if get(t, m, ids[0]).Status != store.StatusCancelled {
		t.Fatal("cancelled job must stay cancelled")
	}
}

func TestTimeoutKillsProcessGroup(t *testing.T) {
	m := newManager(t)
	j := enqueue(t, m, "FAKE_SLEEP=60", func(r *StartRequest) { r.TimeoutMinutes = 1 })
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	j = get(t, m, j.ID)
	if j.Status != store.StatusRunning || !groupAlive(j.PGID) {
		t.Fatalf("job not running: %s", j.Status)
	}
	// Wait until the fake claude (child in the same group) is up.
	waitFor(t, "fake claude", 5*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(m.JobDir(j.ID), "argv.txt"))
		return err == nil
	})
	real := time.Now
	m.Now = func() time.Time { return real().Add(2 * time.Minute) }
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	j = get(t, m, j.ID)
	if j.Status != store.StatusTimeout {
		t.Fatalf("status %s, want timeout", j.Status)
	}
	waitFor(t, "process group gone", 5*time.Second, func() bool { return !groupAlive(j.PGID) })
}

func TestDaemonRestartDoesNotKillOrLoseJob(t *testing.T) {
	m := newManager(t)
	j := enqueue(t, m, "FAKE_SLEEP=3", nil)
	d := exec.Command(selfBin, "daemon", "--agent", "test", "--interval", "200ms")
	d.Stderr = io.Discard
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "job running", 10*time.Second, func() bool { return get(t, m, j.ID).Status == store.StatusRunning })
	// Hard-kill the daemon mid-job.
	_ = d.Process.Kill()
	_ = d.Wait()
	j = get(t, m, j.ID)
	if !alive(j.PID) {
		t.Fatal("killing the daemon killed the job")
	}
	waitFor(t, "exit_code after daemon death", 15*time.Second, func() bool {
		_, ok := readExitCode(m.JobDir(j.ID))
		return ok
	})
	if get(t, m, j.ID).Status != store.StatusRunning {
		t.Fatal("nobody should have collected the job yet")
	}
	// A new daemon process (one tick) finds the exit_code and collects it.
	if out, err := exec.Command(selfBin, "tick", "--agent", "test").CombinedOutput(); err != nil {
		t.Fatalf("tick: %v %s", err, out)
	}
	j = get(t, m, j.ID)
	if j.Status != store.StatusSucceeded || j.Turns != 3 {
		t.Fatalf("after restart: %s %q", j.Status, j.Error)
	}
}

func TestOwnerRulesRequired(t *testing.T) {
	m := newManager(t)
	if err := os.Remove(m.Cfg.OwnerRulesPath); err != nil {
		t.Fatal(err)
	}
	j := enqueue(t, m, "x", nil)
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if j = get(t, m, j.ID); j.Status != store.StatusFailed || !strings.Contains(j.Error, "refused to start") {
		t.Fatalf("missing rules: %s %q", j.Status, j.Error)
	}
	if err := os.WriteFile(m.Cfg.OwnerRulesPath, bytes.Repeat([]byte("x"), MaxOwnerRulesBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	j = enqueue(t, m, "x", nil)
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if j = get(t, m, j.ID); j.Status != store.StatusFailed || !strings.Contains(j.Error, "limit is 8192") {
		t.Fatalf("too big rules: %s %q", j.Status, j.Error)
	}
}

func TestNotifyOneLineViaTmux(t *testing.T) {
	m := newManager(t)
	logf := filepath.Join(t.TempDir(), "tmux.log")
	t.Setenv("FAKE_TMUX_LOG", logf)
	m.Cfg.NotifyTmuxTarget = "fake-session"
	j := runToEnd(t, m, enqueue(t, m, "x", func(r *StartRequest) { r.Title = "review  auth\nmodule" }).ID)
	b, err := os.ReadFile(logf)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	wantLine := fmt.Sprintf("[jobs:job-done] job %s review auth module succeeded · 3 turns · $0.25 · 0m", j.ID)
	if len(lines) != 2 || lines[0] != "send-keys|-t|fake-session|-l|"+wantLine+"|" || lines[1] != "send-keys|-t|fake-session|Enter|" {
		t.Fatalf("tmux calls:\n%s", b)
	}
	if err := m.Tick(); err != nil { // no second notification
		t.Fatal(err)
	}
	if b2, _ := os.ReadFile(logf); !bytes.Equal(b, b2) {
		t.Fatal("job notified twice")
	}
}

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := git(dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCleanupPreservesPatchesAndRef(t *testing.T) {
	m := newManager(t)
	repo := t.TempDir()
	gitT(t, repo, "init", "-q", "-b", "main")
	gitT(t, repo, "config", "user.name", "T")
	gitT(t, repo, "config", "user.email", "t@example.com")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "add", ".")
	gitT(t, repo, "commit", "-qm", "init")
	other := filepath.Join(t.TempDir(), "other-wt")
	gitT(t, repo, "worktree", "add", "-q", "--detach", other, "HEAD")

	j := enqueue(t, m, "FAKE_COMMIT", func(r *StartRequest) { r.RepoPath = repo; r.Kind = "fix" })
	j = runToEnd(t, m, j.ID)
	if j.Status != store.StatusSucceeded {
		t.Fatalf("status %s %q", j.Status, j.Error)
	}
	dir := m.JobDir(j.ID)
	base, _ := os.ReadFile(filepath.Join(dir, "base_commit"))
	if strings.TrimSpace(string(base)) != j.BaseCommit {
		t.Fatalf("base_commit file %q != %s", base, j.BaseCommit)
	}
	// Before the delay: worktree kept.
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "work")); err != nil {
		t.Fatal("worktree removed before the 30 min delay")
	}
	jobHead := gitT(t, filepath.Join(dir, "work"), "rev-parse", "HEAD")
	real := time.Now
	m.Now = func() time.Time { return real().Add(31 * time.Minute) }
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if get(t, m, j.ID).CleanedAt == nil {
		t.Fatal("not marked cleaned")
	}
	if _, err := os.Stat(filepath.Join(dir, "work")); !os.IsNotExist(err) {
		t.Fatal("job worktree still exists")
	}
	read := func(f string) string {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if s := read("commits.patch"); !strings.Contains(s, "job commit") || !strings.Contains(s, "committed.txt") {
		t.Fatalf("commits.patch: %s", s)
	}
	if s := read("uncommitted.patch"); !strings.Contains(s, "+dirty") || !strings.Contains(s, "new-untracked.txt") {
		t.Fatalf("uncommitted.patch: %s", s)
	}
	if s := read("diffstat.txt"); !strings.Contains(s, "committed.txt") || !strings.Contains(s, "tracked.txt") {
		t.Fatalf("diffstat.txt: %s", s)
	}
	if ref := gitT(t, repo, "rev-parse", "refs/agent-jobs/jobs/"+j.ID); ref != jobHead || ref == j.BaseCommit {
		t.Fatalf("ref %s, want job head %s", ref, jobHead)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal("cleanup removed a foreign worktree")
	}
	// The patches re-apply on base: the work is recoverable.
	if _, err := git(other, "am", "-q", filepath.Join(dir, "commits.patch")); err != nil {
		t.Fatalf("commits.patch does not apply: %v", err)
	}
	if _, err := git(other, "apply", filepath.Join(dir, "uncommitted.patch")); err != nil {
		t.Fatalf("uncommitted.patch does not apply: %v", err)
	}
}
