package mcp

import (
	"context"
	"fmt"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/store"
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

// BriefingStartInput is briefing_start's input: the repository and how to run; no brief.
type BriefingStartInput struct {
	Repo        string `json:"repo" jsonschema:"absolute path of the project's git work tree (brief/BRIEF.md and the draft brief/CHECKLIST.md are read there)"`
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
			"(brief passages no row points to), half (passages a row covers in part) and invented (rows with nothing in the " +
			"brief behind them) in a validated JSON " +
			"block of its final message, stored for the checklist id, version and hash. The check is advisory: checklist_set " +
			"approve does not need a result, but while a result for the current version lists an unmapped quote without an owner " +
			"decision, approve is refused. There is no brief: the job's input is the daemon's. A claude job is held to its input " +
			"by the guard (enforced_blind true); a codex job is held by the read-only codex sandbox, which stops writes and network, not reads (enforced_blind true under that sandbox, false under codex_sandbox worktree-full-access).",
	}, func(_ context.Context, _ *sdk.CallToolRequest, in SlicingStartInput) (*sdk.CallToolResult, JobView, error) {
		j, err := d.Manager.StartGate(gateRequest(jobs.KindSlicing, in.Repo, "", in.ChecklistID, in.GateRunInput))
		if err != nil {
			return nil, JobView{}, fmt.Errorf("slicing_start rejected: %w", err)
		}
		return nil, view(d.Manager, j), nil
	})
}

func addBriefingStart(s *sdk.Server, d Deps) {
	addManagerTool(s, d, &sdk.Tool{
		Name: "briefing_start",
		Description: "Queue the briefing (Wave F, F8) of the repo's draft checklist, before the owner approves it: a read-only job of " +
			"kind briefing that gets only brief/BRIEF.md and the checklist rows (read by the daemon now, approved or not; no code, " +
			"no briefs or reports) and returns the owner's question rounds in a validated JSON block of its final message: rounds " +
			"of at most 4 questions {n, closes, kind, q, recommended, why}, each closing named checklist rows, of kind " +
			"cost_or_vendor | owner_visible | costly_to_undo | business_rule, never asking for a secret; decided (the job's " +
			"ASSUMPTIONs, with the rows they close) and an optional adversarial pass of at most 7 questions. Stored for the " +
			"checklist id, version and hash; briefing_get reads it. The owner's replies go back through checklist_set. There is " +
			"no brief: the job's input is the daemon's. A claude job is held to its input by the guard (enforced_blind true); " +
			"a codex job is held by the read-only codex sandbox, which stops writes and network, not reads (enforced_blind true under that sandbox, false under codex_sandbox worktree-full-access).",
	}, func(_ context.Context, _ *sdk.CallToolRequest, in BriefingStartInput) (*sdk.CallToolResult, JobView, error) {
		j, err := d.Manager.StartGate(gateRequest(jobs.KindBriefing, in.Repo, "", in.ChecklistID, in.GateRunInput))
		if err != nil {
			return nil, JobView{}, fmt.Errorf("briefing_start rejected: %w", err)
		}
		return nil, view(d.Manager, j), nil
	})
}

