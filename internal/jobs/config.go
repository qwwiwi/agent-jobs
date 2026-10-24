// Package jobs implements the job lifecycle: enqueue, launch, collect, timeout,
// notify and cleanup. One Manager serves one agent.
package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/brain"
	"github.com/qwwiwi/agent-jobs/internal/skills"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// Limits: defaults and hard ceilings. A request above a ceiling is rejected, never clamped.
const (
	DefaultMaxTurns       = 80
	DefaultMaxBudgetUSD   = 10.0
	DefaultTimeoutMinutes = 45
	CeilingMaxTurns       = 200
	CeilingMaxBudgetUSD   = 50.0
	CeilingTimeoutMinutes = 180
	MaxOwnerRulesBytes    = 8 * 1024
)

// Running jobs per agent (config max_running): the default and the hard ceiling. A value outside
// 1..CeilingMaxRunningPerAgent is a config error, never clamped.
const (
	DefaultMaxRunningPerAgent = 2
	CeilingMaxRunningPerAgent = 8
)

// Reference timings («раз в минуту», «через 30 минут после уведомления»): fixed, not configurable.
const (
	TickInterval = time.Minute
	CleanupDelay = 30 * time.Minute
)

// Kinds of work.
var Kinds = []string{"review", "fix", "research", "general", KindSlicing, KindJudge}

