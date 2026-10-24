package mcp

import (
	"context"
	"fmt"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

// GateRunInput is how a gate job runs: the model or complexity, runner and limits; never a brief.
type GateRunInput struct {
	Model           string  `json:"model,omitempty" jsonschema:"explicit model (a gpt-* model runs the job on codex); give this or complexity"`
	Complexity      string  `json:"complexity,omitempty" jsonschema:"light | normal | hard; picks the model by kind when no model is given"`
	Runner          string  `json:"runner,omitempty" jsonschema:"claude | codex; default by model"`
	ReasoningEffort string  `json:"reasoning_effort,omitempty" jsonschema:"reasoning effort, as job_start"`
	MaxTurns        int     `json:"max_turns,omitempty" jsonschema:"turn limit (claude), as job_start"`
	MaxBudgetUSD    float64 `json:"max_budget_usd,omitempty" jsonschema:"spend cap in USD (claude), as job_start"`
	TimeoutMinutes  int     `json:"timeout_minutes,omitempty" jsonschema:"wall-clock limit in minutes, as job_start"`
}

// SlicingStartInput is slicing_start's input: the repository and how to run; no brief.
type SlicingStartInput struct {
	Repo        string `json:"repo" jsonschema:"absolute path of the project's git work tree (brief/BRIEF.md and brief/CHECKLIST.md are read there)"`
	ChecklistID string `json:"checklist_id,omitempty" jsonschema:"when given, must be the id of the repository's checklist"`
	GateRunInput
}

// JudgeStartInput is judge_start's input: the repository, the commit and how to run; no brief.
type JudgeStartInput struct {
	Repo        string `json:"repo" jsonschema:"absolute path of the project's git work tree with the approved brief/CHECKLIST.md"`
	SHA         string `json:"sha" jsonschema:"the full commit id whose code the judge checks"`
	ChecklistID string `json:"checklist_id,omitempty" jsonschema:"when given, must be the id of the repository's checklist"`
	GateRunInput
}

func gateRequest(kind, repo, sha, id string, in GateRunInput) jobs.GateRequest {
	return jobs.GateRequest{Kind: kind, Repo: repo, SHA: sha, ChecklistID: id, Model: in.Model, Complexity: in.Complexity,
		Runner: in.Runner, ReasoningEffort: in.ReasoningEffort, MaxTurns: in.MaxTurns, MaxBudgetUSD: in.MaxBudgetUSD,
		TimeoutMinutes: in.TimeoutMinutes}
}

func addSlicingStart(s *sdk.Server, d Deps) {
	addManagerTool(s, d, &sdk.Tool{
		Name: "slicing_start",
		Description: "Queue the slicing check of the repo's checklist (Wave F, F3): a read-only job of kind slicing that gets only " +
			"brief/BRIEF.md and the checklist rows (read by the daemon now, no code, no briefs or reports) and returns unmapped " +
			"(brief quotes in no row), half (partly covered) and invented (rows the brief does not support) in a validated JSON " +
			"block, stored for the checklist id, version and hash. checklist_set approve needs the result for the current version " +
			"with no unmapped quote left without an owner decision. There is no brief: the job's input is the daemon's.",
	}, func(_ context.Context, _ *sdk.CallToolRequest, in SlicingStartInput) (*sdk.CallToolResult, JobView, error) {
		j, err := d.Manager.StartGate(gateRequest(jobs.KindSlicing, in.Repo, "", in.ChecklistID, in.GateRunInput))
		if err != nil {
			return nil, JobView{}, fmt.Errorf("slicing_start rejected: %w", err)
		}
		return nil, view(d.Manager, j), nil
	})
}

func addJudgeStart(s *sdk.Server, d Deps) {
	addManagerTool(s, d, &sdk.Tool{
		Name: "judge_start",
		Description: "Queue the blind acceptance judge (Wave F, F5; kind judge, not acceptance_cmd) of the code at sha against the repo's " +
			"approved checklist: a read-only job that gets BRIEF.md in full, the checklist rows (ID, verbatim, contract, Basis) and a " +
			"copy of the files at sha (no git history, no job briefs, reports or diffs), assembled by the daemon. It returns met | " +
			"partial | missing | placeholder | cannot_check per live requirement with evidence in a validated JSON block, stored by " +
			"repo, sha, checklist id and version, judge model and family; a judge of the coders' model family is stored but flagged " +
			"(same_family). Manifest-check (F6) is not built. There is no brief: the job's input is the daemon's.",
	}, func(_ context.Context, _ *sdk.CallToolRequest, in JudgeStartInput) (*sdk.CallToolResult, JobView, error) {
		j, err := d.Manager.StartGate(gateRequest(jobs.KindJudge, in.Repo, in.SHA, in.ChecklistID, in.GateRunInput))
		if err != nil {
			return nil, JobView{}, fmt.Errorf("judge_start rejected: %w", err)
		}
		return nil, view(d.Manager, j), nil
	})
}

// GateGetInput is slicing_get's and judge_get's input.
type GateGetInput struct {
	Repo        string `json:"repo" jsonschema:"absolute path of the project's git work tree"`
	ChecklistID string `json:"checklist_id,omitempty" jsonschema:"default: the id of the repository's checklist"`
	SHA         string `json:"sha,omitempty" jsonschema:"judge_get only: the verdict for this commit (default: the latest verdict of any commit)"`
}

// SlicingGetOutput is slicing_get's output.
type SlicingGetOutput struct {
	ChecklistID string `json:"checklist_id"`
	// Version and SHA256 are the checklist's as it is now.
	Version int    `json:"version"`
	SHA256  string `json:"sha256"`
	*jobs.SlicingView
}

// gateChecklist resolves the repo argument and the checklist id (the work tree's when not given).
func gateChecklist(m *jobs.Manager, repo, id string) (top, main, cid string, err error) {
	if top, main, err = jobs.ChecklistRepo(repo); err != nil {
		return "", "", "", err
	}
	if cid = strings.TrimSpace(id); cid == "" {
		if cid, err = m.ChecklistIDOf(top); err != nil {
			return "", "", "", err
		}
	}
	return top, main, cid, nil
}

// SlicingGet is slicing_get (and `agent-jobs gate slicing`).
func SlicingGet(m *jobs.Manager, repo, id string) (SlicingGetOutput, error) {
	top, main, id, err := gateChecklist(m, repo, id)
	if err != nil {
		return SlicingGetOutput{}, err
	}
	out := SlicingGetOutput{ChecklistID: id}
	if v, err := currentChecklist(m, top, main); err == nil && v.ID == id {
		out.Version, out.SHA256 = v.Version, v.SHA256
	}
	sv, err := m.SlicingOf(main, id, out.Version, out.SHA256)
	if err != nil {
		return SlicingGetOutput{}, err
	}
	if sv == nil {
		sv = &jobs.SlicingView{}
	}
	out.SlicingView = sv
	return out, nil
}

// JudgeGet is judge_get (and `agent-jobs gate verdict`).
func JudgeGet(m *jobs.Manager, repo, id, sha string) (jobs.VerdictView, error) {
	_, main, id, err := gateChecklist(m, repo, id)
	if err != nil {
		return jobs.VerdictView{}, err
	}
	v, err := m.LatestVerdictOf(main, id, strings.TrimSpace(sha))
	if err != nil {
		return jobs.VerdictView{}, err
	}
	return *v, nil
}

func addSlicingGet(s *sdk.Server, d Deps) {
	addManagerTool(s, d, &sdk.Tool{
		Name: "slicing_get",
		Description: "The slicing check (F3) of the repo's checklist: the result for its current version and hash (summary.current true) " +
			"or else the latest one (an older version: it does not count for the approval), its three lists (unmapped, half, invented), " +
			"the owner's decisions on unmapped quotes and the quotes still undecided.",
	}, func(_ context.Context, _ *sdk.CallToolRequest, in GateGetInput) (*sdk.CallToolResult, SlicingGetOutput, error) {
		if in.SHA != "" {
			return nil, SlicingGetOutput{}, fmt.Errorf("slicing_get: sha does not apply")
		}
		out, err := SlicingGet(d.Manager, in.Repo, in.ChecklistID)
		return nil, out, err
	})
}

func addJudgeGet(s *sdk.Server, d Deps) {
	addManagerTool(s, d, &sdk.Tool{
		Name: "judge_get",
		Description: "The latest stored verdict of the blind judge (F5) for the repo's checklist (for sha when given): per requirement " +
			"verdict, how and evidence, the commands run, unmapped_brief, extra_scope, suspect_tests, the judge model and family, the " +
			"coders' families with family_differs / same_family, and current (the verdict is for the latest approved checklist). " +
			"manifest_check is null: F6 is not built.",
	}, func(_ context.Context, _ *sdk.CallToolRequest, in GateGetInput) (*sdk.CallToolResult, jobs.VerdictView, error) {
		v, err := JudgeGet(d.Manager, in.Repo, in.ChecklistID, in.SHA)
		return nil, v, err
	})
}
