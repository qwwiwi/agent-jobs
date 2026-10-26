package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

// G01: `hook skills-picker` (PreToolUse on milestone_create and job_start) asks for skills once per
// milestone: at milestone_create for every task without a skills list, never on job_start of a task;
// a plain job_start without skills is still asked. It never blocks (exit 0) and reads no state.
func TestHookSkillsPicker(t *testing.T) {
	noState := func(string) (*jobs.Manager, error) {
		t.Fatal("the skills picker opened the state")
		return nil, nil
	}
	run := func(tool string, input map[string]any) (int, string) {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"session_id": "s1", "hook_event_name": "PreToolUse", "cwd": "/",
			"tool_name": tool, "tool_input": input})
		var out, errb bytes.Buffer
		code := runHook([]string{"skills-picker"}, bytes.NewReader(b), &out, &errb, noState)
		if errb.Len() > 0 {
			t.Errorf("%s: stderr %q", tool, errb.String())
		}
		return code, out.String()
	}
	context := func(t *testing.T, out string) string {
		t.Helper()
		var v struct {
			HookSpecificOutput struct {
				HookEventName     string `json:"hookEventName"`
				AdditionalContext string `json:"additionalContext"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal([]byte(out), &v); err != nil || v.HookSpecificOutput.HookEventName != "PreToolUse" {
			t.Fatalf("hook output %q: %v", out, err)
		}
		return v.HookSpecificOutput.AdditionalContext
	}
	const create, start = "mcp__agent-jobs__milestone_create", "mcp__agent-jobs__job_start"
	t.Run("[G01] milestone_create asks once for the tasks without skills", func(t *testing.T) {
		code, out := run(create, map[string]any{"title": "m", "tasks": []any{
			map[string]any{"key": "T1", "title": "a", "skills": []any{"+golang"}},
			map[string]any{"key": "T2", "title": "b"}, map[string]any{"key": "T3", "title": "c"},
			map[string]any{"key": "T4", "title": "d", "skills": []any{}},
		}})
		msg := context(t, out)
		if code != 0 || !strings.Contains(msg, "T2, T3") || strings.Contains(msg, "T1") || strings.Contains(msg, "T4") ||
			!strings.Contains(msg, "skill_list") || !strings.Contains(msg, "milestone_set") {
			t.Fatalf("exit %d, context %q", code, msg)
		}
		if code, out := run(create, map[string]any{"title": "m", "tasks": []any{
			map[string]any{"key": "T1", "title": "a", "skills": []any{}}}}); code != 0 || out != "" {
			t.Fatalf("every task planned: exit %d, %q", code, out)
		}
	})
	t.Run("[G01] job_start on a milestone task is not asked again", func(t *testing.T) {
		if code, out := run(start, map[string]any{"title": "t", "brief": "b", "milestone": 3, "task": "T2"}); code != 0 || out != "" {
			t.Fatalf("job_start on a task: exit %d, %q", code, out)
		}
		code, out := run(start, map[string]any{"title": "t", "brief": "b"})
		if msg := context(t, out); code != 0 || !strings.Contains(msg, "skill_list") {
			t.Fatalf("plain job_start: exit %d, %q", code, msg)
		}
		for name, in := range map[string]map[string]any{
			"skills given": {"title": "t", "brief": "b", "skills": []any{}},
			"brief none":   {"title": "t", "brief": "b\nskills: none"},
		} {
			if code, out := run(start, in); code != 0 || out != "" {
				t.Errorf("%s: exit %d, %q", name, code, out)
			}
		}
		if code, out := run("Bash", map[string]any{"command": "ls"}); code != 0 || out != "" {
			t.Fatalf("another tool: exit %d, %q", code, out)
		}
	})
	t.Run("[G01] bad input and a job session stay silent", func(t *testing.T) {
		var out, errb bytes.Buffer
		if code := runHook([]string{"skills-picker"}, strings.NewReader("{"), &out, &errb, noState); code != 0 || out.Len() > 0 ||
			!strings.Contains(errb.String(), "agent-jobs hook skills-picker:") {
			t.Fatalf("bad input: %d %q %q", code, out.String(), errb.String())
		}
		if !silentInsideJob("hook", []string{"skills-picker"}) || silentInsideJob("hook", []string{"bind"}) {
			t.Fatal("inside a job the skills picker must end silently, and only it and the other soft hooks")
		}
	})
}
