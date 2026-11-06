package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// Fix loop 1 of #166. Principle: a persistence or delivery failure causes a duplicate reminder, never a
// lost one.

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

func (h *pendingOutcomeHarness) stateDir() string {
	h.t.Helper()
	dir, err := jobs.StateDir("test")
	if err != nil {
		h.t.Fatal(err)
	}
	return dir
}

// A failed epoch reset (the state file cannot be written) must not suppress the snapshot after a compaction.
func TestHookBindFailedEpochResetEmitsSnapshot(t *testing.T) {
	h := newPendingOutcomeHarness(t)
	h.finish("a1", "first")
	if out := h.bind(); !strings.Contains(out, "a1 first") {
		t.Fatalf("first SessionStart: %q", out)
	}
	// Make every write of the state fail: the temp file path is a directory.
	if err := os.Mkdir(filepath.Join(h.stateDir(), "hook-emit.json.tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	if code := runHook([]string{"bind", "--agent", "test"}, strings.NewReader(bindEvent("s1", h.orch)), &out, &errs, h.open); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errs.String())
	}
	if !strings.Contains(out.String(), "a1 first") {
		t.Errorf("SessionStart after a compaction with an unwritable state: %q, want the snapshot", out.String())
	}
}

// Loop 2: a failed epoch reset followed by a failed stdout write must not leave the old suppression behind
// for the next process (the force flag lives in one manager only).
func TestHookBindFailedEpochAndFailedStdoutStillEmitsNext(t *testing.T) {
	h := newPendingOutcomeHarness(t)
	h.finish("a1", "first")
	if out := h.bind(); !strings.Contains(out, "a1 first") {
		t.Fatalf("first SessionStart: %q", out)
	}
	if err := os.Mkdir(filepath.Join(h.stateDir(), "hook-emit.json.tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	var errs bytes.Buffer
	if code := runHook([]string{"bind", "--agent", "test"}, strings.NewReader(bindEvent("s1", h.orch)), failingWriter{}, &errs, h.open); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errs.String())
	}
	// A fresh process (new manager, no in-memory flag) must emit the post-compaction snapshot.
	// The state is still unwritable, so the emission cannot be recorded (stderr says so); stdout matters.
	var out bytes.Buffer
	errs.Reset()
	in := `{"session_id":"s1","hook_event_name":"UserPromptSubmit","prompt":"hello"}`
	if code := runHook([]string{"brief-reminder", "--agent", "test"}, strings.NewReader(in), &out, &errs, h.open); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errs.String())
	}
	if !strings.Contains(out.String(), "a1 first") {
		t.Errorf("next hook after a failed epoch reset and a failed stdout: %q, want the snapshot", out.String())
	}
}

// A reminder the hook could not write to stdout is not recorded as emitted.
func TestHookPendingOutcomeStdoutFailureIsNotRecorded(t *testing.T) {
	t.Run("brief-reminder", func(t *testing.T) {
		h := newPendingOutcomeHarness(t)
		h.finish("a1", "first")
		h.bind()
		h.run("brief-reminder", `{"session_id":"s1","hook_event_name":"UserPromptSubmit","prompt":"x"}`)
		h.finish("a2", "second")
		var errs bytes.Buffer
		in := `{"session_id":"s1","hook_event_name":"UserPromptSubmit","prompt":"x"}`
		if code := runHook([]string{"brief-reminder", "--agent", "test"}, strings.NewReader(in), failingWriter{}, &errs, h.open); code != 0 {
			t.Fatalf("exit %d", code)
		}
		if out := h.prompt(); !strings.Contains(out, "a2 second") {
			t.Errorf("next prompt after a failed stdout: %q, want the reminder", out)
		}
	})
	t.Run("bind", func(t *testing.T) {
		h := newPendingOutcomeHarness(t)
		h.finish("a1", "first")
		var errs bytes.Buffer
		if code := runHook([]string{"bind", "--agent", "test"}, strings.NewReader(bindEvent("s1", h.orch)), failingWriter{}, &errs, h.open); code != 0 {
			t.Fatalf("exit %d", code)
		}
		if out := h.prompt(); !strings.Contains(out, "a1 first") {
			t.Errorf("prompt after a failed SessionStart stdout: %q, want the reminder", out)
		}
	})
}

