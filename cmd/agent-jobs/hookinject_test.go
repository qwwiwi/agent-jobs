package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

// hookContext is the additionalContext of a PreToolUse hook's JSON output.
func hookContext(t *testing.T, out string) string {
	t.Helper()
	var v struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("hook output %q: %v", out, err)
	}
	return v.HookSpecificOutput.AdditionalContext
}

// wakeReport opens the agent's state and returns its wake report.
func wakeReport(t *testing.T, agent string) jobs.WakeStatsReport {
	t.Helper()
	m, err := jobs.Open(agent)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	rep, err := m.WakeStats(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func categoryBytes(rep jobs.WakeStatsReport, category string) (count, bytes int) {
	for _, c := range rep.Categories {
		if c.Category == category {
			return c.Count, c.Bytes
		}
	}
	return 0, 0
}

// #170 fix loop 1 (H4): the context the merge gate and the skills picker add to the model is counted,
// byte for byte what they emitted as additionalContext.
func TestHookInjectionsCountedForMergeGateAndSkillsPicker(t *testing.T) {
	t.Setenv("AGENT_JOBS_HOME", t.TempDir())
	t.Setenv("GH_REPO", "")
	repo, _ := gatedRepo(t)

	var out, errb bytes.Buffer
	code := runHook([]string{"merge-gate", "--agent", "test"}, strings.NewReader(bashEvent(repo, "sh -c 'gh pr merge 5'")), &out, &errb, open)
	if code != 0 || errb.Len() != 0 {
		t.Fatalf("merge-gate: exit %d, stderr %q", code, errb.String())
	}
	gateBytes := len(hookContext(t, out.String()))
	if gateBytes == 0 {
		t.Fatalf("no warning emitted: %q", out.String())
	}

	pick, _ := json.Marshal(map[string]any{"session_id": "s1", "hook_event_name": "PreToolUse", "cwd": "/",
		"tool_name": "mcp__agent-jobs__milestone_create", "tool_input": map[string]any{"title": "m", "tasks": []any{
			map[string]any{"key": "T1", "title": "a"}}}})
	out.Reset()
	errb.Reset()
	if code := runHook([]string{"skills-picker", "--agent", "test"}, bytes.NewReader(pick), &out, &errb, open); code != 0 || errb.Len() != 0 {
		t.Fatalf("skills-picker: exit %d, stderr %q", code, errb.String())
	}
	pickBytes := len(hookContext(t, out.String()))
	if pickBytes == 0 {
		t.Fatalf("no context emitted: %q", out.String())
	}

	rep := wakeReport(t, "test")
	if n, b := categoryBytes(rep, jobs.InjectMergeGate); n != 1 || b != gateBytes {
		t.Fatalf("merge-gate: %d records, %d bytes, emitted %d", n, b, gateBytes)
	}
	if n, b := categoryBytes(rep, jobs.InjectSkillsPicker); n != 1 || b != pickBytes {
		t.Fatalf("skills-picker: %d records, %d bytes, emitted %d", n, b, pickBytes)
	}
	if rep.InjectionBytes != gateBytes+pickBytes || rep.InjectionsUnrecorded != 0 {
		t.Fatalf("total %d, want %d: %+v", rep.InjectionBytes, gateBytes+pickBytes, rep)
	}
}

// #170 fix loop 1: a hook injection that cannot be stored is not lost silently: the hook says so on
// stderr and `stats wakes` reports the injections it could not store.
func TestHookInjectionWriteFailureIsSurfaced(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_JOBS_HOME", home)
	m, err := jobs.Open("test")
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(m.StateDir, "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER fail_inj BEFORE INSERT ON orchestrator_wakes BEGIN SELECT RAISE(ABORT, 'planted fault'); END`); err != nil {
		t.Fatal(err)
	}
	_ = m.Close()

	pick, _ := json.Marshal(map[string]any{"session_id": "s1", "hook_event_name": "PreToolUse", "cwd": "/",
		"tool_name": "mcp__agent-jobs__milestone_create", "tool_input": map[string]any{"title": "m", "tasks": []any{
			map[string]any{"key": "T1", "title": "a"}}}})
	var out, errb bytes.Buffer
	if code := runHook([]string{"skills-picker", "--agent", "test"}, bytes.NewReader(pick), &out, &errb, open); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(errb.String(), "not recorded") {
		t.Fatalf("stderr %q does not say the injection was not recorded", errb.String())
	}
	if _, err := db.Exec(`DROP TRIGGER fail_inj`); err != nil {
		t.Fatal(err)
	}
	rep := wakeReport(t, "test")
	if rep.InjectionsUnrecorded != 1 || rep.InjectionBytesUnrecorded != len(hookContext(t, out.String())) {
		t.Fatalf("report: %+v", rep)
	}
	text := captureStdout(t, func() error { return cmdStats([]string{"wakes", "--agent", "test"}) })
	if !strings.Contains(text, "incomplete: 1 hook injections") {
		t.Fatalf("stats wakes does not flag the gap:\n%s", text)
	}
}
