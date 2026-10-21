package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/qwwiwi/agent-jobs/internal/brain"
)

// fakeMarker is a job's proxy for check_mark: it records the calls.
type fakeMarker struct{ calls []string }

func (f *fakeMarker) CheckMark(_ context.Context, name, note string) (brain.Object, error) {
	f.calls = append(f.calls, name+"|"+note)
	return brain.Object{"name": name}, nil
}

// Roles: a job may mark a check (through the daemon's proxy) but never add, list, set or remove one.
func TestCheckToolRoles(t *testing.T) {
	for _, n := range []string{"check_add", "check_list", "check_set", "check_remove"} {
		if !slices.Contains(OrchestratorOnly, n) || Allowed(RoleJob, n) || !Allowed(RoleOrchestrator, n) {
			t.Fatalf("%s must be orchestrator-only", n)
		}
	}
	if !slices.Contains(JobAllowed, "check_mark") || !Allowed(RoleJob, "check_mark") || !Allowed(RoleOrchestrator, "check_mark") {
		t.Fatal("check_mark must be open to jobs and the orchestrator")
	}
	fm := &fakeMarker{}
	job, err := NewServer(RoleJob, Deps{JobID: "j1", JobMarker: fm})
	if err != nil {
		t.Fatal(err)
	}
	jcs := connect(t, job)
	names := toolNames(t, jcs)
	if !slices.Contains(names, "check_mark") || slices.Contains(names, "check_add") {
		t.Fatalf("job tools: %v", names)
	}
	for _, n := range []string{"check_add", "check_list", "check_set", "check_remove"} {
		if msg := callErr(t, jcs, n, map[string]any{"name": "x", "cron": "@daily"}); !strings.Contains(msg, "not available") {
			t.Fatalf("job role called %s: %q", n, msg)
		}
	}
	var out CheckMarkOutput
	call(t, jcs, "check_mark", map[string]any{"name": "backup", "note": "ok"}, &out)
	if len(fm.calls) != 1 || fm.calls[0] != "backup|ok" || out.Check["name"] != "backup" {
		t.Fatalf("job mark: %v %v", fm.calls, out)
	}
}

// The orchestrator adds, lists, edits, marks and removes checks; a missed one shows in system_doctor.
func TestCheckToolsOrchestrator(t *testing.T) {
	m := newManager(t)
	m.SelfBin = selfBin // system_doctor's hook probe runs the real binary, not this test binary
	m.Cfg.NotifyTmuxTarget = ""
	now := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	m.Now, m.Loc = func() time.Time { return now }, time.UTC
	cs := connect(t, mustServer(t, m))
	var a CheckOutput
	call(t, cs, "check_add", map[string]any{"name": "backup", "cron": "0 10 * * *"}, &a)
	if a.Check.Check == nil || a.Check.Name != "backup" || a.Check.GraceMinutes != 60 || a.Check.State != "waiting" ||
		a.Check.NextExpectedAt != "2026-10-19T10:00:00Z" {
		t.Fatalf("check_add: %+v", a.Check)
	}
	for _, c := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"name": "Bad Name", "cron": "@daily"}, "slug"},
		{map[string]any{"name": "x", "cron": "nope"}, "5 fields"},
		{map[string]any{"name": "x", "cron": "@daily", "grace_minutes": 20000}, "outside"},
		{map[string]any{"name": "backup", "cron": "@daily"}, "already exists"},
	} {
		if msg := callErr(t, cs, "check_add", c.args); !strings.Contains(msg, c.want) {
			t.Fatalf("check_add %v: %q, want %q", c.args, msg, c.want)
		}
	}
	var set CheckOutput
	call(t, cs, "check_set", map[string]any{"name": "backup", "grace_minutes": 30}, &set)
	if set.Check.GraceMinutes != 30 {
		t.Fatalf("check_set: %+v", set.Check)
	}
	now = time.Date(2026, 10, 19, 10, 31, 0, 0, time.UTC)
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	var doc SystemDoctorOutput
	call(t, cs, "system_doctor", map[string]any{}, &doc)
	if len(doc.MissedChecks) != 1 || doc.OK || !slices.ContainsFunc(doc.Problems, func(p string) bool {
		return strings.HasPrefix(p, "check backup missed: expected 2026-10-19 10:00 UTC, no mark within 30m; alert pending (1 of 5") &&
			strings.Contains(p, "notify off")
	}) {
		t.Fatalf("doctor: %+v %v", doc.MissedChecks, doc.Problems)
	}
	var mk CheckMarkOutput
	call(t, cs, "check_mark", map[string]any{"name": "backup", "note": "done"}, &mk)
	if mk.Check["name"] != "backup" || mk.Check["last_mark_untrusted_note"] != "done" {
		t.Fatalf("check_mark: %v", mk.Check)
	}
	var list ChecksOutput
	call(t, cs, "check_list", map[string]any{}, &list)
	if len(list.Checks) != 1 || list.Checks[0].State != "missed" || list.Checks[0].LateMarkAt == nil ||
		list.Checks[0].LastMissedAt == nil || list.Checks[0].AlertState != "pending" {
		t.Fatalf("check_list: %+v", list.Checks)
	}
	var rm CheckRemoveOutput
	call(t, cs, "check_remove", map[string]any{"name": "backup"}, &rm)
	if rm.Removed != "backup" {
		t.Fatalf("check_remove: %+v", rm)
	}
	if msg := callErr(t, cs, "check_mark", map[string]any{"name": "backup"}); !strings.Contains(msg, "not found") {
		t.Fatalf("mark removed: %q", msg)
	}
}

