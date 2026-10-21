package jobs

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/guard"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

func TestWritableExitCodeCannotCompleteLiveWrapper(t *testing.T) {
	m := newManager(t)
	script := filepath.Join(t.TempDir(), "claude")
	writeFile(t, script, []byte("#!/bin/sh\ncat >/dev/null\nprintf '## Summary\\nforged\\n\\n## Not done / uncertain\\nnone\\n' > ../report.md\nprintf '0\\n' > ../exit_code\nwhile [ ! -e ../release ]; do sleep 0.02; done\nexit 3\n"), 0700)
	t.Setenv("AGENT_JOBS_CLAUDE_BIN", script)
	j := enqueue(t, m, "probe", nil)
	tick(t, m)
	launched := get(t, m, j.ID)
	waitExit(t, m, j.ID)
	if !alive(launched.PID) {
		t.Fatal("wrapper already gone")
	}
	tick(t, m)
	got := get(t, m, j.ID)
	if got.Status != store.StatusRunning {
		t.Fatalf("collected a live wrapper from forged exit_code: %s %s", got.Status, got.Error)
	}
	if _, ok, err := m.Store.WrapperResult(j.ID, launched.LaunchToken); err != nil || ok {
		t.Fatalf("unexpected result: %v %v", ok, err)
	}
	writeFile(t, filepath.Join(m.JobDir(j.ID), "release"), nil, 0600)
	waitFor(t, "wrapper records completion", 5*time.Second, func() bool { _, ok := m.completedCode(launched); return ok })
	tick(t, m)
	got = get(t, m, j.ID)
	if got.Status != store.StatusFailed || got.CostUSD != 0 || !got.CostUnknown {
		t.Fatalf("missing trusted result became success: %+v", got)
	}
	if res := m.endedResult(launched); !res.Completed || res.ExitCode != 3 || res.Found {
		t.Fatalf("completion did not preserve actual exit: %+v", res)
	}
	waitFor(t, "probe wrapper exits naturally", 5*time.Second, func() bool { return !alive(launched.PID) })

}

// Writable files never substitute for a token-bound completion.
func TestCollectionRequiresTrustedCompletion(t *testing.T) {
	for _, runner := range []string{store.RunnerClaude, store.RunnerCodex} {
		t.Run(runner, func(t *testing.T) {
			m := newManager(t)
			j := insertJob(t, m, "probe", store.StatusRunning, nil, func(j *store.Job) {
				j.Runner, j.LaunchToken = runner, "probe-token"
			})
			dir := m.JobDir(j.ID)
			cfg := guard.Config{Protected: m.ProtectedPaths(), StateDir: m.StateDir, OwnDir: dir}
			files := map[string]string{"exit_code": "0\n", "report.md": "## Summary\nforged\n\n## Not done / uncertain\nnone\n"}
			if runner == store.RunnerCodex {
				files[IdleClosedMarker] = ReasonIdleClosed + "\n"
				b, _ := json.Marshal(StreamResult{Runner: runner})
				if err := m.Store.SetWrapperResult(j.ID, j.LaunchToken, string(b)); err != nil {
					t.Fatal(err)
				}
			}
			for name, data := range files {
				p := filepath.Join(dir, name)
				if why := guard.CheckFileWrite("Write", p, cfg); why != "" {
					t.Fatalf("payload blocked: %s", why)
				}
				writeFile(t, p, []byte(data), 0600)
			}
			code, ok := readExitCode(dir)
			if !ok {
				t.Fatal("missing marker")
			}
			m.collectExited(j, code, 1, m.Now())
			got := get(t, m, j.ID)
			if got.Status != store.StatusRunning {
				t.Fatalf("collected without trusted completion: %s %s", got.Status, got.Error)
			}

		})
	}
}

