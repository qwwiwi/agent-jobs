package jobs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

func writeReloadConfig(t *testing.T, m *Manager, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(m.StateDir, "config.json"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Reproduces task 640: selection in a separately opened manager sees a root added after daemon
// startup. The daemon must refresh before delivery, or this job fails with the old catalog error.
func TestDaemonReloadDeliversNewSkillRoot(t *testing.T) {
	daemon := newManager(t)
	daemon.WatchDaemonConfig()
	root := skillCatalog(t, map[string]any{"name": "new-skill", "status": "core", "kinds": []string{"general"}, "risk": []string{"secrets-path"}})
	b, _ := json.Marshal(map[string]any{"skill_roots": []string{root}})
	writeReloadConfig(t, daemon, string(b))
	selector, err := Open(daemon.Agent)
	if err != nil {
		t.Fatal(err)
	}
	defer selector.Close()
	j := enqueue(t, selector, "work", nil)
	if !slices.Equal(j.Skills, []string{"new-skill"}) || len(daemon.Config().SkillRoots) != 0 {
		t.Fatalf("not the startup mismatch: skills=%v daemon roots=%v", j.Skills, daemon.Config().SkillRoots)
	}
	j = runToEnd(t, daemon, j.ID)
	if j.Status != store.StatusSucceeded || !slices.Equal(j.SkillsDelivered, []string{"new-skill"}) {
		t.Fatalf("status=%s delivered=%v error=%s", j.Status, j.SkillsDelivered, j.Error)
	}
}

func TestDaemonInvalidReloadKeepsLastGood(t *testing.T) {
	m, root := skillsManager(t)
	b, _ := json.Marshal(map[string]any{"skill_roots": []string{root}, "max_running": 3})
	writeReloadConfig(t, m, string(b))
	m.WatchDaemonConfig()
	var logs bytes.Buffer
	m.Log = log.New(&logs, "", 0)
	for _, invalid := range []string{"", "{\"skill_roots\": [", `{"max_running": 99}`, "null"} {
		writeReloadConfig(t, m, invalid)
		if err := m.Tick(); err != nil {
			t.Fatal(err)
		}
		if m.Config().MaxRunningJobs() != 3 || !slices.Equal(m.Config().SkillRoots, []string{root}) {
			t.Fatalf("invalid config applied: %+v", m.Config())
		}
	}
	if strings.Count(logs.String(), "reload rejected; keeping last good config") != 4 {
		t.Fatalf("reload logs: %s", logs.String())
	}
	j := runToEnd(t, m, enqueue(t, m, "still works", nil).ID)
	if j.Status != store.StatusSucceeded || len(j.SkillsDelivered) != 2 {
		t.Fatalf("last good delivery: %+v", j)
	}
	writeReloadConfig(t, m, `{"max_running": 5}`)
	m.RefreshDaemonConfig()
	if m.Config().MaxRunningJobs() != 5 || len(m.Config().SkillRoots) != 0 {
		t.Fatal("valid recovery did not apply")
	}
}

func TestDaemonReloadRestartKeysAndLogging(t *testing.T) {
	m := newManager(t)
	m.WatchDaemonConfig()
	var logs bytes.Buffer
	m.Log = log.New(&logs, "", 0)
	writeReloadConfig(t, m, `{"max_running": 6, "skills_max_per_job": 4, "http_api": {"enabled": true, "listen": "127.0.0.1:7788"}, "cgroup_scope": "required", "brain": {"recall_url": "http://127.0.0.1:9/recall", "token_env": "BRAIN_TOKEN"}}`)
	m.RefreshDaemonConfig()
	if m.Config().MaxRunningJobs() != 6 || m.Config().SkillsMaxPerJob != 4 {
		t.Fatal("hot limits did not apply")
	}
	if m.Config().HTTPAPI.Enabled || m.Config().CgroupScope != "" || m.Config().Brain.RecallURL != "" {
		t.Fatal("restart-only config applied")
	}
	for _, key := range []string{"http_api", "cgroup_scope", "brain"} {
		if !strings.Contains(logs.String(), "changed key "+key+" requires daemon restart") {
			t.Fatalf("missing restart log %s: %s", key, logs.String())
		}
	}
	if !strings.Contains(logs.String(), "config.json reloaded") {
		t.Fatalf("missing reload log: %s", logs.String())
	}
	before := logs.String()
	m.RefreshDaemonConfig()
	if logs.String() != before {
		t.Fatal("unchanged file logged another reload")
	}
}

func TestDaemonConfigConcurrentReaders(t *testing.T) {
	m := newManager(t)
	m.WatchDaemonConfig()
	var wg sync.WaitGroup
	done := make(chan struct{})
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
					_ = m.Config().MaxRunningJobs()
					_ = m.Catalog()
				}
			}
		}()
	}
	defer func() { close(done); wg.Wait() }()
	for i := range 20 {
		writeReloadConfig(t, m, fmt.Sprintf(`{"max_running": %d}`, i%8+1))
		m.RefreshDaemonConfig()
	}
}

