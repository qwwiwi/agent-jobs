package jobs

import (
	"context"
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
	// Loc is the time zone schedules are read in (nil = the local time zone; overridable in tests).
	Loc *time.Location
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

	dailyMu      sync.Mutex     // held while a daily run is in progress (two never overlap)
	dailyWG      sync.WaitGroup // the daily run goroutine (WaitDaily)
	dailyPruneFn func(context.Context) (PruneReport, error)
	dailyProcsFn func(context.Context) (ProcsReport, error)
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
	// Runner is claude or codex; empty = by model (gpt-* runs on codex, everything else on claude).
	Runner string
	// ReasoningEffort is the codex reasoning effort (low|medium|high); empty = config, else high.
	ReasoningEffort string
	// ScheduleID is the schedule that starts the job (set by the daemon, never by a caller).
	ScheduleID string
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

// spawnEnv is the wrapper's start environment: jobSpawnEnv, plus the user bus fallback (userBusEnv)
// when it starts through `systemd-run --user --scope`. A scope unit runs no process of its own:
// systemd-run execs the wrapper with this environment, so the user manager's environment never
// reaches the job.
func (m *Manager) spawnEnv(unit string) []string {
	env := m.jobSpawnEnv()
	if unit != "" {
		env = userBusEnv(env)
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
	j, err := m.newJob(r)
	if err != nil {
		return nil, err
	}
	if err := m.Store.Insert(j); err != nil {
		return nil, err
	}
	return j, nil
}

// newJob validates the request (the job_start rules: kind, limits, model, runner, codex rules,
// repo and base) and returns the queued job row it would insert, with defaults applied.
func (m *Manager) newJob(r StartRequest) (*store.Job, error) {
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
	model, err := resolveModel(m.Cfg.Models, r.Kind, strings.TrimSpace(r.Model), r.Complexity)
	if err != nil {
		return nil, err
	}
	runner, err := ResolveRunner(r.Runner, model)
	if err != nil {
		return nil, err
	}
	effort := strings.TrimSpace(r.ReasoningEffort)
	if err := ValidateReasoningEffort(effort); err != nil {
		return nil, err
	}
	if runner == store.RunnerCodex {
		if r.MaxTurns != 0 || r.MaxBudgetUSD != 0 {
			return nil, errors.New("max_turns and max_budget_usd do not apply to a codex job " +
				"(codex has no turn or budget caps; the timeout is the limit)")
		}
		if err := m.Cfg.checkCodexJob(r.Kind); err != nil {
			return nil, err
		}
		effort = m.Cfg.reasoningEffort(effort)
		turns, budget = 0, 0 // not enforced on codex: stored as 0, shown as n/a (codex)
	} else if effort != "" {
		return nil, errors.New("reasoning_effort applies only to a codex job")
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
		Complexity: r.Complexity, Runner: runner, Billing: store.BillingSubscription, ReasoningEffort: effort,
		ScheduleID: r.ScheduleID,
	}
	return j, nil
}

// Tick is one daemon pass: collect finished/timed-out jobs, retry scope stops that failed, fire the
// schedules due this minute, start queued ones (≤2 running), send job-done lines, and clean up worktrees whose notify is older than the
// cleanup delay.
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
	if err := m.retryScopeStops(); err != nil {
		errs = append(errs, fmt.Errorf("scope stop: %w", err))
	}
	if err := m.fireSchedules(); err != nil {
		errs = append(errs, fmt.Errorf("schedules: %w", err))
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
		j.Attempt++ // as Claim counted it
		if err := m.launch(j, token); errors.Is(err, errScopeRetry) {
			m.Log.Printf("job %s: %v", j.ID, err)
			if uerr := m.Store.Unclaim(j.ID, token); uerr != nil {
				m.Log.Printf("job %s: requeue: %v", j.ID, uerr)
			}
			return nil // the next queued jobs would meet the same user manager
		} else if err != nil {
			m.Log.Printf("job %s refused to start: %v", j.ID, err)
			if ferr := m.Store.Finish(j.ID, token, store.StatusFailed, 0, 0, 0, "refused to start: "+err.Error(), m.Now()); ferr != nil {
				m.Log.Printf("job %s: mark failed: %v", j.ID, ferr)
			} else {
				running-- // the failed launch no longer occupies a slot
				m.recordStatsOf(j.ID)
			}
		}
	}
	return nil
}

// ClaudeArgs is the reference command line (prompt.md arrives on stdin) with a pre-assigned session
// id instead of --no-session-persistence (DEVIATIONS #15): the session stays on disk so the job can
// be resumed in it.
func ClaudeArgs(j *store.Job, jobDir string) []string {
	return append(claudeBaseArgs(j, jobDir), "--session-id", j.SessionID)
}

// ResumeClaudeArgs continues the job's session: the same command line with --resume <session id>.
func ResumeClaudeArgs(j *store.Job, jobDir string) []string {
	return append(claudeBaseArgs(j, jobDir), "--resume", j.SessionID)
}

func claudeBaseArgs(j *store.Job, jobDir string) []string {
	return []string{
		"-p", "--output-format", "stream-json", "--verbose", "--model", j.Model,
		"--max-turns", fmt.Sprint(j.MaxTurns), "--max-budget-usd", fmt.Sprintf("%g", j.MaxBudgetUSD),
		"--permission-mode", "bypassPermissions",
		"--settings", filepath.Join(jobDir, "settings.json"), "--setting-sources", "project",
		"--mcp-config", filepath.Join(jobDir, "mcp.json"), "--strict-mcp-config",
		"--add-dir", jobDir,
	}
}

// launchPlan is what one launch runs: the program (empty = claude), its arguments and the wrapper's
// options.
type launchPlan struct {
	bin     string
	args    []string
	wrapper []string // run-wrapper options before "--" (--prompt, --fallback-prompt, --fallback-session)
}

func (m *Manager) launch(j *store.Job, token string) error {
	// Containment first: a requeued job (errScopeRetry) has nothing prepared yet (no session, no
	// archived attempt), so Unclaim can put it back as it was.
	unit, err := m.scopeFor(j.ID)
	if err != nil {
		return err
	}
	var plan launchPlan
	var codexCfg Config
	if j.Runner == store.RunnerCodex {
		if j.Attempt > 1 {
			return errCodexResume
		}
		// The launch is authorized by config.json as it is now, not by the daemon's startup snapshot:
		// codex_write_allowed may have been turned off while the job was queued.
		if codexCfg, err = reloadCodexConfig(m.StateDir); err != nil {
			return err
		}
		if err := codexCfg.checkCodexJob(j.Kind); err != nil {
			return err
		}
	}
	if j.Attempt > 1 {
		p, err := m.prepareResume(j, token)
		if err != nil {
			return err
		}
		plan = p
	} else {
		if j.SessionID == "" {
			j.SessionID = NewSessionID()
		}
		if err := m.Store.SetSession(j.ID, token, j.SessionID, store.RunNew); err != nil {
			return fmt.Errorf("record session: %w", err)
		}
		if err := m.prepare(j, m.JobDir(j.ID)); err != nil {
			return err
		}
		plan = launchPlan{args: ClaudeArgs(j, m.JobDir(j.ID))}
		if j.Runner == store.RunnerCodex {
			// Fail closed on system/managed config layers and on MCP servers codex would load, read where
			// and how the job would run.
			if err := checkCodexSystemConfig(codexSystemConfigDir); err != nil {
				return err
			}
			if err := checkCodexMCP(codexCfg.codexBin(), filepath.Join(m.JobDir(j.ID), "work"), m.jobSpawnEnv()); err != nil {
				return err
			}
			plan = launchPlan{bin: codexCfg.codexBin(), args: CodexArgs(j, codexCfg.reasoningEffort(j.ReasoningEffort)),
				wrapper: []string{"--runner", store.RunnerCodex}}
		}
	}
	if unit != "" || j.ScopeUnit != "" {
		// Recorded before the spawn: a daemon that dies right after it still stops the scope later. A
		// resumed attempt runs in a new scope (same unit name) or, with cgroup_scope off, in none: the
		// previous attempt's unit is never taken for this one's.
		if err := m.Store.SetScope(j.ID, token, unit); err != nil {
			return fmt.Errorf("record scope: %w", err)
		}
		j.ScopeUnit = unit
	}
	pid, err := m.spawn(j, token, unit, plan)
	if err != nil {
		return err
	}
	// If the daemon dies before this line, the next tick adopts the wrapper via wrapper.json.
	if err := m.Store.SetPID(j.ID, token, pid, pid); err != nil {
		m.Log.Printf("job %s: record pid %d: %v (adopted next tick via wrapper.json)", j.ID, pid, err)
	}
	containment := "session only"
	if unit != "" {
		containment = "scope " + unit
	}
	m.Log.Printf("job %s started pid=%d runner=%s model=%s containment=%s", j.ID, pid, j.Runner, j.Model, containment)
	return nil
}

// spawn starts the detached wrapper; the wrapper registers itself in wrapper.json before claude runs.
// With a scope unit the wrapper is started through `systemd-run --user --scope`, which execs it inside
// the new scope (same pid).
func (m *Manager) spawn(j *store.Job, token, unit string, plan launchPlan) (int, error) {
	dir := m.JobDir(j.ID)
	argv := append([]string{m.SelfBin, "run-wrapper", dir, "--token", token}, plan.wrapper...)
	bin := plan.bin
	if bin == "" {
		bin = m.Cfg.claudeBin()
	}
	argv = append(append(argv, "--", bin), plan.args...)
	if unit != "" {
		argv = append(scopeArgv(unit, fmt.Sprintf("agent-jobs job %s (agent %s)", j.ID, m.Agent), m.KillGrace), argv...)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = filepath.Join(dir, "work")
	cmd.Env = m.spawnEnv(unit)
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

// killJob ends a running job (timeout, cancel): its whole scope when it has one, else (or when the
// stop fails) its wrapper's process group.
func (m *Manager) killJob(j *store.Job, pid, pgid int) {
	if j.ScopeUnit != "" {
		err := m.stopScope(j.ID)
		if err == nil {
			return
		}
		m.Log.Printf("job %s: stop scope: %v; falling back to the process group", j.ID, err)
	}
	if pgid != 0 {
		m.killGroup(j.ID, pid, pgid)
	}
}

// endScope stops a scoped job's unit before its row turns terminal (a no-op for an already collected
// scope and for jobs without one). An error means the unit could still hold live processes: the stop is
// then recorded as pending (before the row turns terminal) and retried every tick (retryScopeStops).
func (m *Manager) endScope(j *store.Job) error {
	if j.ScopeUnit == "" {
		return nil
	}
	err := m.stopScope(j.ID)
	if err != nil {
		if perr := m.Store.SetScopePending(j.ID, true); perr != nil {
			m.Log.Printf("job %s: record pending scope stop: %v", j.ID, perr)
		}
	}
	return err
}

// retryScopeStops retries every pending scope stop (a stop that failed when the job ended) until the
// unit is gone; only then is the pending mark cleared.
func (m *Manager) retryScopeStops() error {
	pending, err := m.Store.ScopePending()
	if err != nil {
		return err
	}
	for _, j := range pending {
		if err := m.stopScope(j.ID); err != nil {
			m.Log.Printf("job %s: scope stop still failing (retry next tick): %v", j.ID, err)
			continue
		}
		if err := m.Store.SetScopePending(j.ID, false); err != nil {
			return err
		}
		m.Log.Printf("job %s: scope stopped on retry", j.ID)
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
		m.finish(j, store.StatusFailed, StreamResult{}, false, 0,
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
			res := parseRunnerStream(j.Runner, filepath.Join(dir, "stream.jsonl"))
			status, msg := Verdict(code, res, dir)
			m.finish(j, status, res, false, dur, msg, now)
			continue
		}
		if j.StartedAt != nil && now.Sub(*j.StartedAt) > time.Duration(j.TimeoutMinutes)*time.Minute {
			m.killJob(j, j.PID, j.PGID)
			res, killed := killedResult(j.Runner, dir)
			m.finish(j, store.StatusTimeout, res, killed, dur, fmt.Sprintf("timeout after %d minutes", j.TimeoutMinutes), now)
			continue
		}
		if !alive(j.PID) {
			// Re-check: the wrapper may have written exit_code right before exiting.
			if code, ok := readExitCode(dir); ok {
				res := parseRunnerStream(j.Runner, filepath.Join(dir, "stream.jsonl"))
				status, msg := Verdict(code, res, dir)
				m.finish(j, status, res, false, dur, msg, now)
				continue
			}
			res, killed := killedResult(j.Runner, dir)
			m.finish(j, store.StatusFailed, res, killed, dur, "wrapper died without writing exit_code (see stderr.log)", now)
		}
	}
	return nil
}

// killedResult is the stream result of an attempt the daemon ended by a kill, or whose wrapper died.
// Unless the wrapper wrote exit_code (the attempt ended by itself just before), a result line in
// stream.jsonl may be the job's own (the stream lies in the job dir, which the job can write): only
// the assistant turns are counted and the cost is unknown (killed is true).
func killedResult(runner, dir string) (res StreamResult, killed bool) {
	p := filepath.Join(dir, "stream.jsonl")
	if _, ok := readExitCode(dir); ok {
		return parseRunnerStream(runner, p), false
	}
	if runner == store.RunnerCodex {
		return StreamResult{Runner: runner, Turns: ParseCodexStream(p).Turns}, true
	}
	return StreamResult{Turns: streamTurns(p)}, true
}

// finish ends a running attempt; killed marks one ended by a kill (see killedResult). A timed-out or
// failed attempt can be resumed, so what it left outside its process group is swept first.
func (m *Manager) finish(j *store.Job, status string, res StreamResult, killed bool, dur int, msg string, now time.Time) {
	m.noteFallback(j)
	// Whatever ended the job, nothing of it may outlive the row turning terminal.
	if err := m.endScope(j); err != nil {
		// Fail closed: the job is over for the daemon, but it must not count as a clean finish.
		m.Log.Printf("job %s: %v", j.ID, err)
		note := ReasonSurvivors + " (scope not stopped, retried every tick: " + err.Error() + ")"
		if status == store.StatusTimeout {
			msg = Printable(msg+"; "+note, 300)
		} else {
			status, msg = store.StatusFailed, Printable(note, 300)
		}
	}
	if err := m.Store.Finish(j.ID, j.LaunchToken, status, res.Turns, res.CostUSD, dur, msg, now); err != nil {
		m.Log.Printf("job %s: finish %s: %v", j.ID, status, err)
		return
	}
	if status == store.StatusTimeout || status == store.StatusFailed {
		m.noteSweep(j.ID, m.sweepEscaped(j.ID))
	}
	m.recordAttemptEnd(j, res, killed)
	m.Log.Printf("job %s %s (%s)", j.ID, status, msg)
	m.recordStatsOf(j.ID)
}

// noteSweep appends the sweep's note (if any) to a job that is no longer running.
func (m *Manager) noteSweep(id, note string) {
	if note == "" {
		return
	}
	m.Log.Printf("job %s: %s", id, note)
	if err := m.Store.AppendError(id, note); err != nil {
		m.Log.Printf("job %s: record note: %v", id, err)
	}
}

// recordAttemptEnd records, for a finished attempt, whether its cost is unknown (killed, or claude ran
// but no result event came) and the digest of its session transcript (checked before the session is
// resumed). Call it after the sweep: nothing of the job may change the transcript after the digest.
func (m *Manager) recordAttemptEnd(j *store.Job, res StreamResult, killed bool) {
	dir := m.JobDir(j.ID)
	// Codex reports no price: a codex attempt's cost is always unknown.
	fi, err := os.Lstat(filepath.Join(dir, "stream.jsonl"))
	if killed || j.Runner == store.RunnerCodex || (!res.Found && err == nil && fi.Size() > 0) {
		if err := m.Store.SetCostUnknown(j.ID); err != nil {
			m.Log.Printf("job %s: mark cost unknown: %v", j.ID, err)
		}
	}
	digest := ""
	if p := m.transcriptPath(j); p != "" {
		d, err := sessionDigest(p, j.SessionID)
		if err != nil {
			m.Log.Printf("job %s: session digest: %v", j.ID, err)
		}
		digest = d
	}
	if err := m.Store.SetSessionDigest(j.ID, digest); err != nil {
		m.Log.Printf("job %s: record session digest: %v", j.ID, err)
	}
}

// sweepEscaped ends what a stopped, cancelled, timed-out or failed job left outside its process group (a process that
// called setpgid, found by the job marker or the job's session, as doctor procs --kill does) and
// returns a note for the job: what was killed, what was left running (a process leading its own
// session may host other work and is never killed), or that it could not be checked.
func (m *Manager) sweepEscaped(id string) string {
	rep, err := m.doctorProcs(true, id)
	if err != nil {
		return "processes outside the job's process group not checked: " + err.Error()
	}
	if !rep.Supported {
		return "processes outside the job's process group not checked (needs Linux /proc)"
	}
	var notes, left []string
	if rep.Killed > 0 {
		notes = append(notes, fmt.Sprintf("%d process(es) outside the job's process group killed", rep.Killed))
	}
	for _, g := range rep.Ghosts {
		if g.Killed {
			continue
		}
		why := "leads its own session, never killed"
		if g.KillError != "" {
			why = g.KillError
		}
		left = append(left, fmt.Sprintf("pid %d %s (%s)", g.PID, Printable(g.Cmdline, 80), why))
	}
	if len(left) > 0 {
		notes = append(notes, "left running: "+strings.Join(left, ", ")+"; see doctor procs")
	}
	return strings.Join(notes, "; ")
}

// killRunning ends a running job (cancel, stop): its whole scope when it has one, else its wrapper's
// process group (SIGTERM, KillGrace, SIGKILL). Callers hold the lifecycle lock and the job's admit lock.
func (m *Manager) killRunning(j *store.Job) {
	pid, pgid := j.PID, j.PGID
	if pgid == 0 { // claimed, pid not recorded: the wrapper may still have registered
		if w, err := ReadWrapperInfo(m.JobDir(j.ID)); err == nil && w.Token == j.LaunchToken {
			pid, pgid = w.PID, w.PGID
		}
	}
	m.killJob(j, pid, pgid)
}

// Verdict maps the wrapper's exit code and stream result to a terminal status.
// exit 0 without a report is a failure; so is a codex exit 0 whose stream has no terminal turn.completed.
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
		return store.StatusFailed, fmt.Sprintf("%s exited with code %d", res.program(), code)
	}
	if res.IsError {
		return store.StatusFailed, res.program() + " result is an error: " + res.Subtype
	}
	if res.Runner == store.RunnerCodex {
		if reason := codexStreamFailure(res); reason != "" {
			return store.StatusFailed, reason
		}
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

// Cancel stops a queued or running job, or gives up a stopped one (its work dir is then cleaned up
// like any finished job's). Final statuses cannot be cancelled.
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
	case store.StatusStopped:
		// Nothing runs; keep the turns, cost and duration recorded at the stop.
		if err := m.Store.Finish(id, "", store.StatusCancelled, j.Turns, j.CostUSD, j.DurationSec, "cancelled after stop", m.Now()); err != nil {
			return err
		}
		m.recordStatsOf(id) // the job's stats row turns from stopped into cancelled
		return nil
	case store.StatusRunning:
		m.killRunning(j)
	default:
		return fmt.Errorf("job %s is %s: %w", id, j.Status, store.ErrImmutable)
	}
	now := m.Now()
	if j.Status == store.StatusQueued {
		// Never ran: stream.jsonl, if any, is the previous attempt's, already in the attempts table.
		if err := m.Store.Finish(id, "", store.StatusCancelled, 0, 0, 0, "cancelled", now); err != nil {
			return err
		}
		m.recordStatsOf(id)
		return nil
	}
	dur := 0
	if j.StartedAt != nil {
		dur = int(now.Sub(*j.StartedAt).Seconds())
	}
	// Like a stop: a killed attempt's result event, if any, was written by the job, not by claude.
	res, killed := killedResult(j.Runner, m.JobDir(id))
	msg := "cancelled"
	if err := m.endScope(j); err != nil {
		m.Log.Printf("job %s: %v", id, err)
		msg = Printable("cancelled; "+ReasonSurvivors+" (scope not stopped, retried every tick: "+err.Error()+")", 300)
	}
	if err := m.Store.Finish(id, "", store.StatusCancelled, res.Turns, res.CostUSD, dur, msg, now); err != nil {
		return err
	}
	if killed {
		if err := m.Store.SetCostUnknown(id); err != nil {
			m.Log.Printf("job %s: mark cost unknown: %v", id, err)
		}
	}
	m.noteSweep(id, m.sweepEscaped(id))
	m.recordStatsOf(id)
	return nil
}