// Config is the per-agent configuration, read from <state>/config.json (all fields optional).
type Config struct {
	// OwnerRulesPath is the owner's permanent rules file (≤8 KB) glued into every brief.
	// Default: <state>/owner-rules.md. A job refuses to start if it is unreadable or too big.
	OwnerRulesPath string `json:"owner_rules_path"`
	// CardsDir holds the project cards (<name>.md) attached to briefs of matching repositories.
	// Default: <state>/cards.
	CardsDir string `json:"cards_dir"`
	// NotifyTmuxTarget is the tmux target (e.g. "channel-silvana") for the job-done line. Empty = notify disabled.
	NotifyTmuxTarget string `json:"notify_tmux_target"`
	// TmuxSocket is the tmux socket name (`tmux -L <name>`) of the server that holds NotifyTmuxTarget.
	// Empty = tmux's default socket. Needed for agents whose session runs on a named socket.
	TmuxSocket string `json:"tmux_socket"`
	// OwnerNames must never appear in a job's commit (guard).
	OwnerNames []string `json:"owner_names"`
	// GitName / GitEmail is the job's git identity: written to <jobdir>/gitconfig (GIT_CONFIG_GLOBAL of
	// the job). The guard rejects any other identity; via `git -c` none at all. Empty = no identity.
	GitName  string `json:"git_name"`
	GitEmail string `json:"git_email"`
	// ClaudeBin overrides the claude binary (env AGENT_JOBS_CLAUDE_BIN wins).
	ClaudeBin string `json:"claude_bin"`
	// CodexBin overrides the codex binary of codex jobs (env AGENT_JOBS_CODEX_BIN wins).
	CodexBin string `json:"codex_bin"`
	// CodexWriteAllowed lets codex jobs of kind fix/general run with the workspace-write sandbox. Off by
	// default: the guard hook does not run inside codex, so only read-only codex jobs (review, research)
	// are allowed unless the owner turns this on (DEVIATIONS #5).
	CodexWriteAllowed bool `json:"codex_write_allowed"`
	// CodexReasoningEffort is the default reasoning effort of codex jobs (low|medium|high); empty = high.
	CodexReasoningEffort string `json:"codex_reasoning_effort"`
	// CodexJobsPerDay caps the codex jobs of this agent created in the last 24 hours, whatever became of
	// them (job_start, agent-jobs start and schedule firings past it are refused). Nil (absent) =
	// DefaultCodexJobsPerDay (3); 0 turns the cap off. The count is per agent (each agent has its own
	// state dir); a host-wide cap would need a counter shared by all agents.
	CodexJobsPerDay *int `json:"codex_jobs_per_day"`
	// EffortByKind overrides the default reasoning effort of claude jobs per kind
	// (low|medium|high|xhigh|max), e.g. {"general": "low"}. Unset kinds keep DefaultEffortByKind.
	EffortByKind map[string]string `json:"effort_by_kind"`
	// Models overrides cells of the kind × complexity model table (DEVIATIONS #6), e.g.
	// {"review": {"hard": "gpt-6.1-sol"}}. A gpt-* model runs the job on codex. Unset cells keep the defaults.
	Models map[string]map[string]string `json:"models"`
	// TmuxBin overrides the tmux binary (env AGENT_JOBS_TMUX_BIN wins).
	TmuxBin string `json:"tmux_bin"`
	// Brain is the shared brain (team memory, tasks, messages) behind the memory, task and team tools
	// and the memory notes of a brief. Empty = not configured: those tools say so, briefs carry no notes.
	Brain brain.Config `json:"brain"`
	// CgroupScope is the containment of a job's processes (env AGENT_JOBS_CGROUP_SCOPE wins):
	// "auto" (default) = its own transient systemd user scope when `systemd-run --user` works, else the
	// weaker session containment; "required" = a job that cannot get a scope is refused; "off" = never.
	CgroupScope string `json:"cgroup_scope"`
	// TrashRoots are the absolute directories trash_move may take paths from (and trash_restore put
	// them back into). Empty = none: trash is closed until the owner names its roots. The agent-jobs
	// home, the owner rules, the binary, ~/.claude and any `.git` are refused in every root.
	TrashRoots []string `json:"trash_roots"`
	// LogArchiveDir is where prune moves the records of a cleaned job for later analysis
	// (<dir>/<YYYY-MM>/<id>/). Default: <state>/archive. Absolute; may be another disk or a synced folder.
	LogArchiveDir string `json:"log_archive_dir"`
	// LogArchiveDays is how long archived months are kept (prune deletes older ones). Nil = 365, 0 = forever.
	LogArchiveDays *int `json:"log_archive_days"`
	// IdleAfterResultSeconds is the grace of the idle-after-result rule: claude that printed a final
	// `result` and then stays silent this long, with no live child but the job MCP server, is ended
	// and judged by that result. 0 = DefaultIdleAfterResult (3 minutes); IdleAfterResultOff (-1) turns
	// the rule off (claude's own exit or the timeout ends the job).
	IdleAfterResultSeconds int `json:"idle_after_result_seconds"`
	// DailyHour is the local hour (0..23) after which the daemon runs the daily automation (prune and
	// doctor procs in report mode) once per local day. Nil = DefaultDailyHour (4).
	DailyHour *int `json:"daily_hour"`
	// JobDirNoteMB is the job folder size (MB, worktree of a repo job left out) from which the
	// job-done line says how big the folder is. Nil = DefaultJobDirNoteMB (100), 0 = never.
	JobDirNoteMB *int `json:"job_dir_note_mb"`
	// MaxRunning is how many jobs of this agent run at once; the rest wait in the queue.
	// nil (absent) = DefaultMaxRunningPerAgent (2); 1..CeilingMaxRunningPerAgent (8); explicit null is refused.
	MaxRunning *int `json:"max_running"`
	// DailyOff turns the daily automation off (prune and doctor procs then run only by hand).
	DailyOff bool `json:"daily_off"`
	// KeepRawReports keeps the unredacted report of a job as report.raw.md (0600) next to the redacted
	// report.md. Off by default: secret-looking values are replaced in place and the original is gone.
	KeepRawReports bool `json:"keep_raw_reports"`
	// MaxBriefBytes is the largest brief job_start accepts. 0 = DefaultMaxBriefBytes (32 KB).
	MaxBriefBytes int `json:"max_brief_bytes"`
	// BriefWarnFiles: a brief naming more files than this gets a size_warning. 0 = DefaultBriefWarnFiles (40).
	BriefWarnFiles int `json:"brief_warn_files"`
	// BriefWarnHeadings: a brief with more "## " headings than this gets a size_warning (it likely asks
	// for more than one deliverable). 0 = DefaultBriefWarnHeadings (8).
	BriefWarnHeadings int `json:"brief_warn_headings"`
	// OwnerWaitsMaxHours is how long an «owner reply pending» record of an owner-waits job may hold
	// the orchestrator's Stop hook before it expires on its own. 0 = DefaultOwnerWaitsMaxHours (6).
	OwnerWaitsMaxHours int `json:"owner_waits_max_hours"`
	// OrchestratorDir is the absolute directory the orchestrator session works in. `hook bind`
	// accepts a session only when its cwd lies inside it (and it runs in the pane of
	// NotifyTmuxTarget). Empty = binding refused: the owner-waits Stop hook then never blocks.
	OrchestratorDir string `json:"orchestrator_dir"`
	// HTTPAPI is the read-only HTTP API for a jobs dashboard (DEVIATIONS #8), served by the daemon.
	// Off by default; the listen address must be loopback or a unix socket path.
	HTTPAPI HTTPAPIConfig `json:"http_api"`
	// AcceptanceTimeoutMinutes is the time limit of a job's acceptance command (acceptance_cmd).
	// 0 = DefaultAcceptanceTimeoutMinutes (15); at most CeilingAcceptanceTimeoutMinutes (60).
	AcceptanceTimeoutMinutes int `json:"acceptance_timeout_minutes"`
	// AcceptanceAllowed opts in to acceptance_cmd (default false: job_start and schedule_add refuse it,
	// and the daemon and wrapper do not run it). Enabling requires a daemon restart; disabling on
	// disk revokes it immediately. The command runs the job's code outside the guard.
	AcceptanceAllowed bool `json:"acceptance_allowed"`
	// SkillRoots are absolute directories of skill catalogs (E7), each holding index.json; earlier roots
	// win a duplicate name. A missing or invalid root is a warning (status, system_doctor), never fatal.
	// Empty = no catalog: jobs see only their repository's own .claude/skills.
	SkillRoots []string `json:"skill_roots"`
	// SkillsMaxPerJob caps the skills selected for one job. 0 = skills.DefaultMaxPerJob (6), at most
	// skills.CeilingMaxPerJob (8).
	SkillsMaxPerJob int `json:"skills_max_per_job"`
	// SkillsMaxKB caps the total size of one job's skills in KB. 0 = skills.DefaultMaxKB (400).
	SkillsMaxKB float64 `json:"skills_max_kb"`
	// Heartbeat lets the daemon wake the orchestrator session for the agent's own standing goals:
	// a cheap Go gate, then a small model decides (DEVIATIONS #1). Off by default.
	Heartbeat HeartbeatConfig `json:"heartbeat"`
	// ModelPrices adds or overrides list prices (USD per million tokens) by model name, used to estimate
	// the cost of attempts that ended without a reported one (killed, timed out, codex). A model with no
	// price here or in the built-in table keeps an unknown cost.
	ModelPrices map[string]ModelPrice `json:"model_prices"`
	// ReportContract is what happens to a claude job that ends succeeded with a report missing the
	// "## Summary" or "## Not done / uncertain" section: "warn" (default; it stays succeeded with a
	// note in its error, on the job-done line and in the stats) or "fail" (it fails with that note).
	ReportContract string `json:"report_contract"`
	// AutoContinue continues a claude attempt stopped at its turn limit or its timeout once, in the same
	// session, with a fresh allowance (AutoContinueFraction). Nil (absent) = on; false turns it off.
	AutoContinue *bool `json:"auto_continue"`
	// AutoContinueFraction is the automatic continuation's allowance as a fraction of the job's original
	// limits (turns, budget, minutes), each capped by what the across-attempt ceilings leave.
	// 0 = DefaultAutoContinueFraction (0.5); at most 1.
	AutoContinueFraction float64 `json:"auto_continue_fraction"`
}

