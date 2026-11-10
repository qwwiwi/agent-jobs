package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Gate settings (v0.8.2, G01, G02): how long a gate's command may run and how many gates run on the box
// at once. config.json sets them per agent (gate_timeout_minutes, gate_max_parallel), a project card's
// gate_timeout: per repository; prepare resolves them into the job's gate.json, and the wrapper puts them
// into the runner's environment, so `agent-jobs gate run` inside the job uses them without flags. An
// explicit --timeout still wins. Outside a job the environment or the defaults apply.

// Gate setting defaults, ceilings and names.
const (
	DefaultGateTimeoutMinutes = 60
	CeilingGateTimeoutMinutes = 180
	DefaultGateMaxParallel    = 2
	CeilingGateMaxParallel    = 8

	// GateTimeoutEnvVar is the gate timeout (a Go duration) of a gate run without --timeout.
	GateTimeoutEnvVar = "AGENT_JOBS_GATE_TIMEOUT"
	// GateMaxParallelEnvVar is how many gates may run on the box at once (1..CeilingGateMaxParallel).
	GateMaxParallelEnvVar = "AGENT_JOBS_GATE_MAX_PARALLEL"
	// GateSettingsFile is a job's resolved gate settings, in its job dir.
	GateSettingsFile = "gate.json"

	gateSettingsMaxBytes = 4 << 10
)

// GateTimeout is the gate timeout of config gate_timeout_minutes (default 60 minutes).
func (c Config) GateTimeout() time.Duration {
	if c.GateTimeoutMinutes > 0 {
		return time.Duration(c.GateTimeoutMinutes) * time.Minute
	}
	return DefaultGateTimeoutMinutes * time.Minute
}

// GateMaxParallelSlots is config gate_max_parallel (default 2): the box slots of this agent's gates.
func (c Config) GateMaxParallelSlots() int { return gateSlotCount(c.GateMaxParallel) }

func (c Config) validateGate() error {
	if c.GateTimeoutMinutes < 0 || c.GateTimeoutMinutes > CeilingGateTimeoutMinutes {
		return fmt.Errorf("config.json: gate_timeout_minutes %d must be 0 (default %d) or 1..%d",
			c.GateTimeoutMinutes, DefaultGateTimeoutMinutes, CeilingGateTimeoutMinutes)
	}
	if c.GateMaxParallel < 0 || c.GateMaxParallel > CeilingGateMaxParallel {
		return fmt.Errorf("config.json: gate_max_parallel %d must be 0 (default %d) or 1..%d",
			c.GateMaxParallel, DefaultGateMaxParallel, CeilingGateMaxParallel)
	}
	return nil
}

// parseCardGateTimeout is a card's gate_timeout: value, a Go duration of 1m..CeilingGateTimeoutMinutes.
func parseCardGateTimeout(v string) (time.Duration, error) {
	d, err := time.ParseDuration(v)
	if err != nil || d < time.Minute || d > CeilingGateTimeoutMinutes*time.Minute {
		return 0, fmt.Errorf("gate_timeout %q must be a duration of 1m..%dm (e.g. 90m)", v, CeilingGateTimeoutMinutes)
	}
	return d, nil
}

// GateSettings is a job's gate.json: its gate timeout and the box slots of its gates.
type GateSettings struct {
	TimeoutSec  int64 `json:"timeout_sec"`
	MaxParallel int   `json:"max_parallel"`
}

// gateSettingsFor resolves a job's gate settings: the longest gate_timeout of its matching cards, else
// config gate_timeout_minutes; gate_max_parallel from the config.
func gateSettingsFor(c Config, cards []Card) GateSettings {
	timeout := time.Duration(0)
	for _, card := range cards {
		timeout = max(timeout, card.GateTimeout)
	}
	if timeout == 0 {
		timeout = c.GateTimeout()
	}
	return GateSettings{TimeoutSec: int64(timeout / time.Second), MaxParallel: c.GateMaxParallelSlots()}
}

// writeGateSettings writes s to <dir>/gate.json.
func writeGateSettings(dir string, s GateSettings) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, GateSettingsFile), append(b, '\n'))
}

// readGateSettings reads <dir>/gate.json, a regular file of at most gateSettingsMaxBytes. The job can
// write its own folder, so the values are bounded on use, never trusted further than a flag the job
// could pass itself.
func readGateSettings(dir string) (GateSettings, error) {
	var s GateSettings
	f, err := os.OpenFile(filepath.Join(dir, GateSettingsFile), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return s, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return s, fmt.Errorf("%s is not a regular file", GateSettingsFile)
	}
	b, err := io.ReadAll(io.LimitReader(f, gateSettingsMaxBytes+1))
	if err != nil {
		return s, err
	}
	if len(b) > gateSettingsMaxBytes {
		return s, fmt.Errorf("%s is larger than %d bytes", GateSettingsFile, gateSettingsMaxBytes)
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("%s: %w", GateSettingsFile, err)
	}
	return s, nil
}

// gateSettingsEnv is env with the job's gate settings (<jobDir>/gate.json) in GateTimeoutEnvVar and
// GateMaxParallelEnvVar, replacing inherited values. A job prepared before gate.json existed keeps env.
func gateSettingsEnv(jobDir string, env []string) ([]string, error) {
	s, err := readGateSettings(jobDir)
	if errors.Is(err, fs.ErrNotExist) {
		return env, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(env)+2)
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); k != GateTimeoutEnvVar && k != GateMaxParallelEnvVar {
			out = append(out, kv)
		}
	}
	if s.TimeoutSec > 0 {
		out = append(out, GateTimeoutEnvVar+"="+(time.Duration(s.TimeoutSec)*time.Second).String())
	}
	if s.MaxParallel > 0 {
		out = append(out, GateMaxParallelEnvVar+"="+strconv.Itoa(gateSlotCount(s.MaxParallel)))
	}
	return out, nil
}

// gateTimeoutFromEnv is GateTimeoutEnvVar of env, DefaultGateTimeout when it is unset or unusable (with
// a note then).
func gateTimeoutFromEnv(env []string) (time.Duration, string) {
	v := envValue(env, GateTimeoutEnvVar)
	if v == "" {
		return DefaultGateTimeout, ""
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return DefaultGateTimeout, fmt.Sprintf("%s=%q is no positive duration: timeout %s", GateTimeoutEnvVar,
			Printable(v, 40), DefaultGateTimeout)
	}
	return d, ""
}

// gateMaxParallelFromEnv is GateMaxParallelEnvVar of env (within 1..CeilingGateMaxParallel),
// DefaultGateMaxParallel when it is unset or unusable (with a note then).
func gateMaxParallelFromEnv(env []string) (int, string) {
	v := envValue(env, GateMaxParallelEnvVar)
	if v == "" {
		return DefaultGateMaxParallel, ""
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return DefaultGateMaxParallel, fmt.Sprintf("%s=%q is no positive number: %d box slots", GateMaxParallelEnvVar,
			Printable(v, 40), DefaultGateMaxParallel)
	}
	return gateSlotCount(n), ""
}