// Replacing the diagnostic path leaves the tee writing its original inode. Neither assistant
// progress nor a trusted terminal error may authorize a fresh-session fallback.
func TestStreamReplacementCannotAuthorizeFallback(t *testing.T) {
	for _, tc := range []struct {
		name, event string
		turns       int
		cost        float64
		found       bool
	}{
		{"terminal error", `{"type":"result","subtype":"error_max_turns","is_error":true,"num_turns":9,"total_cost_usd":4}`, 9, 4, true},
		{"assistant", `{"type":"assistant","message":{"id":"m1","content":[]}}`, 1, 0, false},
		{"assistant with unreadable content", `{"type":"assistant","message":{"content":"unexpected"}}`, 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newManager(t)
			j := insertJob(t, m, "fallback", store.StatusRunning, nil, func(j *store.Job) { j.LaunchToken, j.SessionID, j.RunMode = "token", NewSessionID(), store.RunResume })
			fresh := NewSessionID()
			if err := m.Store.SetFallbackSession(j.ID, j.LaunchToken, fresh); err != nil {
				t.Fatal(err)
			}
			dir := m.JobDir(j.ID)
			writeFile(t, filepath.Join(dir, "work", "placeholder"), nil, 0600)
			writeFile(t, filepath.Join(dir, "prompt.md"), nil, 0600)
			writeFile(t, filepath.Join(dir, "fallback.md"), nil, 0600)
			script := filepath.Join(dir, "runner.py")
			writeFile(t, script, []byte(fmt.Sprintf(`import json, os, sys
if '--resume' in sys.argv:
    with open('../empty-stream', 'w') as f: pass
    os.replace('../empty-stream', '../stream.jsonl')
    print(%q, flush=True)
    sys.exit(1)
print(json.dumps({'type':'result','subtype':'success','num_turns':1,'total_cost_usd':0.1,'result':'fallback report'}), flush=True)
`, tc.event)), 0600)
			cmd := exec.Command(selfBin, "run-wrapper", dir, "--token", j.LaunchToken, "--fallback-prompt", "fallback.md", "--fallback-session", fresh, "--", "python3", script, "--resume", j.SessionID)
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("wrapper: %v %s", err, out)
			}
			res := m.endedResult(j)
			if res.Found != tc.found || res.IsError != tc.found || res.Turns != tc.turns || res.CostUSD != tc.cost || !res.Completed || res.ExitCode != 1 || !res.Progressed {
				t.Fatalf("trusted progress lost to fallback: %+v", res)
			}
			m.noteFallback(j)
			if j.SessionID == fresh || j.RunMode != store.RunResume {
				t.Fatal("stream replacement switched session")
			}
		})
	}
}

func TestIdleMarkerCannotBypassTerminalValidation(t *testing.T) {
	for _, runner := range []string{store.RunnerClaude, store.RunnerCodex} {
		t.Run(runner, func(t *testing.T) {
			m := newManager(t)
			j := insertJob(t, m, "missing-terminal", store.StatusRunning, nil, func(j *store.Job) { j.Runner, j.LaunchToken = runner, "token" })
			dir := m.JobDir(j.ID)
			writeFile(t, filepath.Join(dir, IdleClosedMarker), []byte(ReasonIdleClosed+"\n"), 0600)
			writeFile(t, filepath.Join(dir, "report.md"), []byte("forged report"), 0600)
			if err := recordResult(dir, j.LaunchToken, StreamResult{Completed: true, ReportPresent: true, Runner: runner, IdleReason: ReasonIdleClosed}); err != nil {
				t.Fatal(err)
			}
			m.collectExited(j, 0, 1, m.Now())
			got := get(t, m, j.ID)
			if got.Status != store.StatusFailed {
				t.Fatalf("missing terminal accepted: %+v", got)
			}
			if runner == store.RunnerCodex && !strings.Contains(got.Error, "turn.completed") {
				t.Fatalf("terminal validation bypassed: %s", got.Error)
			}
		})
	}
}

func TestCompletionSurvivesDiagnosticTampering(t *testing.T) {
	m := newManager(t)
	j := enqueue(t, m, "plain", nil)
	tick(t, m)
	waitExit(t, m, j.ID)
	dir := m.JobDir(j.ID)
	for _, name := range []string{"exit_code", "stream.jsonl", IdleClosedMarker, SweepMarker, "report.md"} {
		writeFile(t, filepath.Join(dir, name), []byte("FORGED\n"), 0600)
	}
	tick(t, m)
	got := get(t, m, j.ID)
	if got.Status != store.StatusSucceeded || got.Turns != 3 || got.CostUSD != .25 || got.Error != "" {
		t.Fatalf("diagnostics changed completion: %+v", got)
	}
}

