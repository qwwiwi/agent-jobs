package jobs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"
)

// ConfigReloader is shared by the daemon and MCP watch. It parses a stable file in full before
// publishing anything; failed reads retain the last good config. Callers synchronize publication.
type ConfigReloader struct {
	mu       sync.Mutex
	stateDir string
	current  Config
	checked  bool
	id       FileIdentity
	exists   bool
	lastErr  string
	restart  []string
	readFile func(string) ([]byte, error) // per-loader seam for concurrent-write tests
}

func NewConfigReloader(stateDir string, initial Config) *ConfigReloader {
	return &ConfigReloader{stateDir: stateDir, current: initial, readFile: os.ReadFile}
}

// Hot keys affect future selections or runtime limits. All other keys are restart-only, including
// new keys unless explicitly reviewed here. Existing jobs retain their stored launch parameters.
var hotConfigKeys = map[string]bool{
	"skill_roots": true, "skills_max_per_job": true, "skills_max_kb": true,
	"max_running": true, "codex_jobs_per_day": true, "max_brief_bytes": true,
	"brief_warn_files": true, "brief_warn_headings": true, "models": true,
	"effort_by_kind": true, "codex_reasoning_effort": true,
	"acceptance_timeout_minutes": true, "owner_waits_max_hours": true,
	"idle_after_result_seconds": true, "job_dir_note_mb": true, "size_required": true,
}

func (w *ConfigReloader) changed(id FileIdentity, exists bool) bool {
	return !w.checked || exists != w.exists || (exists && !id.Same(w.id))
}

func (w *ConfigReloader) Changed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	id, err := StatIdentity(filepath.Join(w.stateDir, "config.json"))
	return w.changed(id, err == nil)
}

// Reload returns changed=true even on rejection, allowing each observed edit to be logged once.
func (w *ConfigReloader) Reload() (cfg Config, changed bool, reloadErr string, restart []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	path := filepath.Join(w.stateDir, "config.json")
	id, statErr := StatIdentity(path)
	exists := statErr == nil
	if !w.changed(id, exists) {
		return w.current, false, w.lastErr, w.restart
	}
	previouslyExisted := w.exists
	w.checked, w.id, w.exists = true, id, exists
	var next Config
	var err error
	switch {
	case !exists:
		// A missing startup file means defaults. Removing a previously loaded file is an error,
		// never an implicit reset of the running daemon's security or catalog settings.
		if statErr != nil && (!errors.Is(statErr, os.ErrNotExist) || previouslyExisted) {
			err = fmt.Errorf("read config.json: %w", statErr)
		} else {
			next = w.current
		}
	default:
		var b []byte
		b, err = w.readFile(path)
		if err == nil {
			after, e := StatIdentity(path)
			if e != nil || !id.Same(after) {
				err = errors.New("config.json changed while being read; retrying next check")
				w.checked = false
			} else {
				next, err = parseConfig(w.stateDir, b)
			}
		}
	}
	if err != nil {
		w.lastErr = RedactPrintable(err.Error(), 300)
		return w.current, true, w.lastErr, w.restart
	}
	active := next
	a, old, desired := reflect.ValueOf(&active).Elem(), reflect.ValueOf(w.current), reflect.ValueOf(next)
	w.restart = nil
	for i := 0; i < a.NumField(); i++ {
		key := a.Type().Field(i).Tag.Get("json")
		if !hotConfigKeys[key] {
			if !reflect.DeepEqual(old.Field(i).Interface(), desired.Field(i).Interface()) {
				w.restart = append(w.restart, key)
			}
			a.Field(i).Set(old.Field(i))
		}
	}
	w.current, w.lastErr = active, ""
	return active, true, "", w.restart
}

// Config returns the immutable active config. Cfg is retained as the startup config for short-lived
// managers and tests; the daemon only publishes runtime snapshots and never mutates Cfg.
func (m *Manager) Config() Config {
	if c := m.runtimeConfig.Load(); c != nil {
		return *c
	}
	return m.Cfg
}

