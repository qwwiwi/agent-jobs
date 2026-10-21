package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qwwiwi/agent-jobs/internal/brain"
	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

// JobMarker is how a job's server reaches check_mark: through the daemon's proxy socket
// (brain.SocketClient), which checks the job is running and records the mark with its id.
type JobMarker interface {
	CheckMark(ctx context.Context, name, note string) (brain.Object, error)
}

// CheckAddInput is check_add's input.
type CheckAddInput struct {
	AllowedScheduleIDs []int64 `json:"allowed_schedule_ids,omitempty" jsonschema:"only jobs started by these schedule ids may mark; omitted or empty permits every job; owner always permitted"`
	Name               string  `json:"name" jsonschema:"slug: a-z, 0-9, '-', '_'; at most 64 characters"`
	Cron               string  `json:"cron" jsonschema:"5-field cron expression in the host's local time: when the check must have reported (same syntax as schedule_add)"`
	GraceMinutes       int     `json:"grace_minutes,omitempty" jsonschema:"minutes after each expected time a mark may still arrive; default 60, 1..10080"`
}

// CheckSetInput is check_set's input.
type CheckSetInput struct {
	AllowedScheduleIDs []int64 `json:"allowed_schedule_ids,omitempty" jsonschema:"replace permitted schedule ids; omitted keeps current policy; empty permits every job; owner always permitted"`
	Name               string  `json:"name" jsonschema:"check name"`
	Cron               string  `json:"cron,omitempty" jsonschema:"new cron expression (local time)"`
	GraceMinutes       int     `json:"grace_minutes,omitempty" jsonschema:"new grace in minutes, 1..10080"`
	Enabled            *bool   `json:"enabled,omitempty" jsonschema:"false pauses the check, true resumes it"`
}

// CheckNameInput is check_remove's input.
type CheckNameInput struct {
	Name string `json:"name" jsonschema:"check name"`
}

// CheckMarkInput is check_mark's input.
type CheckMarkInput struct {
	Name string `json:"name" jsonschema:"check name (created with check_add)"`
	Note string `json:"note,omitempty" jsonschema:"optional one-line note, at most 300 characters"`
}

// CheckListInput is check_list's input (none).
type CheckListInput struct{}

// CheckOutput is one check with its state.
type CheckOutput struct {
	Check jobs.CheckView `json:"check"`
}

// ChecksOutput is every check, oldest first.
type ChecksOutput struct {
	Checks []jobs.CheckView `json:"checks"`
}

// CheckMarkOutput is check_mark's output: the check as the marker may see it.
type CheckMarkOutput struct {
	Check brain.Object `json:"check"`
}

// CheckRemoveOutput is check_remove's output.
type CheckRemoveOutput struct {
	Removed string `json:"removed"`
}

func addCheckAdd(s *sdk.Server, d Deps) {
	addManagerTool(s, d, &sdk.Tool{
		Name: "check_add",
		Description: "Add a dead-man's switch check: a name, a cron in local time saying when something that stays silent " +
			"while healthy must have reported, and a grace (default 60 minutes). That something calls check_mark (a job can) " +
			"or `agent-jobs check mark NAME`. A mark belongs to one expected time: the earliest whose time plus grace is not " +
			"before the mark. When an expected time plus its grace passes and no mark belongs to it, this session gets " +
			"\"[jobs:check-missed] ...\" (once per expected time, several in one line; a failed delivery is retried up to 5 times). " +
			"Use allowed_schedule_ids to restrict job markers. Owner marks remain permitted. Expected times before the check was added never alert. Checks never expire.",
	}, func(_ context.Context, _ *sdk.CallToolRequest, in CheckAddInput) (*sdk.CallToolResult, CheckOutput, error) {
		c, err := d.Manager.AddCheck(in.Name, in.Cron, in.GraceMinutes, in.AllowedScheduleIDs)
		if err != nil {
			return nil, CheckOutput{}, err
		}
		v, err := d.Manager.CheckViewOf(c)
		return nil, CheckOutput{Check: v}, err
	})
}

