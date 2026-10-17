package mcp

import (
	"context"
	"fmt"
	"path/filepath"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// AttemptView is one attempt of a job in job_result.
type AttemptView struct {
	N              int     `json:"n"`
	Mode           string  `json:"mode"`
	Status         string  `json:"status"`
	SessionID      string  `json:"session_id,omitempty"`
	MaxTurns       int     `json:"max_turns"`
	MaxBudgetUSD   float64 `json:"max_budget_usd"`
	TimeoutMinutes int     `json:"timeout_minutes"`
	Turns          int     `json:"turns"`
	CostUSD        float64 `json:"cost_usd"`
	DurationSec    int     `json:"duration_sec"`
	Error          string  `json:"error,omitempty"`
	// Context is the orchestrator update the attempt was resumed with (empty for the first).
	Context   string `json:"context,omitempty"`
	StartedAt string `json:"started_at,omitempty"`
	// Report is the path of the attempt's report (an earlier attempt's is report.attempt-<n>.md).
	Report string `json:"report,omitempty"`
}

// attemptViews lists the attempts of a job that was resumed (nil for a job with one attempt).
func attemptViews(m *jobs.Manager, j *store.Job) ([]AttemptView, error) {
	atts, err := m.Store.Attempts(j.ID)
	if err != nil || len(atts) == 0 {
		return nil, err
	}
	dir := m.JobDir(j.ID)
	out := make([]AttemptView, 0, len(atts)+1)
	for _, a := range atts {
		out = append(out, AttemptView{N: a.N, Mode: a.Mode, Status: a.Status, SessionID: a.SessionID,
			MaxTurns: a.MaxTurns, MaxBudgetUSD: a.MaxBudgetUSD, TimeoutMinutes: a.TimeoutMinutes, Turns: a.Turns,
			CostUSD: a.CostUSD, DurationSec: a.DurationSec, Error: a.Error, Context: a.Context,
			StartedAt: fmtTS(a.StartedAt), Report: filepath.Join(dir, jobs.AttemptReportName(a.N))})
	}
	out = append(out, AttemptView{N: j.Attempt, Mode: j.RunMode, Status: j.Status, SessionID: j.SessionID,
		MaxTurns: j.MaxTurns, MaxBudgetUSD: j.MaxBudgetUSD, TimeoutMinutes: j.TimeoutMinutes, Turns: j.Turns,
		CostUSD: j.CostUSD, DurationSec: j.DurationSec, Error: j.Error, Context: j.PendingContext,
		StartedAt: fmtTS(j.StartedAt), Report: filepath.Join(dir, "report.md")})
	return out, nil
}

// JobStopInput is job_stop's input.
type JobStopInput struct {
	ID     string `json:"id" jsonschema:"job id as returned by job_start"`
	Reason string `json:"reason,omitempty" jsonschema:"why it is stopped; recorded on the job"`
}

func addJobStop(s *sdk.Server, d Deps) {
	addManagerTool(s, d, &sdk.Tool{
		Name: "job_stop",
		Description: "Stop a queued or running job so it can be resumed with new context: a running job's process group " +
			"gets SIGTERM, then SIGKILL after 10 s (as job_cancel), but the job ends 'stopped', keeping its work dir, " +
			"uncommitted changes and claude session, with its turns and cost so far. It gets no job-done line and no " +
			"cleanup; continue it with job_resume or give it up with job_cancel.",
	}, func(_ context.Context, _ *sdk.CallToolRequest, in JobStopInput) (*sdk.CallToolResult, JobView, error) {
		j, err := getJob(d.Manager, in.ID)
		if err != nil {
			return nil, JobView{}, err
		}
		cur, err := d.Manager.Stop(j.ID, in.Reason)
		if err != nil {
			return nil, JobView{}, fmt.Errorf("job_stop: %w", err)
		}
		return nil, view(d.Manager, cur), nil
	})
}

// JobResumeInput is job_resume's input.
type JobResumeInput struct {
	ID             string  `json:"id" jsonschema:"job id of a stopped, timeout or failed job"`
	Context        string  `json:"context" jsonschema:"the update: changed plan, new facts, corrections; sent to the job's session as a new message"`
	MaxTurns       int     `json:"max_turns,omitempty" jsonschema:"turn limit of the continuation; default what the previous attempt left, ceiling 200"`
	TimeoutMinutes int     `json:"timeout_minutes,omitempty" jsonschema:"minutes for the continuation; default what the previous attempt left, ceiling 180"`
	MaxBudgetUSD   float64 `json:"max_budget_usd,omitempty" jsonschema:"spend cap of the continuation; default what the previous attempt left, ceiling 50"`
}

func addJobResume(s *sdk.Server, d Deps) {
	addManagerTool(s, d, &sdk.Tool{
		Name: "job_resume",
		Description: "Continue a stopped, timeout or failed job with new context. The same job id is queued again as its " +
			"next attempt: same work dir, same claude session (claude --resume) with your context as a new message " +
			"«Orchestrator update»; same kind, guard, profile and MCP config. If the session is gone, a fresh session " +
			"gets the original brief, a summary of the previous attempt and your context (the report says so). " +
			"Limits default to what the previous attempt left; pass new ones (within the ceilings) when nothing is left, " +
			"e.g. after a timeout. Cancelled and succeeded jobs cannot be resumed: start a new job.",
	}, func(_ context.Context, _ *sdk.CallToolRequest, in JobResumeInput) (*sdk.CallToolResult, JobView, error) {
		j, err := getJob(d.Manager, in.ID)
		if err != nil {
			return nil, JobView{}, err
		}
		cur, err := d.Manager.Resume(j.ID, in.Context, jobs.Limits{MaxTurns: in.MaxTurns,
			MaxBudgetUSD: in.MaxBudgetUSD, TimeoutMinutes: in.TimeoutMinutes})
		if err != nil {
			return nil, JobView{}, fmt.Errorf("job_resume rejected: %w", err)
		}
		return nil, view(d.Manager, cur), nil
	})
}

// JobAmendInput is job_amend's input.
type JobAmendInput struct {
	ID      string `json:"id" jsonschema:"job id of a queued job"`
	Context string `json:"context" jsonschema:"text appended to the brief under «Orchestrator update»"`
}

func addJobAmend(s *sdk.Server, d Deps) {
	addManagerTool(s, d, &sdk.Tool{
		Name: "job_amend",
		Description: "Append context to the brief of a job that is still queued (not started yet); it keeps its place in " +
			"the queue. For a running job use job_stop + job_resume.",
	}, func(_ context.Context, _ *sdk.CallToolRequest, in JobAmendInput) (*sdk.CallToolResult, JobView, error) {
		j, err := getJob(d.Manager, in.ID)
		if err != nil {
			return nil, JobView{}, err
		}
		cur, err := d.Manager.Amend(j.ID, in.Context)
		if err != nil {
			return nil, JobView{}, fmt.Errorf("job_amend: %w", err)
		}
		return nil, view(d.Manager, cur), nil
	})
}
