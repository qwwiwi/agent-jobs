package mcp

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

// JobStatsInput is job_stats' input.
type JobStatsInput struct {
	Since string `json:"since,omitempty" jsonschema:"only jobs finished since: <n>d (7d), a duration (36h) or a date YYYY-MM-DD; default all"`
	By    string `json:"by,omitempty" jsonschema:"group by kind | model | skill | effort; default kind"`
}

func addJobStats(s *sdk.Server, d Deps) {
	addManagerTool(s, d, &sdk.Tool{
		Name: "job_stats",
		Description: "Summary of this agent's finished jobs from the per-job stats table (one row per job, written " +
			"when it ends): totals and per-group counts, success rate, median and 90th percentile of cost and duration, " +
			"the most frequent guard refusal rules, tool calls, and skills by uses with the success rate of the jobs " +
			"that used them. Same output as `agent-jobs stats --json`; jobs that ended before the table existed " +
			"appear after `agent-jobs stats --backfill`.",
	}, func(_ context.Context, _ *sdk.CallToolRequest, in JobStatsInput) (*sdk.CallToolResult, jobs.StatsReport, error) {
		since, err := jobs.ParseSince(in.Since, d.Manager.Now())
		if err != nil {
			return nil, jobs.StatsReport{}, err
		}
		rep, err := d.Manager.Stats(since, in.By)
		return nil, rep, err
	})
}
