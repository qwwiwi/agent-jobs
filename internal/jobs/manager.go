package jobs

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/brain"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// Manager runs the job lifecycle for one agent.
type Manager struct {
	Agent    string
	StateDir string
	Cfg      Config
	Store    *store.Store
	// SelfBin is the agent-jobs binary used for run-wrapper and the guard hook.
	SelfBin string
	// Now is the clock (overridable in tests).
	Now func() time.Time
	// KillGrace is the wait between SIGTERM and SIGKILL of a job's process group.
	KillGrace time.Duration
	Log       *log.Logger
	// Brain is the shared brain from config (brain.Disabled when not configured).
	Brain brain.Brain
	// MemoryTimeout bounds the memory notes lookup while a job is prepared.
	MemoryTimeout time.Duration

	memMu        sync.Mutex
	memDownUntil time.Time // memory lookups are skipped until then (MemoryBreaker)

	jobWritesOff atomic.Bool // set once a job's note could not be tracked (brain proxy fails closed)
}

// Open creates the state dir, loads config, opens the DB and resolves the own binary.
func Open(agent string) (*Manager, error) {
	dir, err := StateDir(agent)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "jobs"), 0o700); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	cfg, err := LoadConfig(dir)
	if err != nil {
		return nil, err
	}
	st, err := store.Open(filepath.Join(dir, "jobs.db"))
	if err != nil {
		return nil, err
	}
	br, err := brain.New(cfg.Brain, agent)
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	self, err := os.Executable()
	if err == nil {
		self, err = filepath.EvalSymlinks(self)
	}
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("resolve own binary: %w", err)
	}
	return &Manager{
		Agent: agent, StateDir: dir, Cfg: cfg, Store: st, SelfBin: self,
		Now: time.Now, KillGrace: 10 * time.Second, Brain: br, MemoryTimeout: MemoryNotesTimeout,
		Log: log.New(os.Stderr, "agent-jobs["+agent+"] ", log.LstdFlags),
	}, nil
}

// Close releases the DB.
func (m *Manager) Close() error { return m.Store.Close() }

// JobDir returns <state>/jobs/<id>.
func (m *Manager) JobDir(id string) string { return filepath.Join(m.StateDir, "jobs", id) }

// StartRequest is the input of `start` (job_start).
type StartRequest struct {
	Title          string
	Kind           string
	Brief          string
	RepoPath       string
	Base           string // ref in RepoPath; default HEAD
	Model          string // explicit model; else picked by kind × complexity
	Complexity     string // light|normal|hard; model or complexity is required (no default, no classifier)
	MaxTurns       int    // 0 = default
	MaxBudgetUSD   float64
	TimeoutMinutes int
}

// ValidateLimits applies defaults and rejects anything above a ceiling (never clamps).
func ValidateLimits(turns int, budget float64, timeout int) (int, float64, int, error) {
	if turns == 0 {
		turns = DefaultMaxTurns
	}
	if budget == 0 {
		budget = DefaultMaxBudgetUSD
	}
	if timeout == 0 {
		timeout = DefaultTimeoutMinutes
	}
	var errs []string
	if turns < 0 || turns > CeilingMaxTurns {
		errs = append(errs, fmt.Sprintf("max_turns %d is outside 1..%d", turns, CeilingMaxTurns))
	}
	if budget < 0 || budget > CeilingMaxBudgetUSD {
		errs = append(errs, fmt.Sprintf("max_budget_usd %.2f is outside 0..%.0f", budget, CeilingMaxBudgetUSD))
	}
	if timeout < 0 || timeout > CeilingTimeoutMinutes {
		errs = append(errs, fmt.Sprintf("timeout_minutes %d is outside 1..%d", timeout, CeilingTimeoutMinutes))
	}
	if len(errs) > 0 {
		return 0, 0, 0, fmt.Errorf("limits rejected (not clamped): %s", strings.Join(errs, "; "))
	}
	return turns, budget, timeout, nil
}

func newID(now time.Time) string {
	b := make([]byte, 2)
	_, _ = rand.Read(b)
	return now.Format("060102-150405") + "-" + hex.EncodeToString(b)
}

