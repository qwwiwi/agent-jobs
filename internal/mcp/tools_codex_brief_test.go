package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

func TestJobStartCodexBriefWarnings(t *testing.T) {
	// Keep all state in this test's job-local temp directory; the transport is in memory.
	t.Setenv("AGENT_JOBS_HOME", t.TempDir())
	m, err := jobs.Open("test")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.Cfg.CodexWriteAllowed = true
	off := 0
	m.Cfg.CodexJobsPerDay = &off
	cs := connect(t, mustServer(t, m))
	for _, tc := range []struct {
		name, model, brief string
		want               int
	}{
		{"English", "gpt-6.1-sol", "GIT CLONE repo; GIT COMMIT; GIT PUSH; SCREENSHOT via browser; PSQL pg_virtualenv DOCKER; CURL https://example.invalid; RUN CODEX", 7},
		{"Russian", "gpt-6.1-sol", "Склонируй репозиторий; закоммить; запушь; сделай скриншот в браузере; запусти постгрес; выполни HTTP-запрос; запусти codex", 7},
		{"ordinary Codex work", "gpt-6.1-sol", "Fix parsing and write unit tests", 0},
		{"Claude", "sonnet", "git clone; git commit; screenshot; psql; curl; run codex", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var v JobView
			call(t, cs, "job_start", startArgs(func(a map[string]any) {
				a["model"], a["brief"] = tc.model, tc.brief
			}), &v)
			if v.Status != store.StatusQueued || len(v.Warnings) != tc.want {
				t.Fatalf("status=%s warnings=%v, want queued / %d warnings", v.Status, v.Warnings, tc.want)
			}
			j, err := m.Store.Get(v.ID)
			if err != nil {
				t.Fatal(err)
			}
			p, err := m.BuildPrompt(j, "rules", nil, jobs.MemoryNotes{})
			if err != nil {
				t.Fatal(err)
			}
			for _, warning := range v.Warnings {
				if !strings.Contains(p, warning) || !strings.Contains(warning, "; you cannot") {
					t.Errorf("warning not actionable or missing from prompt: %q", warning)
				}
			}
		})
	}
	t.Run("existing warnings retained", func(t *testing.T) {
		s, err := NewServer(RoleOrchestrator, Deps{Manager: m, Watch: NewServerWatch(m)})
		if err != nil {
			t.Fatal(err)
		}
		watched := connect(t, s)
		if err := os.WriteFile(filepath.Join(m.StateDir, "config.json"), []byte(`{"max_running":99}`), 0o600); err != nil {
			t.Fatal(err)
		}
		var v JobView
		result := rawCall(t, watched, "job_start", startArgs(func(a map[string]any) {
			a["model"], a["brief"] = "gpt-6.1-sol", "git clone repository"
		}))
		if err := json.NewDecoder(strings.NewReader(result)).Decode(&v); err != nil {
			t.Fatal(err)
		}
		if len(v.Warnings) != 2 || !strings.Contains(v.Warnings[0], "config.json changed but does not load") ||
			!strings.Contains(v.Warnings[1], "The brief asks to clone") {
			t.Fatalf("watch and lint warnings not combined: %v", v.Warnings)
		}
	})
}