func addJudgeStart(s *sdk.Server, d Deps) {
	addManagerTool(s, d, &sdk.Tool{
		Name: "judge_start",
		Description: "Queue the acceptance judge (Wave F, F5; kind judge, not acceptance_cmd) of the code at sha against the repo's " +
			"approved checklist: a read-only job that gets BRIEF.md in full, the checklist rows (ID, brief quote, contract, rationale) and a " +
			"copy of the files at sha (no git history, no agent settings or instructions, no job briefs, reports or diffs), assembled " +
			"by the daemon. It returns met | partial | missing | placeholder | cannot_check per live requirement with evidence in a " +
			"validated JSON block of its final message, stored by repo, sha, checklist id and version, judge model and family. The " +
			"judge may run on any model: its independence is that it is a separate blind job, not its model family; a judge of the " +
			"family of a coder (a succeeded fix or general job with covers of the checklist, size not small) is stored with " +
			"same_family true as information only (notes says so at the start). A claude judge is blind by enforcement (the " +
			"guard's read allowlist, enforced_blind true); a codex judge is held by the read-only codex sandbox, which stops " +
			"writes and network, not reads (enforced_blind true under that sandbox, false under codex_sandbox " +
			"worktree-full-access). Before the judge starts, its wrapper runs the repository's scripts/manifest-check.sh (F6) in a " +
			"separate checkout of sha (the job's containment, a scrubbed environment, no guard) and gives the judge its output; the " +
			"exit code and output tail are stored with the job and sha. It needs config acceptance_allowed true and the script at " +
			"sha: without either the job fails before the judge starts and nothing is recorded (warnings says so at the start). " +
			"There is no brief: the job's input is the daemon's.",
	}, func(_ context.Context, _ *sdk.CallToolRequest, in JudgeStartInput) (*sdk.CallToolResult, JobView, error) {
		j, err := d.Manager.StartGate(gateRequest(jobs.KindJudge, in.Repo, in.SHA, in.ChecklistID, in.GateRunInput))
		if err != nil {
			return nil, JobView{}, fmt.Errorf("judge_start rejected: %w", err)
		}
		v := view(d.Manager, j)
		n, err := d.Manager.JudgeFamilyNote(j)
		if err != nil {
			n = "the coders' model families could not be read: " + err.Error()
		}
		if n != "" {
			v.Notes = append(v.Notes, n)
		}
		v.Warnings = append(v.Warnings, d.Manager.ManifestCheckWarnings(j)...)
		return nil, v, nil
	})
}

// GateGetInput is slicing_get's, briefing_get's and judge_get's input.
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

// BriefingGetOutput is briefing_get's output.
type BriefingGetOutput struct {
	ChecklistID string `json:"checklist_id"`
	// Version and SHA256 are the checklist's as it is now.
	Version int    `json:"version"`
	SHA256  string `json:"sha256"`
	// Current is true when the result was run on that version and hash.
	Current bool                  `json:"current"`
	Result  *store.BriefingResult `json:"result"`
}

// BriefingGet is briefing_get (and `agent-jobs gate briefing`).
func BriefingGet(m *jobs.Manager, repo, id string) (BriefingGetOutput, error) {
	top, main, id, err := gateChecklist(m, repo, id)
	if err != nil {
		return BriefingGetOutput{}, err
	}
	out := BriefingGetOutput{ChecklistID: id}
	if v, err := currentChecklist(m, top, main); err == nil && v.ID == id {
		out.Version, out.SHA256 = v.Version, v.SHA256
	}
	bv, err := m.BriefingOf(main, id, out.Version, out.SHA256)
	if err != nil {
		return BriefingGetOutput{}, err
	}
	if bv != nil {
		out.Current, out.Result = bv.Current, bv.Result
	}
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
		Description: "The slicing check (F3) of the repo's checklist: the latest result for its current version and hash (summary.current " +
			"true) or else the latest one (an older version: it does not count for the approval), its three lists (unmapped, half, " +
			"invented), the union of the unmapped quotes of every result for that version and hash (unmapped; summary.results counts " +
			"them), the owner's decisions and the quotes still undecided, and enforced_blind (false for a codex job run outside " +
			"the read-only codex sandbox).",
	}, func(_ context.Context, _ *sdk.CallToolRequest, in GateGetInput) (*sdk.CallToolResult, SlicingGetOutput, error) {
		if in.SHA != "" {
			return nil, SlicingGetOutput{}, fmt.Errorf("slicing_get: sha does not apply")
		}
		out, err := SlicingGet(d.Manager, in.Repo, in.ChecklistID)
		return nil, out, err
	})
}

func addBriefingGet(s *sdk.Server, d Deps) {
	addManagerTool(s, d, &sdk.Tool{
		Name: "briefing_get",
		Description: "The briefing (F8) of the repo's checklist: the latest result for its current version and hash (current true) or " +
			"else the latest one of the checklist id (an older draft; result null when none): rounds of questions with the rows " +
			"each closes and the proposed answer, decided (ASSUMPTIONs), adversarial, the job, model and enforced_blind (false " +
			"for a codex job run outside the read-only codex sandbox). Put the rounds to the owner one at a time; write the replies with checklist_set.",
	}, func(_ context.Context, _ *sdk.CallToolRequest, in GateGetInput) (*sdk.CallToolResult, BriefingGetOutput, error) {
		if in.SHA != "" {
			return nil, BriefingGetOutput{}, fmt.Errorf("briefing_get: sha does not apply")
		}
		out, err := BriefingGet(d.Manager, in.Repo, in.ChecklistID)
		return nil, out, err
	})
}

