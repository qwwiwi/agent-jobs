package jobs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
	// PrunableBytes is what `prune` would remove now (job folders finished > 48 h ago).
	PrunableBytes int64  `json:"prunable_bytes"`
	Error         string `json:"error,omitempty"`
}

// Disk reports the state dir's filesystem.
func (m *Manager) Disk() DiskStatus {
	d := DiskStatus{Path: m.StateDir}
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
	if rep, err := m.Prune(true); err == nil {
		d.PrunableBytes = rep.Bytes
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
// rules are readable (else no job starts), and the notify target exists when one is configured.
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
		r.OK, r.Detail = true, fmt.Sprintf("%s (%d bytes)", m.Cfg.OwnerRulesPath, len(rules))
	}
	out = append(out, r)
	n := Check{Name: "notify target"}
	if t := m.Cfg.NotifyTmuxTarget; t == "" {
		n.OK, n.Detail = true, "notify off (notify_tmux_target not configured): job-done lines go to the daemon log"
	} else if b, err := exec.Command(m.Cfg.tmuxBin(), m.Cfg.tmuxArgs("has-session", "-t", t)...).CombinedOutput(); err != nil {
		n.Detail = fmt.Sprintf("tmux has-session -t %s: %v %s", t, err, strings.TrimSpace(string(b)))
	} else {
		n.OK, n.Detail = true, "tmux session "+t+" exists"
	}
	out = append(out, n)
	return out
}

// QueueStatus counts jobs that still need the daemon.
type QueueStatus struct {
	Queued         int `json:"queued"`
	Running        int `json:"running"`
	PendingNotify  int `json:"pending_notify"`
	PendingCleanup int `json:"pending_cleanup"`
}

// Queue reports the queue.
func (m *Manager) Queue() (QueueStatus, error) {
	var q QueueStatus
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
	CostUSD     float64        `json:"cost_usd"` // of jobs finished in the window
	Turns       int            `json:"turns"`
	DurationSec int            `json:"duration_sec"`
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
	a.Jobs = list
	return a, nil
}
