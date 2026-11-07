package jobs

import (
	"os"
	"syscall"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// GateWaitView is a running job's wait for the box-wide gate lock (#208), for job_list and status: a job
// whose gate or acceptance step waits shows "waiting for gate lock held by <holder>".
type GateWaitView struct {
	Waiting bool       `json:"waiting"`
	Holder  string     `json:"holder,omitempty"`
	Since   *time.Time `json:"since,omitempty"`
	// WaitedSec is the current attempt's wait so far, the current one included: the job's deadline moves
	// by it (up to the job's timeout).
	WaitedSec int64  `json:"waited_sec"`
	Note      string `json:"note,omitempty"`
}

// GateWaitOf is j's gate lock wait as its gate-wait.json says (nil when j is not running or never
// waited in this attempt).
func (m *Manager) GateWaitOf(j *store.Job) *GateWaitView {
	if j.Status != store.StatusRunning || j.StartedAt == nil {
		return nil
	}
	s, err := ReadGateWait(m.StateDir, j.ID)
	if err != nil {
		return nil
	}
	v := &GateWaitView{WaitedSec: int64(s.Credit(*j.StartedAt, m.Now()) / time.Second)}
	for _, w := range s.Waits {
		if v.Since == nil || w.Since.Before(*v.Since) {
			since := w.Since
			v.Waiting, v.Holder, v.Since = true, RedactPrintable(w.Holder, 200), &since
		}
	}
	if v.Waiting {
		v.Note = "waiting for gate lock held by " + v.Holder
	}
	if !v.Waiting && v.WaitedSec == 0 {
		return nil
	}
	return v
}

// creditGateWait moves a running job's deadline by how long its current attempt waited for the gate
// lock (gate-wait.json, written by its gates and its acceptance step), at most by the job's timeout:
// the job's own folder is writable by the job, so the credit is bounded rather than trusted.
func (m *Manager) creditGateWait(j *store.Job, now time.Time) {
	if j.StartedAt == nil {
		return
	}
	s, err := ReadGateWait(m.StateDir, j.ID)
	if err != nil {
		return
	}
	sec := min(int64(s.Credit(*j.StartedAt, now)/time.Second), int64(j.TimeoutMinutes)*60)
	if sec <= j.GateWaitSec {
		return
	}
	if err := m.Store.RaiseGateWait(j.ID, j.LaunchToken, sec); err != nil {
		m.Log.Printf("job %s: record gate lock wait: %v", j.ID, err)
		return
	}
	j.GateWaitSec = sec
}

// GateLockView is the box-wide gate lock as status and system_doctor show it (#208).
type GateLockView struct {
	Path string `json:"path"`
	// Holder is the holder's line ("pid N job J since T at HEAD in DIR: CMD"); "" when the lock is free.
	Holder string `json:"holder,omitempty"`
	// Waiting are this agent's running jobs whose gate or acceptance step waits for the lock.
	Waiting []GateLockWaiter `json:"waiting,omitempty"`
}

// GateLockWaiter is one running job waiting for the gate lock.
type GateLockWaiter struct {
	ID string `json:"id"`
	GateWaitView
}

// gateLockView reads the lock's holder line (without taking the lock) and the waits of the running jobs.
func (m *Manager) gateLockView(running []*store.Job) GateLockView {
	v := GateLockView{Path: GateLockPath()}
	if f, err := os.OpenFile(v.Path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0); err == nil {
		if fi, err := f.Stat(); err == nil && fi.Mode().IsRegular() {
			// A holder that was killed leaves its line behind: only a live holder counts.
			if line := gateHolder(f); line != "unknown" {
				if pid, _ := parseGateHolder(line); pid > 0 && alive(pid) {
					v.Holder = RedactPrintable(line, 300)
				}
			}
		}
		f.Close()
	}
	for _, j := range running {
		if w := m.GateWaitOf(j); w != nil && w.Waiting {
			v.Waiting = append(v.Waiting, GateLockWaiter{ID: j.ID, GateWaitView: *w})
		}
	}
	return v
}