// A worker that misses the two-second answer must not record an emission nobody saw.
func TestHookBriefReminderTimeoutDoesNotRecordEmission(t *testing.T) {
	h := newPendingOutcomeHarness(t)
	h.finish("a1", "first")
	h.bind()
	h.finish("a2", "second")
	old := briefReminderTimeout
	briefReminderTimeout = 50 * time.Millisecond
	t.Cleanup(func() { briefReminderTimeout = old })
	oldOpener := briefReminderOpener
	briefReminderOpener = func(agent string) (*jobs.Manager, error) {
		time.Sleep(300 * time.Millisecond)
		return oldOpener(agent)
	}
	t.Cleanup(func() { briefReminderOpener = oldOpener })
	slow := h.open
	var out, errs bytes.Buffer
	in := `{"session_id":"s1","hook_event_name":"UserPromptSubmit","prompt":"x"}`
	if code := runHook([]string{"brief-reminder", "--agent", "test"}, strings.NewReader(in), &out, &errs, slow); code != 0 || out.Len() != 0 {
		t.Fatalf("timed-out run: exit %d, stdout %q", code, out.String())
	}
	time.Sleep(600 * time.Millisecond) // the late worker finishes
	briefReminderOpener, briefReminderTimeout = oldOpener, old
	if got := h.prompt(); !strings.Contains(got, "a2 second") {
		t.Errorf("prompt after a timed-out worker: %q, want the reminder", got)
	}
}

// failed -> resume -> failed again between two hook runs is a new obligation, not the old one.
func TestHookPendingOutcomeResumedAttemptEmitsAgain(t *testing.T) {
	h := newPendingOutcomeHarness(t)
	finished := h.now.Add(-time.Minute)
	if err := h.m.Store.Insert(&store.Job{ID: "r1", Agent: "test", Title: "flaky", Kind: "fix", Brief: "b",
		Status: store.StatusFailed, Model: "sonnet", CreatedAt: finished, FinishedAt: &finished, NotifiedAt: &finished, Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	if out := h.bind(); !strings.Contains(out, "r1 flaky") {
		t.Fatalf("SessionStart: %q", out)
	}
	if out := h.prompt(); out != "" {
		t.Fatalf("unchanged: %q", out)
	}
	// The resume (attempt 2) fails again before another hook run observed the intervening state.
	db, err := sql.Open("sqlite", filepath.Join(h.stateDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE jobs SET attempt = 2, finished_at = finished_at + 30 WHERE id = 'r1'`); err != nil {
		t.Fatal(err)
	}
	if out := h.prompt(); !strings.Contains(out, "r1 flaky") {
		t.Errorf("second failure of a resumed job: %q, want the reminder", out)
	}
}

// Every injected block obeys hooks.max_bytes, the skills picker too.
func TestHookSkillsPickerHonorsMaxBytes(t *testing.T) {
	bindableAgent(t, "test")
	writeAgentConfig(t, "test", map[string]any{"hooks": map[string]any{"max_bytes": 512}})
	var tasks []any
	for i := range 20 {
		tasks = append(tasks, map[string]any{"key": fmt.Sprintf("%032d", i), "title": "t"})
	}
	in := fmt.Sprintf(`{"session_id":"s1","hook_event_name":"PreToolUse","cwd":"/","tool_name":"mcp__agent-jobs__milestone_create",`+
		`"tool_input":{"title":"m","tasks":%s}}`, mustJSON(t, tasks))
	var out, errb bytes.Buffer
	noState := func(string) (*jobs.Manager, error) { t.Fatal("opened the state"); return nil, nil }
	if code := runHook([]string{"skills-picker", "--agent", "test"}, strings.NewReader(in), &out, &errb, noState); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	var v struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &v); err != nil {
		t.Fatalf("output %q: %v", out.String(), err)
	}
	msg := v.HookSpecificOutput.AdditionalContext
	if len(msg) > 512 || !strings.Contains(msg, "more") || !strings.Contains(msg, "milestone_set") {
		t.Errorf("picker context is %d bytes (cap 512), want a capped context with an omission count and the pointer: %q", len(msg), msg)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
