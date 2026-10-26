package main

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/mcp"
)

// milestoneMCP is an in-memory MCP session on the orchestrator server over the "test" agent's store.
func milestoneMCP(t *testing.T) *sdk.ClientSession {
	t.Helper()
	m, err := jobs.Open("test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
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
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close(); _ = ss.Close() })
	return cs
}

// mcpMilestone calls tool with raw as its arguments, byte for byte: the tree on success, else the error.
func mcpMilestone(t *testing.T, cs *sdk.ClientSession, tool, raw string) (string, string) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: tool, Arguments: json.RawMessage(raw)})
	if err != nil {
		return "", err.Error()
	}
	var text strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			text.WriteString(tc.Text)
		}
	}
	if res.IsError {
		return "", text.String()
	}
	return text.String(), ""
}

// sameTree compares two milestone trees without what differs between two milestones built alike: the
// id and the times.
func sameTree(t *testing.T, a, b string) bool {
	t.Helper()
	norm := func(s string) string {
		v := decodeTree(t, s)
		v.ID, v.CreatedAt, v.UpdatedAt, v.ClosedAt = 0, "", "", ""
		out, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	return norm(a) == norm(b)
}

func milestoneCount(t *testing.T) int {
	t.Helper()
	m, err := jobs.Open("test")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	v, err := m.ListMilestones(context.Background(), jobs.MilestoneListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	return len(v.Milestones)
}

func newStage(t *testing.T) int64 {
	t.Helper()
	m, err := jobs.Open("test")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	v, err := m.CreateMilestone(context.Background(), jobs.MilestoneCreate{Title: "Stage", Goal: "keep",
		Tasks: []jobs.MilestoneTaskInput{{Key: "T1", Title: "one"}}})
	if err != nil {
		t.Fatal(err)
	}
	return v.ID
}

// Sol review, loop 1: the CLI refuses what the MCP tools refuse and changes nothing when it does.
func TestMilestoneCLIInputStrict(t *testing.T) {
	const tasks = `"tasks": [{"key": "T1", "title": "one"}]`

	t.Run("[R01] trailing data after the input, even a stray ] or }, is refused before any change", func(t *testing.T) {
		t.Setenv("AGENT_JOBS_HOME", t.TempDir())
		for _, suffix := range []string{"]", "}", " ]", "\n}", "{}", "[]", "x"} {
			c := captureStdoutErr(t, func() error {
				return cmdMilestoneEdit("create", []string{"--agent", "test", "--input", "-"},
					strings.NewReader(`{"title": "x", `+tasks+`}`+suffix))
			})
			if c.err == nil || !strings.Contains(c.err.Error(), "more than one JSON value") || c.stdout != "" {
				t.Errorf("create with %q after the object: error %v, stdout %q", suffix, c.err, c.stdout)
			}
			c = captureStdoutErr(t, func() error {
				return cmdMilestoneEdit("create", []string{"--agent", "test", "--input", "-", "--title", "x"},
					strings.NewReader(`[{"key": "T1", "title": "one"}]`+suffix))
			})
			if c.err == nil || !strings.Contains(c.err.Error(), "more than one JSON value") || c.stdout != "" {
				t.Errorf("create with %q after the tasks array: error %v, stdout %q", suffix, c.err, c.stdout)
			}
		}
		if n := milestoneCount(t); n != 0 {
			t.Fatalf("refused creates left %d milestones", n)
		}
		id := newStage(t)
		before := treeJSON(t, id)
		for _, suffix := range []string{"]", "}"} {
			c := captureStdoutErr(t, func() error {
				return cmdMilestoneEdit("set", []string{"--agent", "test", strconv.FormatInt(id, 10), "--input", "-"},
					strings.NewReader(`{"goal": "changed"}`+suffix))
			})
			if c.err == nil || !strings.Contains(c.err.Error(), "more than one JSON value") || c.stdout != "" {
				t.Errorf("set with %q after the object: error %v, stdout %q", suffix, c.err, c.stdout)
			}
		}
		if after := treeJSON(t, id); after != before {
			t.Fatalf("a refused set changed the milestone:\n%s\nwas\n%s", after, before)
		}
	})

	t.Run("[R01] keys in another case and null for a field the schema does not allow it in are refused, as over MCP", func(t *testing.T) {
		t.Setenv("AGENT_JOBS_HOME", t.TempDir())
		cs := milestoneMCP(t)
		for _, c := range []struct{ raw, want string }{
			{`{"TITLE": "x", ` + tasks + `}`, "TITLE"},
			{`{"title": "x", "max_parallel": null, ` + tasks + `}`, "max_parallel"},
		} {
			got := captureStdoutErr(t, func() error {
				return cmdMilestoneEdit("create", []string{"--agent", "test", "--input", "-"}, strings.NewReader(c.raw))
			})
			if got.err == nil || !strings.Contains(got.err.Error(), c.want) || !strings.Contains(got.err.Error(), "milestone_create") || got.stdout != "" {
				t.Errorf("CLI create %s: error %v, stdout %q; want a schema refusal naming %s", c.raw, got.err, got.stdout, c.want)
			}
			if _, e := mcpMilestone(t, cs, "milestone_create", c.raw); e == "" {
				t.Errorf("MCP accepted %s", c.raw)
			}
		}
		if n := milestoneCount(t); n != 0 {
			t.Fatalf("refused creates left %d milestones", n)
		}

		// Parity: every input is refused by both or accepted by both with the same tree.
		for _, raw := range []string{
			`{"title": "x", ` + tasks + `}`,
			`{"title": "x", "goal": "g", "order": 2, "max_parallel": 1, "tasks": [{"key": "T1", "title": "one"}, {"key": "T2", "title": "two", "depends_on": ["T1"]}]}`,
			`{"Title": "x", ` + tasks + `}`,
			`{"title": null, ` + tasks + `}`,
			`{"title": "x", "goal": null, ` + tasks + `}`,
			`{"title": "x", "order": null, ` + tasks + `}`,
			`{"title": "x", "tasks": null}`,
			`{"title": "x", "tasks": [{"KEY": "T1", "title": "one"}]}`,
			`{"title": "x", "tasks": [{"key": "T1", "title": "one", "depends_on": null}]}`,
			`{"title": "x", "max_parallel": "1", ` + tasks + `}`,
			`{"title": "x", "approve": true, ` + tasks + `}`,
		} {
			cli := captureStdoutErr(t, func() error {
				return cmdMilestoneEdit("create", []string{"--agent", "test", "--input", "-"}, strings.NewReader(raw))
			})
			tree, e := mcpMilestone(t, cs, "milestone_create", raw)
			switch {
			case (cli.err == nil) != (e == ""):
				t.Errorf("create %s: CLI error %v, MCP error %q", raw, cli.err, e)
			case e == "" && !sameTree(t, cli.stdout, tree):
				t.Errorf("create %s: CLI tree\n%s\nMCP tree\n%s", raw, cli.stdout, tree)
			}
		}
		a, b := newStage(t), newStage(t)
		for _, raw := range []string{
			`{"goal": "g2", "max_parallel": 2}`,
			`{"max_parallel": null}`,
			`{"goal": null}`,
			`{"Goal": "g"}`,
			`{"status": null}`,
			`{"title": null}`,
			`{"tasks": [{"key": "T2", "title": "two", "Depends_on": ["T1"]}]}`,
			`{"drop": null}`,
			`{"max_parallel": 1.5}`,
			`{"max_parallel": 2.0}`,
		} {
			cli := captureStdoutErr(t, func() error {
				return cmdMilestoneEdit("set", []string{"--agent", "test", strconv.FormatInt(a, 10), "--input", "-"}, strings.NewReader(raw))
			})
			tree, e := mcpMilestone(t, cs, "milestone_set", `{"id": `+strconv.FormatInt(b, 10)+`, `+strings.TrimPrefix(raw, "{"))
			switch {
			case (cli.err == nil) != (e == ""):
				t.Errorf("set %s: CLI error %v, MCP error %q", raw, cli.err, e)
			case e == "" && !sameTree(t, cli.stdout, tree):
				t.Errorf("set %s: CLI tree\n%s\nMCP tree\n%s", raw, cli.stdout, tree)
			}
		}
	})

	t.Run("[R01] set refuses any id in the input other than the positional one, 0 and null included", func(t *testing.T) {
		t.Setenv("AGENT_JOBS_HOME", t.TempDir())
		id := newStage(t)
		pos := strconv.FormatInt(id, 10)
		before := treeJSON(t, id)
		for _, c := range []struct{ raw, want string }{
			{`{"id": 0, "goal": "changed"}`, "the input's id 0 is not milestone " + pos},
			{`{"id": null, "goal": "changed"}`, "the input's id null is not milestone " + pos},
			{`{"id": "` + pos + `", "goal": "changed"}`, `the input's id "` + pos + `" is not milestone ` + pos},
			{`{"id": ` + pos + `.5, "goal": "changed"}`, "is not milestone " + pos},
			// #93: a float64 rounds this to the positional id; the ids are compared exactly.
			{`{"id": ` + pos + `.0000000000000001, "goal": "changed"}`, "the input's id " + pos + ".0000000000000001 is not milestone " + pos},
		} {
			got := captureStdoutErr(t, func() error {
				return cmdMilestoneEdit("set", []string{"--agent", "test", pos, "--input", "-"}, strings.NewReader(c.raw))
			})
			if got.err == nil || !strings.Contains(got.err.Error(), c.want) || got.stdout != "" {
				t.Errorf("set %s: error %v, stdout %q, want %q", c.raw, got.err, got.stdout, c.want)
			}
		}
		if after := treeJSON(t, id); after != before {
			t.Fatalf("a refused set changed the milestone:\n%s\nwas\n%s", after, before)
		}
		// The same id, or none, is taken.
		for _, raw := range []string{`{"id": ` + pos + `, "goal": "same id"}`, `{"goal": "no id"}`} {
			out := captureStdout(t, func() error {
				return cmdMilestoneEdit("set", []string{"--agent", "test", pos, "--input", "-"}, strings.NewReader(raw))
			})
			if v := decodeTree(t, out); v.ID != id || !strings.Contains(raw, v.Goal) {
				t.Errorf("set %s:\n%s", raw, out)
			}
		}
	})

	t.Run("[R01] set keeps a positional id above 2^53 exact up to the manager (#93)", func(t *testing.T) {
		t.Setenv("AGENT_JOBS_HOME", t.TempDir())
		const big = "9007199254740993" // 2^53+1: a float64 makes it 9007199254740992
		for _, raw := range []string{`{"goal": "changed"}`, `{"id": ` + big + `, "goal": "changed"}`} {
			got := captureStdoutErr(t, func() error {
				return cmdMilestoneEdit("set", []string{"--agent", "test", big, "--input", "-"}, strings.NewReader(raw))
			})
			if got.err == nil || !strings.Contains(got.err.Error(), "milestone "+big+": ") || got.stdout != "" {
				t.Errorf("set %s %s: error %v, stdout %q; want the manager's not found for milestone %s", big, raw, got.err, got.stdout, big)
			}
		}
		// The input's id is compared with it exactly: one below is another milestone.
		got := captureStdoutErr(t, func() error {
			return cmdMilestoneEdit("set", []string{"--agent", "test", big, "--input", "-"},
				strings.NewReader(`{"id": 9007199254740992, "goal": "changed"}`))
		})
		if got.err == nil || !strings.Contains(got.err.Error(), "the input's id 9007199254740992 is not milestone "+big) {
			t.Errorf("set %s with the input's id 9007199254740992: error %v", big, got.err)
		}
	})

	t.Run("[R01] an input id of the same value in another form is taken, as over MCP (#93)", func(t *testing.T) {
		t.Setenv("AGENT_JOBS_HOME", t.TempDir())
		cs := milestoneMCP(t)
		id := newStage(t)
		pos := strconv.FormatInt(id, 10)
		for _, n := range []string{pos + ".0", pos + "e0", pos + "0e-1"} {
			raw := `{"id": ` + n + `, "goal": "goal ` + n + `"}`
			out := captureStdout(t, func() error {
				return cmdMilestoneEdit("set", []string{"--agent", "test", pos, "--input", "-"}, strings.NewReader(raw))
			})
			if v := decodeTree(t, out); v.ID != id || v.Goal != "goal "+n {
				t.Errorf("CLI set %s:\n%s", raw, out)
			}
			if _, e := mcpMilestone(t, cs, "milestone_set", raw); e != "" {
				t.Errorf("MCP refused %s: %s", raw, e)
			}
		}
	})
}
