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
	"time"
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
	// TmuxBin overrides the tmux binary (env AGENT_JOBS_TMUX_BIN wins).
	TmuxBin string `json:"tmux_bin"`
	// TrashRoots are the absolute directories trash_move may take paths from (and trash_restore put
	// them back into). Empty = the user's home directory. The state dir is always refused.
	TrashRoots []string `json:"trash_roots"`
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
	for _, r := range c.TrashRoots {
		if !filepath.IsAbs(r) {
			return c, fmt.Errorf("config.json: trash_roots entry %q must be an absolute path", r)
		}
	}
	if c.OwnerRulesPath == "" {
		c.OwnerRulesPath = filepath.Join(stateDir, "owner-rules.md")
	}
	return c, nil
}

// TrashRootsOrDefault returns the configured trash roots, or the user's home directory.
func (c Config) TrashRootsOrDefault() []string {
	if len(c.TrashRoots) > 0 {
		return c.TrashRoots
	}
	if h, err := os.UserHomeDir(); err == nil {
		return []string{h}
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
func ModelFor(kind, complexity string) (string, error) {
	if complexity == "" {
		return "", errors.New("complexity is required to pick a model (light|normal|hard)")
	}
	m, ok := defaultModels[kind][complexity]
	if !ok {
		return "", fmt.Errorf("unknown kind/complexity %q/%q (complexity: light|normal|hard)", kind, complexity)
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
	if model == "" && complexity == "" {
		return "", errors.New("model or complexity is required: state an explicit model or a complexity " +
			"(light|normal|hard); there is no default and no classifier")
	}
	if complexity != "" {
		m, err := ModelFor(kind, complexity)
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