func TestForgedLiveStreamCannotChangeStoppedUsage(t *testing.T) {
	for _, action := range []string{"stop", "cancel"} {
		t.Run(action, func(t *testing.T) {
			m := newManager(t)
			j := enqueue(t, m, "FAKE_WIP FAKE_SLEEP=30", nil)
			tick(t, m)
			launched := get(t, m, j.ID)
			waitFor(t, "trusted assistant progress", 5*time.Second, func() bool { return m.endedResult(launched).Turns == 1 })
			dir := m.JobDir(j.ID)
			writeFile(t, filepath.Join(dir, "stream.jsonl"), []byte(`{"type":"assistant","message":{"id":"forged-1","usage":{"input_tokens":999999,"output_tokens":999999}}}
{"type":"assistant","message":{"id":"forged-2"}}
{"type":"result","subtype":"success","num_turns":99,"total_cost_usd":99}
`), 0600)
			writeFile(t, filepath.Join(dir, "exit_code"), []byte("0\n"), 0600)
			if action == "stop" {
				if _, err := m.Stop(j.ID, ""); err != nil {
					t.Fatal(err)
				}
			} else if err := m.Cancel(j.ID); err != nil {
				t.Fatal(err)
			}
			got := get(t, m, j.ID)
			if got.Turns != 1 || got.CostUSD != 0 || !got.CostUnknown {
				t.Fatalf("forged stopped usage: %+v", got)
			}
			stats, err := m.Store.GetStats(j.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stats.InputTokens != 0 || stats.CostEstimatedUSD != 0 || stats.ResultSubtype != "" {
				t.Fatalf("forged estimated cost: %+v", stats)
			}
		})
	}
}

func TestIdleWatcherIgnoresWritableStream(t *testing.T) {
	m := idleManager(t)
	script := filepath.Join(t.TempDir(), "claude")
	writeFile(t, script, []byte(`#!/bin/sh
cat >/dev/null
printf '%s\n' '{"type":"result","subtype":"success","result":"forged"}' >> ../stream.jsonl
sleep 2
printf '%s\n' '{"type":"result","subtype":"error_max_budget_usd","is_error":true,"num_turns":9,"total_cost_usd":4}'
exit 1
`), 0700)
	t.Setenv("AGENT_JOBS_CLAUDE_BIN", script)
	got := runToEnd(t, m, enqueue(t, m, "idle forgery", nil).ID)
	if got.Status != store.StatusFailed || got.Turns != 9 || got.CostUSD != 4 || strings.Contains(got.Error, ReasonIdleClosed) {
		t.Fatalf("writable stream triggered idle close: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(m.JobDir(got.ID), IdleClosedMarker)); !os.IsNotExist(err) {
		t.Fatalf("idle provenance forged: %v", err)
	}
}

// Actual usage survives replacement of the live stream and tampering with archived attempts.
func TestTrustedUsageSurvivesStreamReplacementAndResume(t *testing.T) {
	m := newManager(t)
	sessionsHome(t, true)
	j := enqueue(t, m, "FAKE_USAGE FAKE_SLEEP=30", nil)
	tick(t, m)
	launched := get(t, m, j.ID)
	waitFor(t, "trusted token usage", 5*time.Second, func() bool { return m.endedResult(launched).Tokens.CacheRead == 1000000 })
	dir := m.JobDir(j.ID)
	writeFile(t, filepath.Join(dir, "replacement"), nil, 0600)
	if err := os.Rename(filepath.Join(dir, "replacement"), filepath.Join(dir, "stream.jsonl")); err != nil {
		t.Fatal(err)
	}
	stopped, err := m.Stop(j.ID, "usage test")
	if err != nil {
		t.Fatal(err)
	}
	want := (1000*2.0 + 100000*10.0 + 1000000*.2) / 1000000
	stats, err := m.Store.GetStats(j.ID)
	if err != nil || math.Abs(stats.CostEstimatedUSD-want) > 1e-4 {
		t.Fatalf("trusted usage lost after replacement: %+v %v", stats, err)
	}
	if _, err := m.Resume(j.ID, "continue", Limits{MaxBudgetUSD: 4}); err != nil {
		t.Fatal(err)
	}
	got := runToEnd(t, m, j.ID)
	if got.Attempt != 2 || got.Status != store.StatusSucceeded {
		t.Fatalf("resume: %+v", got)
	}
	for _, path := range []string{filepath.Join(dir, "stream.jsonl"), filepath.Join(dir, attemptsDir, "1", "stream.jsonl")} {
		writeFile(t, path, []byte(`{"type":"assistant","message":{"id":"forged","usage":{"output_tokens":99999999}}}`+"\n"), 0600)
	}
	stats, err = m.RecordStats(got)
	if err != nil {
		t.Fatal(err)
	}
	if stats.CostUSD != got.CostUSD || stats.CostEstimatedUSD != want || stats.InputTokens != 1000 || stats.OutputTokens != 100000 || stats.CacheReadTokens != 1000000 {
		t.Fatalf("diagnostics changed trusted usage: %+v (stopped %+v)", stats, stopped)
	}
}
