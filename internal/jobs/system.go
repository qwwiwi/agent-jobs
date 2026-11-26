package jobs

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// System checks behind the orchestrator tools system_doctor, system_logs and activity_report
// (reference, «MCP-тулы», group «Система»).

// DiskStatus is the free space of the filesystem that holds the agent's state dir.
type DiskStatus struct {
	Path       string  `json:"path"`
	TotalBytes uint64  `json:"total_bytes"`
	FreeBytes  uint64  `json:"free_bytes"` // available to this user
	UsedPct    float64 `json:"used_pct"`
	// PrunableBytes is an estimate from recorded sizes of what `prune` would remove now: the temp dirs
	// of jobs finished > job_tmp_retention_hours ago and the folders of jobs finished > 48 h ago whose
	// records are not archived yet (their kept records included). No directory is read for it and no
	// lock is taken; the exact figure is `agent-jobs prune --dry-run`.
	PrunableBytes int64 `json:"prunable_bytes"`
	// PruneOverdue is how many finished jobs still record a temp dir well past its retention (the
	// retention plus three temp dir passes): prune skips them or fails on them (a pending scope stop,
	// a mount point in the temp dir, openat2 unavailable, a worktree cleanup that has not run).
	// PruneError says so in words and names the oldest of them.
	PruneOverdue int    `json:"prune_overdue,omitempty"`
	PruneError   string `json:"prune_error,omitempty"`
	// LargestJobDirs are the MaxLargestJobDirs biggest job folders (worktrees of repo jobs left out).
	LargestJobDirs []JobDirUsage `json:"largest_job_dirs"`
	// JobTmpBytes is what all job temp dirs (<jobdir>/tmp, the jobs' TMPDIR) hold together.
	JobTmpBytes int64 `json:"job_tmp_bytes"`
	// JobDirsBytes is what all job folders hold together (worktrees of repo jobs left out): the sizes
	// recorded when each job ended or was pruned, and for running jobs a bounded walk made now.
	// JobDirsPartial: a walk stopped at its bound or was skipped for lack of time, so JobDirsBytes
	// and JobTmpBytes are lower bounds.
	JobDirsBytes   int64  `json:"job_dirs_bytes"`
	JobDirsPartial bool   `json:"job_dirs_partial,omitempty"`
	Error          string `json:"error,omitempty"`
}

// runningDirsBudget bounds the walks of all running jobs' folders in one disk status together (each
// walk has the bounds of jobDirSize besides).
var runningDirsBudget = 5 * time.Second

// MaxLargestJobDirs is how many job folders DiskStatus lists.
const MaxLargestJobDirs = 5

// JobDirUsage is the size of one job folder; TmpBytes is the part in its temp dir (JobTmpDir).
// Partial: the walk that measured it stopped at its bound, both are lower bounds.
type JobDirUsage struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Bytes    int64  `json:"bytes"`
	TmpBytes int64  `json:"tmp_bytes"`
	Partial  bool   `json:"partial,omitempty"`
}

