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
// jobs.MilestoneView, built by the same code); the one difference is repo, which is the folder name of
// the repository here (the API returns no paths outside the agent state), the full path in MCP.

// apiMilestone is v as the API returns it: the repo path cut to its folder name.
func apiMilestone(v jobs.MilestoneView) jobs.MilestoneView {
	v.Repo = repoName(v.Repo)
	return v
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

// milestones is /v1/milestones: the filters of milestone_list (status, repo, checklist_id,
// updated_since) and tree=1 for each milestone's full tree.
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
	out, err := a.m.ListMilestones(r.Context(), jobs.MilestoneListRequest{Status: q.Get("status"), Repo: q.Get("repo"),
		ChecklistID: q.Get("checklist_id"), UpdatedSince: q.Get("updated_since"), Tree: tree})
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
