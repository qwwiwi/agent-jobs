package jobs

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// GateWaitView is a running job's wait for the box-wide gate lock (#208), for job_list and status: a job
// whose gate or acceptance step waits shows "waiting for gate lock, untrusted holder line: <holder>" (the
// holder line is in a file every user can write: shown, never trusted).
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
		v.Note = "waiting for gate lock, untrusted holder line: " + v.Holder
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

// GateLockView is the gate lock as status and system_doctor show it (#208): the gates that run, one
// per held box slot (v0.8.2), and a gate of a pre-0.8.2 binary holding the single box lock.
type GateLockView struct {
	Path string `json:"path"`
	// Holder is the first of Holders ("" when no gate runs), for readers of the single box lock.
	Holder string `json:"holder,omitempty"`
	// Holders are the holder lines ("pid N job J since T at HEAD in DIR: CMD") of the running gates,
	// untrusted text (anyone can write the lock files), bounded, printable and redacted.
	Holders []string `json:"holders,omitempty"`
	// Waiting are this agent's running jobs whose gate or acceptance step waits for the lock.
	Waiting []GateLockWaiter `json:"waiting,omitempty"`
}

// gateLockHeld reports whether someone holds the lock at path exclusively, and its holder line then. A
// shared flock tried without blocking (and dropped at once) fails only while one does.
func gateLockHeld(path string) (string, bool) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", false
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return "", false
	}
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
	if err == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return "", false
	}
	if !errors.Is(err, syscall.EWOULDBLOCK) {
		return "", false
	}
	return RedactPrintable(gateHolder(f), 300), true
}

// GateLockWaiter is one running job waiting for the gate lock.
type GateLockWaiter struct {
	ID string `json:"id"`
	GateWaitView
}

// gateLockView reads the holder lines of the held gate locks and the waits of the running jobs: the
// single box lock (held exclusively only by a pre-0.8.2 gate; new gates hold it shared) and every box
// slot. Whether a lock is held is the kernel's answer, not the line's: a holder that was killed leaves
// its line behind, and anyone can write one.
func (m *Manager) gateLockView(running []*store.Job) GateLockView {
	v := GateLockView{Path: GateLockPath()}
	dir := filepath.Dir(v.Path)
	paths := []string{v.Path}
	for i := range CeilingGateMaxParallel {
		paths = append(paths, gateSlotPath(dir, i))
	}
	for _, p := range paths {
		if h, held := gateLockHeld(p); held {
			v.Holders = append(v.Holders, h)
		}
	}
	if len(v.Holders) > 0 {
		v.Holder = v.Holders[0]
	}
	for _, j := range running {
		if w := m.GateWaitOf(j); w != nil && w.Waiting {
			v.Waiting = append(v.Waiting, GateLockWaiter{ID: j.ID, GateWaitView: *w})
		}
	}
	return v
}
