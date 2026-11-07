package mcp

import (
	"os/exec"
	"strings"
	"testing"
)

func overlapRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"-c", "user.name=T", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "init"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	return repo
}

func TestJobStartOverlapWarnAndRefuse(t *testing.T) {
	m := newManager(t)
	cs := connect(t, mustServer(t, m))
	repo := overlapRepo(t)
	args := func(policy string) map[string]any {
		return startArgs(func(a map[string]any) {
			a["kind"], a["repo"], a["paths"], a["on_overlap"] = "fix", repo, []string{"internal/*.go"}, policy
		})
	}
	var first, second map[string]any
	call(t, cs, "job_start", args("warn"), &first)
	call(t, cs, "job_start", args("warn"), &second)
	overlap, ok := second["overlap"].([]any)
	if !ok || len(overlap) != 1 {
		t.Fatalf("missing overlap: %+v", second)
	}
	item := overlap[0].(map[string]any)
	if item["id"] != first["id"] || len(item["paths"].([]any)) == 0 {
		t.Fatalf("overlap lacks job/paths: %+v", item)
	}
	msg := callErr(t, cs, "job_start", args("refuse"))
	if !strings.Contains(msg, "overlap") || !strings.Contains(msg, first["id"].(string)) {
		t.Fatalf("refusal: %s", msg)
	}
}

func TestJobStartSerializeAndParentViews(t *testing.T) {
	m := newManager(t)
	cs := connect(t, mustServer(t, m))
	repo := overlapRepo(t)
	args := func(policy string) map[string]any {
		return startArgs(func(a map[string]any) {
			a["kind"], a["repo"], a["paths"], a["on_overlap"] = "general", repo, []string{"src/**"}, policy
		})
	}
	var parent, child, stack, status, result map[string]any
	call(t, cs, "job_start", args("warn"), &parent)
	call(t, cs, "job_start", args("serialize"), &child)
	if len(child["wait_for"].([]any)) != 1 || !strings.Contains(child["waiting"].(string), parent["id"].(string)) {
		t.Fatalf("serialized queue reply: %+v", child)
	}
	stackArgs := args("warn")
	stackArgs["parent"] = parent["id"]
	call(t, cs, "job_start", stackArgs, &stack)
	call(t, cs, "job_status", map[string]any{"id": stack["id"]}, &status)
	call(t, cs, "job_result", map[string]any{"id": stack["id"]}, &result)
	if status["parent"] != parent["id"] || result["parent"] != parent["id"] || !strings.Contains(status["waiting"].(string), "parent") {
		t.Fatalf("stack views: %+v %+v", status, result)
	}
}
