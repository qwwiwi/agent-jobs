package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

// milestone_create and milestone_set advertise the schema ValidateMilestoneInput checks, and it is the one
// the SDK would infer from their input types, so declaring it changed nothing for MCP clients.
func TestMilestoneInputSchemaShared(t *testing.T) {
	t.Run("[R01] the milestone tools' advertised schema is the CLI's and the inferred one", func(t *testing.T) {
		res, err := connect(t, mustServer(t, newManager(t))).ListTools(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		inferred := map[string]func() (*jsonschema.Schema, error){
			"milestone_create": func() (*jsonschema.Schema, error) {
				return jsonschema.For[jobs.MilestoneCreate](&jsonschema.ForOptions{})
			},
			"milestone_set": func() (*jsonschema.Schema, error) { return jsonschema.For[jobs.MilestoneSet](&jsonschema.ForOptions{}) },
		}
		seen := 0
		for _, tl := range res.Tools {
			infer, ok := inferred[tl.Name]
			if !ok {
				continue
			}
			seen++
			s, err := infer()
			if err != nil {
				t.Fatal(err)
			}
			listed, shared, want := canonical(t, tl.InputSchema), canonical(t, milestoneSchemas[tl.Name].schema), canonical(t, s)
			if listed != shared || shared != want {
				t.Errorf("%s: advertised\n%s\nshared\n%s\ninferred\n%s", tl.Name, listed, shared, want)
			}
		}
		if seen != len(inferred) {
			t.Fatalf("%d of the milestone tools listed", seen)
		}
	})

	t.Run("[R01] ValidateMilestoneInput refuses what the tool refuses", func(t *testing.T) {
		ok := map[string]any{"title": "x", "tasks": []any{map[string]any{"key": "T1", "title": "a"}}}
		if err := ValidateMilestoneInput("milestone_create", ok); err != nil {
			t.Fatalf("valid input: %v", err)
		}
		for _, c := range []struct {
			tool string
			args map[string]any
			want string
		}{
			{"milestone_create", map[string]any{"TITLE": "x", "tasks": ok["tasks"]}, `unexpected additional properties ["TITLE"]`},
			{"milestone_create", map[string]any{"title": "x", "max_parallel": nil, "tasks": ok["tasks"]}, "max_parallel"},
			{"milestone_set", map[string]any{"title": "x"}, "id"},
			{"milestone_get", map[string]any{"id": 1.0}, "not a milestone tool"},
		} {
			if err := ValidateMilestoneInput(c.tool, c.args); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s %v: %v, want %q", c.tool, c.args, err, c.want)
			}
		}
	})
}

// canonical is v as JSON with object keys sorted.
func canonical(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var generic any
	if err := json.Unmarshal(b, &generic); err != nil {
		t.Fatal(err)
	}
	if b, err = json.Marshal(generic); err != nil {
		t.Fatal(err)
	}
	return string(b)
}