// Report contract modes (config report_contract).
const (
	ReportContractWarn = "warn"
	ReportContractFail = "fail"
)

// DefaultAutoContinueFraction is the automatic continuation's share of the original limits when
// auto_continue_fraction is not set.
const DefaultAutoContinueFraction = 0.5

// AutoContinueOn reports whether a stopped attempt is continued automatically (auto_continue, default on).
func (c Config) AutoContinueOn() bool { return c.AutoContinue == nil || *c.AutoContinue }

// AutoContinueShare returns the automatic continuation's fraction of the original limits.
func (c Config) AutoContinueShare() float64 {
	if c.AutoContinueFraction > 0 {
		return c.AutoContinueFraction
	}
	return DefaultAutoContinueFraction
}

// DefaultCodexJobsPerDay is the codex daily cap when codex_jobs_per_day is not set.
const DefaultCodexJobsPerDay = 3

// CodexDailyCap returns the codex daily cap (0 = off).
func (c Config) CodexDailyCap() int {
	if c.CodexJobsPerDay == nil {
		return DefaultCodexJobsPerDay
	}
	return *c.CodexJobsPerDay
}

// DefaultOwnerWaitsMaxHours is the max age of an «owner reply pending» record when
// owner_waits_max_hours is not set.
const DefaultOwnerWaitsMaxHours = 6

