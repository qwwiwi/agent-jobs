package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

// catalogRoot writes a skill catalog root: name -> [status, kind, body sentence].
func catalogRoot(t *testing.T, entries map[string][3]string) string {
	t.Helper()
	root := t.TempDir()
	var idx []map[string]any
	for name, e := range entries {
		dir := filepath.Join(root, "s", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := "---\nname: " + name + "\ndescription: " + e[2] + "\n---\nbody\n"
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		idx = append(idx, map[string]any{"name": name, "path": "s/" + name, "status": e[0], "kinds": []string{e[1]},
			"last_verified": time.Now().Format("2006-01-02"), "risk": []string{"network"}})
	}
	b, _ := json.Marshal(map[string]any{"version": 1, "skills": idx})
	if err := os.WriteFile(filepath.Join(root, "index.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestSkillListAndJobStartSkills(t *testing.T) {
	m := newManager(t)
	m.Cfg.SkillRoots = []string{catalogRoot(t, map[string][3]string{
		"golang":    {"core", "code", "Go code conventions. Second sentence."},
		"reviewing": {"core", "review", "Review checklist for diffs."},
		"legacy":    {"deprecated", "code", "Old Go helpers."},
		"sketch":    {"draft", "code", "Go experiments."},
	})}
	cs := connect(t, mustServer(t, m))
	var out jobs.SkillListOutput
	call(t, cs, "skill_list", map[string]any{"kind": "fix"}, &out)
	if len(out.Skills) != 1 || out.Skills[0].Name != "golang" || out.Skills[0].Description != "Go code conventions." ||
		!slices.Equal(out.Skills[0].Risk, []string{"network"}) || out.Skills[0].Stale {
		t.Fatalf("list fix: %+v", out)
	}
	call(t, cs, "skill_list", map[string]any{"query": "go"}, &out)
	if len(out.Skills) != 1 || out.Skills[0].Name != "golang" || out.Skills[0].Score <= 0 {
		t.Fatalf("search go (no draft, no deprecated): %+v", out.Skills)
	}
	call(t, cs, "skill_list", map[string]any{"query": "go", "status": "draft"}, &out)
	if len(out.Skills) != 1 || out.Skills[0].Name != "sketch" {
		t.Fatalf("search draft: %+v", out.Skills)
	}
	if msg := callErr(t, cs, "skill_list", map[string]any{"status": "retired"}); !strings.Contains(msg, "status") {
		t.Fatalf("bad status: %s", msg)
	}
	// No paths in the output.
	res := text(mustCall(t, cs, "skill_list", map[string]any{}))
	if strings.Contains(res, m.Cfg.SkillRoots[0]) {
		t.Fatalf("skill_list leaks a path: %s", res)
	}
	var v JobView
	call(t, cs, "job_start", startArgs(func(a map[string]any) { a["kind"] = "fix"; a["skills"] = []string{"+reviewing"} }), &v)
	if !slices.Equal(v.Skills, []string{"golang", "reviewing"}) {
		t.Fatalf("job_start skills %v", v.Skills)
	}
	for skill, want := range map[string]string{"legacy": "deprecated", "golnag": "close: golang"} {
		if msg := callErr(t, cs, "job_start", startArgs(func(a map[string]any) { a["skills"] = []string{skill} })); !strings.Contains(msg, want) {
			t.Fatalf("%s: %s", skill, msg)
		}
	}
	call(t, cs, "job_status", map[string]any{"id": v.ID}, &v)
	if !slices.Equal(v.Skills, []string{"golang", "reviewing"}) {
		t.Fatalf("job_status skills %v", v.Skills)
	}
}

func TestSkillListNotForJobs(t *testing.T) {
	if Allowed(RoleJob, "skill_list") || !Allowed(RoleOrchestrator, "skill_list") {
		t.Fatal("skill_list must be orchestrator only")
	}
}

func TestJobStartNoSkills(t *testing.T) {
	m := newManager(t)
	m.Cfg.SkillRoots = []string{catalogRoot(t, map[string][3]string{
		"introspection": {"core", "code", "Inspect the agent."},
	})}
	cs := connect(t, mustServer(t, m))
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"remove defaults", func(a map[string]any) { a["skills"] = []string{"-introspection"} }},
		{"brief none", func(a map[string]any) { a["brief"] = "Do the work.\nskills: none" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var v JobView
			call(t, cs, "job_start", startArgs(tc.change), &v)
			j, err := m.Store.Get(v.ID)
			if err != nil || len(j.Skills) != 0 {
				t.Fatalf("job_start: %+v %v", j, err)
			}
		})
	}
}

func mustCall(t *testing.T, cs *sdk.ClientSession, name string, args map[string]any) *sdk.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil || res.IsError {
		t.Fatalf("%s: %v %v", name, err, res)
	}
	return res
}
