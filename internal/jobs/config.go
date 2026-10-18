// Package jobs implements the job lifecycle: enqueue, launch, collect, timeout,
// notify and cleanup. One Manager serves one agent.
package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/brain"
)

// Limits: defaults and hard ceilings. A request above a ceiling is rejected, never clamped.
const (
	DefaultMaxTurns       = 80
	DefaultMaxBudgetUSD   = 10.0
	DefaultTimeoutMinutes = 45
	CeilingMaxTurns       = 200
	CeilingMaxBudgetUSD   = 50.0
	CeilingTimeoutMinutes = 180
	MaxRunningPerAgent    = 2
	MaxOwnerRulesBytes    = 8 * 1024
)

// Reference timings («раз в минуту», «через 30 минут после уведомления»): fixed, not configurable.
const (
	TickInterval = time.Minute
	CleanupDelay = 30 * time.Minute
)

// Kinds of work.
var Kinds = []string{"review", "fix", "research", "general"}

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
	// DailyOff turns the daily automation off (prune and doctor procs then run only by hand).
	DailyOff bool `json:"daily_off"`
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
	var c Config
	b, err := os.ReadFile(filepath.Join(stateDir, "config.json"))
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return c, fmt.Errorf("read config: %w", err)
	default:
		if err := json.Unmarshal(b, &c); err != nil {
			return c, fmt.Errorf("parse config.json: %w", err)
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(b, &raw); err != nil {
			return c, fmt.Errorf("parse config.json: %w", err)
		}
		for k, why := range removedConfigKeys {
			if _, ok := raw[k]; ok {
				return c, fmt.Errorf("config.json: %q is no longer supported: %s", k, why)
			}
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
	if c.IdleAfterResultSeconds < IdleAfterResultOff {
		return c, fmt.Errorf("config.json: idle_after_result_seconds %d must be -1 (off), 0 (default) or positive", c.IdleAfterResultSeconds)
	}
	for _, r := range c.TrashRoots {
		if !filepath.IsAbs(r) {
			return c, fmt.Errorf("config.json: trash_roots entry %q must be an absolute path", r)
		}
	}
	if c.LogArchiveDir != "" && !filepath.IsAbs(c.LogArchiveDir) {
		return c, fmt.Errorf("config.json: log_archive_dir %q must be an absolute path", c.LogArchiveDir)
	}
	if c.LogArchiveDays != nil && *c.LogArchiveDays < 0 {
		return c, fmt.Errorf("config.json: log_archive_days %d must be 0 (forever) or more", *c.LogArchiveDays)
	}
	if c.DailyHour != nil && (*c.DailyHour < 0 || *c.DailyHour > 23) {
		return c, fmt.Errorf("config.json: daily_hour %d must be 0..23 (local time)", *c.DailyHour)
	}
	if err := c.validateCodex(); err != nil {
		return c, fmt.Errorf("config.json: %w", err)
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