// OwnerWaitsMaxAge returns how long an «owner reply pending» record lives before it expires.
func (c Config) OwnerWaitsMaxAge() time.Duration {
	if c.OwnerWaitsMaxHours > 0 {
		return time.Duration(c.OwnerWaitsMaxHours) * time.Hour
	}
	return DefaultOwnerWaitsMaxHours * time.Hour
}

// HTTPAPIConfig is the http_api section of config.json.
type HTTPAPIConfig struct {
	// Enabled starts the API inside the daemon. Default false.
	Enabled bool `json:"enabled"`
	// Listen is a loopback "ip:port" (127.0.0.0/8 or [::1]) or an absolute unix socket path.
	// Empty = the unix socket <state>/http-api.sock (HTTPAPISocket).
	Listen string `json:"listen"`
}

// HTTPAPISocket is the default listen address of the HTTP API: the unix socket <state>/http-api.sock,
// inside the agent state dir (mode 700, owned by the agent user), so no other local user can bind
// or reach it first.
func HTTPAPISocket(stateDir string) string { return filepath.Join(stateDir, "http-api.sock") }

// HTTPAPIListen is the address the HTTP API listens on: http_api.listen, or HTTPAPISocket.
func (c Config) HTTPAPIListen(stateDir string) string {
	if c.HTTPAPI.Listen != "" {
		return c.HTTPAPI.Listen
	}
	return HTTPAPISocket(stateDir)
}

// ValidateHTTPListen accepts a loopback "ip:port" (an IP literal in 127.0.0.0/8 or ::1, port 0..65535)
// or an absolute unix socket path; anything else (a wildcard, a LAN address, a host name) is refused.
func ValidateHTTPListen(listen string) error {
	if strings.HasPrefix(listen, "/") {
		if filepath.Clean(listen) != listen || strings.ContainsRune(listen, 0) {
			return fmt.Errorf("http_api.listen %q: a unix socket path must be absolute and clean", listen)
		}
		return nil
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("http_api.listen %q: want a loopback ip:port or an absolute unix socket path: %v", listen, err)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
		return fmt.Errorf("http_api.listen %q: port %q is not 0..65535", listen, port)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("http_api.listen %q: the host must be a loopback IP (127.0.0.0/8 or ::1), not %q; "+
			"the API is local only", listen, host)
	}
	return nil
}

// MaxRunningJobs returns how many jobs of the agent may run at once (max_running, default 2).
func (c Config) MaxRunningJobs() int {
	if c.MaxRunning == nil {
		return DefaultMaxRunningPerAgent
	}
	return *c.MaxRunning
}

// DefaultJobDirNoteMB is job_dir_note_mb when it is not set.
const DefaultJobDirNoteMB = 100

// JobDirNoteBytes returns the job folder size from which the job-done line shows it (0 = never).
func (c Config) JobDirNoteBytes() int64 {
	mb := DefaultJobDirNoteMB
	if c.JobDirNoteMB != nil {
		mb = *c.JobDirNoteMB
	}
	return int64(mb) << 20
}