func addJudgeGet(s *sdk.Server, d Deps) {
	addManagerTool(s, d, &sdk.Tool{
		Name: "judge_get",
		Description: "The latest stored verdict of the judge (F5) for the repo's checklist (for sha when given): per requirement " +
			"verdict, how and evidence, the commands run, unmapped_brief, extra_scope, suspect_tests, the judge model and family, " +
			"enforced_blind (true for a claude judge held by the guard and for a codex one under the read-only codex sandbox), " +
			"the families of the coders (the succeeded fix and general jobs with covers of the checklist, size not small) with " +
			"family_differs / same_family (information only; a snapshot taken when the verdict was stored), and " +
			"current (the verdict is for the latest approved checklist), and manifest_check: the record of the run of " +
			"scripts/manifest-check.sh the judge's wrapper made at sha before the judge started (exit_code, duration_sec, the " +
			"output tail).",
	}, func(_ context.Context, _ *sdk.CallToolRequest, in GateGetInput) (*sdk.CallToolResult, jobs.VerdictView, error) {
		v, err := JudgeGet(d.Manager, in.Repo, in.ChecklistID, in.SHA)
		return nil, v, err
	})
}

// ReleaseCheckInput is release_check's input.
type ReleaseCheckInput struct {
	Repo string `json:"repo" jsonschema:"absolute path of the project's git work tree with the approved brief/CHECKLIST.md"`
	SHA  string `json:"sha" jsonschema:"the full commit id to ship"`
}

// ReleaseCheck is release_check (and `agent-jobs gate release-check`).
func ReleaseCheck(m *jobs.Manager, repo, sha string) (jobs.ReleaseCheckResult, error) {
	r, err := m.ReleaseCheck(repo, sha)
	if err != nil {
		return jobs.ReleaseCheckResult{}, err
	}
	return *r, nil
}

func addReleaseCheck(s *sdk.Server, d Deps) {
	addManagerTool(s, d, &sdk.Tool{
		Name: "release_check",
		Description: "Whether the code at sha may ship under the repo's approved checklist (Wave F, F7). Its one write: a pass is recorded " +
			"for the merge hook (`agent-jobs hook merge-gate`, F9 (b)) and any other answer removes the pass for the repo and sha. " +
			"pass is true only " +
			"when every condition holds, and a missing, unreadable or ambiguous record fails its condition: approval (the latest " +
			"owner approval exists and the checklist hash is unchanged), manifest_check (the judge's manifest-check record for sha " +
			"exits 0), verdict (a judge verdict for sha at the approved checklist id and version; every verdict for them must agree), " +
			"requirements (each live requirement met, placeholder on a placeholder-ok row, or judged and carrying an owner waiver " +
			"bound to this checklist id, hash and version and the requirement, not to a commit), unmapped_brief and suspect_tests " +
			"(empty), enforced_blind (true). The judge may run on any model, the coders' too: judge_family, coder_families (of " +
			"the succeeded feature jobs, recomputed now; a feature job is a fix or general job of the repo whose covers name the " +
			"checklist id and whose size is not small), same_family and family_note are information, not a condition. " +
			"feature_jobs and covered_by list the feature jobs and their covers, informational only. " +
			"owner_line is " +
			"the one line for the owner: «Acceptance: N of M» or «Acceptance: N of M, mismatch: R03i — reason».",
	}, func(_ context.Context, _ *sdk.CallToolRequest, in ReleaseCheckInput) (*sdk.CallToolResult, jobs.ReleaseCheckResult, error) {
		out, err := ReleaseCheck(d.Manager, in.Repo, in.SHA)
		if err != nil {
			return nil, out, fmt.Errorf("release_check: %w", err)
		}
		return nil, out, nil
	})
}
