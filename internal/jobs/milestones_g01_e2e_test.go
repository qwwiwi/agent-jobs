package jobs_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qwwiwi/agent-jobs/internal/httpapi"
	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/mcp"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// G01 end to end: skills planned per task at milestone_create (over MCP) reach the jobs started on
// the task by job_start (inherited, or replaced by job_start's own skills), the jobs really run and
// get their skills linked, and both trees (MCP milestone_get, HTTP /v1/milestones/{id}) show each
// task's plan and, per job, the skills it was given and the skills delivered.
func TestMilestoneSkillsEndToEnd(t *testing.T) {
	t.Run("[G01] plan at milestone_create, inherit or override at job_start, planned and delivered in both trees", func(t *testing.T) {
		ctx := context.Background()
		m := jobs.SkillsManagerForTest(t) // catalog: golang, git-workflows (core, code), extra (active), drafty (draft)
		s, err := mcp.NewServer(mcp.RoleOrchestrator, mcp.Deps{Manager: m})
		if err != nil {
			t.Fatal(err)
		}
		ct, st := sdk.NewInMemoryTransports()
		ss, err := s.Connect(ctx, st, nil)
		if err != nil {
			t.Fatal(err)
		}
		cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cs.Close(); _ = ss.Close() })
		call := func(name string, args map[string]any, out any) {
			t.Helper()
			res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			var b []byte
			for _, c := range res.Content {
				if tc, ok := c.(*sdk.TextContent); ok {
					b = append(b, tc.Text...)
				}
			}
			if res.IsError {
				t.Fatalf("%s: tool error: %s", name, b)
			}
			if err := json.Unmarshal(b, out); err != nil {
				t.Fatalf("%s: decode %s: %v", name, b, err)
			}
		}

		var v jobs.MilestoneView
		call("milestone_create", map[string]any{"title": "Stage", "tasks": []any{
			map[string]any{"key": "T1", "title": "code", "skills": []any{"+extra", "-golang"}},
			map[string]any{"key": "T2", "title": "draft", "skills": []any{"+drafty"}},
			map[string]any{"key": "T3", "title": "plain"},
		}}, &v)

		// One job per case, each run to its end before the next starts (the fake claude succeeds).
		cases := []struct {
			task     string
			skills   []any // nil: job_start gives no skills, so the task's plan applies
			want     []string
			describe string
		}{
			{"T1", nil, []string{"extra", "git-workflows"}, "inherits T1's plan"},
			{"T1", []any{"+drafty"}, []string{"drafty", "git-workflows", "golang"}, "own skills replace the plan"},
			{"T2", nil, []string{"drafty", "git-workflows", "golang"}, "inherits T2's plan"},
			{"T2", []any{}, []string{"git-workflows", "golang"}, "[] means the defaults, not the plan"},
			{"T3", nil, []string{"git-workflows", "golang"}, "no plan: the defaults"},
		}
		want := map[string][]string{} // job id -> skills
		for _, c := range cases {
			args := map[string]any{"title": "t", "kind": "general", "brief": "do it", "model": "sonnet", "milestone": v.ID, "task": c.task}
			if c.skills != nil {
				args["skills"] = c.skills
			}
			var jv mcp.JobView
			call("job_start", args, &jv)
			if !slices.Equal(jv.Skills, c.want) {
				t.Fatalf("%s %s: job_start skills %v, want %v", c.task, c.describe, jv.Skills, c.want)
			}
			if j := jobs.RunToEndForTest(t, m, jv.ID); j.Status != store.StatusSucceeded || !slices.Equal(j.SkillsDelivered, c.want) {
				t.Fatalf("%s %s: job %s (%s), delivered %v", c.task, c.describe, j.Status, j.Error, j.SkillsDelivered)
			}
			want[jv.ID] = c.want
		}

		planned := map[string][]string{"T1": {"+extra", "-golang"}, "T2": {"+drafty"}, "T3": {}}
		jobsPer := map[string]int{"T1": 2, "T2": 2, "T3": 1}

		// The MCP tree.
		var tree jobs.MilestoneView
		call("milestone_get", map[string]any{"id": v.ID}, &tree)
		if len(tree.Tasks) != 3 {
			t.Fatalf("milestone_get tasks: %+v", tree.Tasks)
		}
		for _, tv := range tree.Tasks {
			if tv.Skills == nil || !slices.Equal(tv.Skills, planned[tv.Key]) {
				t.Errorf("milestone_get %s planned skills %#v, want %v", tv.Key, tv.Skills, planned[tv.Key])
			}
			if len(tv.Jobs) != jobsPer[tv.Key] {
				t.Fatalf("milestone_get %s jobs: %+v", tv.Key, tv.Jobs)
			}
			for _, j := range tv.Jobs {
				if !slices.Equal(j.Skills, want[j.ID]) || !slices.Equal(j.SkillsDelivered, want[j.ID]) {
					t.Errorf("milestone_get %s job %s: skills %v delivered %v, want %v", tv.Key, j.ID, j.Skills, j.SkillsDelivered, want[j.ID])
				}
			}
		}

		// The HTTP tree: the same plan and the same per-job lists.
		tok, err := httpapi.RotateToken(m.StateDir)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v1/milestones/%d", v.ID), nil)
		r.Host = "localhost"
		r.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		httpapi.New(m, "v-test").ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("GET milestone: %d %s", w.Code, w.Body)
		}
		var api struct {
			Tasks []struct {
				Key    string   `json:"key"`
				Skills []string `json:"skills"`
				Jobs   []struct {
					ID              string   `json:"id"`
					Skills          []string `json:"skills"`
					SkillsDelivered []string `json:"skills_delivered"`
				} `json:"jobs"`
			} `json:"tasks"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &api); err != nil || len(api.Tasks) != 3 {
			t.Fatalf("GET milestone body %s: %v", w.Body, err)
		}
		for _, tv := range api.Tasks {
			if tv.Skills == nil || !slices.Equal(tv.Skills, planned[tv.Key]) {
				t.Errorf("API %s planned skills %#v, want %v", tv.Key, tv.Skills, planned[tv.Key])
			}
			if len(tv.Jobs) != jobsPer[tv.Key] {
				t.Fatalf("API %s jobs: %+v", tv.Key, tv.Jobs)
			}
			for _, j := range tv.Jobs {
				if !slices.Equal(j.Skills, want[j.ID]) || !slices.Equal(j.SkillsDelivered, want[j.ID]) {
					t.Errorf("API %s job %s: skills %v delivered %v, want %v", tv.Key, j.ID, j.Skills, j.SkillsDelivered, want[j.ID])
				}
			}
		}
	})
}