// DefaultEffortByKind is the reasoning effort of a claude job of each kind when neither the job nor
// effort_by_kind names one.
var DefaultEffortByKind = map[string]string{"review": "high", "research": "high", "fix": "high", "general": "medium",
	KindSlicing: "high", KindJudge: "high"}

// claudeEffort returns the effort of a claude job: the job's own, else the config's for its kind,
// else the kind's default.
func (c Config) claudeEffort(kind, job string) string {
	if job != "" {
		return job
	}
	if e := c.EffortByKind[kind]; e != "" {
		return e
	}
	return DefaultEffortByKind[kind]
}

func (c Config) validateEffortByKind() error {
	for kind, e := range c.EffortByKind {
		if !slices.Contains(Kinds, kind) {
			return fmt.Errorf("effort_by_kind: unknown kind %q (want %s)", kind, strings.Join(Kinds, "|"))
		}
		if err := ValidateReasoningEffort(store.RunnerClaude, e); err != nil {
			return fmt.Errorf("effort_by_kind.%s: %w", kind, err)
		}
	}
	return nil
}

// DefaultDailyHour is the local hour of the daily automation when daily_hour is not set.
const DefaultDailyHour = 4

// DailyAt returns the local hour after which the daily automation runs.
func (c Config) DailyAt() int {
	if c.DailyHour == nil {
		return DefaultDailyHour
	}
	return *c.DailyHour
}

// DefaultLogArchiveDays is the archive retention when log_archive_days is not set.
const DefaultLogArchiveDays = 365

// ArchiveDays returns the archive retention in days (0 = forever).
func (c Config) ArchiveDays() int {
	if c.LogArchiveDays == nil {
		return DefaultLogArchiveDays
	}
	return *c.LogArchiveDays
}

// IdleAfterResultOff is the idle_after_result_seconds value that turns the rule off.
const IdleAfterResultOff = -1

// IdleAfterResult returns the idle-after-result grace; 0 means the rule is off.
func (c Config) IdleAfterResult() time.Duration {
	if c.IdleAfterResultSeconds == IdleAfterResultOff {
		return 0
	}
	if c.IdleAfterResultSeconds > 0 {
		return time.Duration(c.IdleAfterResultSeconds) * time.Second
	}
	return DefaultIdleAfterResult
}

// removedConfigKeys were configurable once and now are fixed by the reference; a config that still
// sets one is rejected instead of being silently ignored.
var removedConfigKeys = map[string]string{
	"cleanup_delay_minutes": "cleanup runs 30 minutes after notify (reference), not configurable",
}

var reAgent = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// Home returns the root state directory (~/.agent-jobs, or $AGENT_JOBS_HOME).
func Home() (string, error) {
	if h := os.Getenv("AGENT_JOBS_HOME"); h != "" {
		return h, nil
	}
	u, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(u, ".agent-jobs"), nil
}

// StateDir returns ~/.agent-jobs/<agent>.
func StateDir(agent string) (string, error) {
	if !reAgent.MatchString(agent) {
		return "", fmt.Errorf("invalid agent name %q (want [a-z0-9_-], max 32)", agent)
	}
	h, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, agent), nil
}

