package mcp

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// Thresholds at which system_doctor calls the disk a problem.
const (
	DiskWarnUsedPct   = 90.0
	DiskWarnFreeBytes = 2 << 30
)

// SystemDoctorInput is system_doctor's input (it takes none).
type SystemDoctorInput struct{}

// SystemDoctorOutput is system_doctor's output.
type SystemDoctorOutput struct {
	OK       bool             `json:"ok"`
	Problems []string         `json:"problems"`
	Disk     jobs.DiskStatus  `json:"disk"`
	Queue    jobs.QueueStatus `json:"queue"`
	Procs    jobs.ProcsReport `json:"processes"`
	Hooks    []jobs.Check     `json:"hooks"`
	// Daily is the daily automation (prune + doctor procs, report only) and its last run.
	Daily jobs.DailyStatus `json:"daily"`
	// Undelivered are notify_owner notifications of the last 24 h that did not reach the session.
	Undelivered []*store.Notification `json:"undelivered_notifications"`
	// MissedChecks are the checks (dead-man's switch) whose latest expected time passed its grace
	// without a mark (a late mark keeps them here), and those whose missed alert is still pending,
	// with the alert's delivery state (pending, delivered, failed).
	MissedChecks []jobs.CheckView `json:"missed_checks"`
	// OwnerPending are owner-waits jobs whose result the owner has not been told yet (owner_replied
	// clears them; they expire after owner_waits_max_hours). They hold the session's Stop hook.
	OwnerPending []*store.OwnerPending `json:"owner_reply_pending"`
	// OwnerHookSession is the orchestrator session the Stop hook holds (`agent-jobs hook bind` from
	// its SessionStart hook); "" when none is bound, and then the hook holds no session.
	OwnerHookSession string `json:"owner_hook_session"`
	// SkillWarnings are skill catalog roots or entries (config skill_roots) that did not load: warnings,
	// not problems (jobs still start, without those skills).
	SkillWarnings []string `json:"skill_warnings,omitempty"`
}

func addSystemDoctor(s *sdk.Server, d Deps) {
	addManagerTool(s, d, &sdk.Tool{
		Name: "system_doctor",
		Description: "Health of this agent's job system: disk (free space, what prune would free), the queue, " +
			"processes that outlived their job or session and running rows with a dead pid (doctor procs, report only: " +
			"nothing is killed here), and the hooks every job depends on (the guard blocks git push and allows ls, " +
			"owner rules readable, notify target exists), and the last daily run of prune and doctor procs (time and " +
			"counts: jobs pruned, bytes freed, archived, ghosts found), and notify_owner notifications of the last 24 h that were " +
			"not delivered to the session, and dead-man's switch checks whose latest expected time passed without a mark " +
			"(also when a late mark came) or whose missed alert is pending or failed, and the owner-waits jobs whose result the owner has not been told yet " +
			"(owner_reply_pending; not a problem, owner_replied clears them) with the session their Stop hook holds " +
			"(owner_hook_session; empty: none bound), and skill_warnings for skill catalog roots or entries that did not load. " +
			"ok=false lists the problems.",
	}, func(_ context.Context, _ *sdk.CallToolRequest, _ SystemDoctorInput) (*sdk.CallToolResult, SystemDoctorOutput, error) {
		m := d.Manager
		out := SystemDoctorOutput{Problems: []string{}, Disk: m.Disk(), Hooks: m.Hooks(), SkillWarnings: m.SkillWarnings()}
		var err error
		if out.Queue, err = m.Queue(); err != nil {
			return nil, out, err
		}
		if out.Procs, err = m.DoctorProcs(false); err != nil {
			return nil, out, fmt.Errorf("doctor procs: %w", err)
		}
		if out.Undelivered, err = m.UndeliveredNotifications(); err != nil {
			out.Problems = append(out.Problems, "owner notifications: "+err.Error())
		} else if n := len(out.Undelivered); n > 0 {
			last := out.Undelivered[n-1]
			out.Problems = append(out.Problems, fmt.Sprintf("%d owner notifications not delivered in the last 24 h (latest #%d: %s)",
				n, last.ID, last.Error))
		}
		if out.MissedChecks, err = m.MissedChecks(); err != nil {
			out.Problems = append(out.Problems, "checks: "+err.Error())
		}
		for _, v := range out.MissedChecks {
			out.Problems = append(out.Problems, "check "+jobs.CheckViewText(v))
		}
		if out.OwnerPending, _, err = m.OwnerPending(); err != nil {
			out.Problems = append(out.Problems, "owner reply pending: "+err.Error())
		}
		if out.OwnerHookSession, _, err = m.Store.HookSession(); err != nil {
			out.Problems = append(out.Problems, "owner hook session: "+err.Error())
		}
		if out.Daily, err = m.Daily(); err != nil {
			out.Problems = append(out.Problems, "daily: "+err.Error())
		}
		if r := out.Daily.Result; r != nil {
			for _, e := range []struct{ name, text string }{{"prune", r.PruneError}, {"doctor procs", r.ProcsError}, {"run", r.Error}} {
				if e.text != "" {
					out.Problems = append(out.Problems, fmt.Sprintf("daily %s %s: %s", out.Daily.LastDay, e.name, e.text))
				}
			}
		}
		switch {
		case out.Disk.Error != "":
			out.Problems = append(out.Problems, "disk: "+out.Disk.Error)
		case out.Disk.UsedPct >= DiskWarnUsedPct || out.Disk.FreeBytes < DiskWarnFreeBytes:
			out.Problems = append(out.Problems, fmt.Sprintf("disk: %.1f%% used, %d bytes free (prune would free %d)",
				out.Disk.UsedPct, out.Disk.FreeBytes, out.Disk.PrunableBytes))
		}
		if out.Disk.PruneError != "" {
			out.Problems = append(out.Problems, "prune dry run failed (prunable bytes may be low): "+out.Disk.PruneError)
		}
		for _, c := range out.Hooks {
			if !c.OK {
				out.Problems = append(out.Problems, c.Name+": "+c.Detail)
			}
		}
		jobGhosts, leaders := 0, 0
		for _, g := range out.Procs.Ghosts {
			switch g.Kind {
			case jobs.GhostJob:
				jobGhosts++
			case jobs.GhostLeader:
				leaders++
			}
		}
		if jobGhosts > 0 {
			out.Problems = append(out.Problems, fmt.Sprintf("%d processes outlived their job (agent-jobs doctor procs --kill ends them)", jobGhosts))
		}
		if leaders > 0 {
			out.Problems = append(out.Problems, fmt.Sprintf("%d daemonized processes carry a finished job's marker "+
				"(session leaders: never killed automatically, check them by hand with agent-jobs doctor procs)", leaders))
		}
		if n := len(out.Procs.DeadRows); n > 0 {
			out.Problems = append(out.Problems, fmt.Sprintf("%d running rows with a dead wrapper", n))
		}
		out.OK = len(out.Problems) == 0
		return nil, out, nil
	})
}

