package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// Job list limits.
const (
	DefaultListLimit = 20
	MaxListLimit     = 200
)

// MaxReportBytes caps the report returned by job_result; the full file stays in the job dir.
const MaxReportBytes = 256 << 10

// artifactFiles are the files job_result points at when they exist (what the job changed). They are
// only listed (lstat, regular files only), never read; report.md is the one file read (ReadJobReport).
var artifactFiles = []string{"report.md", "diffstat.txt", "commits.patch", "staged.patch", "uncommitted.patch",
	"untracked.tar", "stream.jsonl", "stderr.log", jobs.AcceptanceLog}

// JobView is one job row as the tools return it.
type JobView struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Kind       string `json:"kind"`
	Status     string `json:"status"`
	Model      string `json:"model"`
	Complexity string `json:"complexity,omitempty"`
	Runner     string `json:"runner"`
	// ReasoningEffort is the job's reasoning effort (codex model_reasoning_effort or claude --effort);
	// empty for a claude job from before it was recorded.
	ReasoningEffort string  `json:"reasoning_effort,omitempty"`
	Billing         string  `json:"billing"`
	MaxTurns        int     `json:"max_turns"`
	MaxBudgetUSD    float64 `json:"max_budget_usd"`
	TimeoutMinutes  int     `json:"timeout_minutes"`
	Turns           int     `json:"turns"`
	CostUSD         float64 `json:"cost_usd"`
	// CostUnknown: the cost was never reported (a codex job, a killed attempt); cost_usd is not a cost then.
	CostUnknown bool `json:"cost_unknown"`
	// Limits is "n/a (codex)" for a codex job: max_turns and max_budget_usd (0) do not apply, the
	// timeout is the only limit.
	Limits      string `json:"limits,omitempty"`
	DurationSec int    `json:"duration_sec"`
	RepoPath    string `json:"repo_path,omitempty"`
	BaseCommit  string `json:"base_commit,omitempty"`
	CreatedAt   string `json:"created_at"`
	StartedAt   string `json:"started_at,omitempty"`
	FinishedAt  string `json:"finished_at,omitempty"`
	NotifiedAt  string `json:"notified_at,omitempty"`
	CleanedAt   string `json:"cleaned_at,omitempty"`
	OwnerWaits  bool   `json:"owner_waits"`
	ResumedFrom string `json:"resumed_from,omitempty"`
	ScheduleID  string `json:"schedule_id,omitempty"`
	Error       string `json:"error,omitempty"`
	JobDir      string `json:"job_dir"`
	SessionID   string `json:"session_id,omitempty"`
	Attempt     int    `json:"attempt"`
	RunMode     string `json:"run_mode,omitempty"`
	// SizeWarning (job_start only): the brief names many files or has many "## " headings, so it may
	// be more than one job.
	SizeWarning string `json:"size_warning,omitempty"`
	// Containment: "scope <unit>" (", stop pending" while a failed stop is retried) or "session only"
	// (the weaker fallback); empty before launch.
	Containment string `json:"containment,omitempty"`
	// AcceptanceCmd is the command the job must pass after claude exits 0 (empty = none).
	AcceptanceCmd string `json:"acceptance_cmd,omitempty"`
	// Skills are the catalog skills selected at job_start; SkillsDelivered those linked into the work
	// dir for the current attempt; SkillsNote why a selected one was not (repository wins, codex).
	Skills          []string `json:"skills,omitempty"`
	SkillsDelivered []string `json:"skills_delivered,omitempty"`
	SkillsNote      string   `json:"skills_note,omitempty"`
}