func TestDaemonLaunchPermissionsRequireActiveAndDisk(t *testing.T) {
	for _, runner := range []string{"codex-fix", "codex-general", "acceptance"} {
		for _, edit := range []struct {
			name, disk string
			initial    bool
		}{
			{"enable without restart", `{"codex_write_allowed":true,"acceptance_allowed":true}`, false},
			{"revoke immediately", `{"codex_write_allowed":false,"acceptance_allowed":false}`, true},
			{"invalid file", `{"max_running":99}`, true},
		} {
			t.Run(runner+"/"+edit.name, func(t *testing.T) {
				m := newManager(t)
				writeReloadConfig(t, m, fmt.Sprintf(`{"codex_write_allowed":%t,"acceptance_allowed":%t}`, edit.initial, edit.initial))
				var err error
				m.Cfg, err = LoadConfig(m.StateDir)
				if err != nil {
					t.Fatal(err)
				}
				m.WatchDaemonConfig()
				if !edit.initial {
					writeReloadConfig(t, m, edit.disk)
				}
				// A fresh CLI/MCP manager can enqueue from the edited file, but cannot grant
				// permissions to the daemon that is still running with its startup opt-ins.
				submitter, err := Open(m.Agent)
				if err != nil {
					t.Fatal(err)
				}
				defer submitter.Close()
				j := enqueue(t, submitter, "work", func(r *StartRequest) {
					r.Kind = "fix"
					if runner == "acceptance" {
						r.AcceptanceCmd = "touch ../acc-ran"
					} else {
						r.Model = "gpt-6.1-sol"
						if runner == "codex-general" {
							r.Kind = "general"
						}
					}
				})
				if edit.initial {
					writeReloadConfig(t, m, edit.disk)
				}
				if err := m.Tick(); err != nil {
					t.Fatal(err)
				}
				got := get(t, m, j.ID)
				want := "codex_write_allowed"
				if runner == "acceptance" {
					want = "acceptance_allowed"
				}
				if edit.name == "invalid file" {
					want = "config reload failed"
				}
				if got.Status != store.StatusFailed || !strings.Contains(got.Error, want) {
					t.Fatalf("launch not refused: status=%s error=%s", got.Status, got.Error)
				}
				if m.Config().CodexWriteAllowed != edit.initial || m.Config().AcceptanceAllowed != edit.initial {
					t.Fatal("restart-only permission changed in active config")
				}
				for _, name := range []string{"wrapper.json", "argv.txt", "acc-ran"} {
					if _, err := os.Stat(filepath.Join(m.JobDir(j.ID), name)); !os.IsNotExist(err) {
						t.Fatalf("refused launch created %s: %v", name, err)
					}
				}
			})
		}
	}
}

