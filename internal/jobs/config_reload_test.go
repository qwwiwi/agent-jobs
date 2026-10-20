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