func fmtTS(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func view(m *jobs.Manager, j *store.Job) JobView {
	return JobView{
		ID: j.ID, Title: j.Title, Kind: j.Kind, Status: j.Status, Model: j.Model, Complexity: j.Complexity,
		Runner: j.Runner, Billing: j.Billing, MaxTurns: j.MaxTurns, MaxBudgetUSD: j.MaxBudgetUSD,
		TimeoutMinutes: j.TimeoutMinutes, Turns: j.Turns, CostUSD: j.CostUSD, CostUnknown: j.CostIsUnknown(),
		Limits: limitsNote(j), DurationSec: j.DurationSec,
		RepoPath: j.RepoPath, BaseCommit: j.BaseCommit, CreatedAt: fmtTS(&j.CreatedAt),
		StartedAt: fmtTS(j.StartedAt), FinishedAt: fmtTS(j.FinishedAt), NotifiedAt: fmtTS(j.NotifiedAt),
		CleanedAt: fmtTS(j.CleanedAt), OwnerWaits: j.OwnerWaits, ResumedFrom: j.ResumedFrom,
		ScheduleID: j.ScheduleID, Error: j.Error, JobDir: m.JobDir(j.ID), Containment: jobs.Containment(j),
		SessionID: j.SessionID, Attempt: j.Attempt, RunMode: j.RunMode, ReasoningEffort: j.ReasoningEffort,
		AcceptanceCmd: j.AcceptanceCmd, Skills: j.Skills, SkillsDelivered: j.SkillsDelivered, SkillsNote: j.SkillsNote,
	}
}

// limitsNote is JobView.Limits: "n/a (codex)" for a codex job, "" otherwise.
func limitsNote(j *store.Job) string {
	if j.Runner == store.RunnerCodex {
		return jobs.CodexLimitsNA
	}
	return ""
}

func finished(status string) bool {
	return status != store.StatusQueued && status != store.StatusRunning
}

// getJob loads a job by id with a readable not-found error.
func getJob(m *jobs.Manager, id string) (*store.Job, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("id is required")
	}
	j, err := m.Store.Get(id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("job %q not found", id)
	}
	return j, err
}

// JobStartInput is job_start's input. The caller states an explicit model or a complexity (the
// kind × complexity table picks the model); with neither the call is rejected (owner 2026-10-17:
// no classifier, no guessing). Limits follow the defaults and ceilings.
type JobStartInput struct {
	Title           string   `json:"title" jsonschema:"short title; shown in the job-done line"`
	Kind            string   `json:"kind" jsonschema:"review | fix | research | general (sets the checklist and whether web tools are open: only research)"`
	Brief           string   `json:"brief" jsonschema:"the full brief, written for a stranger: goal, boundaries, what counts as done, what not to do"`
	Model           string   `json:"model,omitempty" jsonschema:"explicit model: for claude --model (sonnet, opus, fable or a full id), or a gpt-* model, which runs the job on codex exec; give this or complexity"`
	Complexity      string   `json:"complexity,omitempty" jsonschema:"light | normal | hard; picks the model by kind when no model is given; give this or model"`
	MaxTurns        int      `json:"max_turns,omitempty" jsonschema:"turn limit; default 80, ceiling 200"`
	TimeoutMinutes  int      `json:"timeout_minutes,omitempty" jsonschema:"wall-clock limit in minutes; default 45, ceiling 180"`
	MaxBudgetUSD    float64  `json:"max_budget_usd,omitempty" jsonschema:"spend cap in USD; default 10, ceiling 50"`
	Repo            string   `json:"repo,omitempty" jsonschema:"absolute path of a git repository; the job works in a detached worktree of it. Omit for an empty work dir"`
	Base            string   `json:"base,omitempty" jsonschema:"ref in repo to start from; default HEAD"`
	Runner          string   `json:"runner,omitempty" jsonschema:"claude | codex; default: codex for a gpt-* model, else claude. Codex jobs: kind review or research only (read-only sandbox) unless the config allows writes; no turn or budget caps (the timeout is the limit), cost unknown, no resume, no guard hook"`
	ReasoningEffort string   `json:"reasoning_effort,omitempty" jsonschema:"reasoning effort, recorded on the job. Claude: low | medium | high | xhigh | max, passed as --effort (a repository's effortLevel cannot change it); default from config effort_by_kind, else high (general: medium). Codex: low | medium | high; default from config codex_reasoning_effort, else high"`
	AcceptanceCmd   string   `json:"acceptance_cmd,omitempty" jsonschema:"fix and general jobs only, refused unless config acceptance_allowed is true: one-line shell command (max 1 KB) run in the job worktree after claude exits 0, outside the guard; non-zero or timeout (acceptance_timeout_minutes, default 15) fails the job with acceptance failed"`
	OwnerWaits      bool     `json:"owner_waits,omitempty" jsonschema:"true when the owner asked for this job and waits for its result: the job-done line ends with 'owner waits' and this session cannot end a turn silently until you write to the owner and call owner_replied"`
	Skills          []string `json:"skills,omitempty" jsonschema:"edits to the job's default skills from the skill catalog (core skills of the kind plus the repository's project skills): name or +name adds, -name removes; unknown or deprecated names, or more than the per-job count or size budget, refuse the start; a draft only by name. See skill_list"`
}

