package mcp

import (
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

// milestoneSchemas are the input schemas milestone_create and milestone_set declare, inferred from their
// input types as the SDK infers an unset one. The tools carry these values, and ValidateMilestoneInput
// checks against them, so `agent-jobs milestone create|set` refuses exactly what the tools refuse.
var milestoneSchemas = map[string]*inputSchema{
	"milestone_create": newInputSchema[jobs.MilestoneCreate](),
	"milestone_set":    newInputSchema[jobs.MilestoneSet](),
}

type inputSchema struct {
	schema   *jsonschema.Schema
	resolved *jsonschema.Resolved
}

func newInputSchema[T any]() *inputSchema {
	s, err := jsonschema.For[T](&jsonschema.ForOptions{})
	if err != nil {
		panic(fmt.Sprintf("input schema of %T: %v", *new(T), err))
	}
	// The SDK resolves a tool's input schema with the same options.
	r, err := s.Resolve(&jsonschema.ResolveOptions{ValidateDefaults: true})
	if err != nil {
		panic(fmt.Sprintf("input schema of %T: %v", *new(T), err))
	}
	return &inputSchema{schema: s, resolved: r}
}

// ValidateMilestoneInput validates args, the arguments of tool (milestone_create or milestone_set) as
// decoded JSON (objects as map[string]any, numbers as float64 or int64), against the tool's input schema, as the
// server does before it calls the manager.
func ValidateMilestoneInput(tool string, args map[string]any) error {
	s, ok := milestoneSchemas[tool]
	if !ok {
		return fmt.Errorf("%s is not a milestone tool with an input schema", tool)
	}
	if err := s.resolved.Validate(args); err != nil {
		return fmt.Errorf("%s input schema: %w", tool, err)
	}
	return nil
}
