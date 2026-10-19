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
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

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
	dailyNoteMu  sync.Mutex     // guards dailyHeld and dailySkipped
	dailyHeld    string         // the day whose run holds dailyMu
	dailySkipped string         // the day a skip was last logged (at most one line per day)
	dailyWG      sync.WaitGroup // the daily run goroutine (WaitDaily)
	dailyPruneFn func(context.Context) (PruneReport, error)
	dailyProcsFn func(context.Context) (ProcsReport, error)

	hbMu sync.Mutex     // held while a heartbeat started by StartHeartbeat runs
	hbWG sync.WaitGroup // that heartbeat's goroutine (WaitHeartbeat)
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
	// AcceptanceCmd is a shell command the job must pass after claude exits 0 (fix and general jobs only).
	AcceptanceCmd string
	// OwnerWaits marks a job started at the owner's request: when its job-done line is delivered the
	// orchestrator's Stop hook holds the session until owner_replied (or the record expires).
	OwnerWaits bool
	// Skills are edits to the job's default skills (E7): "name" or "+name" adds, "-name" removes.
	Skills []string
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
	if err := m.Cfg.checkBriefSize(r.Brief); err != nil {
		return nil, err
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
	acceptance, err := ValidateAcceptanceCmd(r.Kind, r.AcceptanceCmd, m.Cfg.AcceptanceAllowed)
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
	selected, err := m.selectSkills(r.Kind, repo, r.Skills)
	if err != nil {
		return nil, err
	}
	now := m.Now()
	j := &store.Job{
		ID: newID(now), Agent: m.Agent, Title: strings.TrimSpace(r.Title), Kind: r.Kind, Brief: r.Brief,
		RepoPath: repo, BaseCommit: base, Status: store.StatusQueued, Model: model,
		MaxTurns: turns, MaxBudgetUSD: budget, TimeoutMinutes: timeout, CreatedAt: now,
		Complexity: r.Complexity, Runner: runner, Billing: store.BillingSubscription, ReasoningEffort: effort,
		ScheduleID: r.ScheduleID, AcceptanceCmd: acceptance, OwnerWaits: r.OwnerWaits, Skills: selected,
	}
	return j, nil
}

// Tick is one daemon pass: collect finished/timed-out jobs, retry scope stops and report redactions
// that failed, fire the schedules due this minute, judge the checks (dead-man's switch), start queued
// ones (at most max_running running, default 2), send job-done lines, and clean up worktrees whose
// notify is older than the cleanup delay; then, with the lifecycle lock released, deliver the checks' pending missed alerts.
func (m *Manager) Tick() error {
	err := m.tickLocked()
	// The checks' missed alerts are typed after the lifecycle lock is released: a stalled tmux
	// holds only tmux.lock, never the lifecycle (cancel, a manual tick, the other daemon).
	if derr := m.deliverCheckAlerts(); derr != nil {
		err = errors.Join(err, fmt.Errorf("check alerts: %w", derr))
	}
	return err
}