func addJobStart(s *sdk.Server, d Deps) {
	addManagerTool(s, d, &sdk.Tool{
		Name: "job_start",
		Description: "Queue a background claude -p job (or codex exec: a gpt-* model or runner codex) and return its id immediately. The daemon starts it within a minute " +
			"(at most max_running per agent run at once: config.json key max_running, default 2). The job commits locally only; it never pushes or messages anyone. " +
			"State an explicit model or a complexity (light|normal|hard); with neither the call is rejected. " +
			"Limits default to 80 turns / $10 / 45 min; above 200 / $50 / 180 they are rejected, never clamped.",
	}, func(_ context.Context, _ *sdk.CallToolRequest, in JobStartInput) (*sdk.CallToolResult, JobView, error) {
		j, err := d.Manager.Enqueue(jobs.StartRequest{
			Title: in.Title, Kind: in.Kind, Brief: in.Brief, RepoPath: in.Repo, Base: in.Base, Model: in.Model,
			Complexity: in.Complexity, MaxTurns: in.MaxTurns, MaxBudgetUSD: in.MaxBudgetUSD, TimeoutMinutes: in.TimeoutMinutes,
			Runner: in.Runner, ReasoningEffort: in.ReasoningEffort, AcceptanceCmd: in.AcceptanceCmd,
			OwnerWaits: in.OwnerWaits, Skills: in.Skills,
		})
		if err != nil {
			return nil, JobView{}, fmt.Errorf("job_start rejected: %w", err)
		}
		v := view(d.Manager, j)
		v.SizeWarning = d.Manager.Cfg.BriefSizeWarning(in.Brief)
		return nil, v, nil
	})
}

// JobIDInput is the input of the tools that take one job id.
type JobIDInput struct {
	ID string `json:"id" jsonschema:"job id as returned by job_start"`
}

func addJobStatus(s *sdk.Server, d Deps) {
	addManagerTool(s, d, &sdk.Tool{
		Name:        "job_status",
		Description: "Status, limits, usage and timestamps of one job. Does not include the report (use job_result).",
	}, func(_ context.Context, _ *sdk.CallToolRequest, in JobIDInput) (*sdk.CallToolResult, JobView, error) {
		j, err := getJob(d.Manager, in.ID)
		if err != nil {
			return nil, JobView{}, err
		}
		return nil, view(d.Manager, j), nil
	})
}

// JobResultOutput is job_result's output.
type JobResultOutput struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Finished bool   `json:"finished"`
	// DoneLine is the same line the session got when the job ended.
	DoneLine string `json:"done_line,omitempty"`
	// CostUnknown: the job's cost was never reported (a codex job, a killed attempt).
	CostUnknown bool   `json:"cost_unknown"`
	Error       string `json:"error,omitempty"`
	// NotDone is the report's "Not done / uncertain" section, read first.
	NotDone string `json:"not_done,omitempty"`
	// Progress is the report's "Progress" section (timestamped one-line steps), shown after NotDone;
	// empty when the report has none (not an error).
	Progress string `json:"progress,omitempty"`
	// Report is report.md (Summary, Findings / Changes, Evidence, Not done / uncertain, Progress),
	// secret-looking values redacted.
	Report          string   `json:"report,omitempty"`
	ReportMissing   bool     `json:"report_missing,omitempty"`
	ReportTruncated bool     `json:"report_truncated,omitempty"`
	Artifacts       []string `json:"artifacts"`
	Note            string   `json:"note,omitempty"`
	// Attempts lists every attempt of a resumed job, oldest first; the last one is the current row.
	Attempts []AttemptView `json:"attempts,omitempty"`
	// Acceptance is the acceptance command's outcome (jobs with acceptance_cmd only).
	Acceptance *AcceptanceView `json:"acceptance,omitempty"`
	// JobDirBytes is the size of the job folder last measured (when the job ended, after a prune; the
	// worktree of a repo job left out); JobDirPartial: the measurement stopped at its bound, the size
	// is a lower bound.
	JobDirBytes   int64 `json:"job_dir_bytes"`
	JobDirPartial bool  `json:"job_dir_partial,omitempty"`
}

