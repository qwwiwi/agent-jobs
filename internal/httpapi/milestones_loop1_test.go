package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

// A repository's identity round-trips over the API: every milestone carries repo_id (the same in MCP),
// the list filters by repo_id, and by a folder name when it names one repository (400 when it names
// two); MCP keeps taking the absolute path.
func TestMilestonesRepoIDRoundTrip(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	root := t.TempDir()
	newRepo := func(parts ...string) string {
		t.Helper()
		p := filepath.Join(append([]string{root}, parts...)...)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("git", "init", "-q", p).CombinedOutput(); err != nil {
			t.Fatalf("git init: %v %s", err, out)
		}
		return p
	}
	a, b, c := newRepo("a", "proj"), newRepo("b", "proj"), newRepo("solo")
	ids := map[string]int64{}
	for _, repo := range []string{a, b, c} {
		v, err := f.m.CreateMilestone(ctx, jobs.MilestoneCreate{Title: "m " + repo, Repo: repo, Tasks: []jobs.MilestoneTaskInput{{Key: "T1", Title: "x"}}})
		if err != nil {
			t.Fatal(err)
		}
		ids[repo] = v.ID
	}
	one := f.get(t, fmt.Sprintf("/v1/milestones/%d", ids[a]), http.StatusOK)
	repoID, _ := one["repo_id"].(string)
	if repoID == "" || strings.Contains(repoID, "/") {
		t.Fatalf("milestone without an opaque repo_id: %v", one)
	}
	if mcpID := mcpCall(t, f.m, "milestone_get", map[string]any{"id": ids[a]}).(map[string]any)["repo_id"]; mcpID != repoID {
		t.Fatalf("repo_id over MCP %v, over HTTP %v", mcpID, repoID)
	}
	only := func(path string, want int64) {
		t.Helper()
		list := f.get(t, path, http.StatusOK)["milestones"].([]any)
		if len(list) != 1 || int64(list[0].(map[string]any)["id"].(float64)) != want {
			t.Fatalf("GET %s: %v, want milestone %d", path, list, want)
		}
	}
	only("/v1/milestones?repo_id="+repoID, ids[a])
	only("/v1/milestones?repo="+filepath.Base(c), ids[c])
	only("/v1/milestones?repo="+a, ids[a])
	f.get(t, "/v1/milestones?repo=proj", http.StatusBadRequest)
	if list := f.get(t, "/v1/milestones?repo_id=000000000000", http.StatusOK)["milestones"].([]any); len(list) != 0 {
		t.Fatalf("unknown repo_id: %v", list)
	}
	if list := mcpCall(t, f.m, "milestone_list", map[string]any{"repo": b}).(map[string]any)["milestones"].([]any); len(list) != 1 {
		t.Fatalf("MCP by path: %v", list)
	}
}

// The API caps a milestone's text fields at MaxFieldBytes: titles, goal, close note, task titles, covers.
func TestMilestoneFieldsCapped(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	long := strings.Repeat("я", 200) // 200 characters, 400 bytes
	cover := strings.Repeat("Ж", 40)
	v, err := f.m.CreateMilestone(ctx, jobs.MilestoneCreate{Title: long, Goal: long, Tasks: []jobs.MilestoneTaskInput{
		{Key: "T1", Title: long, Covers: &[]string{cover}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.SetMilestone(ctx, jobs.MilestoneSet{ID: v.ID, CloseNote: long}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{fmt.Sprintf("/v1/milestones/%d", v.ID), "/v1/milestones?tree=1"} {
		body := f.body(t, path)
		if strings.Contains(body, long) {
			t.Errorf("GET %s returns a field longer than %d bytes", path, MaxFieldBytes)
		}
	}
	got := f.get(t, fmt.Sprintf("/v1/milestones/%d", v.ID), http.StatusOK)
	task := got["tasks"].([]any)[0].(map[string]any)
	for name, s := range map[string]string{"title": got["title"].(string), "goal": got["goal"].(string),
		"close_note": got["close_note"].(string), "task title": task["title"].(string)} {
		if len(s) > MaxFieldBytes || len(s) == 0 {
			t.Errorf("%s: %d bytes, want 1..%d", name, len(s), MaxFieldBytes)
		}
	}
}
