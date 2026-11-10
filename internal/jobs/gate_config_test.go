package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// G01, G02: gate_timeout_minutes (default 60) and gate_max_parallel (default 2) are validated config keys.
func TestGateConfigKeys(t *testing.T) {
	c, err := loadConfigBody(t, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if c.GateTimeout() != 60*time.Minute || c.GateMaxParallelSlots() != 2 {
		t.Fatalf("defaults: timeout %s, max parallel %d", c.GateTimeout(), c.GateMaxParallelSlots())
	}
	c, err = loadConfigBody(t, `{"gate_timeout_minutes": 90, "gate_max_parallel": 3}`)
	if err != nil {
		t.Fatal(err)
	}
	if c.GateTimeout() != 90*time.Minute || c.GateMaxParallelSlots() != 3 {
		t.Fatalf("set: timeout %s, max parallel %d", c.GateTimeout(), c.GateMaxParallelSlots())
	}
	for _, body := range []string{`{"gate_timeout_minutes": -1}`, `{"gate_timeout_minutes": 100000}`,
		`{"gate_max_parallel": -1}`, `{"gate_max_parallel": 99}`} {
		if _, err := loadConfigBody(t, body); err == nil || !strings.Contains(err.Error(), "gate_") {
			t.Fatalf("%s: %v; want a gate_ config error", body, err)
		}
	}
	if !hotConfigKeys["gate_timeout_minutes"] || !hotConfigKeys["gate_max_parallel"] {
		t.Fatal("the gate keys need no daemon restart")
	}
}

// G02: a card's gate_timeout: is a duration within the gate timeout ceiling.
func TestParseCardGateTimeout(t *testing.T) {
	c, err := ParseCard("p", []byte("---\nrepo: /src/p\ngate_timeout: 90m\n---\n# P\n"))
	if err != nil || c.GateTimeout != 90*time.Minute {
		t.Fatalf("ParseCard = %+v, %v", c, err)
	}
	for _, v := range []string{"soon", "0s", "-5m", "1000h", "30s"} {
		if _, err := ParseCard("p", []byte("---\nrepo: /src/p\ngate_timeout: "+v+"\n---\n# P\n")); err == nil ||
			!strings.Contains(err.Error(), "gate_timeout") {
			t.Fatalf("gate_timeout %q: %v; want an error", v, err)
		}
	}
}

// G02: prepare writes the job's gate settings (gate.json): a matching card's gate_timeout wins over the
// config's gate_timeout_minutes; gate_max_parallel comes from the config.
func TestPrepareWritesGateSettings(t *testing.T) {
	m := acceptanceManager(t, `, "gate_timeout_minutes": 75, "gate_max_parallel": 3`)
	repo := newRepo(t)
	j := enqueue(t, m, "x", func(r *StartRequest) { r.RepoPath = repo })
	if err := m.prepare(j, "", m.JobDir(j.ID)); err != nil {
		t.Fatal(err)
	}
	if s := readGateSettingsT(t, m.JobDir(j.ID)); s.TimeoutSec != 75*60 || s.MaxParallel != 3 {
		t.Fatalf("from config: %+v", s)
	}
	if err := os.MkdirAll(m.CardsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	card := "---\nrepo: " + repo + "\ngate_timeout: 2h\n---\n- slow suite\n"
	if err := os.WriteFile(filepath.Join(m.CardsDir(), "slow.md"), []byte(card), 0o600); err != nil {
		t.Fatal(err)
	}
	j = enqueue(t, m, "x", func(r *StartRequest) { r.RepoPath = repo })
	if err := m.prepare(j, "", m.JobDir(j.ID)); err != nil {
		t.Fatal(err)
	}
	if s := readGateSettingsT(t, m.JobDir(j.ID)); s.TimeoutSec != 120*60 || s.MaxParallel != 3 {
		t.Fatalf("from the card: %+v", s)
	}
}

// G02: the runner's environment carries the job's gate settings, so `agent-jobs gate run` inside the
// job uses them without flags; an inherited value is replaced.
func TestRunnerEnvCarriesGateSettings(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "jobs", "261110-000000-gate")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeGateSettings(dir, GateSettings{TimeoutSec: 5400, MaxParallel: 1}); err != nil {
		t.Fatal(err)
	}
	env, err := RunnerEnv(dir, []string{"PATH=/bin", GateTimeoutEnvVar + "=1s"}, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if v := envValue(env, GateTimeoutEnvVar); v != "1h30m0s" {
		t.Fatalf("%s = %q", GateTimeoutEnvVar, v)
	}
	if v := envValue(env, GateMaxParallelEnvVar); v != "1" {
		t.Fatalf("%s = %q", GateMaxParallelEnvVar, v)
	}
	n := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, GateTimeoutEnvVar+"=") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d %s entries in %v", n, GateTimeoutEnvVar, env)
	}
}

func readGateSettingsT(t *testing.T, dir string) GateSettings {
	t.Helper()
	var s GateSettings
	if err := json.Unmarshal([]byte(readFileT(t, filepath.Join(dir, GateSettingsFile))), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// G01: status shows every running gate (one per held box slot); new gates hold the single box lock only
// shared, so it does not show them twice.
func TestGateLockViewShowsSlots(t *testing.T) {
	m := newManager(t)
	var leases []*gateLease
	for i, repo := range []string{newRepo(t), newRepo(t)} {
		l, err := acquireGateLock(context.Background(), gateLockReq{path: GateLockPath(), wait: time.Second,
			detail: fmt.Sprintf(": gate %d", i), repo: gateRepoKey(repo), slots: 2})
		if err != nil {
			t.Fatal(err)
		}
		leases = append(leases, l)
	}
	v := m.gateLockView(nil)
	if len(v.Holders) != 2 || !strings.HasSuffix(v.Holders[0], ": gate 0") || !strings.HasSuffix(v.Holders[1], ": gate 1") ||
		v.Holder != v.Holders[0] {
		t.Fatalf("two running gates: %+v", v)
	}
	for _, l := range leases {
		l.release()
	}
	if v := m.gateLockView(nil); len(v.Holders) != 0 || v.Holder != "" {
		t.Fatalf("no gate runs: %+v", v)
	}
}
