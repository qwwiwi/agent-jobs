package mcp

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// IntentionAddInput is intention_add's input.
type IntentionAddInput struct {
	Text     string `json:"text" jsonschema:"the goal, one line (at most 500 characters, longer is cut)"`
	Due      string `json:"due,omitempty" jsonschema:"optional due date YYYY-MM-DD"`
	Priority string `json:"priority,omitempty" jsonschema:"low | normal | high; default normal"`
}

// IntentionListInput is intention_list's input.
type IntentionListInput struct {
	Status string `json:"status,omitempty" jsonschema:"only intentions with this status: open | done | dropped; default all"`
}

// IntentionSetInput is intention_set's input.
type IntentionSetInput struct {
	ID     int64  `json:"id" jsonschema:"intention id from intention_add or intention_list"`
	Status string `json:"status,omitempty" jsonschema:"open | done | dropped"`
	Text   string `json:"text,omitempty" jsonschema:"new text, one line"`
}

// IntentionOutput is one intention.
type IntentionOutput struct {
	Intention *store.Intention `json:"intention"`
}

// IntentionsOutput is a list of intentions, oldest first.
type IntentionsOutput struct {
	Intentions []*store.Intention `json:"intentions"`
}

func addIntentionAdd(s *sdk.Server, d Deps) {
	addManagerTool(s, d, &sdk.Tool{
		Name: "intention_add",
		Description: "Add one of this agent's own goals (an intention), open, with an optional due date and priority; " +
			"returns it with its id. Intentions persist in the agent's job database; text is one line.",
	}, func(_ context.Context, _ *sdk.CallToolRequest, in IntentionAddInput) (*sdk.CallToolResult, IntentionOutput, error) {
		it, err := d.Manager.AddIntention(in.Text, in.Due, in.Priority)
		return nil, IntentionOutput{Intention: it}, err
	})
}

func addIntentionList(s *sdk.Server, d Deps) {
	addManagerTool(s, d, &sdk.Tool{
		Name:        "intention_list",
		Description: "List this agent's intentions, oldest first, optionally only one status (open | done | dropped).",
	}, func(_ context.Context, _ *sdk.CallToolRequest, in IntentionListInput) (*sdk.CallToolResult, IntentionsOutput, error) {
		list, err := d.Manager.ListIntentions(in.Status)
		return nil, IntentionsOutput{Intentions: list}, err
	})
}

func addIntentionSet(s *sdk.Server, d Deps) {
	addManagerTool(s, d, &sdk.Tool{
		Name: "intention_set",
		Description: "Edit an intention in place: its status (open | done | dropped), its text, or both. " +
			"There is no delete: a goal given up is set to dropped.",
	}, func(_ context.Context, _ *sdk.CallToolRequest, in IntentionSetInput) (*sdk.CallToolResult, IntentionOutput, error) {
		it, err := d.Manager.SetIntention(in.ID, in.Text, in.Status)
		return nil, IntentionOutput{Intention: it}, err
	})
}