// daemonGitEnv is the environment of the daemon's own git calls (worktree add/remove, diff,
// format-patch, update-ref): the job may have edited the repository config, so the same command-line
// overrides as in the job (no hooks, fsmonitor, ssh command, credential helper, pager, editor) win
// over it; diffs additionally pass --no-ext-diff --no-textconv. The owner's global config still applies; inherited GIT_* variables are dropped.
func daemonGitEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); !strings.HasPrefix(k, "GIT_") {
			out = append(out, kv)
		}
	}
	overrides := gitOverrides(os.DevNull)
	out = append(out, "GIT_TERMINAL_PROMPT=0", "GIT_PAGER=cat", "GIT_EDITOR=true",
		fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(overrides)))
	for i, kv := range overrides {
		out = append(out, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv[1]))
	}
	return out
}

// jobSpawnEnv is the daemon's environment without the brain token variable (brain.token_env): a job
// reaches the brain only through the daemon's proxy and never holds the credential.
func (m *Manager) jobSpawnEnv() []string {
	env := os.Environ()
	if name := m.Cfg.Brain.TokenEnv; name != "" {
		env = slices.DeleteFunc(env, func(kv string) bool {
			k, _, _ := strings.Cut(kv, "=")
			return k == name
		})
	}
	return env
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = daemonGitEnv()
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Enqueue validates the request and inserts a queued job. It returns immediately.
func (m *Manager) Enqueue(r StartRequest) (*store.Job, error) {
	if strings.TrimSpace(r.Title) == "" {
		return nil, errors.New("title is required")
	}
	if strings.TrimSpace(r.Brief) == "" {
		return nil, errors.New("brief is required")
	}
	if !slices.Contains(Kinds, r.Kind) {
		return nil, fmt.Errorf("kind %q must be one of %s", r.Kind, strings.Join(Kinds, "|"))
	}
	turns, budget, timeout, err := ValidateLimits(r.MaxTurns, r.MaxBudgetUSD, r.TimeoutMinutes)
	if err != nil {
		return nil, err
	}
	model, err := ResolveModel(r.Kind, strings.TrimSpace(r.Model), r.Complexity)
	if err != nil {
		return nil, err
	}
	var repo, base string
	if r.RepoPath != "" {
		if repo, err = filepath.Abs(r.RepoPath); err != nil {
			return nil, fmt.Errorf("repo path: %w", err)
		}
		if repo, err = git(repo, "rev-parse", "--show-toplevel"); err != nil {
			return nil, fmt.Errorf("repo %s is not a git repository: %w", r.RepoPath, err)
		}
		ref := r.Base
		if ref == "" {
			ref = "HEAD"
		}
		if base, err = git(repo, "rev-parse", "--verify", ref+"^{commit}"); err != nil {
			return nil, fmt.Errorf("base %q: %w", ref, err)
		}
	} else if r.Base != "" {
		return nil, errors.New("base given without repo")
	}
	now := m.Now()
	j := &store.Job{
		ID: newID(now), Agent: m.Agent, Title: strings.TrimSpace(r.Title), Kind: r.Kind, Brief: r.Brief,
		RepoPath: repo, BaseCommit: base, Status: store.StatusQueued, Model: model,
		MaxTurns: turns, MaxBudgetUSD: budget, TimeoutMinutes: timeout, CreatedAt: now,
		Complexity: r.Complexity, Runner: store.RunnerClaude, Billing: store.BillingSubscription,
	}
	if err := m.Store.Insert(j); err != nil {
		return nil, err
	}
	return j, nil
}

// Tick is one daemon pass: collect finished/timed-out jobs, start queued ones (≤2 running),
// send job-done lines, and clean up worktrees whose notify is older than the cleanup delay.
func (m *Manager) Tick() error {
	unlock, err := m.lock()
	if err != nil {
		return err
	}
	defer unlock()
	var errs []error
	if err := m.collect(); err != nil {
		errs = append(errs, fmt.Errorf("collect: %w", err))
	}
	if err := m.launchQueued(); err != nil {
		errs = append(errs, fmt.Errorf("launch: %w", err))
	}
	if err := m.notifyPending(); err != nil {
		errs = append(errs, fmt.Errorf("notify: %w", err))
	}
	if err := m.cleanupDue(); err != nil {
		errs = append(errs, fmt.Errorf("cleanup: %w", err))
	}
	return errors.Join(errs...)
}

// LaunchGrace is how long a claimed job may wait for its wrapper to register (wrapper.json)
// before it is failed: a daemon that died between claim and spawn must not leave a ghost row.
const LaunchGrace = time.Minute

// lock takes the per-agent lifecycle lock (flock on <state>/tick.lock), so two daemons,
// a manual `tick` and `cancel` never interleave. Released by the returned func or on exit.
func (m *Manager) lock() (func(), error) {
	f, err := os.OpenFile(filepath.Join(m.StateDir, "tick.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock: %w", err)
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("flock: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

func newToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (m *Manager) launchQueued() error {
	running, err := m.Store.CountRunning()
	if err != nil {
		return err
	}
	queued, err := m.Store.ListByStatus(store.StatusQueued)
	if err != nil {
		return err
	}
	for _, j := range queued {
		if running >= MaxRunningPerAgent {
			return nil
		}
		token := newToken()
		// The row is ours (running + token) before anything is created or spawned.
		if err := m.Store.Claim(j.ID, token, m.Now()); err != nil {
			if errors.Is(err, store.ErrImmutable) {
				continue // cancelled or taken meanwhile
			}
			return err
		}
		running++
		if err := m.launch(j, token); err != nil {
			m.Log.Printf("job %s refused to start: %v", j.ID, err)
			if ferr := m.Store.Finish(j.ID, token, store.StatusFailed, 0, 0, 0, "refused to start: "+err.Error(), m.Now()); ferr != nil {
				m.Log.Printf("job %s: mark failed: %v", j.ID, ferr)
			} else {
				running-- // the failed launch no longer occupies a slot
			}
		}
	}
	return nil
}

// ClaudeArgs is the exact reference command line (prompt.md arrives on stdin).
func ClaudeArgs(j *store.Job, jobDir string) []string {
	return []string{
		"-p", "--output-format", "stream-json", "--verbose", "--model", j.Model,
		"--max-turns", fmt.Sprint(j.MaxTurns), "--max-budget-usd", fmt.Sprintf("%g", j.MaxBudgetUSD),
		"--permission-mode", "bypassPermissions",
		"--settings", filepath.Join(jobDir, "settings.json"), "--setting-sources", "project",
		"--mcp-config", filepath.Join(jobDir, "mcp.json"), "--strict-mcp-config",
		"--no-session-persistence", "--add-dir", jobDir,
	}
}

func (m *Manager) launch(j *store.Job, token string) error {
	if err := m.prepare(j, m.JobDir(j.ID)); err != nil {
		return err
	}
	pid, err := m.spawn(j, token)
	if err != nil {
		return err
	}
	// If the daemon dies before this line, the next tick adopts the wrapper via wrapper.json.
	if err := m.Store.SetPID(j.ID, token, pid, pid); err != nil {
		m.Log.Printf("job %s: record pid %d: %v (adopted next tick via wrapper.json)", j.ID, pid, err)
	}
	m.Log.Printf("job %s started pid=%d model=%s", j.ID, pid, j.Model)
	return nil
}

// spawn starts the detached wrapper; the wrapper registers itself in wrapper.json before claude runs.
func (m *Manager) spawn(j *store.Job, token string) (int, error) {
	dir := m.JobDir(j.ID)
	args := append([]string{"run-wrapper", dir, "--token", token, "--", m.Cfg.claudeBin()}, ClaudeArgs(j, dir)...)
	cmd := exec.Command(m.SelfBin, args...)
	cmd.Dir = filepath.Join(dir, "work")
	cmd.Env = m.jobSpawnEnv()
	// Own session => own process group, detached from the daemon: a daemon restart neither kills nor loses it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start wrapper: %w", err)
	}
	// Reap the wrapper if it finishes while this process is alive; otherwise it is re-parented to launchd/init.
	go func() { _ = cmd.Wait() }()
	return cmd.Process.Pid, nil
}

// alive reports whether a process exists (EPERM counts as alive).
func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func groupAlive(pgid int) bool {
	if pgid <= 0 {
		return false
	}
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// killFn sends a signal (syscall.Kill); a variable so tests can see every kill without risking one.
var killFn = syscall.Kill

// plausibleGroup rejects a pgid that must never be signalled whatever wrapper.json says: 0 and 1
// (kill(-1) reaches every process of the user), negative values and the daemon's own group.
func plausibleGroup(pgid int) error {
	if pgid <= 1 {
		return fmt.Errorf("pgid %d is not a job's process group", pgid)
	}
	if own, err := syscall.Getpgid(0); err != nil || own == pgid {
		return fmt.Errorf("pgid %d is the daemon's own process group (or it is unknown: %v)", pgid, err)
	}
	return nil
}

// verifyGroup proves that pgid is the live wrapper's process group right before a kill.
func verifyGroup(pid, pgid int) error {
	if err := plausibleGroup(pgid); err != nil {
		return err
	}
	got, err := syscall.Getpgid(pid)
	if err != nil {
		return fmt.Errorf("wrapper pid %d: %w", pid, err)
	}
	if got != pgid {
		return fmt.Errorf("wrapper pid %d is in process group %d, not %d", pid, got, pgid)
	}
	return nil
}

// killGroup sends SIGTERM to the wrapper's process group, waits up to KillGrace, then SIGKILL.
// It refuses (and logs) unless pgid is plausible and is the live wrapper pid's group.
func (m *Manager) killGroup(id string, pid, pgid int) {
	if err := verifyGroup(pid, pgid); err != nil {
		m.Log.Printf("job %s: refusing to kill process group %d: %v", id, pgid, err)
		return
	}
	_ = killFn(-pgid, syscall.SIGTERM)
	deadline := time.Now().Add(m.KillGrace)
	for time.Now().Before(deadline) {
		if !groupAlive(pgid) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = killFn(-pgid, syscall.SIGKILL)
}

func readExitCode(dir string) (int, bool) {
	b, err := os.ReadFile(filepath.Join(dir, "exit_code"))
	if err != nil {
		return 0, false
	}
	var code int
	if _, err := fmt.Sscan(strings.TrimSpace(string(b)), &code); err != nil {
		return 0, false
	}
	return code, true
}

// parseStream is ParseStream; a variable so tests can interleave the wrapper's writes.
var parseStream = ParseStream

// adopt fills in the pid of a claimed job from wrapper.json. It returns false when the job
// must be skipped this tick (wrapper not registered yet) or was failed (never registered).
func (m *Manager) adopt(j *store.Job, now time.Time) bool {
	// Same lock as the wrapper's admission: it either registered already or will see the failed row.
	unlock, err := lockAdmitIfExists(m.JobDir(j.ID))
	if err != nil {
		m.Log.Printf("job %s: %v", j.ID, err)
		return false
	}
	defer unlock()
	w, err := ReadWrapperInfo(m.JobDir(j.ID))
	if err == nil && w.Token == j.LaunchToken {
		// wrapper.json lies in the job dir, which the job can write: never adopt an implausible group.
		// A live wrapper must really lead it; a dead one is only collected, never signalled (killGroup re-checks).
		if gerr := plausibleGroup(w.PGID); gerr != nil || (alive(w.PID) && verifyGroup(w.PID, w.PGID) != nil) {
			m.Log.Printf("job %s: refusing to adopt wrapper.json pid=%d pgid=%d: %v", j.ID, w.PID, w.PGID,
				errors.Join(gerr, verifyGroup(w.PID, w.PGID)))
			return false
		}
		if err := m.Store.SetPID(j.ID, j.LaunchToken, w.PID, w.PGID); err != nil {
			m.Log.Printf("job %s: adopt pid %d: %v", j.ID, w.PID, err)
			return false
		}
		j.PID, j.PGID = w.PID, w.PGID
		m.Log.Printf("job %s: adopted wrapper pid=%d", j.ID, w.PID)
		return true
	}
	if j.StartedAt != nil && now.Sub(*j.StartedAt) > LaunchGrace {
		m.finish(j, store.StatusFailed, StreamResult{}, 0,
			"launch interrupted: the wrapper never registered (no valid wrapper.json); work dir kept", now)
	}
	return false
}

func (m *Manager) collect() error {
	running, err := m.Store.ListByStatus(store.StatusRunning)
	if err != nil {
		return err
	}
	now := m.Now()
	for _, j := range running {
		if j.PID == 0 && !m.adopt(j, now) {
			continue
		}
		dir := m.JobDir(j.ID)
		dur := 0
		if j.StartedAt != nil {
			dur = int(now.Sub(*j.StartedAt).Seconds())
		}
		// exit_code is written last: the stream is read only after it is seen, so it is complete.
		if code, ok := readExitCode(dir); ok {
			res := parseStream(filepath.Join(dir, "stream.jsonl"))
			status, msg := Verdict(code, res, dir)
			m.finish(j, status, res, dur, msg, now)
			continue
		}
		if j.StartedAt != nil && now.Sub(*j.StartedAt) > time.Duration(j.TimeoutMinutes)*time.Minute {
			m.killGroup(j.ID, j.PID, j.PGID)
			res := parseStream(filepath.Join(dir, "stream.jsonl"))
			m.finish(j, store.StatusTimeout, res, dur, fmt.Sprintf("timeout after %d minutes", j.TimeoutMinutes), now)
			continue
		}
		if !alive(j.PID) {
			// Re-check: the wrapper may have written exit_code right before exiting.
			code, ok := readExitCode(dir)
			res := parseStream(filepath.Join(dir, "stream.jsonl"))
			if ok {
				status, msg := Verdict(code, res, dir)
				m.finish(j, status, res, dur, msg, now)
				continue
			}
			m.finish(j, store.StatusFailed, res, dur, "wrapper died without writing exit_code (see stderr.log)", now)
		}
	}
	return nil
}

func (m *Manager) finish(j *store.Job, status string, res StreamResult, dur int, msg string, now time.Time) {
	if err := m.Store.Finish(j.ID, j.LaunchToken, status, res.Turns, res.CostUSD, dur, msg, now); err != nil {
		m.Log.Printf("job %s: finish %s: %v", j.ID, status, err)
		return
	}
	m.Log.Printf("job %s %s (%s)", j.ID, status, msg)
}

// Verdict maps the wrapper's exit code and stream result to a terminal status.
// exit 0 without a report is a failure.
// A sweep marker (processes outlived claude) fails the job whatever claude's exit was.
func Verdict(code int, res StreamResult, dir string) (string, string) {
	if reason := sweepFailure(dir); reason != "" {
		return store.StatusFailed, reason
	}
	// Closed by the idle-after-result rule: the wrapper judged the last result (exit_code 0/1); the
	// row records the rule either way.
	if reason := idleClosedReason(dir); reason != "" {
		if code != 0 || res.IsError || !hasReport(dir) {
			return store.StatusFailed, reason
		}
		return store.StatusSucceeded, reason
	}
	if code != 0 {
		return store.StatusFailed, fmt.Sprintf("claude exited with code %d", code)
	}
	if res.IsError {
		return store.StatusFailed, "claude result is an error: " + res.Subtype
	}
	if !hasReport(dir) {
		return store.StatusFailed, "exit 0 without a report"
	}
	return store.StatusSucceeded, ""
}

func hasReport(dir string) bool {
	b, err := os.ReadFile(filepath.Join(dir, "report.md"))
	return err == nil && strings.TrimSpace(string(b)) != ""
}

// Cancel stops a queued or running job. Final statuses cannot be cancelled.
func (m *Manager) Cancel(id string) error {
	unlock, err := m.lock()
	if err != nil {
		return err
	}
	defer unlock()
	j, err := m.Store.Get(id)
	if err != nil {
		return err
	}
	// Same lock as the wrapper's admission: a wrapper registered before this point is killed below,
	// one that comes later sees the cancelled row and does not start claude.
	unlockAdmit, err := lockAdmitIfExists(m.JobDir(id))
	if err != nil {
		return err
	}
	defer unlockAdmit()
	switch j.Status {
	case store.StatusQueued:
	case store.StatusRunning:
		pid, pgid := j.PID, j.PGID
		if pgid == 0 { // claimed, pid not recorded: the wrapper may still have registered
			if w, err := ReadWrapperInfo(m.JobDir(id)); err == nil && w.Token == j.LaunchToken {
				pid, pgid = w.PID, w.PGID
			}
		}
		if pgid != 0 {
			m.killGroup(id, pid, pgid)
		}
	default:
		return fmt.Errorf("job %s is %s: %w", id, j.Status, store.ErrImmutable)
	}
	now := m.Now()
	dur := 0
	if j.StartedAt != nil {
		dur = int(now.Sub(*j.StartedAt).Seconds())
	}
	res := parseStream(filepath.Join(m.JobDir(id), "stream.jsonl"))
	return m.Store.Finish(id, "", store.StatusCancelled, res.Turns, res.CostUSD, dur, "cancelled", now)
}
