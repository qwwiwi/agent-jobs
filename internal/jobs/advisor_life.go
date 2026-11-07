package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// An advisor call's process belongs to its job's lifecycle (issue #169, F2). It is admitted and
// registered under the advisor lock (<AdvisorDir>/admit.lock). The transitions that close admission (the
// timeout claim and the row leaving running in finish, Cancel and Stop) run under the same lock
// (closingAdmission, issue #190), and each of those paths then ends the registered calls under it. So
// either a call registered first (and is ended with the job), or the transition came first (and the
// call's admission refuses to start its process); none can fall between the check and the start. The
// wrapper's completion record (also read by the check) is not taken under this lock: a call admitted
// just before it is ended by the finish that collects the attempt. The advisor dir is the daemon's, outside the
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

// closingAdmission runs fn, a transition that closes advisor admission for job j (the timeout claim,
// the row leaving running), under the advisor lock (issue #190): an advisor call that passed its final
// liveness check holds that lock until its process is started and registered, so the transition either
// comes first (and the call is refused) or waits for the registration (and endAdvisors then ends it).
// A plain job never admits an advisor call and takes no lock. Call it with the advisor lock not held;
// release it before endAdvisors. If the advisor dir or the lock cannot be had (descriptors exhausted,
// say), fn does not run and the error is returned: without the lock the transition could complete while
// an admitted call is between its check and its start. The row stays running, so the transition is
// taken again once the lock can be had (the tick retries a timeout, a finish and a dead wrapper's end;
// a cancel or a stop is retried by its caller or ends with its dead wrapper).
func (m *Manager) closingAdmission(j *store.Job, fn func() error) error {
	if !j.Crew.HasAdvisor() {
		return fn()
	}
	dir := m.AdvisorDir(j.ID)
	// Created here too: an admission that creates the dir after this point checks the row after fn.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("advisor admission not serialized, transition deferred: %w", err)
	}
	lock := admitLock
	if m.closingLock != nil {
		lock = m.closingLock
	}
	unlock, err := lock(dir)
	if err != nil {
		return fmt.Errorf("advisor admission not serialized, transition deferred: %w", err)
	}
	defer unlock()
	return fn()
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
	if m.afterAdvisorAdmit != nil {
		m.afterAdvisorAdmit(j)
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
	// Once cmd has been waited for, its leader is gone, not necessarily its group or its scope: the
	// registration is dropped only when nothing of the call is left (issue #190). One that cannot be
	// ended stays registered, so the job's lifecycle (endAdvisors) ends it.
	return func() {
		unlock, err := admitLock(dir)
		if err != nil {
			m.Log.Printf("job %s: advisor call %d kept registered: %v", j.ID, n, err)
			return
		}
		defer unlock()
		if _, err := os.Stat(path); err != nil {
			return // ended (and dropped) by the job's lifecycle already
		}
		if note := m.endAdvisorRun(j.ID, r); note != "" {
			m.Log.Printf("job %s: advisor call %d kept registered: %s", j.ID, n, note)
			return
		}
		_ = os.Remove(path)
	}, nil
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
	return m.endAdvisorsHeld(id)
}

