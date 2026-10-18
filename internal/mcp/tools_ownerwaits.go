package mcp

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// OwnerRepliedInput is owner_replied's input.
type OwnerRepliedInput struct {
	JobID string `json:"job_id,omitempty" jsonschema:"the owner-waits job whose result you wrote to the owner; omit to clear every pending one"`
}

// OwnerRepliedOutput is owner_replied's output: the cleared job ids and what is still pending.
type OwnerRepliedOutput struct {
	Cleared []string              `json:"cleared"`
	Pending []*store.OwnerPending `json:"pending"`
}

func addOwnerReplied(s *sdk.Server, d Deps) {
	addManagerTool(s, d, &sdk.Tool{
		Name: "owner_replied",
		Description: "Call after you have written to the owner about an owner-waits job (its job-done line ends with " +
			"'owner waits'): clears its «owner reply pending» record, so the Stop hook lets this session end its turn again. " +
			"With job_id only that job; without it every pending one. A record also expires on its own after " +
			"owner_waits_max_hours (default 6). Returns the cleared ids and what is still pending.",
	}, func(_ context.Context, _ *sdk.CallToolRequest, in OwnerRepliedInput) (*sdk.CallToolResult, OwnerRepliedOutput, error) {
		cleared, err := d.Manager.OwnerReplied(in.JobID)
		if err != nil {
			return nil, OwnerRepliedOutput{}, err
		}
		pending, _, err := d.Manager.OwnerPending()
		return nil, OwnerRepliedOutput{Cleared: cleared, Pending: pending}, err
	})
}
