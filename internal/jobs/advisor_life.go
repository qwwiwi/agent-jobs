package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// An advisor call's process belongs to its job's lifecycle (issue #169, F2). It is admitted and
// registered under the advisor lock (<AdvisorDir>/admit.lock), and every path that takes the job out of
// running (finish, Cancel, Stop) ends the registered calls under the same lock once the row has left
// running. So either a call registered first (and is ended with the job), or the row left running first
// (and the call's admission refuses to start its process). The advisor dir is the daemon's, outside the
// job's folder: the job cannot forge a registration. whileJobRuns' polling stays as a fallback.

// advisorRun is the registration of one running advisor call (<AdvisorDir>/run.<n>.json).
type advisorRun struct {
	N   int `json:"n"`
	PID int `json:"pid"` // leads its own session and process group (setsid)
	// Start is the process's /proc start time (0 where /proc is not available): a pid reused by
	// another process is never signalled.
	Start uint64 `json:"start"`
	Token string `json:"token"`
	// Scope is the scope id (stopScope) of a call contained in a cgroup scope, "" for session only.
	Scope string `json:"scope,omitempty"`
}

func advisorRunPath(dir string, n int) string {
	return filepath.Join(dir, "run."+strconv.Itoa(n)+".json")
}

// writeAdvisorRun records r in dir.
func writeAdvisorRun(dir string, r advisorRun) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return writeAtomic(advisorRunPath(dir, r.N), append(b, '\n'))
}

// advisorLive reports why job j may no longer start an advisor process (nil = it may): it must still
// run under the launch it asked from, not be timing out and its wrapper must not have recorded the
// attempt's completion.
func (m *Manager) advisorLive(j *store.Job) error {
	cur, err := m.Store.Get(j.ID)
	if err != nil {
		return fmt.Errorf("job %s: %w", j.ID, err)
	}
	switch {
	case cur.Status != store.StatusRunning:
		return fmt.Errorf("job %s left running (now %s)", j.ID, cur.Status)
	case cur.LaunchToken != j.LaunchToken:
		return fmt.Errorf("job %s left running (another launch)", j.ID)
	case cur.TimedOutAt != nil:
		return fmt.Errorf("job %s left running (timing out)", j.ID)
	}
	raw, ok, err := m.Store.WrapperResult(j.ID, cur.LaunchToken)
	if err != nil {
		return fmt.Errorf("job %s: %w", j.ID, err)
	}
	if ok {
		var res StreamResult
		if json.Unmarshal([]byte(raw), &res) != nil || res.Completed {
			return fmt.Errorf("job %s left running (its attempt completed)", j.ID)
		}
	}
	return nil
}

// errAdvisorRefused marks an advisor call whose process was never started.
var errAdvisorRefused = errors.New("the advisor call was not started")

// startAdvisor starts cmd (advisor call n of job j, in its own session; scope: its scope id, "" for
// none) only while j still runs, and registers it before the advisor lock is released. It returns the
// func that drops the registration once cmd has been waited for. An error wrapping errAdvisorRefused
// means no process ran.
func (m *Manager) startAdvisor(j *store.Job, n int, cmd *exec.Cmd, scope string) (func(), error) {
	dir := m.AdvisorDir(j.ID)
	unlock, err := admitLock(dir)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errAdvisorRefused, err)
	}
	defer unlock()
	if err := m.advisorLive(j); err != nil {
		return nil, fmt.Errorf("%w: %v", errAdvisorRefused, err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%w: %v", errAdvisorRefused, err)
	}
	r := advisorRun{N: n, PID: cmd.Process.Pid, Token: j.LaunchToken, Scope: scope}
	if procAvailable() {
		if p, err := procStatFull(r.PID); err == nil {
			r.Start = p.start
		}
	}
	if err := writeAdvisorRun(dir, r); err != nil {
		// Unregistered, it could outlive its job: it does not run at all.
		_ = syscall.Kill(-r.PID, syscall.SIGKILL)
		_ = cmd.Wait()
		if scope != "" {
			_ = m.stopScope(scope)
		}
		return nil, fmt.Errorf("register the advisor call: %w", err)
	}
	path := advisorRunPath(dir, n)
	return func() { _ = os.Remove(path) }, nil
}

// advisorIsRun reports whether r's process still exists as registered: its pid leads its process
// group and, where /proc tells, has the recorded start time.
func advisorIsRun(r advisorRun) bool {
	if plausibleGroup(r.PID) != nil {
		return false
	}
	if procAvailable() {
		p, err := procStatFull(r.PID)
		return err == nil && r.Start != 0 && p.start == r.Start && p.pgid == r.PID
	}
	g, err := syscall.Getpgid(r.PID)
	return err == nil && g == r.PID
}

// endAdvisors ends every registered advisor call of job id: its scope (when it has one) and its
// session's process group. Call it once the job's row has left running, before the job's shutdown
// completes. It returns a note for the job when a call could not be ended ("" otherwise).
func (m *Manager) endAdvisors(id string) string {
	dir := m.AdvisorDir(id)
	if _, err := os.Stat(dir); err != nil {
		return "" // no advisor call was ever admitted: admission creates the dir before it checks the row
	}
	unlock, err := admitLock(dir)
	if err != nil {
		return "advisor calls not checked: " + err.Error()
	}
	defer unlock()
	paths, err := filepath.Glob(filepath.Join(dir, "run.*.json"))
	if err != nil {
		return "advisor calls not checked: " + err.Error()
	}
	var notes []string
	for _, p := range paths {
		var r advisorRun
		b, err := os.ReadFile(p)
		if err == nil {
			err = json.Unmarshal(b, &r)
		}
		if err != nil {
			notes = append(notes, fmt.Sprintf("advisor registration %s unreadable: %v", filepath.Base(p), err))
			continue
		}
		if note := m.endAdvisorRun(id, r); note != "" {
			notes = append(notes, note)
			continue
		}
		_ = os.Remove(p)
	}
	return strings.Join(notes, "; ")
}

// endAdvisorRun ends one registered advisor call: its scope's stop ends everything in its cgroup; the
// kill of its process group covers session containment (and a scope that could not be stopped).
func (m *Manager) endAdvisorRun(id string, r advisorRun) string {
	var scopeErr error
	if r.Scope != "" {
		scopeErr = m.stopScope(r.Scope)
	}
	if advisorIsRun(r) {
		if err := syscall.Kill(-r.PID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Sprintf("advisor call %d (process group %d) not killed: %v", r.N, r.PID, err)
		}
		m.Log.Printf("job %s: advisor call %d ended with its job", id, r.N)
	}
	if scopeErr != nil {
		m.Log.Printf("job %s: advisor call %d: stop scope: %v", id, r.N, scopeErr)
		return fmt.Sprintf("advisor call %d: scope not stopped: %v", r.N, scopeErr)
	}
	return ""
}
