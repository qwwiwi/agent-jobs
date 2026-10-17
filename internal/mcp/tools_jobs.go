package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
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
// only listed (lstat, regular files only), never read; report.md is the one file read (readCapped).
var artifactFiles = []string{"report.md", "diffstat.txt", "commits.patch", "staged.patch", "uncommitted.patch",
	"untracked.tar", "stream.jsonl", "stderr.log"}

// JobView is one job row as the tools return it.
type JobView struct {
	ID             string  `json:"id"`
	Title          string  `json:"title"`
	Kind           string  `json:"kind"`
	Status         string  `json:"status"`
	Model          string  `json:"model"`
	Complexity     string  `json:"complexity,omitempty"`
	Runner         string  `json:"runner"`
	Billing        string  `json:"billing"`
	MaxTurns       int     `json:"max_turns"`
	MaxBudgetUSD   float64 `json:"max_budget_usd"`
	TimeoutMinutes int     `json:"timeout_minutes"`
	Turns          int     `json:"turns"`
	CostUSD        float64 `json:"cost_usd"`
	DurationSec    int     `json:"duration_sec"`
	RepoPath       string  `json:"repo_path,omitempty"`
	BaseCommit     string  `json:"base_commit,omitempty"`
	CreatedAt      string  `json:"created_at"`
	StartedAt      string  `json:"started_at,omitempty"`
	FinishedAt     string  `json:"finished_at,omitempty"`
	NotifiedAt     string  `json:"notified_at,omitempty"`
	CleanedAt      string  `json:"cleaned_at,omitempty"`
	OwnerWaits     bool    `json:"owner_waits"`
	ResumedFrom    string  `json:"resumed_from,omitempty"`
	ScheduleID     string  `json:"schedule_id,omitempty"`
	Error          string  `json:"error,omitempty"`
	JobDir         string  `json:"job_dir"`
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
		TimeoutMinutes: j.TimeoutMinutes, Turns: j.Turns, CostUSD: j.CostUSD, DurationSec: j.DurationSec,
		RepoPath: j.RepoPath, BaseCommit: j.BaseCommit, CreatedAt: fmtTS(&j.CreatedAt),
		StartedAt: fmtTS(j.StartedAt), FinishedAt: fmtTS(j.FinishedAt), NotifiedAt: fmtTS(j.NotifiedAt),
		CleanedAt: fmtTS(j.CleanedAt), OwnerWaits: j.OwnerWaits, ResumedFrom: j.ResumedFrom,
		ScheduleID: j.ScheduleID, Error: j.Error, JobDir: m.JobDir(j.ID),
	}
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
	Title          string  `json:"title" jsonschema:"short title; shown in the job-done line"`
	Kind           string  `json:"kind" jsonschema:"review | fix | research | general (sets the checklist and whether web tools are open: only research)"`
	Brief          string  `json:"brief" jsonschema:"the full brief, written for a stranger: goal, boundaries, what counts as done, what not to do"`
	Model          string  `json:"model,omitempty" jsonschema:"explicit model for claude --model (sonnet, opus, fable or a full id); give this or complexity"`
	Complexity     string  `json:"complexity,omitempty" jsonschema:"light | normal | hard; picks the model by kind when no model is given; give this or model"`
	MaxTurns       int     `json:"max_turns,omitempty" jsonschema:"turn limit; default 80, ceiling 200"`
	TimeoutMinutes int     `json:"timeout_minutes,omitempty" jsonschema:"wall-clock limit in minutes; default 45, ceiling 180"`
	MaxBudgetUSD   float64 `json:"max_budget_usd,omitempty" jsonschema:"spend cap in USD; default 10, ceiling 50"`
	Repo           string  `json:"repo,omitempty" jsonschema:"absolute path of a git repository; the job works in a detached worktree of it. Omit for an empty work dir"`
	Base           string  `json:"base,omitempty" jsonschema:"ref in repo to start from; default HEAD"`
}

func addJobStart(s *sdk.Server, d Deps) {
	sdk.AddTool(s, &sdk.Tool{
		Name: "job_start",
		Description: "Queue a background claude -p job and return its id immediately. The daemon starts it within a minute " +
			"(at most 2 running per agent). The job commits locally only; it never pushes or messages anyone. " +
			"State an explicit model or a complexity (light|normal|hard); with neither the call is rejected. " +
			"Limits default to 80 turns / $10 / 45 min; above 200 / $50 / 180 they are rejected, never clamped.",
	}, func(_ context.Context, _ *sdk.CallToolRequest, in JobStartInput) (*sdk.CallToolResult, JobView, error) {
		j, err := d.Manager.Enqueue(jobs.StartRequest{
			Title: in.Title, Kind: in.Kind, Brief: in.Brief, RepoPath: in.Repo, Base: in.Base, Model: in.Model,
			Complexity: in.Complexity, MaxTurns: in.MaxTurns, MaxBudgetUSD: in.MaxBudgetUSD, TimeoutMinutes: in.TimeoutMinutes,
		})
		if err != nil {
			return nil, JobView{}, fmt.Errorf("job_start rejected: %w", err)
		}
		return nil, view(d.Manager, j), nil
	})
}

