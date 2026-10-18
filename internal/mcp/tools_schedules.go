package mcp

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// ScheduleAddInput is schedule_add's input.
type ScheduleAddInput struct {
	Cron   string `json:"cron" jsonschema:"5-field cron expression in the host's local time: minute hour day-of-month month day-of-week; ranges a-b, steps /n, lists a,b; month and weekday names; @hourly @daily @weekly @monthly @yearly"`
	Prompt string `json:"prompt" jsonschema:"the instruction, one line, at most 2000 characters: typed into this session, or the job's brief when a kind is given"`
	Kind   string `json:"kind,omitempty" jsonschema:"omit to deliver the prompt into this session; review | fix | research | general to start a job instead (then model or complexity is required, like job_start)"`
	Title  string `json:"title,omitempty" jsonschema:"job schedules: title of the jobs it starts; default \"schedule <id>\" (never taken from the prompt)"`
	// The job fields below are validated like job_start and stored on the schedule at creation.
	Model           string   `json:"model,omitempty" jsonschema:"job schedules: explicit model (a gpt-* model runs on codex exec)"`
	Complexity      string   `json:"complexity,omitempty" jsonschema:"job schedules: light | normal | hard; the model is resolved now and stored"`
	MaxTurns        int      `json:"max_turns,omitempty" jsonschema:"job schedules: turn limit; default 80, ceiling 200"`
	TimeoutMinutes  int      `json:"timeout_minutes,omitempty" jsonschema:"job schedules: wall-clock limit in minutes; default 45, ceiling 180"`
	MaxBudgetUSD    float64  `json:"max_budget_usd,omitempty" jsonschema:"job schedules: spend cap in USD; default 10, ceiling 50"`
	Repo            string   `json:"repo,omitempty" jsonschema:"job schedules: absolute path of a git repository"`
	Base            string   `json:"base,omitempty" jsonschema:"job schedules: ref in repo, resolved at each firing; default HEAD"`
	Runner          string   `json:"runner,omitempty" jsonschema:"job schedules: claude | codex (codex rules as in job_start)"`
	ReasoningEffort string   `json:"reasoning_effort,omitempty" jsonschema:"codex job schedules only: low | medium | high"`
	AcceptanceCmd   string   `json:"acceptance_cmd,omitempty" jsonschema:"fix and general job schedules only: the acceptance command of the jobs it starts (see job_start)"`
	Skills          []string `json:"skills,omitempty" jsonschema:"job schedules: edits to the default skills of each job it starts, as in job_start (+name or name adds, -name removes); checked now and again at each firing"`
}

// ScheduleSetInput is schedule_set's input.
type ScheduleSetInput struct {
	ID      int64  `json:"id" jsonschema:"schedule id from schedule_add or schedule_list"`
	Cron    string `json:"cron,omitempty" jsonschema:"new cron expression (local time)"`
	Prompt  string `json:"prompt,omitempty" jsonschema:"new prompt, one line"`
	Enabled *bool  `json:"enabled,omitempty" jsonschema:"false pauses the schedule, true resumes it"`
}

// ScheduleListInput is schedule_list's input (none).
type ScheduleListInput struct{}

// ScheduleIDInput is schedule_remove's input.
type ScheduleIDInput struct {
	ID int64 `json:"id" jsonschema:"schedule id"`
}

// ScheduleView is one schedule with its next firing.
type ScheduleView struct {
	*store.Schedule
	// NextFireAt is the next minute it fires (local time), empty when disabled.
	NextFireAt string `json:"next_fire_at,omitempty"`
}

// ScheduleOutput is one schedule.
type ScheduleOutput struct {
	Schedule ScheduleView `json:"schedule"`
}

// SchedulesOutput is every schedule, oldest first.
type SchedulesOutput struct {
	Schedules []ScheduleView `json:"schedules"`
}

// ScheduleRemoveOutput is schedule_remove's output.
type ScheduleRemoveOutput struct {
	Removed int64 `json:"removed"`
}