// MaxAcceptanceTailBytes caps the acceptance log tail job_result returns; the log keeps 64 KB.
const MaxAcceptanceTailBytes = 16 << 10

// AcceptanceView is the acceptance command of a job and how it ended.
type AcceptanceView struct {
	Cmd string `json:"cmd"`
	// Ran is false when the command did not run (claude failed, or the job is not finished).
	Ran bool `json:"ran"`
	// Exit is the exit code (128+signal when killed by a signal); -1 with TimedOut.
	Exit        int  `json:"exit"`
	TimedOut    bool `json:"timed_out,omitempty"`
	DurationSec int  `json:"duration_sec"`
	// LogTail is the end of acceptance.log (stdout and stderr), at most MaxAcceptanceTailBytes, redacted like report.md.
	LogTail string `json:"log_tail,omitempty"`
}

// acceptanceView is the AcceptanceView of j, nil when it has no acceptance command.
func acceptanceView(m *jobs.Manager, j *store.Job) *AcceptanceView {
	if j.AcceptanceCmd == "" {
		return nil
	}
	v := &AcceptanceView{Cmd: j.AcceptanceCmd, DurationSec: j.AcceptanceDurationSec}
	if j.AcceptanceExit != nil {
		v.Ran, v.Exit, v.TimedOut = true, *j.AcceptanceExit, *j.AcceptanceExit == store.AcceptanceTimedOut
	}
	if tail, err := jobs.ReadAcceptanceLogTail(m.StateDir, j.ID, MaxAcceptanceTailBytes); err == nil {
		v.LogTail = tail
	}
	return v
}

func addJobResult(s *sdk.Server, d Deps) {
	addManagerTool(s, d, &sdk.Tool{
		Name: "job_result",
		Description: "The report of a finished job (report.md) with its job-done line and the paths of what it changed " +
			"(patches, diffstat, logs). Read \"Not done / uncertain\" first (not_done), then the job's steps (progress); " +
			"the report is a claim, check the diff. Secret-looking values are shown as [redacted:<kind>]. " +
			"For a queued or running job it returns the status only.",
	}, func(_ context.Context, _ *sdk.CallToolRequest, in JobIDInput) (*sdk.CallToolResult, JobResultOutput, error) {
		m := d.Manager
		j, err := getJob(m, in.ID)
		if err != nil {
			return nil, JobResultOutput{}, err
		}
		out := JobResultOutput{ID: j.ID, Status: j.Status, Finished: finished(j.Status), Error: jobs.RedactText(j.Error),
			CostUnknown: j.CostIsUnknown(), Artifacts: []string{}}
		if out.Attempts, err = attemptViews(m, j); err != nil {
			return nil, JobResultOutput{}, err
		}
		if !out.Finished {
			out.Note = "job is " + j.Status + "; the session gets a [jobs:job-done] line when it ends"
			return nil, out, nil
		}
		out.DoneLine = jobs.DoneLine(j, m.Cfg.JobDirNoteBytes())
		out.JobDirBytes, out.JobDirPartial = j.DirBytes, j.DirPartial
		out.Acceptance = acceptanceView(m, j)
		dir := m.JobDir(j.ID)
		for _, f := range artifactFiles {
			if fi, err := os.Lstat(filepath.Join(dir, f)); err == nil && fi.Mode().IsRegular() {
				out.Artifacts = append(out.Artifacts, filepath.Join(dir, f))
			}
		}
		report, truncated, err := ReadJobReport(m.StateDir, j.ID, MaxReportBytes)
		switch {
		case errors.Is(err, os.ErrNotExist):
			out.ReportMissing = true
			out.Note = "no report.md; see " + filepath.Join(dir, "stderr.log")
		case errors.Is(err, errNotRegular):
			out.ReportMissing = true
			out.Note = "report.md is not a regular file, not read: " + err.Error()
		case err != nil:
			return nil, JobResultOutput{}, fmt.Errorf("read report: %w", err)
		default:
			// Always redacted on the fly: the report of a job whose in-place redaction is still pending
			// (or of a running job) may hold raw secrets.
			out.Report, out.ReportTruncated = jobs.RedactRead(report, truncated), truncated
			secs := jobs.ReportSections(out.Report, jobs.SectionNotDone, jobs.SectionProgress)
			out.NotDone, out.Progress = secs[0], secs[1]
			if truncated {
				out.Note = fmt.Sprintf("report cut at %d bytes; full file: %s", MaxReportBytes, filepath.Join(dir, "report.md"))
			}
		}
		if j.Status == store.StatusStopped {
			out.Note = strings.TrimSpace("stopped: work dir and session kept; job_resume continues it, job_cancel gives it up. " + out.Note)
		}
		return nil, out, nil
	})
}