func TestCheckToolsSchedulePolicy(t *testing.T) {
	m := newManager(t)
	cs := connect(t, mustServer(t, m))
	var result CheckOutput
	call(t, cs, "check_add", map[string]any{"name": "restricted", "cron": "@daily", "allowed_schedule_ids": []int{7, 9}}, &result)
	if !slices.Equal(result.Check.AllowedScheduleIDs, []int64{7, 9}) {
		t.Fatalf("add policy: %+v", result.Check)
	}
	call(t, cs, "check_set", map[string]any{"name": "restricted", "grace_minutes": 30}, &result)
	if !slices.Equal(result.Check.AllowedScheduleIDs, []int64{7, 9}) {
		t.Fatal("omitted list cleared policy")
	}
	call(t, cs, "check_set", map[string]any{"name": "restricted", "allowed_schedule_ids": []int{9}}, &result)
	if !slices.Equal(result.Check.AllowedScheduleIDs, []int64{9}) {
		t.Fatal("policy edit failed")
	}
	var marked CheckMarkOutput
	call(t, cs, "check_mark", map[string]any{"name": "restricted", "note": "owner done"}, &marked)
	if marked.Check["last_mark_job"] != "owner" || marked.Check["last_mark_untrusted_note"] != "owner done" {
		t.Fatalf("owner provenance: %v", marked.Check)
	}
	call(t, cs, "check_set", map[string]any{"name": "restricted", "allowed_schedule_ids": []int{}}, &result)
	if len(result.Check.AllowedScheduleIDs) != 0 {
		t.Fatal("empty list did not clear policy")
	}
	if msg := callErr(t, cs, "check_set", map[string]any{"name": "restricted", "allowed_schedule_ids": []int{-1}}); !strings.Contains(msg, "positive") {
		t.Fatalf("invalid policy: %s", msg)
	}
}

func TestCheckToolsLegacyNoteProtection(t *testing.T) {
	m := newManager(t)
	if _, err := m.AddCheck("legacy", "@daily", 0); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(m.StateDir, "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	secret := "ghp_" + strings.Repeat("D", 36)
	if _, err := db.Exec(`UPDATE checks SET last_mark_at = created_at, last_mark_note = ?, last_mark_job = 'old-job' WHERE name = 'legacy'`, "old "+secret+" "+strings.Repeat("x", 500)); err != nil {
		t.Fatal(err)
	}
	cs := connect(t, mustServer(t, m))
	// Both list and edit return the old mark and must sanitize it.
	for _, tool := range []string{"check_list", "check_set"} {
		args := map[string]any{}
		if tool == "check_set" {
			args = map[string]any{"name": "legacy", "enabled": false}
		}
		var out map[string]any
		call(t, cs, tool, args, &out)
		b, err := json.Marshal(out)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), secret) || strings.Contains(string(b), `"last_mark_note"`) {
			t.Fatalf("unsafe MCP: %s", b)
		}
		var check map[string]any
		if tool == "check_list" {
			check = out["checks"].([]any)[0].(map[string]any)
		} else {
			check = out["check"].(map[string]any)
		}
		note, _ := check["last_mark_untrusted_note"].(string)
		if !strings.Contains(note, "redacted:github") || utf8.RuneCountInString(note) != 300 || check["last_mark_job"] != "old-job" {
			t.Fatalf("legacy view: %v", check)
		}
	}
}
