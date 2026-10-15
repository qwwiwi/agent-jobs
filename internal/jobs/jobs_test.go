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
echo launch >> ../launches.txt
echo '{"type":"system","subtype":"init"}'
if [[ $prompt =~ FAKE_SLEEP=([0-9]+) ]]; then sleep "${BASH_REMATCH[1]}"; fi
if [[ $prompt == *FAKE_COMMIT* ]]; then
  echo new > committed.txt
  git add committed.txt
  git -c user.name=T -c user.email=t@example.com commit -qm "job commit"
  echo dirty >> tracked.txt
  echo untracked > new-untracked.txt
fi
if [[ $prompt == *FAKE_STAGED* ]]; then
  echo new > committed.txt
  git add committed.txt
  git -c user.name=T -c user.email=t@example.com commit -qm "job commit"
  printf 'staged\n' > tracked.txt
  git add tracked.txt
  printf 'v1\n' > tracked.txt
  echo more >> committed.txt
  mkdir -p d
  echo u > d/new-untracked.txt
fi
if [[ $prompt == *FAKE_ONLY_COMMIT* ]]; then
  echo c > only-commit.txt
  git add only-commit.txt
  git -c user.name=T -c user.email=t@example.com commit -qm "only commit"
fi
if [[ $prompt == *FAKE_ONLY_STAGED* ]]; then printf 'staged\n' > tracked.txt; git add tracked.txt; fi
if [[ $prompt == *FAKE_ONLY_UNSTAGED* ]]; then printf 'unstaged\n' >> tracked.txt; fi
if [[ $prompt == *FAKE_ONLY_UNTRACKED* ]]; then echo u > only-untracked.txt; fi
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
	if j.NotifiedAt != nil {
		t.Fatal("without a notify target the job must stay pending, not be marked delivered")
	}
	// Terminal status never changes.
	if err := m.Store.Finish(j.ID, "", store.StatusFailed, 0, 0, 0, "", time.Now()); !errors.Is(err, store.ErrImmutable) {
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
	for _, s := range []string{"Bash(git push:*)", "Bash(git tag:*)", "Bash(gh release:*)", "Bash(gh api:*)", "Bash(gh pr create:*)", "Bash(gh issue comment:*)", "Bash(git -c*)", "Bash(env -S*)", "WebFetch", "WebSearch"} {
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
	m.Cfg.NotifyTmuxTarget = "fake-session"
	t.Setenv("FAKE_TMUX_LOG", filepath.Join(t.TempDir(), "tmux.log"))
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
	if s := read("uncommitted.patch"); !strings.Contains(s, "+dirty") {
		t.Fatalf("uncommitted.patch: %s", s)
	}
	if s := read("untracked.tar"); !strings.Contains(s, "new-untracked.txt") {
		t.Fatal("untracked.tar lacks new-untracked.txt")
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

// ---- Fix-loop 1 regressions ----

func newRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	gitT(t, repo, "init", "-q", "-b", "main")
	gitT(t, repo, "config", "user.name", "T")
	gitT(t, repo, "config", "user.email", "t@example.com")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "add", ".")
	gitT(t, repo, "commit", "-qm", "init")
	return repo
}

func notifyTo(t *testing.T, m *Manager) string {
	t.Helper()
	logf := filepath.Join(t.TempDir(), "tmux.log")
	t.Setenv("FAKE_TMUX_LOG", logf)
	m.Cfg.NotifyTmuxTarget = "fake-session"
	return logf
}

func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == 0x202e || r == 0x200b {
			return true
		}
	}
	return false
}

