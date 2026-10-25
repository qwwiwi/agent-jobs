package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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
	"github.com/qwwiwi/agent-jobs/internal/checklist"
	"github.com/qwwiwi/agent-jobs/internal/skills"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// Manager runs the job lifecycle for one agent.
type Manager struct {
	Agent                string
	StateDir             string
	Cfg                  Config
	runtimeConfig        atomic.Pointer[Config]
	configWatch          *ConfigReloader
	configPublishPending bool
	daemonLockHeld       bool
	Store                *store.Store
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

	mergeMu     sync.Mutex
	mergeChecks map[string]mergeCheck // job ID -> its last merge-tree result (MergeNote)

	hbMu sync.Mutex     // held while a heartbeat started by StartHeartbeat runs
	hbWG sync.WaitGroup // that heartbeat's goroutine (WaitHeartbeat)

	cleanupFails map[string]cleanupFail // job ID -> its consecutive cleanup failures (under the lifecycle lock)

	codexProbeMu  sync.Mutex
	codexProbe    *CodexSandboxProbe // the cached native sandbox probe (codexSandboxProbe)
	codexProbeKey string             // the codex binary identity it ran on (codexBinKey)
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

// OpenReadOnly opens an existing state for reads only (store.OpenReadOnly): it creates no directory or
// file, runs no migration and never waits for the store's lock. The manager has no brain and no own
// binary; it serves read paths such as the brief reminder, never a job's lifecycle.
func OpenReadOnly(agent string) (*Manager, error) {
	dir, err := StateDir(agent)
	if err != nil {
		return nil, err
	}
	cfg, err := LoadConfig(dir)
	if err != nil {
		return nil, err
	}
	st, err := store.OpenReadOnly(filepath.Join(dir, "jobs.db"))
	if err != nil {
		return nil, err
	}
	return &Manager{
		Agent: agent, StateDir: dir, Cfg: cfg, Store: st, Now: time.Now, Brain: brain.Disabled{},
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
	// ReasoningEffort is the job's reasoning effort: low|medium|high on codex (empty = config, else high),
	// low|medium|high|xhigh|max on claude (empty = effort_by_kind, else the kind's default).
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
	// Covers are the checklist requirement IDs the job works on (Wave F, F4): its report must then
	// carry a «### Requirements» block with one line per ID.
	Covers []string
	// Size is small or large (Wave F, F9 (a)); see checkSize.
	Size string
	// SizeRule is set by the callers that take a size (job_start, agent-jobs start): with config
	// size_required on, a fix or general job without one is refused. Schedules and gate jobs leave it off.
	SizeRule bool
	// Gate is the daemon's input of a gate job (StartGate only; Wave F, F3 / F5).
	Gate *store.GateInput
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
	if name := m.Config().Brain.TokenEnv; name != "" {
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

// Enqueue validates the request and inserts a queued job. It returns immediately. A codex job past the
// codex daily cap (codex_jobs_per_day) is refused.
func (m *Manager) Enqueue(r StartRequest) (*store.Job, error) {
	j, err := m.newJob(r)
	if err != nil {
		return nil, err
	}
	if limit := m.Config().CodexDailyCap(); j.Runner == store.RunnerCodex && limit > 0 {
		ok, err := m.Store.InsertCapped(j, j.CreatedAt.Add(-CodexCapWindow), limit)
		if err != nil {
			return nil, err
		}
		if !ok {
			n, _ := m.Store.CountCodexSince(m.Agent, j.CreatedAt.Add(-CodexCapWindow))
			return nil, fmt.Errorf("codex daily cap reached: %d codex jobs of agent %s in the last 24 hours, limit %d "+
				"(config codex_jobs_per_day, per agent; 0 turns it off); start a claude job or wait", n, m.Agent, limit)
		}
		m.logSize(j)
		return j, nil
	}
	if err := m.Store.Insert(j); err != nil {
		return nil, err
	}
	m.logSize(j)
	return j, nil
}

// CodexCapWindow is the window of the codex daily cap.
const CodexCapWindow = 24 * time.Hour

// CodexJobsToday is the codex daily cap's state: "n/limit" (codex jobs of the agent created in the last
// 24 hours), "n (cap off)" when codex_jobs_per_day is 0.
func (m *Manager) CodexJobsToday() (string, error) {
	n, err := m.Store.CountCodexSince(m.Agent, m.Now().Add(-CodexCapWindow))
	if err != nil {
		return "", err
	}
	if limit := m.Config().CodexDailyCap(); limit > 0 {
		return fmt.Sprintf("%d/%d", n, limit), nil
	}
	return fmt.Sprintf("%d (cap off)", n), nil
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
	if err := m.Config().checkBriefSize(r.Brief); err != nil {
		return nil, err
	}
	if !slices.Contains(Kinds, r.Kind) {
		return nil, fmt.Errorf("kind %q must be one of %s", r.Kind, strings.Join(Kinds, "|"))
	}
	if err := checkGateRequest(r); err != nil {
		return nil, err
	}
	turns, budget, timeout, err := ValidateLimits(r.MaxTurns, r.MaxBudgetUSD, r.TimeoutMinutes)
	if err != nil {
		return nil, err
	}
	model, err := resolveModel(m.Config().Models, r.Kind, strings.TrimSpace(r.Model), r.Complexity)
	if err != nil {
		return nil, err
	}
	runner, err := ResolveRunner(r.Runner, model)
	if err != nil {
		return nil, err
	}
	acceptance, err := ValidateAcceptanceCmd(r.Kind, r.AcceptanceCmd, m.Config().AcceptanceAllowed)
	if err != nil {
		return nil, err
	}
	effort := strings.TrimSpace(r.ReasoningEffort)
	if err := ValidateReasoningEffort(runner, effort); err != nil {
		return nil, err
	}
	if runner == store.RunnerCodex {
		if r.MaxTurns != 0 || r.MaxBudgetUSD != 0 {
			return nil, errors.New("max_turns and max_budget_usd do not apply to a codex job " +
				"(codex has no turn or budget caps; the timeout is the limit)")
		}
		if err := m.Config().checkCodexJob(r.Kind); err != nil {
			return nil, err
		}
		effort = m.Config().reasoningEffort(effort)
		turns, budget = 0, 0 // not enforced on codex: stored as 0, shown as n/a (codex)
	} else {
		// Passed as --effort, so a project effortLevel in the worktree cannot change it.
		effort = m.Config().claudeEffort(r.Kind, effort)
	}
	var repo, top, base, branch string
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
		// The base is resolved in the worktree that was given (its HEAD, its branch).
		if base, err = git(repo, "rev-parse", "--verify", ref+"^{commit}"); err != nil {
			return nil, fmt.Errorf("base %q: %w", ref, err)
		}
		branch = baseBranch(repo, ref)
		top = repo
		// The job stores the main worktree: a linked one (e.g. an earlier job's) may be removed
		// before this job's cleanup, which then could not reach the repository (#57).
		repo = mainWorktree(repo)
	} else if r.Base != "" {
		return nil, errors.New("base given without repo")
	}
	covers, err := ValidateCovers(r.Covers)
	if err != nil {
		return nil, err
	}
	// A covered job gets its rows in its prompt (F4): they are read now, from the work tree it was
	// started from, and kept with the job.
	var cover *store.CoverContract
	if len(covers) > 0 {
		if top == "" {
			return nil, errors.New("covers need a repo: the covered rows are read from its " + checklist.ChecklistRel)
		}
		if cover, err = m.coverContract(top, repo, base, covers); err != nil {
			return nil, err
		}
	}
	size, err := m.checkSize(r, covers, cover)
	if err != nil {
		return nil, err
	}
	var selected []string
	// A gate job gets nothing but its input: no catalog skills (checkGateRequest refused edits).
	if skills.BriefDeclinesSkills(r.Brief) {
		if len(r.Skills) > 0 {
			return nil, errors.New("skills: none conflicts with skills edits; omit the edits or remove the no-skills line")
		}
	} else if r.Gate == nil {
		selected, err = m.selectSkills(r.Kind, repo, r.Skills)
		if err != nil {
			return nil, err
		}
	}
	now := m.Now()
	j := &store.Job{
		ID: newID(now), Agent: m.Agent, Title: strings.TrimSpace(r.Title), Kind: r.Kind, Brief: r.Brief,
		RepoPath: repo, BaseCommit: base, Status: store.StatusQueued, Model: model,
		MaxTurns: turns, MaxBudgetUSD: budget, TimeoutMinutes: timeout, CreatedAt: now,
		Complexity: r.Complexity, Runner: runner, Billing: store.BillingSubscription, ReasoningEffort: effort,
		ScheduleID: r.ScheduleID, AcceptanceCmd: acceptance, OwnerWaits: r.OwnerWaits, Skills: selected,
		BaseBranch: branch, Covers: covers, CoverContract: cover, Gate: r.Gate, Size: size,
	}
	return j, nil
}

// Tick is one daemon pass: collect finished/timed-out jobs, retry scope stops and report redactions
// that failed, fire the schedules due this minute, judge the checks (dead-man's switch), start queued
// ones (at most max_running running, default 2), send job-done lines, and clean up worktrees whose
// notify is older than the cleanup delay; then, with the lifecycle lock released, deliver the checks' pending missed alerts.
func (m *Manager) Tick() error {
	m.RefreshDaemonConfig()
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
	if !m.daemonLockHeld {
		unlockLauncher, err := m.lockLauncher()
		if err != nil {
			return err
		}
		defer unlockLauncher()
	}
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
	limit := m.Config().MaxRunningJobs()
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

// claudeSettingSources is the --setting-sources value of a job: project (the work dir's settings, as a
// repository has them; the job's own settings.json outranks them), none for a gate job (Wave F), which
// gets only the daemon-written settings.json: no settings, hooks, skills or CLAUDE.md of the code it
// checks reach it (the copy at the sha holds none either, checkGateWork).
func claudeSettingSources(kind string) string {
	if IsGateKind(kind) {
		return ""
	}
	return "project"
}

func claudeBaseArgs(j *store.Job, jobDir string) []string {
	args := []string{
		"-p", "--output-format", "stream-json", "--verbose", "--model", j.Model,
		"--max-turns", fmt.Sprint(j.MaxTurns), "--max-budget-usd", fmt.Sprintf("%g", j.MaxBudgetUSD),
		"--permission-mode", "bypassPermissions",
		"--settings", filepath.Join(jobDir, "settings.json"), "--setting-sources", claudeSettingSources(j.Kind),
		"--mcp-config", filepath.Join(jobDir, "mcp.json"), "--strict-mcp-config",
		"--add-dir", jobDir,
	}
	// The flag outranks an effortLevel in the worktree's project settings; a row from before the effort
	// was recorded has none and runs at the CLI default.
	if j.ReasoningEffort != "" {
		args = append(args, "--effort", j.ReasoningEffort)
	}
	return args
}

// launchPlan is what one launch runs: the program (empty = claude), its arguments and the wrapper's
// options.
type launchPlan struct {
	bin     string
	args    []string
	wrapper []string // run-wrapper options before "--" (--prompt, --fallback-prompt, --fallback-session)
	// codexFullAccess: a codex job without the codex sandbox (codex_sandbox worktree-full-access); its
	// environment loses the credentials of codexFullAccessDroppedEnv.
	codexFullAccess bool
}

func (m *Manager) launch(j *store.Job, token string) error {
	// Containment first: a requeued job (errScopeRetry) has nothing prepared yet (no session, no
	// archived attempt), so Unclaim can put it back as it was.
	unit, err := m.scopeFor(j.ID)
	if err != nil {
		return err
	}
	var plan launchPlan
	var codexSandbox string // a codex job's `codex exec --sandbox` value
	launchCfg := m.Config()
	if j.Runner == store.RunnerCodex || j.AcceptanceCmd != "" || j.Kind == KindJudge {
		launchCfg, err = m.authorizedLaunchConfig()
		if err != nil {
			return err
		}
	}
	if j.AcceptanceCmd != "" && !launchCfg.AcceptanceAllowed {
		return errors.New("acceptance_allowed is off in active daemon config or config.json; acceptance job not launched")
	}
	if j.Kind == KindJudge && !launchCfg.AcceptanceAllowed {
		// The judge's manifest-check step (F5) runs repository code the way acceptance_cmd does.
		return errors.New("acceptance_allowed is off in active daemon config or config.json: the judge's manifest-check " +
			"(scripts/manifest-check.sh) cannot run, so the judge is not launched")
	}
	if j.Runner == store.RunnerCodex {
		if j.Attempt > 1 {
			return errCodexResume
		}
		if err := launchCfg.checkCodexJob(j.Kind); err != nil {
			return err
		}
		if codexSandbox, err = m.codexLaunchSandbox(launchCfg, j.Kind); err != nil {
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
			if err := checkCodexMCP(launchCfg.codexBin(), filepath.Join(m.JobDir(j.ID), "work"), m.jobSpawnEnv()); err != nil {
				return err
			}
			plan = launchPlan{bin: launchCfg.codexBin(), args: CodexArgs(j, launchCfg.reasoningEffort(j.ReasoningEffort), codexSandbox),
				wrapper: []string{"--runner", store.RunnerCodex}, codexFullAccess: codexSandbox == CodexSandboxFullAccess}
		}
	}
	if err := m.Store.SetLaunchAuthorization(j.ID, token, j.Attempt, store.LaunchAuthorization{
		CodexWriteAllowed: j.Runner == store.RunnerCodex && launchCfg.CodexWriteAllowed,
		AcceptanceAllowed: (j.AcceptanceCmd != "" || j.Kind == KindJudge) && launchCfg.AcceptanceAllowed,
	}); err != nil {
		return fmt.Errorf("record launch authorization: %w", err)
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
	if codexSandbox != "" {
		containment += ", codex sandbox " + codexSandbox
	}
	m.Log.Printf("job %s started pid=%d runner=%s model=%s containment=%s", j.ID, pid, j.Runner, j.Model, containment)
	return nil
}

// buildWrapperArgv is the run-wrapper command line of j's launch, before "--" and the runner's argv.
func (m *Manager) buildWrapperArgv(j *store.Job, token string, plan launchPlan) []string {
	argv := append([]string{m.SelfBin, "run-wrapper", m.JobDir(j.ID), "--token", token}, plan.wrapper...)
	if IsGateKind(j.Kind) {
		// The wrapper stores a slicing result under the checklist lock: told where the locks are, never
		// deriving it from the job dir's layout.
		argv = append(argv, "--checklist-lock-dir", m.ChecklistLockDir())
	}
	if j.Kind == KindJudge {
		// The judge's manifest-check step (F5) is decided here, at launch, from the job the daemon claimed.
		argv = append(argv, "--manifest-check")
	}
	return argv
}

// spawn starts the detached wrapper; the wrapper registers itself in wrapper.json before claude runs.
// With a scope unit the wrapper is started through `systemd-run --user --scope`, which execs it inside
// the new scope (same pid).
func (m *Manager) spawn(j *store.Job, token, unit string, plan launchPlan) (int, error) {
	dir := m.JobDir(j.ID)
	argv := m.buildWrapperArgv(j, token, plan)
	bin := plan.bin
	if bin == "" {
		bin = m.Config().claudeBin()
	}
	argv = append(append(argv, "--", bin), plan.args...)
	if unit != "" {
		argv = append(scopeArgv(unit, fmt.Sprintf("agent-jobs job %s (agent %s)", j.ID, m.Agent), m.KillGrace), argv...)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = filepath.Join(dir, "work")
	cmd.Env = m.spawnEnv(unit)
	if plan.codexFullAccess {
		cmd.Env = codexFullAccessEnv(cmd.Env, dir)
	}
	if IsGateKind(j.Kind) {
		// claude loads CLAUDE.md from the --add-dir directories (the job dir) with this set, whatever
		// --setting-sources says: a gate job gets no instructions but its prompt.
		cmd.Env = slices.DeleteFunc(cmd.Env, func(kv string) bool {
			return strings.HasPrefix(kv, "CLAUDE_CODE_ADDITIONAL_DIRECTORIES_CLAUDE_MD=")
		})
	}
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

// groupExists is groupAlive for the kill paths: the kernel's word that process group pgid has a
// process (kill(-pgid, 0) not ESRCH); a variable so tests with a fake /proc can stand in for it.
var groupExists = groupAlive

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
// be a stranger's group, so nothing is signalled on it before ownership is proven again
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

// acceptanceGroupAlive holds lifecycle operations even if every survivor dropped the marker.
// Once a terminal attempt's group is gone, forget it before its id can be reused.
func (m *Manager) acceptanceGroupAlive(j *store.Job) bool {
	if j.AcceptancePGID == 0 || plausibleGroup(j.AcceptancePGID) != nil {
		return false
	}
	if groupExists(j.AcceptancePGID) {
		return true
	}
	if finished(j.Status) {
		if err := m.Store.ClearAcceptancePGID(j.ID, j.LaunchToken, j.AcceptancePGID); err != nil {
			m.Log.Printf("job %s: forget acceptance process group: %v", j.ID, err)
		} else {
			j.AcceptancePGID = 0
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
// (a group of its own, which neither a session-only job's group kill nor a dead wrapper ends). It
// returns a note for the row when a dead wrapper's group was not signalled or members survived.
// Survivors carry ReasonSurvivors: the job is not auto-continued, and while the recorded group lives
// its cleanup, prune and resume stay held up. Otherwise it returns "".
func (m *Manager) killJob(j *store.Job, pid, pgid int) (note string) {
	defer func() { note = joinGroupNotes(note, m.killAcceptance(j.ID)) }()
	if j.ScopeUnit != "" {
		err := m.stopScope(j.ID)
		if err == nil {
			return ""
		}
		m.Log.Printf("job %s: stop scope: %v; falling back to the process group", j.ID, err)
	}
	if pgid == 0 {
		return ""
	}
	if j.PGIDSpawned && pgid == j.PGID && !alive(pid) {
		// The wrapper is gone: prove current ownership before ending the recorded group.
		note := m.endSpawnedGroup(j)
		if note != "" {
			m.Log.Printf("job %s: %s", j.ID, note)
		}
		if strings.Contains(note, "not signalled:") || strings.Contains(note, ReasonSurvivors) {
			return note
		}
		return ""
	}
	if res, why := m.killGroup(j, pid, pgid); res == groupSurvived {
		if why != "" {
			why = " (" + why + ")"
		}
		return fmt.Sprintf("process group %d survived SIGKILL%s; %s", pgid, why, ReasonSurvivors)
	}
	return ""
}

// killAcceptance ends the acceptance command's process group recorded in the job row (read now: it is
// recorded after the caller's row was read). The row is written by the wrapper, not the job. While the
// group's leader is alive and leads it, the group is ended only if the leader carries this job's
// marker (killLedAcceptance). Once the leader is gone (the shell died on SIGTERM while a child that
// ignores it keeps running, and the wrapper was killed before its own grace ended the group), the
// surviving members are checked by killLeaderlessGroup. Linux pins each process with a pidfd; Darwin
// signals the group only while a live member carries the job marker (endMarkedGroupBySysctl).
func (m *Manager) killAcceptance(id string) string {
	j, err := m.Store.Get(id)
	if err != nil || j.AcceptancePGID == 0 {
		return ""
	}
	g := j.AcceptancePGID
	if err := plausibleGroup(g); err != nil {
		m.Log.Printf("job %s: refusing to kill acceptance process group %d: %v", id, g, err)
		return acceptanceGroupNote(g, groupRefused, err.Error())
	}
	res, why := m.killAcceptanceGroup(id, g)
	// Linux's leaderless path tracks marked members; their disappearance can mean marker loss.
	// Preserve its signal decisions, but never report a live group as ended.
	if (res == groupGone || res == groupEnded) && groupExists(g) {
		res, why = groupSurvived, "group still exists after termination"
	}
	return acceptanceGroupNote(g, res, why)
}

// acceptanceGroupNote uses the same refusal and survival wording as the main process group.
func acceptanceGroupNote(g int, res groupEnd, why string) string {
	switch res {
	case groupRefused:
		return fmt.Sprintf("acceptance process group %d not signalled: %s", g, why)
	case groupSurvived:
		if why != "" {
			why = " (" + why + ")"
		}
		return fmt.Sprintf("acceptance process group %d survived SIGKILL%s; %s", g, why, ReasonSurvivors)
	}
	return ""
}

func joinGroupNotes(main, acceptance string) string {
	if main == "" {
		return acceptance
	}
	if acceptance == "" {
		return main
	}
	return main + "; " + acceptance
}

// killLedAcceptance ends acceptance process group g of job id while its leader looks alive: the leader
// is pinned by a pidfd, its marker and group are checked after that, and the group is ended through
// pidfds (endLedGroup). A leader that died in between leaves a leaderless group (killLeaderlessGroup).
func (m *Manager) killLedAcceptance(id string, g int) (groupEnd, string) {
	fd, err := pidfdOpen(g)
	if errors.Is(err, unix.ESRCH) {
		return m.killLeaderlessGroup(id, g)
	}
	if err != nil {
		m.Log.Printf("job %s: refusing to kill acceptance process group %d: pidfd_open %d: %v", id, g, g, err)
		return groupRefused, fmt.Sprintf("pidfd_open %d: %v", g, err)
	}
	defer unix.Close(fd)
	if p, err := procStatFull(g); err != nil || p.state == "Z" || p.pgid != g || pidfdSend(fd, 0) != nil {
		return m.killLeaderlessGroup(id, g)
	}
	if procMarker(g) != id {
		m.Log.Printf("job %s: refusing to kill acceptance process group %d: its leader carries no marker of the job", id, g)
		return groupRefused, "its leader carries no marker of the job"
	}
	return m.endLedGroup(id, fmt.Sprintf("acceptance process group %d", g), g, fd, acceptanceMember(id, g))
}

// killLeaderlessGroup ends acceptance process group g whose leader is gone, after proving current
// ownership with the job marker (endMarkedGroup).
func (m *Manager) killLeaderlessGroup(id string, g int) (groupEnd, string) {
	return m.endMarkedGroup(id, fmt.Sprintf("acceptance process group %d", g), g, acceptanceMember(id, g))
}

// groupEnd is what endMarkedGroup did.
type groupEnd int

const (
	groupGone     groupEnd = iota // no live member was left
	groupRefused                  // ownership is not proven, or the group could not be read: nothing signalled
	groupEnded                    // every member was ended
	groupSurvived                 // members survived SIGKILL (or could not be signalled)
)

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

// killGroup ends the wrapper's process group: SIGTERM, up to KillGrace, then SIGKILL. It refuses (and
// logs) unless pgid is plausible, is the live wrapper pid's group and that pid is shown to be this
// job's wrapper (jobWrapper), since a live pid alone proves nothing: the wrapper may have died and its
// pid and group id been reused by an unrelated group. The path is chosen by OS, never by whether /proc
// happens to be readable. On Linux the leader is pinned by a pidfd while it is checked, an unreadable
// /proc refuses, and the group is ended member by member through pidfds (endLedGroup), never by a
// numeric kill(-pgid). Elsewhere (macOS) the identity is read through sysctl (killGroupBySysctl).
func (m *Manager) killGroup(j *store.Job, pid, pgid int) (res groupEnd, why string) {
	refuse := func(why string) (groupEnd, string) {
		m.Log.Printf("job %s: refusing to kill process group %d: %s", j.ID, pgid, why)
		return groupRefused, why
	}
	if err := verifyGroup(pid, pgid); err != nil {
		return refuse(err.Error())
	}
	if runtime.GOOS != "linux" {
		return m.killGroupBySysctl(j, pid, pgid)
	}
	if !procAvailable() {
		return refuse(fmt.Sprintf("/proc (%s) is unreadable, the wrapper's identity cannot be checked", procRoot))
	}
	fd, err := pidfdOpen(pid)
	if err != nil {
		return refuse(fmt.Sprintf("pidfd_open %d: %v", pid, err))
	}
	defer unix.Close(fd)
	// Checked only now: the descriptor pins one process, so these reads are of that process (or, if it
	// died meanwhile, of a stranger, and then the liveness check below fails).
	if err := verifyGroup(pid, pgid); err != nil {
		return refuse(err.Error())
	}
	if !jobWrapper(j, m.JobDir(j.ID))(pid) {
		return refuse(fmt.Sprintf("pid %d is not proven to be the job's wrapper (a recycled pid?)", pid))
	}
	if err := pidfdSend(fd, 0); err != nil {
		return refuse(fmt.Sprintf("wrapper pid %d: %v", pid, err))
	}
	return m.endLedGroup(j.ID, fmt.Sprintf("process group %d", pgid), pgid, fd, jobGroupMember(j, pgid))
}

// heldProc is a process shown to be the job's while a pidfd held it; later signals go through fd.
// sent is the last signal it got (0: none yet).
type heldProc struct {
	pid, fd int
	sent    syscall.Signal
}

// endLedGroup ends process group g of job id (named what in the log) whose leader is held by the pidfd
// leaderFD and was shown to be the job's: SIGTERM, KillGrace, then SIGKILL, every signal through a
// pidfd and none by number. Only the process with pid g can create a group g, so while the held leader
// still exists (its pid is not free, even as a zombie) the id cannot be a stranger's group. A member is
// therefore the job's when its pidfd is open, /proc then shows it in g, and the leader still exists
// after that read; once the leader is gone a new member must pass isMember (the job's marker, its
// scope) instead, and one that does not is never signalled. A member proven once stays held by its
// descriptor and gets each later signal through it, wherever it moved. Unproven members count as
// survivors. why says what kept the group from being ended.
func (m *Manager) endLedGroup(id, what string, g, leaderFD int, isMember func(int) bool) (res groupEnd, why string) {
	var held []heldProc
	isHeld := map[int]bool{}
	defer func() {
		for _, p := range held {
			if p.fd >= 0 {
				_ = unix.Close(p.fd)
			}
		}
	}()
	leaderAlive := func() bool { return pidfdSend(leaderFD, 0) == nil }
	// heldAlive reads the stat first and confirms the descriptor after it, so the stat was of the held
	// process (a reaped pid may already belong to another).
	heldAlive := func(p heldProc) bool {
		st, err := procStatFull(p.pid)
		return pidfdSend(p.fd, 0) == nil && err == nil && st.state != "Z"
	}
	// round first holds every new member of g shown to be the job's (all of them before any signal, so
	// the leader is not yet signalled while they are proven), then sends sig to every live held process
	// that has not had it yet (a member found during the grace still gets SIGTERM, one found after the
	// SIGKILL round still gets SIGKILL). It returns how many processes of the group are left (unproven
	// ones included).
	scan := func(sig syscall.Signal) (left int, retired bool) {
		unproven := 0
		members, err := groupMembers(g)
		if err != nil {
			why = "members not checked: " + err.Error()
			m.Log.Printf("job %s: %s: %s", id, what, why)
			unproven++ // not enumerable: never taken for an ended group
		}
		for _, pid := range members {
			if isHeld[pid] {
				continue
			}
			fd, err := pidfdOpen(pid)
			if errors.Is(err, unix.ESRCH) {
				continue
			}
			if err != nil {
				why = fmt.Sprintf("pidfd_open %d: %v", pid, err)
				m.Log.Printf("job %s: %s: %s", id, what, why)
				unproven++
				continue
			}
			// Read only now that the descriptor pins the process; the leader is checked after the read.
			st, err := procStatFull(pid)
			if err != nil || st.state == "Z" || st.pgid != g {
				_ = unix.Close(fd)
				continue
			}
			if !leaderAlive() && !isMember(pid) {
				_ = unix.Close(fd)
				why = fmt.Sprintf("member %d is not proven to be the job's (leader gone, no marker of the job, or outside its scope)", pid)
				m.Log.Printf("job %s: not signalling a member of %s: %s", id, what, why)
				unproven++
				continue
			}
			held = append(held, heldProc{pid: pid, fd: fd})
			isHeld[pid] = true
		}
		left = unproven
		for i := range held {
			p := &held[i]
			if p.fd < 0 {
				continue
			}
			if !heldAlive(*p) {
				// Retire it and forget the pid: a new member that reuses it must be proven afresh.
				_ = unix.Close(p.fd)
				p.fd = -1
				delete(isHeld, p.pid)
				retired = true
				continue
			}
			left++
			if p.sent == sig {
				continue
			}
			p.sent = sig
			if err := pidfdSend(p.fd, sig); err != nil && !errors.Is(err, unix.ESRCH) {
				why = fmt.Sprintf("pidfd_send_signal %d: %v", p.pid, err)
				m.Log.Printf("job %s: %s: %s", id, what, why)
			}
		}
		return left, retired
	}
	// round is scan until a scan finds the group empty with no process retired in it and the kernel
	// then has no process in g (groupExists). A listing alone proves nothing: a member can fork a child
	// after it was listed (or after the listing passed the child's pid) and exit before it is checked,
	// two generations deep as well, so the child appears in no listing of this scan. Only kill(-g, 0)
	// failing with ESRCH shows no process is left in g; while one is, g is listed again. A group that
	// is still there after maxRescans (a missed member that keeps forking, or members not yet reaped)
	// is not taken for an ended one.
	const maxRescans = 10
	round := func(sig syscall.Signal) int {
		for range maxRescans {
			left, retired := scan(sig)
			if left > 0 {
				return left
			}
			if !retired && !groupExists(g) {
				return 0
			}
		}
		why = fmt.Sprintf("process group %d still exists though no member of it was listed (not reaped yet, or a member forking faster than the listing)", g)
		return 1
	}
	round(syscall.SIGTERM)
	deadline := time.Now().Add(m.KillGrace)
	for time.Now().Before(deadline) {
		if round(syscall.SIGTERM) == 0 {
			return groupEnded, ""
		}
		time.Sleep(100 * time.Millisecond)
	}
	round(syscall.SIGKILL)
	for i := 0; i < 20; i++ {
		if round(syscall.SIGKILL) == 0 {
			return groupEnded, ""
		}
		time.Sleep(50 * time.Millisecond)
	}
	m.Log.Printf("job %s: %s: members survived SIGKILL", id, what)
	return groupSurvived, why
}

// killGroupBySysctl is killGroup where there are no pidfds and no /proc (macOS). The wrapper's
// identity (argv `run-wrapper <jobDir> --token <the job's token>`, not a zombie, leading pgid) is read
// through sysctl (kern.procargs2) right before each group signal, and the group is signalled by number
// only while that holds: a group id is not reused while its leader lives. Without a descriptor to pin
// a process, nothing is ever signalled by pid: one checked and then reused before the kill would be
// hit. Once the wrapper is gone (it exits on SIGTERM) the group is still signalled as a whole, but only
// right after a live member of it is shown, through sysctl, to carry the job's marker (owned): a group
// id is not reused while the group has a process, and a process can join only a group of its own
// session, so a group holding a marked member is the job's. A group without one gets no signal; the
// ones still there at the deadline are reported as survivors. Residual: the marked member and every
// other one exiting, and the id going to a new group, between that read and the kill (a few syscalls).
func (m *Manager) killGroupBySysctl(j *store.Job, pid, pgid int) (res groupEnd, why string) {
	proven := func() error {
		if err := verifyGroup(pid, pgid); err != nil {
			return err
		}
		if j.ScopeUnit != "" {
			return fmt.Errorf("scope %s cannot be checked here", j.ScopeUnit)
		}
		info, err := sysctlProc(pid)
		if err != nil {
			return fmt.Errorf("wrapper pid %d: %w", pid, err)
		}
		if info.zombie || info.pgid != pgid || !wrapperArgv(j, m.JobDir(j.ID), info.argv) {
			return fmt.Errorf("pid %d is not proven to be the job's wrapper (a recycled pid?)", pid)
		}
		return nil
	}
	owned := func() error {
		werr := proven()
		if werr == nil {
			return nil
		}
		pids, err := sysctlGroup(pgid)
		if err != nil {
			return fmt.Errorf("wrapper not proven (%v), members not listed: %w", werr, err)
		}
		for _, p := range pids {
			info, err := sysctlProc(p)
			if err == nil && !info.zombie && info.pgid == pgid && envMarker(info.env) == j.ID {
				return nil
			}
		}
		return fmt.Errorf("wrapper not proven (%v) and no live member carries the job's marker", werr)
	}
	if err := proven(); err != nil {
		m.Log.Printf("job %s: refusing to kill process group %d: %v", j.ID, pgid, err)
		return groupRefused, err.Error()
	}
	_ = killFn(-pgid, syscall.SIGTERM)
	deadline := time.Now().Add(m.KillGrace)
	for time.Now().Before(deadline) {
		if !groupAlive(pgid) {
			return groupEnded, ""
		}
		time.Sleep(100 * time.Millisecond)
	}
	// SIGKILL goes to the group, again each round while it is still shown to be the job's (so a member
	// that joined since gets it too), and never once it is not.
	for i := 0; i < 20; i++ {
		if !groupAlive(pgid) {
			return groupEnded, ""
		}
		if err := owned(); err == nil {
			why = ""
			_ = killFn(-pgid, syscall.SIGKILL)
		} else {
			why = fmt.Sprintf("group not signalled: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !groupAlive(pgid) {
		return groupEnded, ""
	}
	left, err := sysctlGroup(pgid)
	if err != nil {
		m.Log.Printf("job %s: process group %d: members survived (not listed: %v); %s", j.ID, pgid, err, why)
	} else {
		m.Log.Printf("job %s: process group %d: members survived: %v; %s", j.ID, pgid, left, why)
	}
	if why == "" {
		why = "members survived SIGKILL"
	}
	return groupSurvived, why
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
		if err != nil || !wrapperArgv(j, jobDir, argv) {
			return false
		}
		if j.ScopeUnit == "" {
			return true
		}
		b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "cgroup"))
		return err == nil && jobScopeIn(string(b)) == j.ScopeUnit
	}
}

// wrapperArgv reports whether argv is that of job j's wrapper: `run-wrapper <jobDir> --token <the
// job's launch token>`.
func wrapperArgv(j *store.Job, jobDir string, argv []string) bool {
	if j.LaunchToken == "" || !IsWrapperCmdline(argv) {
		return false
	}
	i := slices.Index(argv, "run-wrapper")
	return i >= 0 && len(argv) >= i+4 && argv[i+1] == jobDir && argv[i+3] == j.LaunchToken
}

// endOrphanedGroup ends what is left of a job whose wrapper died without trusted completion: its
// process group as recorded at the spawn (liveSpawnedGroup; a group adopted from wrapper.json is never
// signalled here) and its acceptance command. It returns a note for the row, "" when no group was left.
func (m *Manager) endOrphanedGroup(j *store.Job) (note string) {
	defer func() { note = joinGroupNotes(note, m.killAcceptance(j.ID)) }()
	if !liveSpawnedGroup(j) {
		return ""
	}
	m.Log.Printf("job %s: wrapper died, its process group %d is still alive; ending it", j.ID, j.PGID)
	return m.endSpawnedGroup(j)
}

// endSpawnedGroup ends the group recorded at the job's spawn once its wrapper is gone: on Linux, each
// member must be proven and pinned; on Darwin, a live marked member must prove ownership before each
// group signal (endMarkedGroup). The id alone proves nothing once the group may have emptied and
// the id been reused. It fails closed (nothing signalled)
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
		// Only the token-bound completion can end an attempt; files are diagnostic.
		if code, ok := m.completedCode(j); ok {
			m.collectExited(j, code, dur, now)
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
			survivors := m.killJob(j, j.PID, j.PGID)
			res, killed := m.killedResult(j)
			if !killed {
				// The wrapper recorded completion since the check above: the attempt ended by itself, so its
				// own verdict is recorded (and auto-continued only as such), not a timeout.
				if code, ok := m.completedCode(j); ok {
					m.collectExited(j, code, dur, now)
					continue
				}
			}
			if j.Runner != store.RunnerCodex {
				m.writeStopReport(j, dir, stopTimeout, res.Turns)
			}
			msg := fmt.Sprintf("timeout after %d minutes", j.TimeoutMinutes)
			if survivors != "" {
				msg += "; " + survivors
			}
			if st, got, ok := m.finish(j, store.StatusTimeout, res, killed, dur, msg, now); ok && st == store.StatusTimeout && got == msg {
				m.autoContinue(j.ID, stopTimeout)
			}
			continue
		}
		if !alive(j.PID) {
			// Re-check: the wrapper may have recorded completion right before exiting.
			if code, ok := m.completedCode(j); ok {
				m.collectExited(j, code, dur, now)
				continue
			}
			// Nothing of the job may outlive the row: claude would spend on, write the worktree that
			// cleanup removes, and race a resume in the same session.
			msg := "wrapper died without trusted completion (see stderr.log)"
			if note := m.endOrphanedGroup(j); note != "" {
				msg += "; " + note
			}
			res, killed := m.killedResult(j)
			m.finish(j, store.StatusFailed, res, killed, dur, msg, now)
		}
	}
	return nil
}

// collectExited finishes an attempt whose wrapper recorded trusted completion. An attempt stopped at its turn
// limit gets a stop report when it wrote none and is then continued once (autoContinue).
func (m *Manager) collectExited(j *store.Job, code, dur int, now time.Time) {
	dir := m.JobDir(j.ID)
	res := m.endedResult(j)
	if !res.Completed {
		return
	}
	code = res.ExitCode
	status, msg := m.verdict(j, code, res, dir)
	// Written after the verdict: the stop report must never make the attempt count as reported.
	turnLimit := turnLimitStop(j, res) && status == store.StatusFailed
	if turnLimit {
		m.writeStopReport(j, dir, stopTurnLimit, res.Turns)
	}
	// Continued only when finish recorded the verdict as it was (not a failed scope stop or skill-link check).
	if st, got, ok := m.finish(j, status, res, false, dur, msg, now); turnLimit && ok && st == status && got == msg {
		m.autoContinue(j.ID, stopTurnLimit)
	}
}

// killedResult reads the latest trusted progress snapshot. A result already observed by the wrapper
// keeps its usage, while any attempt without completion still has unknown final cost.
func (m *Manager) killedResult(j *store.Job) (res StreamResult, killed bool) {
	res = m.endedResult(j)
	return res, !res.Completed
}

// completedCode distinguishes progress snapshots from a fully completed wrapper attempt.
func (m *Manager) completedCode(j *store.Job) (int, bool) {
	res := m.endedResult(j)
	return res.ExitCode, res.Completed
}

// endedResult reads only the wrapper's DB record for this launch, for either runner. A missing
// record (including attempts run by an older wrapper) is no trusted result; stream.jsonl is diagnostic.
func (m *Manager) endedResult(j *store.Job) StreamResult {
	raw, ok, err := m.Store.WrapperResult(j.ID, j.LaunchToken)
	if err != nil {
		m.Log.Printf("job %s: read the wrapper's result: %v", j.ID, err)
	}
	if !ok {
		return StreamResult{Runner: j.Runner}
	}
	var res StreamResult
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		m.Log.Printf("job %s: the wrapper's result is unreadable: %v", j.ID, err)
		return StreamResult{Runner: j.Runner}
	}
	res.Runner = j.Runner // runner identity belongs to the daemon's job row
	return res
}

// finish ends a running attempt; killed marks one ended by a kill (see killedResult). A timed-out or
// failed attempt can be resumed, so what it left outside its process group is swept first. It returns
// the status and message recorded (a failed scope stop or skill-link check changes them; ok is false
// when the row was not finished).
func (m *Manager) finish(j *store.Job, status string, res StreamResult, killed bool, dur int, msg string, now time.Time) (string, string, bool) {
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
		return status, msg, false
	}
	if status == store.StatusTimeout || status == store.StatusFailed {
		m.noteSweep(j.ID, m.sweepEscaped(j.ID))
	}
	m.noteReviewChanges(j)
	m.recordAttemptEnd(j, res, killed)
	m.Log.Printf("job %s %s (%s)", j.ID, status, RedactText(msg)) // msg can carry the job's own text
	m.settled(j.ID)
	return status, msg, true
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
	// Codex reports no price: a codex attempt's cost is always unknown.
	if killed || j.Runner == store.RunnerCodex || (!res.Found && j.LaunchToken != "") {
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
// It returns killJob's note on survivors ("" when none).
func (m *Manager) killRunning(j *store.Job) string {
	pid, pgid := j.PID, j.PGID
	if pgid == 0 { // claimed, pid not recorded: the wrapper may still have registered
		if w, err := ReadWrapperInfo(m.JobDir(j.ID)); err == nil && w.Token == j.LaunchToken {
			pid, pgid = w.PID, w.PGID
		}
	}
	return m.killJob(j, pid, pgid)
}

// Verdict maps the wrapper's exit code and stream result to a terminal status.
// exit 0 without a report is a failure; so is a codex exit 0 whose stream has no terminal turn.completed.
// A trusted incomplete sweep fails the job whatever the runner's exit was.
func Verdict(code int, res StreamResult, dir string) (string, string) {
	if res.SweepFailure != "" {
		return store.StatusFailed, res.SweepFailure
	}
	// A natural failed exit retains its diagnostic reason; idle closure uses its trusted provenance.
	if code != 0 && res.IdleReason == "" {
		return store.StatusFailed, fmt.Sprintf("%s exited with code %d", res.program(), code)
	}
	// Runner-specific terminal validation always precedes idle-close handling.
	if res.Runner == store.RunnerCodex {
		if reason := codexStreamFailure(res); reason != "" {
			return store.StatusFailed, reason
		}
	} else if !res.Found {
		return store.StatusFailed, "claude ended without a trusted result"
	}
	reported := res.ReportPresent
	if !res.Completed {
		reported = hasReport(dir)
	} // standalone verdict callers
	if res.IdleReason != "" {
		if code != 0 || res.IsError || res.Subtype != "success" || !reported {
			return store.StatusFailed, res.IdleReason
		}
		return store.StatusSucceeded, res.IdleReason
	}
	if res.IsError {
		return store.StatusFailed, res.program() + " result is an error: " + res.Subtype
	}
	if !reported {
		return store.StatusFailed, "exit 0 without a report"
	}
	if res.Runner != store.RunnerCodex && res.Subtype != "success" {
		return store.StatusFailed, res.program() + " result is an error: " + res.Subtype
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
	survivors := ""
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
		survivors = m.killRunning(j)
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
	res, killed := m.killedResult(j)
	msg := "cancelled"
	if survivors != "" {
		msg = RedactPrintable(msg+"; "+survivors, 300)
	}
	if err := m.endScope(j); err != nil {
		m.Log.Printf("job %s: %s", id, RedactText(err.Error()))
		msg = RedactPrintable(msg+"; "+ReasonSurvivors+" (scope not stopped, retried every tick: "+err.Error()+")", 300)
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