func TestDaemonConfigPublicationRetriesWithoutEdit(t *testing.T) {
	m := newManager(t)
	m.WatchDaemonConfig()
	previousRename := renameFile
	fail := true
	renameFile = func(old, next string) error {
		if fail && filepath.Base(next) == "daemon-config.json" {
			return syscall.ENOSPC
		}
		return previousRename(old, next)
	}
	defer func() { renameFile = previousRename }()
	var logs bytes.Buffer
	m.Log = log.New(&logs, "", 0)
	writeReloadConfig(t, m, `{"max_running":3}`)
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if !m.configPublishPending || m.configWatch.Changed() {
		t.Fatal("failed publication was not pending for an unchanged config")
	}
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if strings.Count(logs.String(), "record daemon config:") != 2 {
		t.Fatalf("publication not retried each tick: %s", logs.String())
	}
	fail = false
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(m.StateDir, "daemon-config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var status DaemonConfigStatus
	if err := json.Unmarshal(b, &status); err != nil {
		t.Fatal(err)
	}
	if m.configPublishPending || status.Fingerprint != configFingerprint(m.Config()) || m.Config().MaxRunningJobs() != 3 {
		t.Fatalf("recovery did not publish active config: %+v", status)
	}
	if strings.Count(logs.String(), "config.json reloaded") != 1 {
		t.Fatalf("publication retry logged a reload: %s", logs.String())
	}
}

func TestWrapperAcceptanceRequiresLaunchAndDiskPermission(t *testing.T) {
	for _, tc := range []struct {
		name, disk, want string
		authorized       bool
	}{
		{"missing launch authorization", `{"acceptance_allowed":true}`, "not authorized by the launching daemon", false},
		{"disk revoked", `{"acceptance_allowed":false}`, "acceptance_allowed is off", true},
		{"invalid disk", `{"acceptance_allowed":true,"max_running":99}`, "config (acceptance timeout)", true},
		{"allowed", `{"acceptance_allowed":true}`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := acceptanceManager(t, "")
			j := enqueue(t, m, "work", withAcceptance("true"))
			writeReloadConfig(t, m, tc.disk)
			a, err := openAcceptance(m.JobDir(j.ID), tc.authorized)
			if a != nil {
				defer a.st.Close()
			}
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("wrapper permission: %v, want %q", err, tc.want)
				}
			} else if err != nil || a == nil || a.cmd != "true" {
				t.Fatalf("authorized acceptance unavailable: acceptance=%+v error=%v", a, err)
			}
		})
	}
}

func TestConfigReloaderRejectsConcurrentWriteAndRetries(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(fmt.Sprintf("replace=%t", replace), func(t *testing.T) {
			m := newManager(t)
			writeReloadConfig(t, m, `{"max_running":3}`)
			w := NewConfigReloader(m.StateDir, m.Cfg)
			w.readFile = func(path string) ([]byte, error) {
				b, err := os.ReadFile(path)
				if err != nil {
					return nil, err
				}
				next := []byte(`{"max_running":7,"skills_max_per_job":4}`)
				if replace {
					err = writeAtomic(path, next)
				} else {
					err = os.WriteFile(path, next, 0o600)
				}
				return b, err
			}
			cfg, changed, errText, _ := w.Reload()
			if !changed || !strings.Contains(errText, "changed while being read") || cfg.MaxRunningJobs() != m.Cfg.MaxRunningJobs() {
				t.Fatalf("concurrent edit applied: changed=%t error=%q cfg=%+v", changed, errText, cfg)
			}
			if !w.Changed() {
				t.Fatal("concurrent write did not schedule a retry")
			}
			w.readFile = os.ReadFile
			cfg, changed, errText, _ = w.Reload()
			if !changed || errText != "" || cfg.MaxRunningJobs() != 7 || cfg.SkillsMaxPerJob != 4 {
				t.Fatalf("stable edit not retried: changed=%t error=%q cfg=%+v", changed, errText, cfg)
			}
		})
	}
}

func TestSkillDeliveryExplainsMissingCatalog(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		roots      bool
	}{
		{"no roots", "daemon catalog has no skill roots", false},
		{"missing entry", "skill missing from all daemon skill roots", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newManager(t)
			root := skillCatalog(t, map[string]any{"name": "other", "status": "active"})
			if tc.roots {
				m.Cfg.SkillRoots = []string{root}
			}
			b, _ := json.Marshal(map[string]any{"skill_roots": []string{root}})
			writeReloadConfig(t, m, string(b))
			err := m.deliverSkills(&store.Job{Skills: []string{"missing"}}, "", t.TempDir())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("refusal: %v", err)
			}
			if !tc.roots && !strings.Contains(err.Error(), "daemon config differs from the file (skill_roots)") {
				t.Fatalf("missing config divergence: %v", err)
			}
		})
	}
}

func TestNoSkillsConflictingEditsRefused(t *testing.T) {
	m, _ := skillsManager(t)
	_, err := m.Enqueue(StartRequest{Title: "test", Kind: "general", Model: "sonnet", Brief: "skills: none", Skills: []string{"+extra"}})
	if err == nil || !strings.Contains(err.Error(), "skills: none conflicts") {
		t.Fatalf("conflict: %v", err)
	}
}
