package mcp

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

// MilestoneGetInput is milestone_get's input.
type MilestoneGetInput struct {
	ID int64 `json:"id" jsonschema:"milestone id from milestone_create or milestone_list"`
}

func addMilestoneCreate(s *sdk.Server, d Deps) {
	addManagerTool(s, d, &sdk.Tool{
		Name: "milestone_create",
		Description: "Create a milestone (a stage of work) with its tasks in one call and return its tree. Only title and tasks " +
			"are required; each task has a key (T1, T2, ...), a title, optional depends_on (keys) and covers (requirement IDs). " +
			"Run its tasks with job_start(milestone, task): a task waits (blocked) until every task it depends on is done " +
			"(its job's outcome accepted), independent tasks run in parallel (max_parallel caps them, 1 = one after another). " +
			"A job on a task with depends_on gets a \"Prior results\" section with those tasks' accepted jobs.",
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in jobs.MilestoneCreate) (*sdk.CallToolResult, jobs.MilestoneView, error) {
		v, err := d.Manager.CreateMilestone(ctx, in)
		if err != nil {
			return nil, jobs.MilestoneView{}, err
		}
		return nil, *v, nil
	})
}

func addMilestoneSet(s *sdk.Server, d Deps) {
	addManagerTool(s, d, &sdk.Tool{
		Name: "milestone_set",
		Description: "Edit a milestone in place and return its tree: title, goal, branch, order, max_parallel, status " +
			"(open | done | dropped), close_note; tasks to add or edit by key; keys to drop. Nothing is deleted. Closing as done " +
			"is refused while a task is not done or dropped. Task status is derived from its jobs, never set here.",
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in jobs.MilestoneSet) (*sdk.CallToolResult, jobs.MilestoneView, error) {
		v, err := d.Manager.SetMilestone(ctx, in)
		if err != nil {
			return nil, jobs.MilestoneView{}, err
		}
		return nil, *v, nil
	})
}

func addMilestoneGet(s *sdk.Server, d Deps) {
	addManagerTool(s, d, &sdk.Tool{
		Name: "milestone_get",
		Description: "The full tree of one milestone: its fields; each task with its derived status (dropped, blocked, running, " +
			"review, done, failed, ready), depends_on, covers and jobs; ready (the keys a job can start on now); progress " +
			"(tasks done of total, dropped left out; acceptance met of total covered, from the latest judge verdict, null without " +
			"one); uncovered (live checklist rows no task covers, when tied to a checklist).",
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in MilestoneGetInput) (*sdk.CallToolResult, jobs.MilestoneView, error) {
		v, err := d.Manager.MilestoneTree(ctx, in.ID)
		if err != nil {
			return nil, jobs.MilestoneView{}, err
		}
		return nil, *v, nil
	})
}

func addMilestoneList(s *sdk.Server, d Deps) {
	addManagerTool(s, d, &sdk.Tool{
		Name: "milestone_list",
		Description: "List milestones ordered by repo, then order, then creation; filters: status, repo, checklist_id, " +
			"updated_since (the change cursor: updated_at moves on any change to the milestone, its tasks or their jobs). " +
			"tree=true returns each with its full tree (as milestone_get): with repo, the whole project in one call.",
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in jobs.MilestoneListRequest) (*sdk.CallToolResult, jobs.MilestonesView, error) {
		v, err := d.Manager.ListMilestones(ctx, in)
		if err != nil {
			return nil, jobs.MilestonesView{}, err
		}
		return nil, *v, nil
	})
}