// jobDirs fills the job folder part of the disk status: the largest folders, biggest first, the total
// of all folders and of their temp dirs, the prunable estimate and the overdue temp dirs. The sizes are those recorded on the rows (recordDirSize:
// when an attempt ended, after a prune); only the folder of a running job, which has no current
// record, is measured here (jobDirSize, a bounded read-only walk), all of them within
// runningDirsBudget.
func (m *Manager) jobDirs(d *DiskStatus) error {
	all, err := m.Store.List(-1) // negative: no limit
	if err != nil {
		return err
	}
	deadline := time.Now().Add(runningDirsBudget)
	now := m.Now()
	cutoff, tmpCutoff, overdueCutoff := now.Add(-PruneAge), m.tmpCutoff(), now.Add(-m.tmpOverdueAfter())
	var out []JobDirUsage
	var oldest *store.Job // the overdue job that finished first
	for _, j := range all {
		u := JobDirUsage{ID: j.ID, Status: j.Status, Bytes: j.DirBytes, TmpBytes: j.DirTmpBytes, Partial: j.DirPartial}
		d.JobDirsPartial = d.JobDirsPartial || j.DirPartial
		if slices.Contains(pruneStatuses, j.Status) && j.FinishedAt != nil {
			// What prune would take, from the row alone (no walk, no lifecycle lock).
			if !j.FinishedAt.After(cutoff) {
				if _, archived := m.existingArchiveDir(j); !archived {
					d.PrunableBytes += j.DirBytes
				}
			} else if !j.FinishedAt.After(tmpCutoff) {
				d.PrunableBytes += j.DirTmpBytes
			}
			if j.DirTmpBytes > 0 && j.FinishedAt.Before(overdueCutoff) {
				d.PruneOverdue++
				if oldest == nil || j.FinishedAt.Before(*oldest.FinishedAt) {
					oldest = j
				}
			}
		}
		if j.Status == store.StatusRunning {
			if time.Now().After(deadline) {
				d.JobDirsPartial = true
			} else if s, err := jobDirSize(j, m.JobDir(j.ID)); err == nil {
				u.Bytes, u.TmpBytes, u.Partial = s.Bytes, s.TmpBytes, s.Partial
				d.JobDirsPartial = d.JobDirsPartial || s.Partial
			}
		}
		d.JobDirsBytes += u.Bytes
		d.JobTmpBytes += u.TmpBytes
		if u.Bytes > 0 {
			out = append(out, u)
		}
	}
	slices.SortStableFunc(out, func(a, b JobDirUsage) int { return cmp.Compare(b.Bytes, a.Bytes) })
	if len(out) > MaxLargestJobDirs {
		out = out[:MaxLargestJobDirs]
	}
	if out != nil {
		d.LargestJobDirs = out
	}
	if oldest != nil {
		d.PruneError = fmt.Sprintf("%d finished jobs still hold their temp dir past its retention, the oldest is %s (finished %s): "+
			"prune skips them or fails on them; the reason is in the daemon log and in `agent-jobs prune --dry-run`",
			d.PruneOverdue, oldest.ID, oldest.FinishedAt.UTC().Format(time.RFC3339))
	}
	return nil
}

// tmpOverdueAfter is how long after its finish a job that still records a temp dir counts as overdue:
// the temp retention plus three passes, or with the short retention off, the folder's 48 h plus a day
// (the daily prune).
func (m *Manager) tmpOverdueAfter() time.Duration {
	if r := m.Config().JobTmpRetention(); r > 0 && r < PruneAge {
		return r + 3*TmpPruneInterval
	}
	return PruneAge + 24*time.Hour
}

// Disk reports the state dir's filesystem. It reads rows and measures running jobs only: it takes no
// lifecycle lock and opens no directory of a finished job (a prune dry run used to do both).
func (m *Manager) Disk() DiskStatus {
	d := DiskStatus{Path: m.StateDir, LargestJobDirs: []JobDirUsage{}}
	if err := m.jobDirs(&d); err != nil {
		d.Error = "job folders: " + err.Error()
	}
	var st unix.Statfs_t
	if err := unix.Statfs(m.StateDir, &st); err != nil {
		d.Error = err.Error()
		return d
	}
	bs := uint64(st.Bsize)
	d.TotalBytes, d.FreeBytes = st.Blocks*bs, st.Bavail*bs
	if st.Blocks > 0 {
		d.UsedPct = float64(st.Blocks-st.Bfree) * 100 / float64(st.Blocks)
	}
	return d
}

// Check is one hook/health check.
type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// guardProbe runs the guard hook exactly as a job's settings.json runs it (sh -c <command>, the
// hook payload on stdin) and returns its exit code.
func (m *Manager) guardProbe(command string) (int, error) {
	in, _ := json.Marshal(map[string]any{"tool_name": "Bash", "tool_input": map[string]string{"command": command}})
	c := exec.Command("sh", "-c", m.GuardCommand())
	c.Stdin = bytes.NewReader(in)
	c.Stdout, c.Stderr = io.Discard, io.Discard
	err := c.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	return 0, err
}

