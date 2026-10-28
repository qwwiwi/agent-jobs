package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

// treeJSON is the milestone tree as milestone_get returns it (the manager call behind the MCP tool).
func treeJSON(t *testing.T, id int64) string {
	t.Helper()
	m, err := jobs.Open("test")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	v, err := m.MilestoneTree(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

func decodeTree(t *testing.T, out string) jobs.MilestoneView {
	t.Helper()
	var v jobs.MilestoneView
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("not a milestone tree: %v\n%s", err, out)
	}
	return v
}

// `agent-jobs milestone create` and `milestone set` are milestone_create and milestone_set on the command
// line: the same manager calls (same validation, same errors) and the same JSON tree on stdout.
func TestMilestoneCLICreateSet(t *testing.T) {
	t.Run("[R01] CLI create from a JSON file round trips through milestone get and milestones", func(t *testing.T) {
		t.Setenv("AGENT_JOBS_HOME", t.TempDir())
		input := filepath.Join(t.TempDir(), "milestone.json")
		if err := os.WriteFile(input, []byte(`{"title": "from the file", "goal": "ship it", "max_parallel": 1,
			"tasks": [{"key": "T1", "title": "one"}, {"key": "T2", "title": "two", "depends_on": ["T1"], "covers": ["R01"]}]}`), 0o644); err != nil {
			t.Fatal(err)
		}
		out := captureStdout(t, func() error {
			return cmdMilestoneEdit("create", []string{"--agent", "test", "--input", input, "--title", "Stage one"}, nil)
		})
		v := decodeTree(t, out)
		if v.ID <= 0 || v.Title != "Stage one" || v.Goal != "ship it" || v.MaxParallel == nil || *v.MaxParallel != 1 || len(v.Tasks) != 2 ||
			v.Tasks[1].Key != "T2" || strings.Join(v.Tasks[1].DependsOn, ",") != "T1" || v.Tasks[1].Status != "blocked" ||
			strings.Join(v.Ready, ",") != "T1" {
			t.Fatalf("created tree:\n%s", out)
		}
		if want := treeJSON(t, v.ID); out != want {
			t.Fatalf("CLI create output is not the milestone_get tree:\n%s\nwant\n%s", out, want)
		}
		if got := captureStdout(t, func() error { return cmdMilestone([]string{"--agent", "test", "--json", strconv.FormatInt(v.ID, 10)}) }); got != out {
			t.Fatalf("milestone <id> --json differs from the create output:\n%s", got)
		}
		list := captureStdout(t, func() error { return cmdMilestones([]string{"--agent", "test", "--json"}) })
		if !strings.Contains(list, `"title": "Stage one"`) {
			t.Fatalf("milestones --json lacks the new milestone:\n%s", list)
		}

		// Only the tasks array, on stdin, with the title as a flag.
		out = captureStdout(t, func() error {
			return cmdMilestoneEdit("create", []string{"--agent", "test", "--input", "-", "--title", "From stdin"},
				strings.NewReader(`[{"key": "T1", "title": "only"}]`))
		})
		if v2 := decodeTree(t, out); v2.Title != "From stdin" || len(v2.Tasks) != 1 || v2.ID == v.ID {
			t.Fatalf("tasks array on stdin:\n%s", out)
		}

		// The manager's validation, word for word: a cycle, no tasks, an unknown field, no input.
		cycle := jobs.MilestoneCreate{Title: "loop", Tasks: []jobs.MilestoneTaskInput{
			{Key: "T1", Title: "a", DependsOn: &[]string{"T2"}}, {Key: "T2", Title: "b", DependsOn: &[]string{"T1"}}}}
		m, err := jobs.Open("test")
		if err != nil {
			t.Fatal(err)
		}
		_, want := m.CreateMilestone(context.Background(), cycle)
		_ = m.Close()
		if want == nil {
			t.Fatal("the manager accepted a cycle")
		}
		raw, err := json.Marshal(cycle)
		if err != nil {
			t.Fatal(err)
		}
		c := captureStdoutErr(t, func() error {
			return cmdMilestoneEdit("create", []string{"--agent", "test", "--input", "-"}, strings.NewReader(string(raw)))
		})
		if c.err == nil || c.err.Error() != want.Error() || c.stdout != "" {
			t.Fatalf("cycle: CLI error %v, manager error %v, stdout %q", c.err, want, c.stdout)
		}
		for _, bad := range []struct {
			args        []string
			stdin, want string
		}{
			{[]string{"--input", "-"}, `{"title": "no tasks"}`, "task"},
			{[]string{"--input", "-"}, `{"title": "x", "tasks": [{"key": "T1", "title": "a"}], "approve": true}`, `milestone_create input schema: validating root: unexpected additional properties ["approve"]`},
			{[]string{"--input", "-"}, `{"title": "x", "tasks": [{"key": "T1", "title": "a"}]} {}`, "more than one JSON value"},
			{nil, "", "usage: agent-jobs milestone create"},
		} {
			c := captureStdoutErr(t, func() error {
				return cmdMilestoneEdit("create", append([]string{"--agent", "test"}, bad.args...), strings.NewReader(bad.stdin))
			})
			if c.err == nil || !strings.Contains(c.err.Error(), bad.want) {
				t.Errorf("%v %s: error %v, want %q", bad.args, bad.stdin, c.err, bad.want)
			}
		}
	})

	t.Run("[R01] CLI set edits title, tasks and status and round trips through milestone get", func(t *testing.T) {
		t.Setenv("AGENT_JOBS_HOME", t.TempDir())
		m, err := jobs.Open("test")
		if err != nil {
			t.Fatal(err)
		}
		v, err := m.CreateMilestone(context.Background(), jobs.MilestoneCreate{Title: "Stage", Tasks: []jobs.MilestoneTaskInput{
			{Key: "T1", Title: "one"}, {Key: "T2", Title: "two"}}})
		_ = m.Close()
		if err != nil {
			t.Fatal(err)
		}
		id := strconv.FormatInt(v.ID, 10)
		out := captureStdout(t, func() error {
			return cmdMilestoneEdit("set", []string{"--agent", "test", id, "--input", "-", "--title", "Stage renamed"},
				strings.NewReader(`{"goal": "new goal", "tasks": [{"key": "T3", "title": "three", "depends_on": ["T1"]}], "drop": ["T2"]}`))
		})
		got := decodeTree(t, out)
		status := map[string]string{}
		for _, task := range got.Tasks {
			status[task.Key] = task.Status
		}
		if got.ID != v.ID || got.Title != "Stage renamed" || got.Goal != "new goal" || len(got.Tasks) != 3 ||
			status["T2"] != "dropped" || status["T3"] != "blocked" || status["T1"] != "ready" {
			t.Fatalf("edited tree:\n%s", out)
		}
		if want := treeJSON(t, v.ID); out != want {
			t.Fatalf("CLI set output is not the milestone_get tree:\n%s\nwant\n%s", out, want)
		}

		// Flags only, no input: dropping the milestone with a note.
		out = captureStdout(t, func() error {
			return cmdMilestoneEdit("set", []string{"--agent", "test", id, "--status", "dropped", "--close-note", "not needed"}, nil)
		})
		if got := decodeTree(t, out); got.Status != "dropped" || got.CloseNote != "not needed" || got.Title != "Stage renamed" {
			t.Fatalf("status flags:\n%s", out)
		}

		// The manager's validation: done is refused while a task is not done; same error text.
		m, err = jobs.Open("test")
		if err != nil {
			t.Fatal(err)
		}
		_, want := m.SetMilestone(context.Background(), jobs.MilestoneSet{ID: v.ID, Status: "done"})
		_ = m.Close()
		if want == nil {
			t.Fatal("the manager closed a milestone with open tasks")
		}
		c := captureStdoutErr(t, func() error { return cmdMilestoneEdit("set", []string{"--agent", "test", id, "--status", "done"}, nil) })
		if c.err == nil || c.err.Error() != want.Error() {
			t.Fatalf("done: CLI error %v, manager error %v", c.err, want)
		}
		for _, bad := range []struct {
			args  []string
			stdin string
			want  string
		}{
			{[]string{id, "--input", "-"}, `{"id": 999}`, "the input's id 999 is not milestone " + id},
			{[]string{"--title", "x"}, "", "usage: agent-jobs milestone create"},
			{[]string{"0", "--title", "x"}, "", `milestone id "0" must be a positive number`},
			{[]string{"999", "--title", "x"}, "", "999"},
		} {
			c := captureStdoutErr(t, func() error {
				return cmdMilestoneEdit("set", append([]string{"--agent", "test"}, bad.args...), strings.NewReader(bad.stdin))
			})
			if c.err == nil || !strings.Contains(c.err.Error(), bad.want) {
				t.Errorf("%v %s: error %v, want %q", bad.args, bad.stdin, c.err, bad.want)
			}
		}
	})
}