// SystemLogsInput is system_logs' input.
type SystemLogsInput struct {
	Lines int    `json:"lines,omitempty" jsonschema:"how many last lines, 1..2000; default 100"`
	JobID string `json:"job_id,omitempty" jsonschema:"a job id: return that job's stderr.log instead of the daemon log"`
}

// SystemLogsOutput is system_logs' output.
type SystemLogsOutput struct {
	Source string `json:"source"`
	Text   string `json:"text"`
}

func addSystemLogs(s *sdk.Server, d Deps) {
	addManagerTool(s, d, &sdk.Tool{
		Name: "system_logs",
		Description: "The last lines of the daemon's log (launch, collect, notify, cleanup, prune, refusals): " +
			"<state>/daemon.log under launchd, else the systemd user journal of agent-jobs@<agent>. " +
			"With job_id: that job's stderr.log, the first place to look when a job failed without a report.",
	}, func(_ context.Context, _ *sdk.CallToolRequest, in SystemLogsInput) (*sdk.CallToolResult, SystemLogsOutput, error) {
		n := in.Lines
		if n == 0 {
			n = jobs.DefaultLogLines
		}
		if n < 1 || n > jobs.MaxLogLines {
			return nil, SystemLogsOutput{}, fmt.Errorf("lines %d is outside 1..%d", n, jobs.MaxLogLines)
		}
		if in.JobID != "" {
			j, err := getJob(d.Manager, in.JobID)
			if err != nil {
				return nil, SystemLogsOutput{}, err
			}
			p := filepath.Join(d.Manager.JobDir(j.ID), "stderr.log")
			t, err := jobs.TailFile(p, n)
			if err != nil {
				return nil, SystemLogsOutput{}, fmt.Errorf("job %s stderr.log: %w", j.ID, err)
			}
			return nil, SystemLogsOutput{Source: p, Text: t}, nil
		}
		src, t, err := d.Manager.DaemonLog(n)
		if err != nil {
			return nil, SystemLogsOutput{}, err
		}
		return nil, SystemLogsOutput{Source: src, Text: t}, nil
	})
}

// ActivityReportInput is activity_report's input.
type ActivityReportInput struct {
	Hours int `json:"hours,omitempty" jsonschema:"window in hours, 1..720; default 24"`
}

// ActivityReportOutput is activity_report's output.
type ActivityReportOutput struct {
	jobs.Activity
	Jobs []JobView `json:"jobs"`
}

// MaxActivityHours bounds activity_report's window (30 days).
const MaxActivityHours = 720

func addActivityReport(s *sdk.Server, d Deps) {
	addManagerTool(s, d, &sdk.Tool{
		Name: "activity_report",
		Description: "What this agent's jobs did over a window (default the last 24 hours): jobs started and finished, " +
			"counts by status, cost, turns and time of the finished ones, and the jobs themselves (queued and running " +
			"ones always included), newest first.",
	}, func(_ context.Context, _ *sdk.CallToolRequest, in ActivityReportInput) (*sdk.CallToolResult, ActivityReportOutput, error) {
		h := in.Hours
		if h == 0 {
			h = 24
		}
		if h < 1 || h > MaxActivityHours {
			return nil, ActivityReportOutput{}, fmt.Errorf("hours %d is outside 1..%d", h, MaxActivityHours)
		}
		a, err := d.Manager.ActivityReport(time.Duration(h) * time.Hour)
		if err != nil {
			return nil, ActivityReportOutput{}, err
		}
		out := ActivityReportOutput{Activity: a, Jobs: make([]JobView, 0, len(a.Jobs))}
		for _, j := range a.Jobs {
			out.Jobs = append(out.Jobs, view(d.Manager, j))
		}
		return nil, out, nil
	})
}
