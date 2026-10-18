package mcp

import (
	"slices"
	"strings"
	"testing"
)

// The schedule tools are orchestrator-only, absent from the job role, and add / list / set / remove
// a schedule in the agent's store; a session schedule needs a tmux target, a job schedule a model.
func TestScheduleTools(t *testing.T) {
	names := []string{"schedule_add", "schedule_list", "schedule_set", "schedule_remove"}
	for _, n := range names {
		if !slices.Contains(OrchestratorOnly, n) || slices.Contains(JobAllowed, n) {
			t.Fatalf("%s must be orchestrator-only", n)
		}
		if Allowed(RoleJob, n) || !Allowed(RoleOrchestrator, n) {
			t.Fatalf("%s: wrong role access", n)
		}
	}
	job, err := NewServer(RoleJob, Deps{})
	if err != nil {
		t.Fatal(err)
	}
	jcs := connect(t, job)
	for _, n := range names {
		if msg := callErr(t, jcs, n, map[string]any{"id": 1}); msg == "" {
			t.Fatalf("job role called %s", n)
		}
	}

	m := newManager(t)
	m.Cfg.NotifyTmuxTarget = "fake-session"
	cs := connect(t, mustServer(t, m))
	var a, b ScheduleOutput
	call(t, cs, "schedule_add", map[string]any{"cron": "0 9 * * mon-fri", "prompt": "check the inbox"}, &a)
	if a.Schedule.Schedule == nil || a.Schedule.ID == 0 || a.Schedule.Cron != "0 9 * * mon-fri" || a.Schedule.Kind != "" ||
		!a.Schedule.Enabled || a.Schedule.NextFireAt == "" {
		t.Fatalf("schedule_add: %+v", a.Schedule)
	}
	call(t, cs, "schedule_add", map[string]any{"cron": "@daily", "prompt": "summarize", "kind": "research",
		"complexity": "light", "max_turns": 20}, &b)
	if b.Schedule.Kind != "research" || b.Schedule.Model == "" || b.Schedule.MaxTurns != 20 || b.Schedule.TimeoutMinutes == 0 {
		t.Fatalf("job schedule: %+v", b.Schedule)
	}
	for _, c := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"cron": "x", "prompt": "p"}, "5 fields"},
		{map[string]any{"cron": "* * * * *", "prompt": "a\nb"}, "control"},
		{map[string]any{"cron": "* * * * *", "prompt": "p", "kind": "general"}, "model or complexity"},
		{map[string]any{"cron": "* * * * *", "prompt": "p", "model": "sonnet"}, "need a kind"},
	} {
		if msg := callErr(t, cs, "schedule_add", c.args); !strings.Contains(msg, c.want) {
			t.Fatalf("schedule_add %v: %q, want %q", c.args, msg, c.want)
		}
	}
	var set ScheduleOutput
	call(t, cs, "schedule_set", map[string]any{"id": a.Schedule.ID, "enabled": false, "prompt": "read the inbox"}, &set)
	if set.Schedule.Enabled || set.Schedule.Prompt != "read the inbox" || set.Schedule.NextFireAt != "" {
		t.Fatalf("schedule_set: %+v", set.Schedule)
	}
	if msg := callErr(t, cs, "schedule_set", map[string]any{"id": a.Schedule.ID}); !strings.Contains(msg, "give a cron") {
		t.Fatalf("empty set: %q", msg)
	}
	var list SchedulesOutput
	call(t, cs, "schedule_list", map[string]any{}, &list)
	if len(list.Schedules) != 2 || list.Schedules[0].ID != a.Schedule.ID || list.Schedules[1].ID != b.Schedule.ID {
		t.Fatalf("schedule_list: %+v", list.Schedules)
	}
	var rm ScheduleRemoveOutput
	call(t, cs, "schedule_remove", map[string]any{"id": a.Schedule.ID}, &rm)
	if rm.Removed != a.Schedule.ID {
		t.Fatalf("schedule_remove: %+v", rm)
	}
	if msg := callErr(t, cs, "schedule_remove", map[string]any{"id": a.Schedule.ID}); !strings.Contains(msg, "not found") {
		t.Fatalf("remove twice: %q", msg)
	}
	call(t, cs, "schedule_list", map[string]any{}, &list)
	if len(list.Schedules) != 1 {
		t.Fatalf("after remove: %+v", list.Schedules)
	}
}