func addCheckSet(s *sdk.Server, d Deps) {
	addManagerTool(s, d, &sdk.Tool{
		Name: "check_set",
		Description: "Edit a check in place: cron, grace, enabled (false pauses it), or allowed_schedule_ids (empty clears restrictions). A new cron or grace, " +
			"or enabling it, re-arms the check: expected times before now never alert and marks before now never count. " +
			"Any check_set acknowledges the check's failed alerts (they leave system_doctor); pending ones are still delivered.",
	}, func(_ context.Context, _ *sdk.CallToolRequest, in CheckSetInput) (*sdk.CallToolResult, CheckOutput, error) {
		var policy [][]int64
		if in.AllowedScheduleIDs != nil {
			policy = append(policy, in.AllowedScheduleIDs)
		}
		c, err := d.Manager.SetCheck(in.Name, in.Cron, in.GraceMinutes, in.Enabled, policy...)
		if err != nil {
			return nil, CheckOutput{}, err
		}
		v, err := d.Manager.CheckViewOf(c)
		return nil, CheckOutput{Check: v}, err
	})
}

func addCheckList(s *sdk.Server, d Deps) {
	addManagerTool(s, d, &sdk.Tool{
		Name: "check_list",
		Description: "List this agent's checks, oldest first: state (ok, missed, waiting, disabled) for the latest expected " +
			"time whose grace has passed (missed stays missed after a late mark: late_mark_at), the next expected time, " +
			"the last mark, the newest missed alert with its delivery (alert_state pending, delivered or failed) and " +
			"alerts_pending / alerts_failed (failed and not acknowledged by check_set) over all its alerts.",
	}, func(_ context.Context, _ *sdk.CallToolRequest, _ CheckListInput) (*sdk.CallToolResult, ChecksOutput, error) {
		list, err := d.Manager.CheckViews()
		if err != nil {
			return nil, ChecksOutput{}, err
		}
		return nil, ChecksOutput{Checks: list}, nil
	})
}

func addCheckRemove(s *sdk.Server, d Deps) {
	addManagerTool(s, d, &sdk.Tool{
		Name: "check_remove",
		Description: "Delete a check and its marks. Only on the owner's explicit instruction: checks belong to the owner; " +
			"to pause one, use check_set enabled=false.",
	}, func(_ context.Context, _ *sdk.CallToolRequest, in CheckNameInput) (*sdk.CallToolResult, CheckRemoveOutput, error) {
		if err := d.Manager.RemoveCheck(in.Name); err != nil {
			return nil, CheckRemoveOutput{}, err
		}
		return nil, CheckRemoveOutput{Removed: in.Name}, nil
	})
}

func addCheckMark(s *sdk.Server, d Deps) {
	sdk.AddTool(s, &sdk.Tool{
		Name: "check_mark",
		Description: "Leave a completion mark for a check (dead-man's switch): call it when the watched work finished well. " +
			"An optional one-line note (at most 300 characters) is stored as redacted untrusted data with marker provenance. A job must match the check allowed_schedule_ids when restricted. The check must exist (check_add). " +
			"mark \"ok, already marked\" means the mark was accepted but not stored (its window already holds 20).",
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in CheckMarkInput) (*sdk.CallToolResult, CheckMarkOutput, error) {
		if d.JobID != "" {
			if d.JobMarker == nil {
				return nil, CheckMarkOutput{}, errors.New("check_mark: no daemon proxy in this server")
			}
			obj, err := d.JobMarker.CheckMark(ctx, in.Name, in.Note)
			if err != nil {
				return nil, CheckMarkOutput{}, err
			}
			return nil, CheckMarkOutput{Check: obj}, nil
		}
		if d.Manager == nil {
			return nil, CheckMarkOutput{}, errors.New("check_mark needs the job database or a job's daemon proxy")
		}
		c, err := d.Manager.MarkCheck(in.Name, in.Note, "")
		if err != nil {
			return nil, CheckMarkOutput{}, err
		}
		b, err := json.Marshal(c)
		if err != nil {
			return nil, CheckMarkOutput{}, fmt.Errorf("check_mark: %w", err)
		}
		var o brain.Object
		if err := json.Unmarshal(b, &o); err != nil {
			return nil, CheckMarkOutput{}, fmt.Errorf("check_mark: %w", err)
		}
		return nil, CheckMarkOutput{Check: o}, nil
	})
}