// authorizedLaunchConfig uses the launcher's active settings (manual ticks are excluded while a
// daemon is active). Disk edits can revoke permissions
// immediately, but granting restart-only permissions requires a daemon restart. Invalid edits deny
// the launch even while the daemon retains its last good config for other work.
func (m *Manager) authorizedLaunchConfig() (Config, error) {
	disk, err := LoadConfig(m.StateDir)
	if err != nil {
		return Config{}, fmt.Errorf("job not launched: config reload failed: %w", err)
	}
	active := m.Config()
	active.CodexWriteAllowed = active.CodexWriteAllowed && disk.CodexWriteAllowed
	active.AcceptanceAllowed = active.AcceptanceAllowed && disk.AcceptanceAllowed
	return active, nil
}

// WatchDaemonConfig enables reloads at the start of every tick, before schedules and launches.
func (m *Manager) WatchDaemonConfig() {
	m.configWatch = NewConfigReloader(m.StateDir, m.Cfg)
	m.RefreshDaemonConfig()
}

func (m *Manager) RefreshDaemonConfig() {
	if m.configWatch == nil {
		return
	}
	cfg, changed, errText, restart := m.configWatch.Reload()
	if !changed && !m.configPublishPending {
		return
	}
	if changed && errText != "" {
		m.Log.Printf("config.json reload rejected; keeping last good config: %s", errText)
	} else if changed {
		m.runtimeConfig.Store(&cfg)
		m.Log.Printf("config.json reloaded")
		for _, key := range restart {
			m.Log.Printf("config.json changed key %s requires daemon restart", key)
		}
	}
	status := DaemonConfigStatus{PID: os.Getpid(), CheckedAt: m.Now().UTC(), Fingerprint: configFingerprint(m.Config()), Error: errText, RestartKeys: restart}
	m.configPublishPending = true
	if err := writeJSON(filepath.Join(m.StateDir, "daemon-config.json"), status); err != nil {
		m.Log.Printf("record daemon config: %v", err)
	} else {
		m.configPublishPending = false
	}
}

// DaemonConfigStatus contains no config values or credentials; doctor compares a normalized digest.
type DaemonConfigStatus struct {
	PID         int       `json:"pid"`
	CheckedAt   time.Time `json:"checked_at"`
	Fingerprint string    `json:"fingerprint"`
	Error       string    `json:"error,omitempty"`
	RestartKeys []string  `json:"restart_keys,omitempty"`
}

func configFingerprint(c Config) string {
	b, _ := json.Marshal(c)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// DaemonConfigWarning also diagnoses older daemons that have no runtime config status.
func (m *Manager) DaemonConfigWarning() string {
	di, err := m.ReadDaemonInfo()
	if err != nil || !alive(di.PID) {
		return ""
	}
	path := filepath.Join(m.StateDir, "config.json")
	id, statErr := StatIdentity(path)
	b, err := os.ReadFile(filepath.Join(m.StateDir, "daemon-config.json"))
	var status DaemonConfigStatus
	if err != nil || json.Unmarshal(b, &status) != nil || status.PID != di.PID || status.CheckedAt.Before(di.StartedAt) {
		if statErr == nil && id.ModTime.After(di.StartedAt) {
			return "daemon config older than config.json; daemon has no reload status; restart the daemon"
		}
		return ""
	}
	cfg, loadErr := LoadConfig(m.StateDir)
	if loadErr != nil || (statErr != nil && (!errors.Is(statErr, os.ErrNotExist) || status.Error != "")) {
		return "daemon config differs from file: config.json does not load; daemon keeps the last good config"
	}
	if configFingerprint(cfg) != status.Fingerprint {
		warning := "daemon config differs from file (config.json); waiting for the next daemon tick"
		if len(status.RestartKeys) > 0 {
			warning = "daemon config differs from file; changed keys require daemon restart: " + strings.Join(status.RestartKeys, ", ")
		}
		return warning
	}
	return ""
}