// B1: the title can't smuggle control characters (Ctrl-U, ESC, CR/LF, tabs) into tmux.
func TestDoneLineSanitized(t *testing.T) {
	j := &store.Job{ID: "id1", Title: "x\x15echo injected #\x1b[2J\r\nnext\tcol\x7f‮end​", Status: store.StatusSucceeded}
	line := DoneLine(j)
	if hasControl(line) {
		t.Fatalf("control chars in line: %q", line)
	}
	if !strings.Contains(line, "xecho injected #[2J next col") {
		t.Fatalf("printable text lost: %q", line)
	}
	j.Title = strings.Repeat("я", 1000)
	if n := len([]rune(DoneLine(j))); n > 220 {
		t.Fatalf("title not capped: line is %d runes", n)
	}
	m := newManager(t)
	logf := notifyTo(t, m)
	if err := m.sendTmux("fake-session", "a\x15b\x1bc\rd"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(logf)
	if hasControl(strings.TrimSuffix(strings.ReplaceAll(string(b), "|\n", "|"), "\n")) {
		t.Fatalf("sendTmux passed control chars: %q", b)
	}
}

// #26: no notify target = not delivered: the job stays pending (and is not cleaned up).
func TestNotifyKeepsPendingWithoutTarget(t *testing.T) {
	m := newManager(t)
	j := runToEnd(t, m, enqueue(t, m, "x", nil).ID)
	if j.NotifiedAt != nil {
		t.Fatal("marked notified without a target")
	}
	logf := notifyTo(t, m)
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if get(t, m, j.ID).NotifiedAt == nil {
		t.Fatal("pending notify not retried once a target exists")
	}
	if b, _ := os.ReadFile(logf); !strings.Contains(string(b), j.ID) {
		t.Fatalf("tmux log: %s", b)
	}
	// A failing tmux (missing session) keeps it pending too.
	m2 := newManager(t)
	t.Setenv("AGENT_JOBS_TMUX_BIN", "/usr/bin/false")
	m2.Cfg.NotifyTmuxTarget = "missing-session"
	j2 := runToEnd(t, m2, enqueue(t, m2, "x", nil).ID)
	if j2.NotifiedAt != nil {
		t.Fatal("failed tmux delivery marked as notified")
	}
}

// B3: two daemons ticking at once never double-start or fail each other's jobs.
func TestConcurrentTicksAreSerialized(t *testing.T) {
	m := newManager(t)
	m2, err := Open("test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m2.Close() })
	m2.SelfBin, m2.Log, m2.KillGrace = selfBin, m.Log, m.KillGrace
	var ids []string
	for i := 0; i < 3; i++ {
		ids = append(ids, enqueue(t, m, "FAKE_SLEEP=5", nil).ID)
	}
	done := make(chan error, 6)
	for i := 0; i < 3; i++ {
		go func() { done <- m.Tick() }()
		go func() { done <- m2.Tick() }()
	}
	for i := 0; i < 6; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	n, _ := m.Store.CountRunning()
	failed, _ := m.Store.ListByStatus(store.StatusFailed)
	if n != 2 || len(failed) != 0 {
		for _, f := range failed {
			t.Logf("failed %s: %s", f.ID, f.Error)
		}
		t.Fatalf("running=%d failed=%d, want 2/0", n, len(failed))
	}
	for _, id := range ids {
		j := get(t, m, id)
		if j.Status != store.StatusRunning {
			continue
		}
		waitFor(t, "launch", 5*time.Second, func() bool {
			_, err := os.Stat(filepath.Join(m.JobDir(id), "launches.txt"))
			return err == nil
		})
		b, _ := os.ReadFile(filepath.Join(m.JobDir(id), "launches.txt"))
		if c := strings.Count(string(b), "launch"); c != 1 {
			t.Fatalf("job %s launched %d times", id, c)
		}
	}
}

// B3: a finish from a tick that does not own the launch cannot touch the running row.
func TestFinishRequiresLaunchOwnership(t *testing.T) {
	m := newManager(t)
	j := enqueue(t, m, "x", nil)
	if err := m.Store.Claim(j.ID, "tok-a", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.Claim(j.ID, "tok-b", time.Now()); !errors.Is(err, store.ErrImmutable) {
		t.Fatalf("second claim: %v", err)
	}
	if err := m.Store.Finish(j.ID, "tok-b", store.StatusFailed, 0, 0, 0, "x", time.Now()); !errors.Is(err, store.ErrImmutable) {
		t.Fatalf("foreign finish: %v", err)
	}
	if get(t, m, j.ID).Status != store.StatusRunning {
		t.Fatal("foreign finish changed the row")
	}
	if err := m.Store.Finish(j.ID, "tok-a", store.StatusFailed, 0, 0, 0, "x", time.Now()); err != nil {
		t.Fatal(err)
	}
}

// B4: a daemon dying between spawning the wrapper and recording its PID loses nothing:
// the next tick adopts the wrapper from wrapper.json instead of failing or launching twice.
func TestOrphanWrapperAdopted(t *testing.T) {
	m := newManager(t)
	repo := newRepo(t)
	j := enqueue(t, m, "FAKE_SLEEP=1", func(r *StartRequest) { r.RepoPath = repo })
	const tok = "crash-token"
	if err := m.Store.Claim(j.ID, tok, m.Now()); err != nil {
		t.Fatal(err)
	}
	if err := m.prepare(j, m.JobDir(j.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.spawn(j, tok); err != nil { // "crash": PID never written
		t.Fatal(err)
	}
	if get(t, m, j.ID).PID != 0 {
		t.Fatal("precondition: pid must be unset")
	}
	waitFor(t, "exit_code", 15*time.Second, func() bool {
		_, ok := readExitCode(m.JobDir(j.ID))
		return ok
	})
	if out, err := exec.Command(selfBin, "tick", "--agent", "test").CombinedOutput(); err != nil {
		t.Fatalf("tick: %v %s", err, out)
	}
	got := get(t, m, j.ID)
	if got.Status != store.StatusSucceeded || got.PID == 0 {
		t.Fatalf("after restart: %s pid=%d %q", got.Status, got.PID, got.Error)
	}
	b, _ := os.ReadFile(filepath.Join(m.JobDir(j.ID), "launches.txt"))
	if c := strings.Count(string(b), "launch"); c != 1 {
		t.Fatalf("launched %d times", c)
	}
}

// B4: a claim whose wrapper never registered is failed loudly after a grace period (work kept).
func TestClaimWithoutWrapperFailsAfterGrace(t *testing.T) {
	m := newManager(t)
	j := enqueue(t, m, "x", nil)
	if err := m.Store.Claim(j.ID, "tok", m.Now()); err != nil {
		t.Fatal(err)
	}
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if get(t, m, j.ID).Status != store.StatusRunning {
		t.Fatal("failed inside the grace period")
	}
	real := time.Now
	m.Now = func() time.Time { return real().Add(2 * LaunchGrace) }
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if g := get(t, m, j.ID); g.Status != store.StatusFailed || !strings.Contains(g.Error, "never registered") {
		t.Fatalf("got %s %q", g.Status, g.Error)
	}
}

// B4: a second wrapper for the same job directory refuses to run.
func TestSecondWrapperRefuses(t *testing.T) {
	m := newManager(t)
	j := enqueue(t, m, "FAKE_SLEEP=3", nil)
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	dir := m.JobDir(j.ID)
	waitFor(t, "wrapper.json", 5*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(dir, "wrapper.json"))
		return err == nil
	})
	out, err := exec.Command(selfBin, "run-wrapper", dir, "--token", "other", "--", filepath.Join(fakeHelpr, "claude")).CombinedOutput()
	if err == nil || !strings.Contains(string(out), "already") {
		t.Fatalf("second wrapper: err=%v out=%s", err, out)
	}
}

// B5: the stream is read after exit_code is seen, never before.
func TestCollectReadsStreamAfterExitCode(t *testing.T) {
	m := newManager(t)
	j := enqueue(t, m, "x", nil)
	dir := m.JobDir(j.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ok := `{"type":"result","subtype":"success","is_error":false,"num_turns":1,"total_cost_usd":0.1,"result":"r"}` + "\n"
	bad := `{"type":"result","subtype":"error_max_turns","is_error":true,"num_turns":9,"total_cost_usd":0.9,"result":"r"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "stream.jsonl"), []byte(ok), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte("r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.Claim(j.ID, "tok", m.Now()); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.SetPID(j.ID, "tok", os.Getpid(), os.Getpid()); err != nil {
		t.Fatal(err)
	}
	// The wrapper finishes right after the daemon's first look at the stream.
	finished := false
	orig := parseStream
	t.Cleanup(func() { parseStream = orig })
	parseStream = func(p string) StreamResult {
		r := orig(p)
		if !finished {
			finished = true
			_ = os.WriteFile(filepath.Join(dir, "stream.jsonl"), []byte(ok+bad), 0o600)
			_ = os.WriteFile(filepath.Join(dir, "exit_code"), []byte("0\n"), 0o600)
		}
		return r
	}
	for i := 0; i < 2; i++ {
		if !finished && i == 1 { // fixed order never peeked: let the wrapper finish now
			finished = true
			_ = os.WriteFile(filepath.Join(dir, "stream.jsonl"), []byte(ok+bad), 0o600)
			_ = os.WriteFile(filepath.Join(dir, "exit_code"), []byte("0\n"), 0o600)
		}
		if err := m.collect(); err != nil {
			t.Fatal(err)
		}
	}
	g := get(t, m, j.ID)
	if g.Status != store.StatusFailed || g.Turns != 9 || !strings.Contains(g.Error, "error_max_turns") {
		t.Fatalf("got %s turns=%d %q (stale stream used)", g.Status, g.Turns, g.Error)
	}
}

// B6: staged-only content, unstaged edits and untracked files all survive cleanup and restore.
func TestCleanupKeepsStagedUnstagedUntracked(t *testing.T) {
	m := newManager(t)
	notifyTo(t, m)
	repo := newRepo(t)
	j := runToEnd(t, m, enqueue(t, m, "FAKE_STAGED", func(r *StartRequest) { r.RepoPath = repo; r.Kind = "fix" }).ID)
	if j.Status != store.StatusSucceeded {
		t.Fatalf("status %s %q", j.Status, j.Error)
	}
	dir := m.JobDir(j.ID)
	real := time.Now
	m.Now = func() time.Time { return real().Add(31 * time.Minute) }
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if get(t, m, j.ID).CleanedAt == nil {
		t.Fatal("not cleaned")
	}
	other := filepath.Join(t.TempDir(), "restore")
	gitT(t, repo, "worktree", "add", "-q", "--detach", other, j.BaseCommit)
	gitT(t, other, "am", "-q", filepath.Join(dir, "commits.patch"))
	gitT(t, other, "apply", "--cached", filepath.Join(dir, "staged.patch"))
	gitT(t, other, "apply", filepath.Join(dir, "uncommitted.patch"))
	if out, err := exec.Command("tar", "-xf", filepath.Join(dir, "untracked.tar"), "-C", other).CombinedOutput(); err != nil {
		t.Fatalf("untar: %v %s", err, out)
	}
	if s := gitT(t, other, "show", ":tracked.txt"); s != "staged" {
		t.Fatalf("index tracked.txt = %q, want staged", s)
	}
	read := func(f string) string {
		b, err := os.ReadFile(filepath.Join(other, f))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if s := read("tracked.txt"); s != "v1\n" {
		t.Fatalf("worktree tracked.txt = %q", s)
	}
	if s := read("committed.txt"); s != "new\nmore\n" {
		t.Fatalf("committed.txt = %q", s)
	}
	if s := read("d/new-untracked.txt"); s != "u\n" {
		t.Fatalf("untracked = %q", s)
	}
}

// Suspected: cleanup never touches a worktree it did not create, even at the same path.
func TestCleanupRefusesForeignWorktree(t *testing.T) {
	m := newManager(t)
	notifyTo(t, m)
	repo := newRepo(t)
	j := runToEnd(t, m, enqueue(t, m, "x", func(r *StartRequest) { r.RepoPath = repo }).ID)
	work := filepath.Join(m.JobDir(j.ID), "work")
	gitT(t, repo, "worktree", "remove", "--force", work)
	gitT(t, repo, "worktree", "add", "-q", "--detach", work, "HEAD")
	if err := os.WriteFile(filepath.Join(work, "precious.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	real := time.Now
	m.Now = func() time.Time { return real().Add(31 * time.Minute) }
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if get(t, m, j.ID).CleanedAt != nil {
		t.Fatal("foreign worktree was cleaned")
	}
	if _, err := os.Stat(filepath.Join(work, "precious.txt")); err != nil {
		t.Fatal("foreign worktree content removed")
	}
}

// DEVIATIONS #6 model table.
func TestModelTable(t *testing.T) {
	for _, c := range []struct{ kind, cx, want string }{
		{"review", "light", "sonnet"}, {"review", "normal", "opus"}, {"review", "hard", "fable"},
		{"fix", "hard", "fable"}, {"general", "hard", "fable"}, {"general", "", "opus"},
		{"research", "light", "sonnet"}, {"research", "normal", "sonnet"}, {"research", "hard", "opus"},
	} {
		if got, _ := ModelFor(c.kind, c.cx); got != c.want {
			t.Fatalf("%s/%s = %s, want %s", c.kind, c.cx, got, c.want)
		}
	}
}

// N4: a launch that fails frees its slot in the same tick.
func TestFailedLaunchFreesSlot(t *testing.T) {
	m := newManager(t)
	var bad []string
	for i := 0; i < 2; i++ {
		repo := newRepo(t)
		bad = append(bad, enqueue(t, m, "x", func(r *StartRequest) { r.RepoPath = repo }).ID)
		if err := os.RemoveAll(repo); err != nil { // worktree creation will fail
			t.Fatal(err)
		}
	}
	good := enqueue(t, m, "FAKE_SLEEP=2", nil)
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	for _, id := range bad {
		if g := get(t, m, id); g.Status != store.StatusFailed {
			t.Fatalf("bad job %s: %s", id, g.Status)
		}
	}
	if g := get(t, m, good.ID); g.Status != store.StatusRunning {
		t.Fatalf("healthy job waits for the next tick: %s", g.Status)
	}
}

// snapshot is what a job left in its worktree, independent of commit hashes.
func snapshot(t *testing.T, dir, base string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("log:\n" + gitT(t, dir, "log", "--format=%s", base+"..HEAD"))
	b.WriteString("\nstaged:\n" + gitT(t, dir, "diff", "--cached", "HEAD"))
	b.WriteString("\nunstaged:\n" + gitT(t, dir, "diff"))
	untracked := gitT(t, dir, "ls-files", "--others", "--exclude-standard")
	b.WriteString("\nuntracked:\n" + untracked)
	for _, f := range strings.Fields(untracked) {
		c, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		b.WriteString("\n" + f + "=" + string(c))
	}
	return b.String()
}

// restoreScript is the restore block from docs/ARCHITECTURE.md (the one users copy).
func restoreScript(t *testing.T) string {
	t.Helper()
	doc, err := os.ReadFile("../../docs/ARCHITECTURE.md")
	if err != nil {
		t.Fatal(err)
	}
	_, after, ok := strings.Cut(string(doc), "<!-- restore -->\n```sh\n")
	if !ok {
		t.Fatal("docs/ARCHITECTURE.md has no restore block")
	}
	block, _, ok := strings.Cut(after, "```")
	if !ok {
		t.Fatal("unterminated restore block")
	}
	return block
}

// N5: the documented restore works for every combination, empty patches included.
func TestRestoreFromDocsAllCombinations(t *testing.T) {
	script := restoreScript(t)
	for _, mode := range []string{"FAKE_ONLY_COMMIT", "FAKE_ONLY_STAGED", "FAKE_ONLY_UNSTAGED", "FAKE_ONLY_UNTRACKED", "FAKE_STAGED", "nothing"} {
		t.Run(mode, func(t *testing.T) {
			m := newManager(t)
			notifyTo(t, m)
			repo := newRepo(t)
			j := runToEnd(t, m, enqueue(t, m, mode, func(r *StartRequest) { r.RepoPath = repo; r.Kind = "fix" }).ID)
			if j.Status != store.StatusSucceeded {
				t.Fatalf("status %s %q", j.Status, j.Error)
			}
			dir := m.JobDir(j.ID)
			want := snapshot(t, filepath.Join(dir, "work"), j.BaseCommit)
			real := time.Now
			m.Now = func() time.Time { return real().Add(31 * time.Minute) }
			if err := m.Tick(); err != nil {
				t.Fatal(err)
			}
			if get(t, m, j.ID).CleanedAt == nil {
				t.Fatal("not cleaned")
			}
			other := filepath.Join(t.TempDir(), "restore")
			gitT(t, repo, "worktree", "add", "-q", "--detach", other, j.BaseCommit)
			cmd := exec.Command("bash", "-c", script)
			cmd.Dir = other
			cmd.Env = append(os.Environ(), "J="+dir)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("restore: %v\n%s", err, out)
			}
			if got := snapshot(t, other, j.BaseCommit); got != want {
				t.Fatalf("restored state differs\nwant:\n%s\ngot:\n%s", want, got)
			}
		})
	}
}
