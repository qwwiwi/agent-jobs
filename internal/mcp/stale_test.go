package mcp

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	tmp := dst + ".new"
	if err := os.WriteFile(tmp, b, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, dst); err != nil { // as an install does: a new file over the old path
		t.Fatal(err)
	}
}

func rawCall(t *testing.T, cs *sdk.ClientSession, name string, args map[string]any) string {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return text(res)
}

const staleLine = "agent-jobs MCP server is older than the installed binary"

// Replacing the binary under a running server adds the stale-binary line to the next result (job_status
// here, and job_start's warnings); a server started after the replacement does not warn.
func TestStaleBinaryWarning(t *testing.T) {
	m := newManager(t)
	bin := filepath.Join(t.TempDir(), "agent-jobs")
	copyFile(t, selfBin, bin)
	m.SelfBin = bin
	done := time.Now()
	insertRow(t, m, "j1", store.StatusSucceeded, &done, 1)
	s, err := NewServer(RoleOrchestrator, Deps{Manager: m, Watch: NewServerWatch(m)})
	if err != nil {
		t.Fatal(err)
	}
	cs := connect(t, s)
	if got := rawCall(t, cs, "job_status", map[string]any{"id": "j1"}); strings.Contains(got, staleLine) {
		t.Fatalf("fresh server warns: %s", got)
	}
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(bin+".new", []byte("#!/bin/sh\n# new build\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(bin+".new", bin); err != nil {
		t.Fatal(err)
	}
	if got := rawCall(t, cs, "job_status", map[string]any{"id": "j1"}); !strings.Contains(got, staleLine+" (started ") ||
		!strings.Contains(got, "binary replaced ") || !strings.Contains(got, "restart the session") {
		t.Fatalf("stale server does not warn: %s", got)
	}
	got := rawCall(t, cs, "job_start", startArgs(nil))
	var v JobView
	if err := json.NewDecoder(strings.NewReader(got)).Decode(&v); err != nil {
		t.Fatalf("decode %q: %v", got, err)
	}
	if !slices.ContainsFunc(v.Warnings, func(w string) bool { return strings.HasPrefix(w, staleLine) }) {
		t.Fatalf("job_start warnings: %+v", v.Warnings)
	}
	restarted, err := NewServer(RoleOrchestrator, Deps{Manager: m, Watch: NewServerWatch(m)})
	if err != nil {
		t.Fatal(err)
	}
	if got := rawCall(t, connect(t, restarted), "job_status", map[string]any{"id": "j1"}); strings.Contains(got, staleLine) {
		t.Fatalf("restarted server warns: %s", got)
	}
}

// An edited config.json (a new models cell) takes effect on the next job_start without a restart; a
// config that no longer loads keeps the old one and says so.
func TestConfigReloadedBeforeCall(t *testing.T) {
	m := newManager(t)
	s, err := NewServer(RoleOrchestrator, Deps{Manager: m, Watch: NewServerWatch(m)})
	if err != nil {
		t.Fatal(err)
	}
	cs := connect(t, s)
	args := startArgs(func(a map[string]any) { delete(a, "model"); a["kind"] = "review"; a["complexity"] = "hard" })
	var v JobView
	call(t, cs, "job_start", args, &v)
	if v.Model != "fable" {
		t.Fatalf("default model: %s", v.Model)
	}
	cfg := filepath.Join(m.StateDir, "config.json")
	if err := os.WriteFile(cfg, []byte(`{"models": {"review": {"hard": "claude-opus-4-6"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	call(t, cs, "job_start", args, &v)
	if v.Model != "claude-opus-4-6" {
		t.Fatalf("config edit not applied: %s", v.Model)
	}
	if err := os.WriteFile(cfg, []byte(`{"max_running": 99}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got := rawCall(t, cs, "job_start", args)
	if !strings.Contains(got, "config.json changed but does not load") || !strings.Contains(got, `"model":"claude-opus-4-6"`) {
		t.Fatalf("broken config: %s", got)
	}
}

// system_doctor and job_start report a daemon of another version (daemon.json).
func TestDaemonVersionMismatch(t *testing.T) {
	m := newManager(t)
	m.SelfBin = selfBin
	if err := m.WriteDaemonInfo(); err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(RoleOrchestrator, Deps{Manager: m, Watch: NewServerWatch(m)})
	if err != nil {
		t.Fatal(err)
	}
	cs := connect(t, s)
	var out SystemDoctorOutput
	call(t, cs, "system_doctor", map[string]any{}, &out)
	if len(out.MCPServer.Warnings) != 0 || out.MCPServer.DaemonVersion != jobs.Version {
		t.Fatalf("same version: %+v", out.MCPServer)
	}
	b, err := os.ReadFile(filepath.Join(m.StateDir, jobs.DaemonInfoFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.StateDir, jobs.DaemonInfoFile),
		[]byte(strings.Replace(string(b), `"version": "`+jobs.Version+`"`, `"version": "9.9.9"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	got := rawCall(t, cs, "system_doctor", map[string]any{})
	var doc SystemDoctorOutput
	if err := json.NewDecoder(strings.NewReader(got)).Decode(&doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	mismatch := func(p string) bool { return strings.Contains(p, "the daemon runs agent-jobs 9.9.9") }
	if doc.MCPServer.DaemonVersion != "9.9.9" || !slices.ContainsFunc(doc.Problems, mismatch) {
		t.Fatalf("doctor: %+v %v", doc.MCPServer, doc.Problems)
	}
	var v JobView
	if err := json.NewDecoder(strings.NewReader(rawCall(t, cs, "job_start", startArgs(nil)))).Decode(&v); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(v.Warnings, mismatch) {
		t.Fatalf("job_start warnings: %v", v.Warnings)
	}
	// job_status does not compare with the daemon.
	insertRow(t, m, "j1", store.StatusSucceeded, nil, 0)
	if got := rawCall(t, cs, "job_status", map[string]any{"id": "j1"}); mismatch(got) {
		t.Fatalf("job_status: %s", got)
	}
}

// A real orchestrator server over stdio whose binary is replaced: its next result carries the stale line,
// and system_doctor (Linux) lists the process as running a deleted binary.
func TestStaleMCPProcessOverStdio(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs /proc")
	}
	home := t.TempDir()
	t.Setenv("AGENT_JOBS_HOME", home)
	bin := filepath.Join(t.TempDir(), "agent-jobs")
	copyFile(t, selfBin, bin)
	cmd := exec.Command(bin, "mcp", "--role", "orchestrator", "--agent", "test")
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil).
		Connect(context.Background(), &sdk.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	var v JobView
	call(t, cs, "job_start", startArgs(nil), &v)
	copyFile(t, selfBin, bin) // same content, new file: the running process keeps the old one
	if got := rawCall(t, cs, "job_status", map[string]any{"id": v.ID}); !strings.Contains(got, staleLine) {
		t.Fatalf("no stale line over stdio: %s", got)
	}
	m, err := jobs.Open("test")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.SelfBin = bin
	procs, ok, err := m.StaleMCPProcs()
	if err != nil || !ok {
		t.Fatalf("stale procs: %v %v", ok, err)
	}
	if !slices.ContainsFunc(procs, func(p jobs.StaleMCPProc) bool {
		return p.PID == cmd.Process.Pid && strings.Contains(p.Reason, "deleted or replaced")
	}) {
		t.Fatalf("pid %d not listed: %+v", cmd.Process.Pid, procs)
	}
}

// A call in progress (mu held shared) that asks for the config warning is not blocked by a reload
// waiting for that call to end: the reload never holds cfgMu while it waits for mu.
func TestConfigReloadDoesNotDeadlockRunningCall(t *testing.T) {
	m := newManager(t)
	w := NewServerWatch(m)
	waiting := make(chan struct{})
	w.beforeSwap = func() { close(waiting) }
	w.mu.RLock() // a running tool call
	if err := os.WriteFile(filepath.Join(m.StateDir, "config.json"), []byte(`{"max_brief_bytes": 1234}`), 0o600); err != nil {
		t.Fatal(err)
	}
	reloaded := make(chan struct{})
	go func() { w.refreshConfig(); close(reloaded) }()
	<-waiting // the reload now waits for the running call
	warned := make(chan string)
	go func() { warned <- w.ConfigWarning() }()
	select {
	case <-warned:
	case <-time.After(5 * time.Second):
		t.Fatal("ConfigWarning blocked behind a waiting reload (deadlock)")
	}
	w.mu.RUnlock()
	select {
	case <-reloaded:
	case <-time.After(5 * time.Second):
		t.Fatal("reload never finished")
	}
	if m.Cfg.MaxBriefBytes != 1234 {
		t.Fatalf("config not swapped: %d", m.Cfg.MaxBriefBytes)
	}
}

// A call that arrives while another call's reload has not swapped the config yet does not run on the
// old config: the new identity becomes visible only with the new config.
func TestConfigIdentityVisibleOnlyAfterSwap(t *testing.T) {
	m := newManager(t)
	w := NewServerWatch(m)
	w.refreshConfig() // the first check
	if err := os.WriteFile(filepath.Join(m.StateDir, "config.json"), []byte(`{"max_brief_bytes": 4321}`), 0o600); err != nil {
		t.Fatal(err)
	}
	seen := -1
	w.beforeSwap = func() {
		w.beforeSwap = nil
		// Another call, between the first reload's check and its swap.
		w.refreshConfig()
		w.mu.RLock()
		seen = m.Cfg.MaxBriefBytes
		w.mu.RUnlock()
	}
	w.refreshConfig()
	if seen != 4321 || m.Cfg.MaxBriefBytes != 4321 {
		t.Fatalf("concurrent call saw max_brief_bytes %d (now %d)", seen, m.Cfg.MaxBriefBytes)
	}
}

// config.json written after the manager loaded it but before the server's first call is applied: the
// first call reloads, whatever the file's identity when the watch was created.
func TestConfigChangedBeforeFirstCall(t *testing.T) {
	m := newManager(t)
	if err := os.WriteFile(filepath.Join(m.StateDir, "config.json"), []byte(`{"max_brief_bytes": 2345}`), 0o600); err != nil {
		t.Fatal(err)
	}
	w := NewServerWatch(m)
	w.refreshConfig()
	if m.Cfg.MaxBriefBytes != 2345 {
		t.Fatalf("config of the first call: %d", m.Cfg.MaxBriefBytes)
	}
}

// A reloaded brain block is not applied to the brain built at start, and the result says so.
func TestConfigBrainChangeWarns(t *testing.T) {
	m := newManager(t)
	w := NewServerWatch(m)
	cfg := filepath.Join(m.StateDir, "config.json")
	if err := os.WriteFile(cfg, []byte(`{"max_brief_bytes": 3456}`), 0o600); err != nil {
		t.Fatal(err)
	}
	w.refreshConfig()
	if s := w.ConfigWarning(); s != "" {
		t.Fatalf("no brain change: %q", s)
	}
	if err := os.WriteFile(cfg, []byte(`{"brain": {"recall_url": "http://127.0.0.1:9/recall", "token_env": "BRAIN_TOKEN"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	w.refreshConfig()
	if m.Cfg.Brain.RecallURL == "" {
		t.Fatalf("config not reloaded: %+v", m.Cfg.Brain)
	}
	if s := w.ConfigWarning(); !strings.Contains(s, "brain block") || !strings.Contains(s, "/mcp reconnect") {
		t.Fatalf("brain change: %q", s)
	}
}
