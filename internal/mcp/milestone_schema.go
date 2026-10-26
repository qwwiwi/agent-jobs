package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"

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

// SameMilestoneID reports whether n, an id from a milestone tool's input, is exactly id: a number of the
// same value (1, 1.0 or 1e0 for 1), not one that only rounds to it as a float64 (#93, #94).
func SameMilestoneID(n json.Number, id int64) bool {
	r, ok := new(big.Rat).SetString(n.String())
	return ok && r.IsInt() && r.Num().IsInt64() && r.Num().Int64() == id
}

// exactMilestoneID refuses raw, a milestone_set call's arguments, unless their id is exactly id, the one
// the SDK decoded through a float64 (#94): 1.0000000000000001 is not milestone 1, and 2^53+1 is not 2^53.
func exactMilestoneID(raw json.RawMessage, id int64) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var args map[string]any
	if err := dec.Decode(&args); err != nil {
		return fmt.Errorf("milestone_set arguments: %w", err)
	}
	n, ok := args["id"].(json.Number)
	if !ok || !SameMilestoneID(n, id) {
		b, _ := json.Marshal(args["id"])
		return fmt.Errorf("milestone_set: id %s is not exactly a milestone id (it reads as %d)", b, id)
	}
	return nil
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