// Hooks checks what every job depends on: the guard hook blocks and allows as it must, the owner
// rules are readable (else no job starts), the notify target exists when one is configured, and the
// HTTP API serves when http_api is enabled.
func (m *Manager) Hooks() []Check {
	var out []Check
	g := Check{Name: "guard hook"}
	switch blocked, err := m.guardProbe("git push origin main"); {
	case err != nil:
		g.Detail = "cannot run the hook: " + err.Error()
	case blocked != 2:
		g.Detail = fmt.Sprintf("`git push` exited %d, want 2 (blocked)", blocked)
	default:
		if allowed, err := m.guardProbe("ls"); err != nil || allowed != 0 {
			g.Detail = fmt.Sprintf("`ls` exited %d (%v), want 0 (allowed)", allowed, err)
		} else {
			g.OK, g.Detail = true, "blocks `git push`, allows `ls`: "+m.GuardCommand()
		}
	}
	out = append(out, g)
	r := Check{Name: "owner rules"}
	if rules, err := m.readOwnerRules(); err != nil {
		r.Detail = "jobs refuse to start: " + err.Error()
	} else {
		r.OK, r.Detail = true, fmt.Sprintf("%s (%d bytes)", m.Config().OwnerRulesPath, len(rules))
	}
	out = append(out, r)
	n := Check{Name: "notify target"}
	if t := m.Config().NotifyTmuxTarget; t == "" {
		n.OK, n.Detail = true, "notify off (notify_tmux_target not configured): job-done lines go to the daemon log"
	} else if b, err := exec.Command(m.Config().tmuxBin(), m.Config().tmuxArgs("has-session", "-t", t)...).CombinedOutput(); err != nil {
		n.Detail = fmt.Sprintf("tmux has-session -t %s: %v %s", t, err, strings.TrimSpace(string(b)))
	} else {
		n.OK, n.Detail = true, "tmux session "+t+" exists"
	}
	out = append(out, n)
	if c, ok := m.HTTPAPICheck(); ok {
		out = append(out, c)
	}
	return out
}

// QueueStatus counts jobs that still need the daemon.
type QueueStatus struct {
	Queued         int `json:"queued"`
	Running        int `json:"running"`
	PendingNotify  int `json:"pending_notify"`
	PendingCleanup int `json:"pending_cleanup"`
	// ConfiguredMaxRunning is max_running (default 2) as this process loaded config.json when it
	// opened the manager. The daemon enforces its own copy, read at its start; after a config change
	// the two can differ until both restart. /v1/health shows the daemon's value.
	ConfiguredMaxRunning int `json:"configured_max_running"`
	// GateLock is the box-wide gate lock (#208): who holds it now and which of this agent's running jobs
	// wait for it (their gates or acceptance steps).
	GateLock GateLockView `json:"gate_lock"`
}

// Queue reports the queue.
func (m *Manager) Queue() (QueueStatus, error) {
	q := QueueStatus{ConfiguredMaxRunning: m.Config().MaxRunningJobs()}
	for _, c := range []struct {
		n *int
		f func() ([]*store.Job, error)
	}{
		{&q.Queued, func() ([]*store.Job, error) { return m.Store.ListByStatus(store.StatusQueued) }},
		{&q.Running, func() ([]*store.Job, error) { return m.Store.ListByStatus(store.StatusRunning) }},
		{&q.PendingNotify, m.Store.PendingNotify},
		{&q.PendingCleanup, m.Store.PendingCleanup},
	} {
		l, err := c.f()
		if err != nil {
			return q, err
		}
		*c.n = len(l)
		if c.n == &q.Running {
			q.GateLock = m.gateLockView(l)
		}
	}
	return q, nil
}