// LoadConfig reads <state>/config.json; a missing file yields defaults.
func LoadConfig(stateDir string) (Config, error) {
	b, err := os.ReadFile(filepath.Join(stateDir, "config.json"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	return parseConfig(stateDir, b)
}

func parseConfig(stateDir string, b []byte) (Config, error) {
	var c Config
	if b != nil {
		if err := json.Unmarshal(b, &c); err != nil {
			return c, fmt.Errorf("parse config.json: %w", err)
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(b, &raw); err != nil {
			return c, fmt.Errorf("parse config.json: %w", err)
		}
		if raw == nil {
			return c, errors.New("config.json must be a JSON object, not null")
		}
		for k, why := range removedConfigKeys {
			if _, ok := raw[k]; ok {
				return c, fmt.Errorf("config.json: %q is no longer supported: %s", k, why)
			}
		}
		// null decodes to a nil pointer, which reads as absent; an explicit null is a mistake.
		if v, ok := raw["codex_jobs_per_day"]; ok && strings.TrimSpace(string(v)) == "null" {
			return c, fmt.Errorf("config.json: codex_jobs_per_day must be an integer (0 = off), not null (omit the key for default %d)",
				DefaultCodexJobsPerDay)
		}
		if v, ok := raw["max_running"]; ok && strings.TrimSpace(string(v)) == "null" {
			return c, fmt.Errorf("config.json: max_running must be an integer 1..%d, not null (omit the key for default %d)",
				CeilingMaxRunningPerAgent, DefaultMaxRunningPerAgent)
		}
		if v, ok := raw["auto_continue"]; ok && strings.TrimSpace(string(v)) == "null" {
			return c, errors.New("config.json: auto_continue must be true or false, not null (omit the key for default true)")
		}
	}
	if c.TmuxSocket != "" && !reTmuxSocket.MatchString(c.TmuxSocket) {
		return c, fmt.Errorf("config.json: tmux_socket %q must be a socket name (letters, digits, _ . -), not a path", c.TmuxSocket)
	}
	if err := c.Brain.Validate(); err != nil {
		return c, fmt.Errorf("config.json: %w", err)
	}
	if !slices.Contains(CgroupScopeModes, c.CgroupScope) {
		return c, fmt.Errorf("config.json: cgroup_scope %q must be one of auto|required|off", c.CgroupScope)
	}
	if c.JobDirNoteMB != nil && *c.JobDirNoteMB < 0 {
		return c, fmt.Errorf("config.json: job_dir_note_mb %d must be 0 (never) or positive", *c.JobDirNoteMB)
	}
	if c.IdleAfterResultSeconds < IdleAfterResultOff {
		return c, fmt.Errorf("config.json: idle_after_result_seconds %d must be -1 (off), 0 (default) or positive", c.IdleAfterResultSeconds)
	}
	for _, r := range c.TrashRoots {
		if !filepath.IsAbs(r) {
			return c, fmt.Errorf("config.json: trash_roots entry %q must be an absolute path", r)
		}
	}
	if c.SkillsMaxPerJob < 0 || c.SkillsMaxPerJob > skills.CeilingMaxPerJob {
		return c, fmt.Errorf("config.json: skills_max_per_job %d must be 0 (default %d) .. %d", c.SkillsMaxPerJob,
			skills.DefaultMaxPerJob, skills.CeilingMaxPerJob)
	}
	if c.SkillsMaxKB < 0 {
		return c, fmt.Errorf("config.json: skills_max_kb %g must be 0 (default %d) or positive", c.SkillsMaxKB, skills.DefaultMaxKB)
	}
	if c.LogArchiveDir != "" && !filepath.IsAbs(c.LogArchiveDir) {
		return c, fmt.Errorf("config.json: log_archive_dir %q must be an absolute path", c.LogArchiveDir)
	}
	if c.LogArchiveDays != nil && *c.LogArchiveDays < 0 {
		return c, fmt.Errorf("config.json: log_archive_days %d must be 0 (forever) or more", *c.LogArchiveDays)
	}
	if c.OwnerWaitsMaxHours < 0 {
		return c, fmt.Errorf("config.json: owner_waits_max_hours %d must be 0 (default %d) or positive", c.OwnerWaitsMaxHours, DefaultOwnerWaitsMaxHours)
	}
	if c.OrchestratorDir != "" && (!filepath.IsAbs(c.OrchestratorDir) || filepath.Clean(c.OrchestratorDir) == "/") {
		return c, fmt.Errorf("config.json: orchestrator_dir %q must be an absolute path other than /", c.OrchestratorDir)
	}
	if c.MaxRunning != nil && (*c.MaxRunning < 1 || *c.MaxRunning > CeilingMaxRunningPerAgent) {
		return c, fmt.Errorf("config.json: max_running %d must be 1..%d (absent = default %d)", *c.MaxRunning,
			CeilingMaxRunningPerAgent, DefaultMaxRunningPerAgent)
	}
	if c.CodexJobsPerDay != nil && *c.CodexJobsPerDay < 0 {
		return c, fmt.Errorf("config.json: codex_jobs_per_day %d must be 0 (off) or more (absent = default %d)",
			*c.CodexJobsPerDay, DefaultCodexJobsPerDay)
	}
	if c.DailyHour != nil && (*c.DailyHour < 0 || *c.DailyHour > 23) {
		return c, fmt.Errorf("config.json: daily_hour %d must be 0..23 (local time)", *c.DailyHour)
	}
	if c.ReportContract != "" && c.ReportContract != ReportContractWarn && c.ReportContract != ReportContractFail {
		return c, fmt.Errorf("config.json: report_contract %q must be warn (default) or fail", c.ReportContract)
	}
	if c.AutoContinueFraction < 0 || c.AutoContinueFraction > 1 {
		return c, fmt.Errorf("config.json: auto_continue_fraction %g must be 0 (default %g) .. 1", c.AutoContinueFraction,
			DefaultAutoContinueFraction)
	}
	if err := c.validateAcceptance(); err != nil {
		return c, err
	}
	if c.MaxBriefBytes < 0 || c.BriefWarnFiles < 0 || c.BriefWarnHeadings < 0 {
		return c, errors.New("config.json: max_brief_bytes, brief_warn_files and brief_warn_headings must be 0 (default) or positive")
	}
	if err := c.validateCodex(); err != nil {
		return c, fmt.Errorf("config.json: %w", err)
	}
	if err := c.validateEffortByKind(); err != nil {
		return c, fmt.Errorf("config.json: %w", err)
	}
	if err := validatePrices(c.ModelPrices); err != nil {
		return c, fmt.Errorf("config.json: %w", err)
	}
	if err := c.Heartbeat.validate(); err != nil {
		return c, fmt.Errorf("config.json: %w", err)
	}
	// An empty listen is the default socket (HTTPAPISocket), resolved against the state dir when served.
	if c.HTTPAPI.Listen != "" {
		if err := ValidateHTTPListen(c.HTTPAPI.Listen); err != nil {
			return c, fmt.Errorf("config.json: %w", err)
		}
	}
	if c.LogArchiveDir == "" {
		c.LogArchiveDir = filepath.Join(stateDir, "archive")
	}
	if c.OwnerRulesPath == "" {
		c.OwnerRulesPath = filepath.Join(stateDir, "owner-rules.md")
	}
	if c.CardsDir == "" {
		c.CardsDir = filepath.Join(stateDir, "cards")
	}
	if c.Heartbeat.GoalsFile == "" {
		c.Heartbeat.GoalsFile = filepath.Join(stateDir, DefaultHeartbeatGoalsFile)
	}
	return c, nil
}

// TrashRootsOrDefault returns the configured trash roots; nil when none are configured (fail
// closed: there is no default root).
func (c Config) TrashRootsOrDefault() []string {
	if len(c.TrashRoots) > 0 {
		return c.TrashRoots
	}
	return nil
}

func (c Config) claudeBin() string {
	if b := os.Getenv("AGENT_JOBS_CLAUDE_BIN"); b != "" {
		return b
	}
	if c.ClaudeBin != "" {
		return c.ClaudeBin
	}
	return "claude"
}

// reTmuxSocket is a tmux socket name: a plain file name, never a path or a flag.
var reTmuxSocket = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,63}$`)

// tmuxArgs prefixes args with `-L <socket>` when a socket is configured.
func (c Config) tmuxArgs(args ...string) []string {
	if c.TmuxSocket == "" {
		return args
	}
	return append([]string{"-L", c.TmuxSocket}, args...)
}

func (c Config) tmuxBin() string {
	if b := os.Getenv("AGENT_JOBS_TMUX_BIN"); b != "" {
		return b
	}
	if c.TmuxBin != "" {
		return c.TmuxBin
	}
	return "tmux"
}

// Complexities a caller may state instead of a model.
var Complexities = []string{"light", "normal", "hard"}

// defaultModels implements DEVIATIONS #6: model chosen by kind × complexity.
var defaultModels = map[string]map[string]string{
	// "fable" is the claude CLI alias of the latest Fable model (claude-fable-5-1 on 2026-10-15).
	"review":   {"light": "sonnet", "normal": "opus", "hard": "fable"},
	"fix":      {"light": "sonnet", "normal": "opus", "hard": "fable"},
	"research": {"light": "sonnet", "normal": "sonnet", "hard": "opus"},
	"general":  {"light": "sonnet", "normal": "opus", "hard": "fable"},
	// The gate kinds (Wave F): a judge for claude coders runs on codex (an explicit gpt-* model). The
	// default judge model is a claude one, so a default verdict on claude-coded work is same_family:
	// judge_start warns (JudgeFamilyWarning).
	KindSlicing: {"light": "sonnet", "normal": "opus", "hard": "fable"},
	KindJudge:   {"light": "opus", "normal": "opus", "hard": "fable"},
}

// ModelFor returns the model for kind × complexity. The complexity must be stated: there is no
// default complexity and no classifier (owner 2026-10-17, docs/DECISIONS.md).
func ModelFor(kind, complexity string) (string, error) { return modelFor(nil, kind, complexity) }

// modelFor is ModelFor with the config's overrides (config models) applied first.
func modelFor(overrides map[string]map[string]string, kind, complexity string) (string, error) {
	if complexity == "" {
		return "", errors.New("complexity is required to pick a model (light|normal|hard)")
	}
	m, ok := defaultModels[kind][complexity]
	if !ok {
		return "", fmt.Errorf("unknown kind/complexity %q/%q (complexity: light|normal|hard)", kind, complexity)
	}
	if o := overrides[kind][complexity]; o != "" {
		return o, nil
	}
	return m, nil
}

// reModel is the shape of a model name or alias passed to `claude --model` (e.g. "opus",
// "claude-opus-4-6", "claude-sonnet-4-6[1m]"). It never starts with "-", so it cannot become a flag.
var reModel = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:\[\]-]{0,63}$`)

// ValidateModel rejects a malformed explicit model.
func ValidateModel(model string) error {
	if !reModel.MatchString(model) {
		return fmt.Errorf("model %q is not a model name or alias", model)
	}
	return nil
}

// ResolveModel returns the model for a request: the explicit model if given (a stated complexity
// must still be valid), else the kind × complexity table. Neither given = rejected, never guessed.
func ResolveModel(kind, model, complexity string) (string, error) {
	return resolveModel(nil, kind, model, complexity)
}

func resolveModel(overrides map[string]map[string]string, kind, model, complexity string) (string, error) {
	if model == "" && complexity == "" {
		return "", errors.New("model or complexity is required: state an explicit model or a complexity " +
			"(light|normal|hard); there is no default and no classifier")
	}
	if complexity != "" {
		m, err := modelFor(overrides, kind, complexity)
		if err != nil {
			return "", err
		}
		if model == "" {
			return m, nil
		}
	}
	if err := ValidateModel(model); err != nil {
		return "", err
	}
	return model, nil
}

// PinScopeRequired sets cgroup_scope to "required" in <state>/config.json when the file does not set it
// yet and a transient user scope can be created right now (the installer runs it: on a host where scopes
// work, a job never silently falls back to the weaker session containment). Other keys are kept. It
// returns what it did, for the installer's output.
func PinScopeRequired(stateDir string) (string, error) {
	path := filepath.Join(stateDir, "config.json")
	raw := map[string]json.RawMessage{}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return "", fmt.Errorf("read config: %w", err)
	default:
		if err := json.Unmarshal(b, &raw); err != nil {
			return "", fmt.Errorf("parse config.json: %w", err)
		}
	}
	if v, ok := raw["cgroup_scope"]; ok {
		return "cgroup_scope already set (" + string(v) + "), left as is", nil
	}
	if err := probeScope(); err != nil {
		return "cgroup_scope left at auto (no user scope on this host: " + err.Error() + ")", nil
	}
	raw["cgroup_scope"] = json.RawMessage(`"required"`)
	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return "", err
	}
	tmp := path + ".new"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return "cgroup_scope set to required (user scopes work on this host): " + path, nil
}