// endAdvisorsHeld is endAdvisors for a caller that holds the advisor lock (closingAdmission's fn). A
// call that could not be ended keeps its registration (durable ownership, issue #190): the tick retries
// it (retryAdvisorEnds), and the job is neither resumed nor pruned while it is there (advisorPending).
func (m *Manager) endAdvisorsHeld(id string) string {
	dir := m.AdvisorDir(id)
	if _, err := os.Stat(dir); err != nil {
		return ""
	}
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
// kill of its process group covers session containment (and a scope that could not be stopped), and
// the members of that group are ended and waited for even when its leader is gone (endAdvisorGroup).
// "" means nothing of the call is left; otherwise the registration must be kept.
func (m *Manager) endAdvisorRun(id string, r advisorRun) string {
	var scopeErr error
	if r.Scope != "" {
		scopeErr = m.stopScope(r.Scope)
	}
	if advisorIsRun(r) {
		if err := syscall.Kill(-r.PID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Sprintf("advisor call %d (process group %d) not killed: %v", r.N, r.PID, err)
		}
		m.Log.Printf("job %s: advisor call %d ended", id, r.N)
	}
	var notes []string
	if note := m.endAdvisorGroup(id, r); note != "" {
		m.Log.Printf("job %s: %s", id, note)
		notes = append(notes, note)
	}
	if scopeErr != nil {
		m.Log.Printf("job %s: advisor call %d: stop scope: %v", id, r.N, scopeErr)
		notes = append(notes, fmt.Sprintf("advisor call %d: scope not stopped: %v", r.N, scopeErr))
	}
	return strings.Join(notes, "; ")
}

// advisorMember reports whether pid is a live (non-zombie) member of registered advisor call r's
// process group or session that is proven the call's whether or not its leader is still there: it
// carries job id's marker and started no earlier than the call's leader.
func advisorMember(id string, r advisorRun) func(int) bool {
	return func(pid int) bool {
		p, err := procStatFull(pid)
		return err == nil && p.state != "Z" && (p.pgid == r.PID || p.sid == r.PID) && procMarker(pid) == id &&
			(r.Start == 0 || p.start >= r.Start)
	}
}

// advisorMembers lists the live processes of call r's process group and session: those proven the
// call's (advisorMember) and the others that may still be the call's (unproven: no marker, say, after
// env -u AGENT_JOBS_JOB). reused reports that the ids are proven another's now: r's pid is a process with
// another start time, so the call's group and session were gone before it was reused (a pid is not
// reused while its process group or session has a member). A process that started before the call's
// leader cannot be in its session either, so it is no member at all.
func advisorMembers(id string, r advisorRun) (proven, unproven []int, reused bool, err error) {
	if r.Start != 0 {
		if p, err := procStatFull(r.PID); err == nil && p.start != r.Start {
			return nil, nil, true, nil
		}
	}
	group, err := groupMembers(r.PID)
	if err != nil {
		return nil, nil, false, err
	}
	session, err := sessionMembers(r.PID)
	if err != nil {
		return nil, nil, false, err
	}
	pids := slices.Compact(slices.Sorted(slices.Values(append(group, session...))))
	isMember := advisorMember(id, r)
	for _, pid := range pids {
		switch p, err := procStatFull(pid); {
		case err != nil || p.state == "Z" || (p.pgid != r.PID && p.sid != r.PID):
			// gone, or no longer a member
		case isMember(pid):
			proven = append(proven, pid)
		case r.Start == 0 || p.start >= r.Start:
			unproven = append(unproven, pid)
		}
	}
	return proven, unproven, false, nil
}

// advisorGroupRounds bounds endAdvisorGroup's wait (50 ms each) for the members it killed to go.
const advisorGroupRounds = 60

// endAdvisorGroup ends the members of registered call r's process group and session proven the call's
// (advisorMember), also when the leader has exited or been reaped, each through a pidfd (signalMember),
// and waits until none is left. A member that is not proven the call's is never signalled, and while
// one is there (unless the ids are proven reused) the registration is kept: nothing else can find it
// (durable ownership, issue #190). Without /proc nothing can be proven: a group that keeps processes
// keeps the registration (fail closed). It returns a note when members survived or could not be
// checked ("" when none is left).
func (m *Manager) endAdvisorGroup(id string, r advisorRun) string {
	if plausibleGroup(r.PID) != nil {
		return ""
	}
	if !procAvailable() {
		for round := 0; round < advisorGroupRounds; round++ {
			if !groupAlive(r.PID) {
				return ""
			}
			time.Sleep(50 * time.Millisecond)
		}
		return fmt.Sprintf("advisor call %d: process group %d still has processes, not checked (needs Linux /proc)", r.N, r.PID)
	}
	isMember := advisorMember(id, r)
	var signalErr error
	for round := 0; round < advisorGroupRounds; round++ {
		pids, unproven, reused, err := advisorMembers(id, r)
		if err != nil {
			return fmt.Sprintf("advisor call %d: process group %d not checked: %v", r.N, r.PID, err)
		}
		if reused {
			return ""
		}
		if len(pids) == 0 {
			if len(unproven) == 0 {
				return ""
			}
			return fmt.Sprintf("advisor call %d: %d process(es) in its group or session %d not proven the call's (no job marker), not signalled",
				r.N, len(unproven), r.PID)
		}
		if round%20 == 0 {
			m.Log.Printf("job %s: advisor call %d: ending %d process(es) left in its group %d", id, r.N, len(pids), r.PID)
			for _, pid := range pids {
				if _, err := signalMember(pid, isMember, syscall.SIGKILL); err != nil {
					signalErr = err
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	note := fmt.Sprintf("advisor call %d: processes of its group %d survived SIGKILL", r.N, r.PID)
	if signalErr != nil {
		note += ": " + signalErr.Error()
	}
	return note
}

// advisorPending reports whether job j still owns advisor calls that could not be ended (a kept
// registration, issue #190): resume, cleanup and prune wait until the tick has ended them.
func (m *Manager) advisorPending(j *store.Job) bool {
	if !j.Crew.HasAdvisor() {
		return false
	}
	runs, err := filepath.Glob(filepath.Join(m.AdvisorDir(j.ID), "run.*.json"))
	return err != nil || len(runs) > 0
}

// errAdvisorPending is the refusal of a resume while advisor calls of the job are not ended.
func errAdvisorPending(id string) error {
	return fmt.Errorf("job %s: advisor calls of its last attempt are not ended yet (retried every tick); resume once they are", id)
}

// advisorsNotEnded is the status and message of an attempt whose advisor calls could not be ended
// (note): like a failed scope stop, a timeout stays a timeout and a clean finish fails.
func advisorsNotEnded(status, msg, note string) (string, string) {
	note = ReasonSurvivors + " (advisor calls not ended, retried every tick: " + note + ")"
	if msg != "" {
		note = msg + "; " + note
	}
	if status == store.StatusTimeout || status == store.StatusFailed {
		return status, RedactPrintable(note, 300)
	}
	return store.StatusFailed, RedactPrintable(note, 300)
}

// retryAdvisorEnds retries, every tick, the advisor calls that a job which left running could not end
// (their registrations were kept): a registration is dropped only once nothing of its call is left.
// A running job's registrations belong to its live calls and its own lifecycle.
func (m *Manager) retryAdvisorEnds() error {
	runs, err := filepath.Glob(filepath.Join(m.StateDir, "advisor", "*", "run.*.json"))
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, p := range runs {
		id := filepath.Base(filepath.Dir(p))
		if seen[id] || m.AdvisorDir(id) != filepath.Dir(p) {
			continue
		}
		seen[id] = true
		j, err := m.Store.Get(id)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			m.Log.Printf("job %s: advisor calls not retried: %v", id, err)
			continue
		}
		if err == nil && j.Status == store.StatusRunning {
			continue
		}
		if note := m.endAdvisors(id); note != "" {
			m.Log.Printf("job %s: advisor calls still not ended (retry next tick): %s", id, note)
			continue
		}
		m.Log.Printf("job %s: advisor calls ended on retry", id)
	}
	return nil
}