// JobListInput is job_list's input.
type JobListInput struct {
	Limit  int    `json:"limit,omitempty" jsonschema:"max rows, 1..200; default 20"`
	Status string `json:"status,omitempty" jsonschema:"only jobs with this status: queued | running | succeeded | failed | timeout | cancelled | stopped"`
}

// JobListOutput is job_list's output, newest first.
type JobListOutput struct {
	Jobs []JobView `json:"jobs"`
}

var statuses = []string{store.StatusQueued, store.StatusRunning, store.StatusSucceeded, store.StatusFailed,
	store.StatusTimeout, store.StatusCancelled, store.StatusStopped}

func addJobList(s *sdk.Server, d Deps) {
	addManagerTool(s, d, &sdk.Tool{
		Name:        "job_list",
		Description: "Recent jobs of this agent, newest first, optionally only one status.",
	}, func(_ context.Context, _ *sdk.CallToolRequest, in JobListInput) (*sdk.CallToolResult, JobListOutput, error) {
		limit := in.Limit
		if limit == 0 {
			limit = DefaultListLimit
		}
		if limit < 1 || limit > MaxListLimit {
			return nil, JobListOutput{}, fmt.Errorf("limit %d is outside 1..%d", limit, MaxListLimit)
		}
		var list []*store.Job
		var err error
		switch {
		case in.Status == "":
			list, err = d.Manager.Store.List(limit)
		case slices.Contains(statuses, in.Status):
			list, err = d.Manager.Store.ListRecentByStatus(in.Status, limit)
		default:
			return nil, JobListOutput{}, fmt.Errorf("status %q must be one of %s", in.Status, strings.Join(statuses, "|"))
		}
		if err != nil {
			return nil, JobListOutput{}, err
		}
		out := JobListOutput{Jobs: make([]JobView, 0, len(list))}
		for _, j := range list {
			out.Jobs = append(out.Jobs, view(d.Manager, j))
		}
		return nil, out, nil
	})
}

// JobCancelOutput is job_cancel's output.
type JobCancelOutput struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

func addJobCancel(s *sdk.Server, d Deps) {
	addManagerTool(s, d, &sdk.Tool{
		Name: "job_cancel",
		Description: "Cancel a queued or running job (a running job's process group gets SIGTERM, then SIGKILL after 10 s), " +
			"or give up a stopped one (its work dir is then cleaned up). A finished job cannot be cancelled. " +
			"To change course and keep the work, use job_stop + job_resume instead.",
	}, func(_ context.Context, _ *sdk.CallToolRequest, in JobIDInput) (*sdk.CallToolResult, JobCancelOutput, error) {
		j, err := getJob(d.Manager, in.ID)
		if err != nil {
			return nil, JobCancelOutput{}, err
		}
		if err := d.Manager.Cancel(j.ID); err != nil {
			if errors.Is(err, store.ErrImmutable) {
				if cur, gerr := d.Manager.Store.Get(j.ID); gerr == nil {
					return nil, JobCancelOutput{}, fmt.Errorf("job %s already finished (%s)", j.ID, cur.Status)
				}
			}
			return nil, JobCancelOutput{}, err
		}
		cur, err := d.Manager.Store.Get(j.ID)
		if err != nil {
			return nil, JobCancelOutput{}, err
		}
		return nil, JobCancelOutput{ID: cur.ID, Status: cur.Status}, nil
	})
}