// ScheduleViewOf is the view of a schedule (next firing in local time, RFC 3339).
func ScheduleViewOf(m *jobs.Manager, sc *store.Schedule) ScheduleView {
	v := ScheduleView{Schedule: sc}
	if n := m.NextFire(sc); n != nil {
		v.NextFireAt = n.Format("2006-01-02T15:04:05Z07:00")
	}
	return v
}

func addScheduleAdd(s *sdk.Server, d Deps) {
	addManagerTool(s, d, &sdk.Tool{
		Name: "schedule_add",
		Description: "Add a schedule: a cron expression in local time plus a prompt, checked every minute by the daemon. " +
			"Without a kind the prompt arrives in this session as one line \"[jobs:schedule] ...\" (several due in the same minute " +
			"arrive as one numbered line); with a kind it starts a job with the model and limits stored now (validated like job_start), " +
			"and this session only sees its job-done line. A missed minute (daemon down) is lost, not queued. Schedules never expire.",
	}, func(_ context.Context, _ *sdk.CallToolRequest, in ScheduleAddInput) (*sdk.CallToolResult, ScheduleOutput, error) {
		sc, err := d.Manager.AddSchedule(jobs.ScheduleRequest{
			Cron: in.Cron, Prompt: in.Prompt, Kind: in.Kind, Title: in.Title, Model: in.Model, Complexity: in.Complexity,
			MaxTurns: in.MaxTurns, MaxBudgetUSD: in.MaxBudgetUSD, TimeoutMinutes: in.TimeoutMinutes,
			Repo: in.Repo, Base: in.Base, Runner: in.Runner, ReasoningEffort: in.ReasoningEffort, Skills: in.Skills,
			AcceptanceCmd: in.AcceptanceCmd,
		})
		if err != nil {
			return nil, ScheduleOutput{}, err
		}
		return nil, ScheduleOutput{Schedule: ScheduleViewOf(d.Manager, sc)}, nil
	})
}

func addScheduleList(s *sdk.Server, d Deps) {
	addManagerTool(s, d, &sdk.Tool{
		Name:        "schedule_list",
		Description: "List this agent's schedules, oldest first, with the next firing, the last firing and its outcome.",
	}, func(_ context.Context, _ *sdk.CallToolRequest, _ ScheduleListInput) (*sdk.CallToolResult, SchedulesOutput, error) {
		list, err := d.Manager.ListSchedules()
		if err != nil {
			return nil, SchedulesOutput{}, err
		}
		out := SchedulesOutput{Schedules: make([]ScheduleView, 0, len(list))}
		for _, sc := range list {
			out.Schedules = append(out.Schedules, ScheduleViewOf(d.Manager, sc))
		}
		return nil, out, nil
	})
}

func addScheduleSet(s *sdk.Server, d Deps) {
	addManagerTool(s, d, &sdk.Tool{
		Name: "schedule_set",
		Description: "Edit a schedule in place: its cron, its prompt, enabled (false pauses it), or several. " +
			"The job fields of a job schedule are fixed at creation.",
	}, func(_ context.Context, _ *sdk.CallToolRequest, in ScheduleSetInput) (*sdk.CallToolResult, ScheduleOutput, error) {
		sc, err := d.Manager.SetSchedule(in.ID, in.Cron, in.Prompt, in.Enabled)
		if err != nil {
			return nil, ScheduleOutput{}, err
		}
		return nil, ScheduleOutput{Schedule: ScheduleViewOf(d.Manager, sc)}, nil
	})
}

func addScheduleRemove(s *sdk.Server, d Deps) {
	addManagerTool(s, d, &sdk.Tool{
		Name: "schedule_remove",
		Description: "Delete a schedule. Only on the owner's explicit instruction: schedules belong to the owner; " +
			"to pause one, use schedule_set enabled=false.",
	}, func(_ context.Context, _ *sdk.CallToolRequest, in ScheduleIDInput) (*sdk.CallToolResult, ScheduleRemoveOutput, error) {
		if err := d.Manager.RemoveSchedule(in.ID); err != nil {
			return nil, ScheduleRemoveOutput{}, err
		}
		return nil, ScheduleRemoveOutput{Removed: in.ID}, nil
	})
}
