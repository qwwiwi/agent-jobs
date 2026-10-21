//go:build !darwin

package jobs

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// killAcceptanceGroup uses Linux process identities and pidfds, failing closed elsewhere.
func (m *Manager) killAcceptanceGroup(id string, g int) (groupEnd, string) {
	if p, err := procStatFull(g); err == nil && p.state != "Z" && p.pgid == g {
		return m.killLedAcceptance(id, g)
	}
	return m.killLeaderlessGroup(id, g)
}

// acceptanceMember reports whether pid is a live (non-zombie) member of process group g carrying the
// marker of job id.
func acceptanceMember(id string, g int) func(int) bool {
	return func(pid int) bool {
		p, err := procStatFull(pid)
		return err == nil && p.state != "Z" && p.pgid == g && procMarker(pid) == id
	}
}

// groupMembers lists the live (non-zombie) processes of process group g, except this process and its
// ancestors.
func groupMembers(g int) ([]int, error) {
	ents, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, err
	}
	skip := selfAndAncestors()
	var out []int
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 || skip[pid] {
			continue
		}
		if p, err := procStatFull(pid); err == nil && p.state != "Z" && p.pgid == g {
			out = append(out, pid)
		}
	}
	return out, nil
}

// endMarkedGroup ends the live members of process group g (named what in the log) of job id: only
// when every member passes isMember (one that does not means the pgid is not, or no longer, the job's
// group, and nothing is signalled), each through a pidfd opened before its membership is re-checked
// (signalMember), SIGTERM, KillGrace, then SIGKILL to whatever member is still there. Without pidfd
// support nothing is signalled. why says what kept the group from being ended.
func (m *Manager) endMarkedGroup(id, what string, g int, isMember func(int) bool) (res groupEnd, why string) {
	members, err := groupMembers(g)
	if err != nil {
		m.Log.Printf("job %s: %s not checked: %v", id, what, err)
		return groupRefused, "members not checked: " + err.Error()
	}
	if len(members) == 0 {
		return groupGone, ""
	}
	for _, pid := range members {
		if !isMember(pid) {
			why = fmt.Sprintf("member %d is not proven to be the job's (no marker of the job, or outside its scope)", pid)
			m.Log.Printf("job %s: refusing to kill %s: %s", id, what, why)
			return groupRefused, why
		}
	}
	marked := func() []int {
		pids, err := groupMembers(g)
		if err != nil {
			m.Log.Printf("job %s: %s: %v", id, what, err)
			return nil
		}
		return slices.DeleteFunc(pids, func(pid int) bool { return !isMember(pid) })
	}
	signal := func(sig syscall.Signal) {
		for _, pid := range marked() {
			if _, err := signalMember(pid, isMember, sig); err != nil {
				m.Log.Printf("job %s: %s: %v", id, what, err)
				why = err.Error()
			}
		}
	}
	m.Log.Printf("job %s: %s has no leader; ending %d marked member(s)", id, what, len(members))
	signal(syscall.SIGTERM)
	deadline := time.Now().Add(m.KillGrace)
	for time.Now().Before(deadline) {
		if len(marked()) == 0 {
			return groupEnded, ""
		}
		time.Sleep(100 * time.Millisecond)
	}
	signal(syscall.SIGKILL)
	for i := 0; i < 20; i++ {
		if len(marked()) == 0 {
			return groupEnded, ""
		}
		time.Sleep(50 * time.Millisecond)
	}
	m.Log.Printf("job %s: %s: marked members survived SIGKILL", id, what)
	return groupSurvived, why
}

// jobGroupMember reports whether pid is a live (non-zombie) member of process group g that carries
// the marker of job j and, for a job run in a scope, lives in that scope's cgroup: the identity that
// shows a group recorded at the spawn is still the job's, not a stranger's group that reuses the id.
func jobGroupMember(j *store.Job, g int) func(int) bool {
	return func(pid int) bool {
		p, err := procStatFull(pid)
		if err != nil || p.state == "Z" || p.pgid != g || procMarker(pid) != j.ID {
			return false
		}
		if j.ScopeUnit == "" {
			return true
		}
		b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "cgroup"))
		return err == nil && jobScopeIn(string(b)) == j.ScopeUnit
	}
}
