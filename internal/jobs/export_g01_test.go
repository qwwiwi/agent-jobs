package jobs

import (
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// Test-only exports for the external package jobs_test (milestones_g01_e2e_test.go), which drives the
// MCP and HTTP layers over a manager that really runs jobs (the fake claude of TestMain).
var (
	SkillsManagerForTest = func(t *testing.T) *Manager { m, _ := skillsManager(t); return m }
	RunToEndForTest      = func(t *testing.T, m *Manager, id string) *store.Job { return runToEnd(t, m, id) }
)
