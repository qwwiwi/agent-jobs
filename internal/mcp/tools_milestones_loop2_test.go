package mcp

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

// A checklist_set that writes the files and then fails to store the approval still moves the cursor of
// the milestones tied to the checklist: their rows follow the files, which changed.
func TestChecklistSetFailedApprovalMovesMilestoneCursor(t *testing.T) {
	m := newManager(t)
	repo, _ := gitRepo(t)
	cs := connect(t, mustServer(t, m))
	var cv ChecklistView
	call(t, cs, "checklist_init", map[string]any{"repo": repo, "brief": "I want a login page."}, &cv)
	call(t, cs, "checklist_set", map[string]any{"repo": repo, "rows": []map[string]any{
		{"id": "R01", "verbatim": "a login page", "contract": "live", "rationale": "brief"}}}, &cv)
	storeSlicing(t, m, repo)
	var ms jobs.MilestoneView
	call(t, cs, "milestone_create", map[string]any{"title": "m", "repo": repo, "checklist_id": cv.ID,
		"tasks": []any{map[string]any{"key": "T1", "title": "login", "covers": []any{"R01"}}}}, &ms)
	db, err := sql.Open("sqlite", filepath.Join(m.StateDir, "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// The milestone's last change is long past; the approval write fails.
	if _, err := db.Exec(`UPDATE milestones SET updated_at = 1000 WHERE id = ?`, ms.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER approval_fail BEFORE INSERT ON checklist_approvals
		BEGIN SELECT RAISE(ABORT, 'simulated write failure'); END`); err != nil {
		t.Fatal(err)
	}
	msg := callErr(t, cs, "checklist_set", map[string]any{"repo": repo, "approve": true, "approve_quote": "yes"})
	if !strings.Contains(msg, "files written") || !strings.Contains(msg, "simulated write failure") {
		t.Fatalf("checklist_set with a failing approval: %s", msg)
	}
	var list jobs.MilestonesView
	call(t, cs, "milestone_list", map[string]any{"updated_since": "1001"}, &list)
	if len(list.Milestones) != 1 || list.Milestones[0].ID != ms.ID {
		t.Fatalf("milestone_list since before the failed checklist_set: %+v", list)
	}
}

// job_status and job_list show why a queued job of a task waits; milestone_get shows it on the task.
func TestJobViewShowsMilestoneWaiting(t *testing.T) {
	m := newManager(t)
	cs := connect(t, mustServer(t, m))
	var v jobs.MilestoneView
	call(t, cs, "milestone_create", map[string]any{"title": "Stage", "tasks": []any{
		map[string]any{"key": "T1", "title": "one"}, map[string]any{"key": "T2", "title": "two"}}}, &v)
	var jv JobView
	call(t, cs, "job_start", startArgs(func(a map[string]any) { a["milestone"], a["task"] = v.ID, "T2" }), &jv)
	if jv.Waiting != "" {
		t.Fatalf("job_start of a ready task: waiting %q", jv.Waiting)
	}
	call(t, cs, "milestone_set", map[string]any{"id": v.ID, "tasks": []any{map[string]any{"key": "T2", "depends_on": []any{"T1"}}}}, &v)
	want := "waiting for T1 (ready)"
	call(t, cs, "job_status", map[string]any{"id": jv.ID}, &jv)
	if jv.Waiting != want {
		t.Errorf("job_status: waiting %q, want %q", jv.Waiting, want)
	}
	var list JobListOutput
	call(t, cs, "job_list", map[string]any{}, &list)
	if len(list.Jobs) != 1 || list.Jobs[0].Waiting != want {
		t.Errorf("job_list: %+v", list.Jobs)
	}
	call(t, cs, "milestone_get", map[string]any{"id": v.ID}, &v)
	if v.Tasks[1].Waiting != want {
		t.Errorf("milestone_get task T2: %+v", v.Tasks[1])
	}
}
