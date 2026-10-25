package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/mcp"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// mcpCall calls an orchestrator tool of m over an in-memory MCP session and returns its JSON result,
// decoded as generic JSON.
func mcpCall(t *testing.T, m *jobs.Manager, name string, args map[string]any) any {
	t.Helper()
	s, err := mcp.NewServer(mcp.RoleOrchestrator, mcp.Deps{Manager: m})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ct, st := sdk.NewInMemoryTransports()
	ss, err := s.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil || res.IsError {
		t.Fatalf("%s: %v %+v", name, err, res)
	}
	var out any
	if err := json.Unmarshal([]byte(res.Content[0].(*sdk.TextContent).Text), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// withRepoName sets every milestone's repo of an MCP result to its folder name, the API's one difference.
func withRepoName(v any) any {
	switch x := v.(type) {
	case map[string]any:
		if r, ok := x["repo"].(string); ok {
			x["repo"] = filepath.Base(r)
		}
		if list, ok := x["milestones"].([]any); ok {
			for _, ms := range list {
				withRepoName(ms)
			}
		}
	}
	return v
}

// /v1/milestones and /v1/milestones/{id} answer the same JSON as milestone_list and milestone_get (repo
// as its folder name), with the filters, tree and the cursor; bad filters are 400, unknown ids 404.
func TestMilestonesSameJSONAsMCP(t *testing.T) {
	f := newFixture(t)
	repo := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	ctx := context.Background()
	v, err := f.m.CreateMilestone(ctx, jobs.MilestoneCreate{Title: "Stage", Repo: repo, Branch: "integ", Tasks: []jobs.MilestoneTaskInput{
		{Key: "T1", Title: "one"}, {Key: "T2", Title: "two", DependsOn: &[]string{"T1"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.CreateMilestone(ctx, jobs.MilestoneCreate{Title: "Other", Tasks: []jobs.MilestoneTaskInput{{Key: "T1", Title: "x"}}}); err != nil {
		t.Fatal(err)
	}
	f.insert(t, "job-1", store.StatusSucceeded) // a job of no milestone
	if err := f.m.Store.Insert(&store.Job{ID: "job-2", Agent: "test", Title: "t", Kind: "fix", Brief: "b", Status: store.StatusQueued,
		Model: "opus", MaxTurns: 1, MaxBudgetUSD: 1, TimeoutMinutes: 1, CreatedAt: time.Now(), MilestoneID: v.ID, TaskKey: "T1"}); err != nil {
		t.Fatal(err)
	}
	same := func(path string, tool string, args map[string]any) map[string]any {
		t.Helper()
		got := f.get(t, path, http.StatusOK)
		want := withRepoName(mcpCall(t, f.m, tool, args))
		if !reflect.DeepEqual(any(got), want) {
			a, _ := json.MarshalIndent(got, "", " ")
			b, _ := json.MarshalIndent(want, "", " ")
			t.Fatalf("GET %s differs from %s:\n%s\nwant:\n%s", path, tool, a, b)
		}
		return got
	}
	one := same(fmt.Sprintf("/v1/milestones/%d", v.ID), "milestone_get", map[string]any{"id": v.ID})
	if one["repo"] != filepath.Base(repo) || strings.Contains(fmt.Sprint(one), repo) {
		t.Fatalf("the API returned the repo path: %v", one)
	}
	tasks := one["tasks"].([]any)
	if t1 := tasks[0].(map[string]any); t1["status"] != jobs.TaskRunning || len(t1["jobs"].([]any)) != 1 {
		t.Fatalf("task T1: %v", t1)
	}
	list := same("/v1/milestones?tree=1", "milestone_list", map[string]any{"tree": true})
	if n := len(list["milestones"].([]any)); n != 2 {
		t.Fatalf("list: %d milestones", n)
	}
	same("/v1/milestones?status=open&repo="+repo, "milestone_list", map[string]any{"status": "open", "repo": repo})
	since := time.Now().Add(time.Hour).Unix()
	if got := f.get(t, fmt.Sprintf("/v1/milestones?updated_since=%d", since), http.StatusOK); len(got["milestones"].([]any)) != 0 {
		t.Fatalf("future cursor: %v", got)
	}
	f.get(t, "/v1/milestones?status=closed", http.StatusBadRequest)
	f.get(t, "/v1/milestones?updated_since=soon", http.StatusBadRequest)
	f.get(t, "/v1/milestones?tree=yes", http.StatusBadRequest)
	f.get(t, "/v1/milestones/999", http.StatusNotFound)
	f.get(t, "/v1/milestones/abc", http.StatusNotFound)
	if res, _ := f.do(t, http.MethodGet, "/v1/milestones", ""); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("without token: %d", res.StatusCode)
	}
}
