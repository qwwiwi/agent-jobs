package mcp

import (
	"context"
	"errors"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qwwiwi/agent-jobs/internal/brain"
)

// JobAdvisor is how a crew job's server reaches advisor_ask: through the daemon's proxy socket
// (brain.SocketClient), which checks the job's crew and holds the caps.
type JobAdvisor interface {
	AdvisorAsk(ctx context.Context, question, background string) (brain.Object, error)
}

// AdvisorAskInput is advisor_ask's input.
type AdvisorAskInput struct {
	Question string `json:"question" jsonschema:"the design question, with the options you see; at most 4000 characters"`
	Context  string `json:"context,omitempty" jsonschema:"what the advisor needs to know: constraints, what you tried, file paths to read (it can read the work tree); at most 32 KB"`
}

// AdvisorAskOutput is advisor_ask's output: the advice, the call's record and what is left.
type AdvisorAskOutput struct {
	Answer brain.Object `json:"answer"`
}

// addAdvisorAsk registers advisor_ask only in a crew job's server (d.JobAdvisor set): a plain job and
// the orchestrator never see it.
func addAdvisorAsk(s *sdk.Server, d Deps) {
	if d.JobAdvisor == nil {
		return
	}
	sdk.AddTool(s, &sdk.Tool{
		Name: "advisor_ask",
		Description: "Ask this crew job's advisor (a stronger model) at a design fork: an architecture choice, an API shape, " +
			"a trade-off you cannot settle from the code. It runs a short read-only claude -p in your work tree (it can Read, Glob " +
			"and Grep, nothing else) and returns its advice; you decide. Capped per job (advisor_max_calls, default 2, and " +
			"advisor_max_budget_usd); one call at a time; each call is recorded (turn, question, outcome, turns, cost). " +
			"Do not use it for routine steps.",
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in AdvisorAskInput) (*sdk.CallToolResult, AdvisorAskOutput, error) {
		if d.JobID == "" {
			return nil, AdvisorAskOutput{}, errors.New("advisor_ask runs only inside a crew job")
		}
		obj, err := d.JobAdvisor.AdvisorAsk(ctx, in.Question, in.Context)
		if err != nil {
			return nil, AdvisorAskOutput{}, err
		}
		return nil, AdvisorAskOutput{Answer: obj}, nil
	})
}