// Log line limits for system_logs.
const (
	DefaultLogLines = 100
	MaxLogLines     = 2000
	maxLogTailBytes = 1 << 20
)

// TailFile returns the last n lines of a regular file. The file may lie in a job dir the job could
// write, so it is opened without following a link or blocking and read only if it is a regular file;
// at most the last 1 MiB is read.
func TailFile(path string, n int) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file (%s)", path, fi.Mode().Type())
	}
	off := fi.Size() - maxLogTailBytes
	if off < 0 {
		off = 0
	}
	b, err := io.ReadAll(io.NewSectionReader(f, off, fi.Size()-off))
	if err != nil {
		return "", err
	}
	return lastLines(string(b), n), nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// DaemonLog returns the last n lines of the daemon's log and where they came from: the launchd
// log file <state>/daemon.log when it exists (macOS), else the systemd user journal of
// agent-jobs@<agent> (Linux, DEVIATIONS #11).
func (m *Manager) DaemonLog(n int) (source, text string, err error) {
	p := filepath.Join(m.StateDir, "daemon.log")
	if _, serr := os.Lstat(p); serr == nil {
		t, err := TailFile(p, n)
		return p, t, err
	}
	bin := os.Getenv("AGENT_JOBS_JOURNALCTL_BIN")
	if bin == "" {
		bin = "journalctl"
	}
	unit := "agent-jobs@" + m.Agent + ".service"
	out, err := exec.Command(bin, "--user", "--unit", unit, "--lines", fmt.Sprint(n), "--no-pager", "--output", "short-iso").Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			err = fmt.Errorf("%w: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "journal " + unit, "", fmt.Errorf("no %s and journalctl failed: %w", p, err)
	}
	return "journal " + unit, lastLines(string(out), n), nil
}

// Activity is a summary of the agent's jobs over a window.
type Activity struct {
	Since       string         `json:"since"`
	Until       string         `json:"until"`
	ByStatus    map[string]int `json:"by_status"`
	Started     int            `json:"started"`  // jobs created in the window
	Finished    int            `json:"finished"` // jobs finished in the window
	CostUSD     float64        `json:"cost_usd"` // of jobs and earlier attempts finished in the window
	Turns       int            `json:"turns"`
	DurationSec int            `json:"duration_sec"`
	Attempts    int            `json:"earlier_attempts,omitempty"` // earlier attempts of resumed jobs finished in the window
	Jobs        []*store.Job   `json:"-"`
	Truncated   bool           `json:"truncated,omitempty"`
}

// MaxActivityJobs caps the rows an activity report reads.
const MaxActivityJobs = 1000

// ActivityReport summarises jobs created or finished since now-window, plus every queued and
// running job.
func (m *Manager) ActivityReport(window time.Duration) (Activity, error) {
	now := m.Now()
	since := now.Add(-window)
	a := Activity{Since: since.UTC().Format(time.RFC3339), Until: now.UTC().Format(time.RFC3339), ByStatus: map[string]int{}}
	list, err := m.Store.ListActivity(since, MaxActivityJobs+1)
	if err != nil {
		return a, err
	}
	if len(list) > MaxActivityJobs {
		list, a.Truncated = list[:MaxActivityJobs], true
	}
	for _, j := range list {
		a.ByStatus[j.Status]++
		if !j.CreatedAt.Before(since) {
			a.Started++
		}
		if j.FinishedAt != nil && !j.FinishedAt.Before(since) {
			a.Finished++
			a.CostUSD += j.CostUSD
			a.Turns += j.Turns
			a.DurationSec += j.DurationSec
		}
	}
	// A resumed job's row holds only its current attempt; the earlier ones are in the attempts table.
	n, turns, cost, dur, err := m.Store.AttemptTotals(since)
	if err != nil {
		return a, err
	}
	a.Attempts, a.Turns, a.CostUSD, a.DurationSec = n, a.Turns+turns, a.CostUSD+cost, a.DurationSec+dur
	a.Jobs = list
	return a, nil
}
