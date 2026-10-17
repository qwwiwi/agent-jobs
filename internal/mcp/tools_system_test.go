package mcp

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/store"
	"github.com/qwwiwi/agent-jobs/internal/trash"
)

// Reference, «MCP-тулы»: system and file tools are orchestrator tools; a job gets none of them.
func TestSystemAndFileToolsAreOrchestratorOnly(t *testing.T) {
	for _, name := range []string{"system_doctor", "system_logs", "activity_report", "trash_move", "trash_restore"} {
		if !slices.Contains(OrchestratorOnly, name) || slices.Contains(JobAllowed, name) {
			t.Fatalf("%s must be orchestrator-only", name)
		}
		if Allowed(RoleJob, name) || !Allowed(RoleOrchestrator, name) {
			t.Fatalf("%s: wrong audience", name)
		}
	}
}

// insertRow adds a job row directly.
func insertRow(t *testing.T, m *jobs.Manager, id, status string, finished *time.Time, cost float64) {
	t.Helper()
	created := m.Now().Add(-time.Hour)
	if err := m.Store.Insert(&store.Job{ID: id, Agent: m.Agent, Title: "t " + id, Kind: "general", Brief: "b",
		Status: status, Model: "sonnet", MaxTurns: 80, MaxBudgetUSD: 10, TimeoutMinutes: 45, CreatedAt: created,
		StartedAt: &created, FinishedAt: finished, CostUSD: cost, Turns: 3, DurationSec: 60}); err != nil {
		t.Fatal(err)
	}
}

