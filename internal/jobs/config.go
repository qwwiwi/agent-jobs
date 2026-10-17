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
	if c.OwnerRulesPath == "" {
		c.OwnerRulesPath = filepath.Join(stateDir, "owner-rules.md")
	}
	return c, nil
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

func (c Config) tmuxBin() string {
	if b := os.Getenv("AGENT_JOBS_TMUX_BIN"); b != "" {
		return b
	}
	if c.TmuxBin != "" {
		return c.TmuxBin
	}
	return "tmux"
}

// defaultModels implements DEVIATIONS #6: model chosen by kind × complexity.
var defaultModels = map[string]map[string]string{
	// "fable" is the claude CLI alias of the latest Fable model (claude-fable-5-1 on 2026-10-15).
	"review":   {"light": "sonnet", "normal": "opus", "hard": "fable"},
	"fix":      {"light": "sonnet", "normal": "opus", "hard": "fable"},
	"research": {"light": "sonnet", "normal": "sonnet", "hard": "opus"},
	"general":  {"light": "sonnet", "normal": "opus", "hard": "fable"},
}

// ModelFor returns the default model for kind × complexity.
func ModelFor(kind, complexity string) (string, error) {
	if complexity == "" {
		complexity = "normal"
	}
	m, ok := defaultModels[kind][complexity]
	if !ok {
		return "", fmt.Errorf("unknown kind/complexity %q/%q (complexity: light|normal|hard)", kind, complexity)
	}
	return m, nil
}
