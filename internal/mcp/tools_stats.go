package mcp

import (
	"context"
	"fmt"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

// JobStatsInput is job_stats' input.
type JobStatsInput struct {
	Since string `json:"since,omitempty" jsonschema:"only jobs finished since: <n>d (7d), a duration (36h) or a date YYYY-MM-DD; default all"`
	By    string `json:"by,omitempty" jsonschema:"group by kind | model | skill | repo; default kind"`
}

func addJobStats(s *sdk.Server, d Deps) {
	addManagerTool(s, d, &sdk.Tool{
		Name: "job_stats",
		Description: "Summary of this agent's finished jobs from the per-job stats table (one row per job, written " +
			"when it ends): totals and per-group counts, success rate, median and 90th percentile of cost and duration, " +
			"the most frequent guard refusal rules, tool calls, and skills by uses with the success rate of the jobs " +
			"that used them, the outcome shares (job_outcome; unrecorded = finished with a report and no outcome) and " +
			"cost_per_usable_result_usd = total cost / (accepted + extended). Attempts that ended without a reported cost " +
			"(killed, timed out, codex) are priced from their stream's tokens at list prices and counted in the cost " +
			"(cost_estimated_usd, estimated_jobs); cost_unknown_jobs are those with no price row or no stream. Same output as `agent-jobs stats --json`; jobs that ended before the table existed " +
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

// JobOutcomeInput is job_outcome's input.
type JobOutcomeInput struct {
	ID      string `json:"id" jsonschema:"job id as returned by job_start"`
	Outcome string `json:"outcome" jsonschema:"accepted (used as delivered) | extended (used, more work added on top) | reworked (its own work had to be fixed first) | rerun (run again on another model or brief) | discarded"`
	Note    string `json:"note,omitempty" jsonschema:"optional note, at most 300 characters"`
}

// JobOutcomeOutput is job_outcome's output.
type JobOutcomeOutput struct {
	ID          string `json:"id"`
	Outcome     string `json:"outcome"`
	OutcomeNote string `json:"outcome_note,omitempty"`
}

func addJobOutcome(s *sdk.Server, d Deps) {
	addManagerTool(s, d, &sdk.Tool{
		Name: "job_outcome",
		Description: "Record what you did with a finished job's result, after reading it and acting on it: accepted, " +
			"extended, reworked, rerun or discarded, with an optional note (at most 300 characters). Stored on the job " +
			"and its stats row; calling it again overwrites the outcome. Refused for unknown ids, other agents' jobs and " +
			"jobs that are queued, running or stopped. job_stats reports the shares and the cost per usable result.",
	}, func(_ context.Context, _ *sdk.CallToolRequest, in JobOutcomeInput) (*sdk.CallToolResult, JobOutcomeOutput, error) {
		if err := d.Manager.SetOutcome(in.ID, in.Outcome, in.Note); err != nil {
			return nil, JobOutcomeOutput{}, fmt.Errorf("job_outcome rejected: %w", err)
		}
		j, err := d.Manager.Store.Get(strings.TrimSpace(in.ID))
		if err != nil {
			return nil, JobOutcomeOutput{}, err
		}
		return nil, JobOutcomeOutput{ID: j.ID, Outcome: j.Outcome, OutcomeNote: j.OutcomeNote}, nil
	})
}