// JobIDInput is the input of the tools that take one job id.
type JobIDInput struct {
	ID string `json:"id" jsonschema:"job id as returned by job_start"`
}

func addJobStatus(s *sdk.Server, d Deps) {
	sdk.AddTool(s, &sdk.Tool{
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
	Error    string `json:"error,omitempty"`
	// Report is report.md (Summary, Findings / Changes, Evidence, Not done / uncertain).
	Report          string   `json:"report,omitempty"`
	ReportMissing   bool     `json:"report_missing,omitempty"`
	ReportTruncated bool     `json:"report_truncated,omitempty"`
	Artifacts       []string `json:"artifacts"`
	Note            string   `json:"note,omitempty"`
}

// errNotRegular marks a job file that is not a regular file (FIFO, device, socket, directory, symlink).
var errNotRegular = errors.New("not a regular file")

// readCapped reads at most max bytes of path; truncated reports whether there was more. The job dir
// is writable by the job, so the file is opened without following a symlink and without blocking
// (a FIFO would otherwise hang the open forever), the opened fd is checked to be a regular file,
// and only then read.
func readCapped(path string, max int) (string, bool, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return "", false, fmt.Errorf("%s: %w (symlink)", path, errNotRegular)
		}
		return "", false, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", false, err
	}
	if !fi.Mode().IsRegular() {
		return "", false, fmt.Errorf("%s: %w (%s)", path, errNotRegular, fi.Mode().Type())
	}
	b, err := io.ReadAll(io.LimitReader(f, int64(max)+1))
	if err != nil {
		return "", false, err
	}
	if len(b) > max {
		return string(b[:max]), true, nil
	}
	return string(b), false, nil
}

func addJobResult(s *sdk.Server, d Deps) {
	sdk.AddTool(s, &sdk.Tool{
		Name: "job_result",
		Description: "The report of a finished job (report.md) with its job-done line and the paths of what it changed " +
			"(patches, diffstat, logs). Read \"Not done / uncertain\" first; the report is a claim, check the diff. " +
			"For a queued or running job it returns the status only.",
	}, func(_ context.Context, _ *sdk.CallToolRequest, in JobIDInput) (*sdk.CallToolResult, JobResultOutput, error) {
		m := d.Manager
		j, err := getJob(m, in.ID)
		if err != nil {
			return nil, JobResultOutput{}, err
		}
		out := JobResultOutput{ID: j.ID, Status: j.Status, Finished: finished(j.Status), Error: j.Error, Artifacts: []string{}}
		if !out.Finished {
			out.Note = "job is " + j.Status + "; the session gets a [jobs:job-done] line when it ends"
			return nil, out, nil
		}
		out.DoneLine = jobs.DoneLine(j)
		dir := m.JobDir(j.ID)
		for _, f := range artifactFiles {
			if fi, err := os.Lstat(filepath.Join(dir, f)); err == nil && fi.Mode().IsRegular() {
				out.Artifacts = append(out.Artifacts, filepath.Join(dir, f))
			}
		}
		report, truncated, err := readCapped(filepath.Join(dir, "report.md"), MaxReportBytes)
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
			out.Report, out.ReportTruncated = report, truncated
			if truncated {
				out.Note = fmt.Sprintf("report cut at %d bytes; full file: %s", MaxReportBytes, filepath.Join(dir, "report.md"))
			}
		}
		return nil, out, nil
	})
}

// JobListInput is job_list's input.
type JobListInput struct {
	Limit  int    `json:"limit,omitempty" jsonschema:"max rows, 1..200; default 20"`
	Status string `json:"status,omitempty" jsonschema:"only jobs with this status: queued | running | succeeded | failed | timeout | cancelled"`
}

// JobListOutput is job_list's output, newest first.
type JobListOutput struct {
	Jobs []JobView `json:"jobs"`
}

var statuses = []string{store.StatusQueued, store.StatusRunning, store.StatusSucceeded, store.StatusFailed,
	store.StatusTimeout, store.StatusCancelled}

func addJobList(s *sdk.Server, d Deps) {
	sdk.AddTool(s, &sdk.Tool{
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
	sdk.AddTool(s, &sdk.Tool{
		Name: "job_cancel",
		Description: "Cancel a queued or running job (a running job's process group gets SIGTERM, then SIGKILL after 10 s). " +
			"A finished job cannot be cancelled.",
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