func TestSystemDoctor(t *testing.T) {
	m := newManager(t)
	m.SelfBin = selfBin // the guard hook probe runs the real binary, as a job's settings.json does
	cs := connect(t, mustServer(t, m))
	var out SystemDoctorOutput
	call(t, cs, "system_doctor", map[string]any{}, &out)
	hooks := map[string]jobs.Check{}
	for _, c := range out.Hooks {
		hooks[c.Name] = c
	}
	if !hooks["guard hook"].OK {
		t.Fatalf("guard hook check failed: %+v", hooks["guard hook"])
	}
	if hooks["owner rules"].OK || !slices.ContainsFunc(out.Problems, func(p string) bool { return strings.HasPrefix(p, "owner rules") }) {
		t.Fatalf("missing owner rules must be a problem: %+v", out)
	}
	if out.OK || out.Disk.TotalBytes == 0 || out.Disk.Path != m.StateDir {
		t.Fatalf("doctor: %+v", out)
	}
	if err := os.WriteFile(m.Cfg.OwnerRulesPath, []byte("- rule\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A broken guard binary is a problem too.
	m.SelfBin = filepath.Join(t.TempDir(), "missing-agent-jobs")
	out = SystemDoctorOutput{}
	call(t, cs, "system_doctor", map[string]any{}, &out)
	for _, c := range out.Hooks {
		if c.Name == "guard hook" && c.OK {
			t.Fatal("a missing guard binary passed the hook check")
		}
		if c.Name == "owner rules" && !c.OK {
			t.Fatalf("owner rules now readable: %+v", c)
		}
	}
}

func TestSystemLogs(t *testing.T) {
	m := newManager(t)
	cs := connect(t, mustServer(t, m))
	t.Setenv("AGENT_JOBS_JOURNALCTL_BIN", filepath.Join(t.TempDir(), "no-journalctl"))
	if msg := callErr(t, cs, "system_logs", nil); !strings.Contains(msg, "journalctl failed") {
		t.Fatalf("no log anywhere: %q", msg)
	}
	if err := os.WriteFile(filepath.Join(m.StateDir, "daemon.log"), []byte("l1\nl2\nl3\nl4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out SystemLogsOutput
	call(t, cs, "system_logs", map[string]any{"lines": 2}, &out)
	if out.Text != "l3\nl4" || !strings.HasSuffix(out.Source, "daemon.log") {
		t.Fatalf("daemon log: %+v", out)
	}
	if msg := callErr(t, cs, "system_logs", map[string]any{"lines": 5000}); !strings.Contains(msg, "outside") {
		t.Fatalf("lines cap: %q", msg)
	}
	insertRow(t, m, "j1", store.StatusFailed, nil, 0)
	dir := m.JobDir("j1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stderr.log"), []byte("boom\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	call(t, cs, "system_logs", map[string]any{"job_id": "j1"}, &out)
	if out.Text != "boom" {
		t.Fatalf("job stderr: %+v", out)
	}
	// The job dir is the job's: a stderr.log symlink is not followed.
	_ = os.Remove(filepath.Join(dir, "stderr.log"))
	if err := os.Symlink(filepath.Join(m.StateDir, "daemon.log"), filepath.Join(dir, "stderr.log")); err != nil {
		t.Fatal(err)
	}
	if msg := callErr(t, cs, "system_logs", map[string]any{"job_id": "j1"}); !strings.Contains(msg, "stderr.log") {
		t.Fatalf("symlinked stderr.log: %q", msg)
	}
	if msg := callErr(t, cs, "system_logs", map[string]any{"job_id": "nope"}); !strings.Contains(msg, "not found") {
		t.Fatalf("unknown job: %q", msg)
	}
}

func TestActivityReport(t *testing.T) {
	m := newManager(t)
	now := time.Now()
	m.Now = func() time.Time { return now }
	recent, old := now.Add(-time.Hour), now.Add(-30*time.Hour)
	insertRow(t, m, "ok1", store.StatusSucceeded, &recent, 1.5)
	insertRow(t, m, "bad1", store.StatusFailed, &recent, 0.5)
	insertRow(t, m, "q1", store.StatusQueued, nil, 0)
	// Created and finished before the window: not in a 24 h report.
	if err := m.Store.Insert(&store.Job{ID: "old1", Agent: m.Agent, Title: "old", Kind: "general", Brief: "b",
		Status: store.StatusSucceeded, Model: "sonnet", MaxTurns: 80, MaxBudgetUSD: 10, TimeoutMinutes: 45,
		CreatedAt: old, FinishedAt: &old, CostUSD: 9}); err != nil {
		t.Fatal(err)
	}
	cs := connect(t, mustServer(t, m))
	var out ActivityReportOutput
	call(t, cs, "activity_report", map[string]any{}, &out)
	if out.Finished != 2 || out.CostUSD != 2.0 || out.ByStatus["queued"] != 1 || out.ByStatus["succeeded"] != 1 || len(out.Jobs) != 3 {
		t.Fatalf("24 h report: %+v", out)
	}
	call(t, cs, "activity_report", map[string]any{"hours": 48}, &out)
	if out.Finished != 3 || len(out.Jobs) != 4 {
		t.Fatalf("48 h report: %+v", out)
	}
	if msg := callErr(t, cs, "activity_report", map[string]any{"hours": 721}); !strings.Contains(msg, "outside") {
		t.Fatalf("hours cap: %q", msg)
	}
}

// trash_move / trash_restore through the tools: a round trip inside the configured root, a path
// outside it refused, an unknown id answered with the recent entries.
func TestTrashTools(t *testing.T) {
	m := newManager(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m.Cfg.TrashRoots = []string{root}
	cs := connect(t, mustServer(t, m))
	p := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(p, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	var e trash.Entry
	call(t, cs, "trash_move", map[string]any{"path": p}, &e)
	if _, err := os.Lstat(p); !os.IsNotExist(err) || e.ID == "" || !strings.HasPrefix(e.TrashPath, filepath.Join(m.StateDir, "trash")) {
		t.Fatalf("move: %+v %v", e, err)
	}
	if msg := callErr(t, cs, "trash_restore", map[string]any{"id": "261017-000000-00000000"}); !strings.Contains(msg, e.ID) {
		t.Fatalf("unknown id must list recent entries: %q", msg)
	}
	var r trash.Entry
	call(t, cs, "trash_restore", map[string]any{"id": e.ID}, &r)
	if b, err := os.ReadFile(p); err != nil || string(b) != "keep" {
		t.Fatalf("restore: %q %v", b, err)
	}
	if msg := callErr(t, cs, "trash_move", map[string]any{"path": filepath.Join(m.StateDir, "jobs.db")}); !strings.Contains(msg, "refused") {
		t.Fatalf("state dir outside the root: %q", msg)
	}
	if _, err := os.Stat(filepath.Join(m.StateDir, "jobs.db")); err != nil {
		t.Fatal("jobs.db moved")
	}
}

// The trash is closed without configured roots, and inside a root it never moves the agent-jobs
// home (another agent's state too), the owner rules, the agent-jobs binary, ~/.claude or a .git.
func TestTrashToolsFailClosedAndProtected(t *testing.T) {
	m := newManager(t)
	base, err := filepath.EvalSymlinks(filepath.Dir(filepath.Dir(m.StateDir)))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", base)
	mk := func(p string) string {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	free := mk(filepath.Join(base, "free.txt"))
	cs := connect(t, mustServer(t, m))
	if msg := callErr(t, cs, "trash_move", map[string]any{"path": free}); !strings.Contains(msg, "trash_roots") {
		t.Fatalf("no roots must refuse and explain: %q", msg)
	}
	if msg := callErr(t, cs, "trash_restore", map[string]any{"id": "261017-000000-00000000"}); !strings.Contains(msg, "trash_roots") {
		t.Fatalf("no roots must refuse restore and explain: %q", msg)
	}
	if _, err := os.Stat(free); err != nil {
		t.Fatal("moved without roots")
	}

	m.Cfg.TrashRoots = []string{base}
	m.Cfg.OwnerRulesPath = mk(filepath.Join(base, "elsewhere", "owner-rules.md"))
	m.SelfBin = mk(filepath.Join(base, ".local", "bin", "agent-jobs"))
	sibling := mk(filepath.Join(filepath.Dir(m.StateDir), "silvana", "jobs.db"))
	settings := mk(filepath.Join(base, ".claude", "settings.json"))
	gitCfg := mk(filepath.Join(base, "repo", ".git", "config"))
	cs = connect(t, mustServer(t, m))
	for _, p := range []string{filepath.Dir(m.StateDir), filepath.Dir(sibling), sibling, m.StateDir,
		m.Cfg.OwnerRulesPath, m.SelfBin, filepath.Dir(settings), settings, filepath.Dir(gitCfg), gitCfg} {
		if msg := callErr(t, cs, "trash_move", map[string]any{"path": p}); !strings.Contains(msg, "refused") {
			t.Fatalf("move %s: %q", p, msg)
		}
		if _, err := os.Lstat(p); err != nil {
			t.Fatalf("%s moved", p)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(trashProtected(m), exe) {
		t.Fatalf("the running binary is not protected: %v", trashProtected(m))
	}
	var e trash.Entry
	call(t, cs, "trash_move", map[string]any{"path": free}, &e)
}
