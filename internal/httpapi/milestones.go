package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// Milestones are served in the shapes milestone_list and milestone_get return (jobs.MilestonesView,
// jobs.MilestoneView, built by the same code). The API returns no paths outside the agent state, so repo
// is the repository's folder name here (the full path in MCP; repo_id is the same in both and is what a
// frontend filters by) and a note is a fixed message. Text fields (skill names too) are capped at MaxFieldBytes.

// noteUnreadable replaces a milestone's note on the API: the note is an error text that may hold paths.
const noteUnreadable = "the checklist or the judge verdict could not be read (see agent-jobs milestone <id>)"

// apiMilestone is v as the API returns it: the repo path cut to its folder name, a note replaced by
// noteUnreadable, every text field capped (capField).
func apiMilestone(v jobs.MilestoneView) jobs.MilestoneView {
	v.Repo = repoName(v.Repo)
	if v.Note != "" {
		v.Note = noteUnreadable
	}
	v.Title, v.Goal, v.Branch, v.ChecklistID, v.CloseNote = capField(v.Title), capField(v.Goal), capField(v.Branch),
		capField(v.ChecklistID), capField(v.CloseNote)
	v.Ready = capList(v.Ready)
	v.Uncovered = capList(v.Uncovered)
	tasks := v.Tasks
	if tasks != nil {
		v.Tasks = make([]jobs.MilestoneTaskView, len(tasks))
	}
	for i, t := range tasks {
		t.Key, t.Title, t.Status = capField(t.Key), capField(t.Title), capField(t.Status)
		t.DependsOn, t.Covers, t.Skills = capList(t.DependsOn), capList(t.Covers), capList(t.Skills)
		js := t.Jobs
		if js != nil {
			t.Jobs = make([]jobs.MilestoneJobView, len(js))
		}
		for k, j := range js {
			j.ID, j.Status, j.Outcome, j.Model = capField(j.ID), capField(j.Status), capField(j.Outcome), capField(j.Model)
			j.Skills, j.SkillsDelivered = capList(j.Skills), capList(j.SkillsDelivered)
			t.Jobs[k] = j
		}
		v.Tasks[i] = t
	}
	return v
}

// capList is capField on every item of s (nil stays nil).
func capList(s []string) []string {
	if s == nil {
		return nil
	}
	out := make([]string, len(s))
	for i, x := range s {
		out[i] = capField(x)
	}
	return out
}

// boolParam reads a 0/1/true/false query parameter (false when absent); ok is false for another value.
func boolParam(s string) (val, ok bool) {
	switch s {
	case "", "0", "false":
		return false, true
	case "1", "true":
		return true, true
	}
	return false, false
}

// milestones is /v1/milestones: the filters of milestone_list (status, repo, repo_id, checklist_id,
// updated_since) and tree=1 for each milestone's full tree. repo is a folder name, as the API returns it
// (400 when it names more than one repository: filter by repo_id then), or a full path.
func (a *API) milestones(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tree, ok := boolParam(q.Get("tree"))
	if !ok {
		writeError(w, http.StatusBadRequest, "tree must be 0 or 1")
		return
	}
	if s := q.Get("status"); s != "" && !slices.Contains(jobs.MilestoneStatuses, s) {
		writeError(w, http.StatusBadRequest, "status must be one of "+strings.Join(jobs.MilestoneStatuses, "|"))
		return
	}
	if _, err := jobs.ParseMilestoneCursor(q.Get("updated_since")); err != nil {
		writeError(w, http.StatusBadRequest, "updated_since must be RFC 3339 or Unix seconds")
		return
	}
	req := jobs.MilestoneListRequest{Status: q.Get("status"), RepoID: q.Get("repo_id"), ChecklistID: q.Get("checklist_id"),
		UpdatedSince: q.Get("updated_since"), Tree: tree}
	if repo := q.Get("repo"); strings.HasPrefix(repo, "/") {
		req.Repo = repo
	} else {
		req.RepoName = repo
	}
	out, err := a.m.ListMilestones(r.Context(), req)
	if errors.Is(err, jobs.ErrAmbiguousRepo) {
		writeError(w, http.StatusBadRequest, "repo names more than one repository: filter by repo_id")
		return
	}
	if err != nil {
		a.internalError(w, "list milestones", err)
		return
	}
	for i := range out.Milestones {
		out.Milestones[i] = apiMilestone(out.Milestones[i])
	}
	writeJSON(w, http.StatusOK, out)
}

// milestone is /v1/milestones/{id}: the tree of one milestone (milestone_get).
func (a *API) milestone(w http.ResponseWriter, r *http.Request, id string) {
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil || n <= 0 || strconv.FormatInt(n, 10) != id {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	v, err := a.m.MilestoneTree(r.Context(), n)
	if errors.Is(err, store.ErrMilestoneNotFound) {
		writeError(w, http.StatusNotFound, fmt.Sprintf("milestone %d not found", n))
		return
	}
	if err != nil {
		a.internalError(w, "get milestone", err)
		return
	}
	writeJSON(w, http.StatusOK, apiMilestone(*v))
}