// tickLocked is the part of Tick that runs under the lifecycle lock.
func (m *Manager) tickLocked() error {
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
	if err := m.retryRedactions(); err != nil {
		errs = append(errs, fmt.Errorf("redaction: %w", err))
	}
	if err := m.fireSchedules(); err != nil {
		errs = append(errs, fmt.Errorf("schedules: %w", err))
	}
	if err := m.watchChecks(); err != nil {
		errs = append(errs, fmt.Errorf("checks: %w", err))
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
	limit := m.Cfg.MaxRunningJobs()
	for _, j := range queued {
		if running >= limit {
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
			m.Log.Printf("job %s: %s", j.ID, RedactText(err.Error()))
			if uerr := m.Store.Unclaim(j.ID, token); uerr != nil {
				m.Log.Printf("job %s: requeue: %v", j.ID, uerr)
			}
			return nil // the next queued jobs would meet the same user manager
		} else if err != nil {
			m.Log.Printf("job %s refused to start: %s", j.ID, RedactText(err.Error()))
			m.removeSkills(j)
			if ferr := m.Store.Finish(j.ID, token, store.StatusFailed, 0, 0, 0, "refused to start: "+err.Error(), m.Now()); ferr != nil {
				m.Log.Printf("job %s: mark failed: %v", j.ID, ferr)
			} else {
				running-- // the failed launch no longer occupies a slot
				m.settled(j.ID)
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
		if err := m.prepare(j, token, m.JobDir(j.ID)); err != nil {
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
	if err := m.Store.SetSpawnedPID(j.ID, token, pid); err != nil {
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

// liveSpawnedGroup reports whether the job's process group as the daemon recorded it at the spawn
// (PGIDSpawned; never a group adopted from the job-writable wrapper.json) still has members. That
// proves past ownership only: once the group emptied (unseen, say while the daemon was down) the id may
// be a stranger's group, so nothing is signalled on it before every member is shown to be the job's
// (endSpawnedGroup). Holding up cleanup, prune or resume on a live group is the safe side.
func liveSpawnedGroup(j *store.Job) bool {
	return j.PGIDSpawned && plausibleGroup(j.PGID) == nil && groupAlive(j.PGID)
}

// spawnedGroupAlive is liveSpawnedGroup for a job that is no longer running: once its group is gone
// the mark is cleared, so a later group that reuses the id never holds up its cleanup, prune or resume.
func (m *Manager) spawnedGroupAlive(j *store.Job) bool {
	if liveSpawnedGroup(j) {
		return true
	}
	if j.PGIDSpawned && finished(j.Status) {
		if err := m.Store.ClearPGIDSpawned(j.ID); err != nil {
			m.Log.Printf("job %s: forget spawned process group: %v", j.ID, err)
		} else {
			j.PGIDSpawned = false
		}
	}
	return false
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

// beforeTimeoutClaim runs between collect's timeout preselection and its ClaimTimeout; a variable so
// tests can interleave the wrapper's acceptance start.
var beforeTimeoutClaim = func(string) {}

// killJob ends a running job (timeout, cancel, stop): its whole scope when it has one, else (or when
// the stop fails) its wrapper's process group, then the process group of a running acceptance command
// (a group of its own, which neither a session-only job's group kill nor a dead wrapper ends).
func (m *Manager) killJob(j *store.Job, pid, pgid int) {
	defer m.killAcceptance(j.ID)
	if j.ScopeUnit != "" {
		err := m.stopScope(j.ID)
		if err == nil {
			return
		}
		m.Log.Printf("job %s: stop scope: %v; falling back to the process group", j.ID, err)
	}
	if pgid == 0 {
		return
	}
	if j.PGIDSpawned && pgid == j.PGID && !alive(pid) {
		// The wrapper is gone: the recorded group is ended only member by member, each shown to be the job's.
		if note := m.endSpawnedGroup(j); note != "" {
			m.Log.Printf("job %s: %s", j.ID, note)
		}
		return
	}
	m.killGroup(j, pid, pgid)
}

// killAcceptance ends the acceptance command's process group recorded in the job row (read now: it is
// recorded after the caller's row was read). The row is written by the wrapper, not the job. While the
// group's leader is alive and leads it, the whole group is signalled only if the leader carries this
// job's marker. Once the leader is gone (the shell died on SIGTERM while a child that ignores it keeps
// running, and the wrapper was killed before its own grace ended the group), the surviving members are
// ended one by one (killLeaderlessGroup). Either way a recycled pid or pgid is never hit.
func (m *Manager) killAcceptance(id string) {
	j, err := m.Store.Get(id)
	if err != nil || j.AcceptancePGID == 0 {
		return
	}
	g := j.AcceptancePGID
	if err := plausibleGroup(g); err != nil {
		m.Log.Printf("job %s: refusing to kill acceptance process group %d: %v", id, g, err)
		return
	}
	if p, err := procStatFull(g); err == nil && p.state != "Z" && p.pgid == g {
		if procMarker(g) != id {
			m.Log.Printf("job %s: refusing to kill acceptance process group %d: its leader carries no marker of the job", id, g)
			return
		}
		m.signalGroup(g)
		return
	}
	m.killLeaderlessGroup(id, g)
}

// acceptanceMember reports whether pid is a live (non-zombie) member of process group g carrying the
// marker of job id.
func acceptanceMember(id string, g int) func(int) bool {
	return func(pid int) bool {
		p, err := procStatFull(pid)
		return err == nil && p.state != "Z" && p.pgid == g && procMarker(pid) == id
	}
}

// groupMembers lists the live (non-zombie) processes of process group g, except this process and its
// ancestors.
func groupMembers(g int) ([]int, error) {
	ents, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, err
	}
	skip := selfAndAncestors()
	var out []int
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 || skip[pid] {
			continue
		}
		if p, err := procStatFull(pid); err == nil && p.state != "Z" && p.pgid == g {
			out = append(out, pid)
		}
	}
	return out, nil
}

// killLeaderlessGroup ends the surviving members of acceptance process group g whose leader is gone,
// each carrying the marker of job id (endMarkedGroup).
func (m *Manager) killLeaderlessGroup(id string, g int) {
	m.endMarkedGroup(id, fmt.Sprintf("acceptance process group %d", g), g, acceptanceMember(id, g))
}

// groupEnd is what endMarkedGroup did.
type groupEnd int

const (
	groupGone     groupEnd = iota // no live member was left
	groupRefused                  // a member is not proven to be the job's, or the group could not be read: nothing signalled
	groupEnded                    // every member was ended
	groupSurvived                 // members survived SIGKILL (or could not be signalled)
)

// endMarkedGroup ends the live members of process group g (named what in the log) of job id: only
// when every member passes isMember (one that does not means the pgid is not, or no longer, the job's
// group, and nothing is signalled), each through a pidfd opened before its membership is re-checked
// (signalMember), SIGTERM, KillGrace, then SIGKILL to whatever member is still there. Without pidfd
// support nothing is signalled. why says what kept the group from being ended.
func (m *Manager) endMarkedGroup(id, what string, g int, isMember func(int) bool) (res groupEnd, why string) {
	members, err := groupMembers(g)
	if err != nil {
		m.Log.Printf("job %s: %s not checked: %v", id, what, err)
		return groupRefused, "members not checked: " + err.Error()
	}
	if len(members) == 0 {
		return groupGone, ""
	}
	for _, pid := range members {
		if !isMember(pid) {
			why = fmt.Sprintf("member %d is not proven to be the job's (no marker of the job, or outside its scope)", pid)
			m.Log.Printf("job %s: refusing to kill %s: %s", id, what, why)
			return groupRefused, why
		}
	}
	marked := func() []int {
		pids, err := groupMembers(g)
		if err != nil {
			m.Log.Printf("job %s: %s: %v", id, what, err)
			return nil
		}
		return slices.DeleteFunc(pids, func(pid int) bool { return !isMember(pid) })
	}
	signal := func(sig syscall.Signal) {
		for _, pid := range marked() {
			if _, err := signalMember(pid, isMember, sig); err != nil {
				m.Log.Printf("job %s: %s: %v", id, what, err)
				why = err.Error()
			}
		}
	}
	m.Log.Printf("job %s: %s has no leader; ending %d marked member(s)", id, what, len(members))
	signal(syscall.SIGTERM)
	deadline := time.Now().Add(m.KillGrace)
	for time.Now().Before(deadline) {
		if len(marked()) == 0 {
			return groupEnded, ""
		}
		time.Sleep(100 * time.Millisecond)
	}
	signal(syscall.SIGKILL)
	for i := 0; i < 20; i++ {
		if len(marked()) == 0 {
			return groupEnded, ""
		}
		time.Sleep(50 * time.Millisecond)
	}
	m.Log.Printf("job %s: %s: marked members survived SIGKILL", id, what)
	return groupSurvived, why
}

// jobGroupMember reports whether pid is a live (non-zombie) member of process group g that carries
// the marker of job j and, for a job run in a scope, lives in that scope's cgroup: the identity that
// shows a group recorded at the spawn is still the job's, not a stranger's group that reuses the id.
func jobGroupMember(j *store.Job, g int) func(int) bool {
	return func(pid int) bool {
		p, err := procStatFull(pid)
		if err != nil || p.state == "Z" || p.pgid != g || procMarker(pid) != j.ID {
			return false
		}
		if j.ScopeUnit == "" {
			return true
		}
		b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "cgroup"))
		return err == nil && jobScopeIn(string(b)) == j.ScopeUnit
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
// It refuses (and logs) unless pgid is plausible, is the live wrapper pid's group and that pid is
// shown to be this job's wrapper (jobWrapper), since a live pid alone proves nothing: the wrapper may
// have died and its pid and group id been reused by an unrelated group. On Linux the leader is pinned
// by a pidfd while it is checked, and the group is signalled only while that same leader is alive (a
// group id cannot be reused while its leader lives). Without /proc (macOS) the identity cannot be read
// and only the group check applies, as before.
func (m *Manager) killGroup(j *store.Job, pid, pgid int) {
	if err := verifyGroup(pid, pgid); err != nil {
		m.Log.Printf("job %s: refusing to kill process group %d: %v", j.ID, pgid, err)
		return
	}
	if !procAvailable() {
		m.signalGroup(pgid)
		return
	}
	fd, err := pidfdOpen(pid)
	if err != nil {
		m.Log.Printf("job %s: refusing to kill process group %d: pidfd_open %d: %v", j.ID, pgid, pid, err)
		return
	}
	defer unix.Close(fd)
	// Checked only now: the descriptor pins one process, so these reads are of that process (or, if it
	// died meanwhile, of a stranger, and then the liveness check below fails).
	if err := verifyGroup(pid, pgid); err != nil {
		m.Log.Printf("job %s: refusing to kill process group %d: %v", j.ID, pgid, err)
		return
	}
	if !jobWrapper(j, m.JobDir(j.ID))(pid) {
		m.Log.Printf("job %s: refusing to kill process group %d: pid %d is not proven to be the job's wrapper (a recycled pid?)", j.ID, pgid, pid)
		return
	}
	if err := pidfdSend(fd, 0); err != nil {
		m.Log.Printf("job %s: refusing to kill process group %d: wrapper pid %d: %v", j.ID, pgid, pid, err)
		return
	}
	m.signalGroup(pgid)
}

// procAvailable reports whether process identities can be read from /proc (Linux).
func procAvailable() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	_, err := os.Stat(filepath.Join(procRoot, "self", "stat"))
	return err == nil
}

// jobWrapper reports whether pid is the wrapper of job j: a live (non-zombie) process whose argv is
// `run-wrapper <jobDir> --token <the job's launch token>` and, for a job run in a scope, which lives in
// that scope's cgroup.
func jobWrapper(j *store.Job, jobDir string) func(int) bool {
	return func(pid int) bool {
		if j.LaunchToken == "" {
			return false
		}
		if p, err := procStatFull(pid); err != nil || p.state == "Z" {
			return false
		}
		argv, err := procCmdline(pid)
		if err != nil || !IsWrapperCmdline(argv) {
			return false
		}
		i := slices.Index(argv, "run-wrapper")
		if i < 0 || len(argv) < i+4 || argv[i+1] != jobDir || argv[i+3] != j.LaunchToken {
			return false
		}
		if j.ScopeUnit == "" {
			return true
		}
		b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "cgroup"))
		return err == nil && jobScopeIn(string(b)) == j.ScopeUnit
	}
}

// endOrphanedGroup ends what is left of a job whose wrapper died without writing exit_code: its
// process group as recorded at the spawn (liveSpawnedGroup; a group adopted from wrapper.json is never
// signalled here) and its acceptance command. It returns a note for the row, "" when no group was left.
func (m *Manager) endOrphanedGroup(j *store.Job) string {
	defer m.killAcceptance(j.ID)
	if !liveSpawnedGroup(j) {
		return ""
	}
	m.Log.Printf("job %s: wrapper died, its process group %d is still alive; ending it", j.ID, j.PGID)
	return m.endSpawnedGroup(j)
}

// endSpawnedGroup ends the group recorded at the job's spawn once its wrapper is gone: member by member
// and only when every member is shown to be the job's (jobGroupMember), since the id alone proves
// nothing once the group may have emptied and the id been reused. It fails closed (nothing signalled)
// and returns a note for the row, "" when no member was left.
func (m *Manager) endSpawnedGroup(j *store.Job) string {
	g := j.PGID
	if err := plausibleGroup(g); err != nil {
		return fmt.Sprintf("leftover process group %d not signalled: %v", g, err)
	}
	res, why := m.endMarkedGroup(j.ID, fmt.Sprintf("process group %d", g), g, jobGroupMember(j, g))
	switch res {
	case groupRefused:
		return fmt.Sprintf("leftover process group %d not signalled: %s", g, why)
	case groupSurvived:
		if why != "" {
			why = " (" + why + ")"
		}
		return fmt.Sprintf("leftover process group %d survived SIGKILL%s; %s", g, why, ReasonSurvivors)
	case groupEnded:
		return fmt.Sprintf("leftover process group %d killed", g)
	}
	return ""
}

// signalGroup sends SIGTERM to the process group, waits up to KillGrace, then SIGKILL.
func (m *Manager) signalGroup(pgid int) {
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
		m.Log.Printf("job %s: %s", j.ID, RedactText(err.Error()))
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
			status, msg := m.verdict(j, code, res, dir)
			m.finish(j, status, res, false, dur, msg, now)
			continue
		}
		if j.StartedAt != nil && now.After(jobDeadline(j)) {
			// j was read at the start of the tick: the wrapper may have started the acceptance command
			// since. The decision is a conditional update against the row as it is now; it also closes
			// the start of an acceptance command from here on.
			beforeTimeoutClaim(j.ID)
			claimed, err := m.Store.ClaimTimeout(j.ID, j.LaunchToken, now)
			if err != nil {
				m.Log.Printf("job %s: claim timeout: %v", j.ID, err)
				continue
			}
			if !claimed {
				continue // the acceptance command started meanwhile: its deadline counts from the next tick
			}
			m.killJob(j, j.PID, j.PGID)
			res, killed := killedResult(j.Runner, dir)
			m.finish(j, store.StatusTimeout, res, killed, dur, fmt.Sprintf("timeout after %d minutes", j.TimeoutMinutes), now)
			continue
		}
		if !alive(j.PID) {
			// Re-check: the wrapper may have written exit_code right before exiting.
			if code, ok := readExitCode(dir); ok {
				res := parseRunnerStream(j.Runner, filepath.Join(dir, "stream.jsonl"))
				status, msg := m.verdict(j, code, res, dir)
				m.finish(j, status, res, false, dur, msg, now)
				continue
			}
			// Nothing of the job may outlive the row: claude would spend on, write the worktree that
			// cleanup removes, and race a resume in the same session.
			msg := "wrapper died without writing exit_code (see stderr.log)"
			if note := m.endOrphanedGroup(j); note != "" {
				msg += "; " + note
			}
			res, killed := killedResult(j.Runner, dir)
			m.finish(j, store.StatusFailed, res, killed, dur, msg, now)
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
	delivered := slices.Clone(j.SkillsDelivered)
	m.removeSkills(j)
	// Whatever ended the job, nothing of it may outlive the row turning terminal.
	if err := m.endScope(j); err != nil {
		// Fail closed: the job is over for the daemon, but it must not count as a clean finish.
		m.Log.Printf("job %s: %s", j.ID, RedactText(err.Error()))
		status, msg = scopeStopFailed(status, msg, err)
	} else if err := m.checkSkillLinks(j, delivered); err != nil {
		// Checked once nothing of the job runs any more.
		m.Log.Printf("job %s: %s", j.ID, RedactText(err.Error()))
		if status == store.StatusTimeout {
			msg = RedactPrintable(msg+"; "+err.Error(), 300)
		} else {
			status, msg = store.StatusFailed, RedactPrintable(err.Error(), 300)
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
	m.Log.Printf("job %s %s (%s)", j.ID, status, RedactText(msg)) // msg can carry the job's own text
	m.settled(j.ID)
}

// scopeStopFailed is the status and message of an attempt whose scope could not be stopped: a timeout
// stays a timeout, anything else fails. The message (the job's own text, the error) is redacted
// before it is cut.
func scopeStopFailed(status, msg string, err error) (string, string) {
	note := ReasonSurvivors + " (scope not stopped, retried every tick: " + err.Error() + ")"
	if status == store.StatusTimeout {
		return status, RedactPrintable(msg+"; "+note, 300)
	}
	return store.StatusFailed, RedactPrintable(note, 300)
}

// noteSweep appends the sweep's note (if any) to a job that is no longer running.
func (m *Manager) noteSweep(id, note string) {
	if note == "" {
		return
	}
	m.Log.Printf("job %s: %s", id, RedactText(note)) // command lines of processes the job left
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
		left = append(left, fmt.Sprintf("pid %d %s (%s)", g.PID, RedactPrintable(g.Cmdline, 80), why))
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
		m.settled(id) // the job's stats row turns from stopped into cancelled
		return nil
	case store.StatusRunning:
		m.killRunning(j)
		m.removeSkills(j)
	default:
		return fmt.Errorf("job %s is %s: %w", id, j.Status, store.ErrImmutable)
	}
	now := m.Now()
	if j.Status == store.StatusQueued {
		// Never ran: stream.jsonl, if any, is the previous attempt's, already in the attempts table.
		if err := m.Store.Finish(id, "", store.StatusCancelled, 0, 0, 0, "cancelled", now); err != nil {
			return err
		}
		m.settled(id)
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
		m.Log.Printf("job %s: %s", id, RedactText(err.Error()))
		msg = RedactPrintable("cancelled; "+ReasonSurvivors+" (scope not stopped, retried every tick: "+err.Error()+")", 300)
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
	m.settled(id)
	return nil
}
