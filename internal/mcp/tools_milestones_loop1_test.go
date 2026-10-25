package mcp

import (
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

// checklist_set writing a milestone's checklist moves the milestone's updated_at (its uncovered rows
// follow the file): a poller whose cursor has passed the milestone's last change still gets it.
func TestChecklistSetMovesMilestoneCursor(t *testing.T) {
	m := newManager(t)
	repo, _ := gitRepo(t)
	cs := connect(t, mustServer(t, m))
	var cv ChecklistView
	call(t, cs, "checklist_init", map[string]any{"repo": repo, "brief": "I want a login page."}, &cv)
	row := func(id string) map[string]any {
		return map[string]any{"id": id, "verbatim": "words " + id, "contract": "live", "rationale": "brief"}
	}
	call(t, cs, "checklist_set", map[string]any{"repo": repo, "rows": []map[string]any{row("R01"), row("R02")}}, &cv)
	var ms jobs.MilestoneView
	call(t, cs, "milestone_create", map[string]any{"title": "m", "repo": repo, "checklist_id": cv.ID,
		"tasks": []any{map[string]any{"key": "T1", "title": "login", "covers": []any{"R01"}}}}, &ms)
	at, err := time.Parse(time.RFC3339, ms.UpdatedAt)
	if err != nil {
		t.Fatal(err)
	}
	cursor := at.Add(time.Second).UTC().Format(time.RFC3339) // past the creation
	time.Sleep(time.Until(at.Add(time.Second)))
	call(t, cs, "checklist_set", map[string]any{"repo": repo, "rows": []map[string]any{row("R01"), row("R02"), row("R03")}}, &cv)
	var list jobs.MilestonesView
	call(t, cs, "milestone_list", map[string]any{"updated_since": cursor, "tree": true}, &list)
	if len(list.Milestones) != 1 || len(list.Milestones[0].Uncovered) != 2 {
		t.Fatalf("milestone_list since %s after checklist_set: %+v", cursor, list)
	}
}
