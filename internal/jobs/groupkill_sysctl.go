package jobs

import (
	"fmt"
	"syscall"
	"time"
)

// sysctlMarkedMember proves a live process's group and job marker in one sysctl identity read.
func sysctlMarkedMember(id string, g int) func(int) bool {
	return func(pid int) bool {
		info, err := sysctlProc(pid)
		return err == nil && !info.zombie && info.pgid == g && envMarker(info.env) == id
	}
}

// endMarkedGroupBySysctl ends a group without a wrapper or pidfds (macOS). Like killGroupBySysctl,
// it signals only the group, and only immediately after a live member proves it still belongs to
// the job. A marked member also proves ownership of members that dropped their marker: processes
// can join only a group in their own session. No live marked member means no signal. The residual
// race is the same as killGroupBySysctl: every member exiting and the group id being reused between
// the identity read and killpg. This helper is shared so its signal decisions can be tested on Linux.
func (m *Manager) endMarkedGroupBySysctl(id, what string, g int, isMember func(int) bool) (groupEnd, string) {
	refuse := func(why string) (groupEnd, string) {
		m.Log.Printf("job %s: refusing to kill %s: %s", id, what, why)
		return groupRefused, why
	}
	if err := plausibleGroup(g); err != nil {
		return refuse(err.Error())
	}
	if !groupExists(g) {
		return groupGone, ""
	}
	owned := func() error {
		members, err := sysctlGroup(g)
		if err != nil {
			return fmt.Errorf("members not checked: %w", err)
		}
		for _, pid := range members {
			if pid > 1 && isMember(pid) {
				return nil
			}
		}
		return fmt.Errorf("no live member carries the job's marker (or its scope cannot be checked)")
	}
	m.Log.Printf("job %s: checking %s for a live marked member", id, what)
	if err := owned(); err != nil {
		if !groupExists(g) {
			return groupGone, ""
		}
		return refuse(err.Error())
	}
	why := ""
	signal := func(sig syscall.Signal) {
		if err := killFn(-g, sig); err != nil {
			why = fmt.Sprintf("signal %s: %v", sig, err)
		} else {
			why = ""
		}
	}
	signal(syscall.SIGTERM)
	deadline := time.Now().Add(m.KillGrace)
	for time.Now().Before(deadline) {
		if !groupExists(g) {
			return groupEnded, ""
		}
		time.Sleep(100 * time.Millisecond)
	}
	for i := 0; i < 20; i++ {
		if !groupExists(g) {
			return groupEnded, ""
		}
		if err := owned(); err != nil {
			why = "group not signalled: " + err.Error()
		} else {
			signal(syscall.SIGKILL)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !groupExists(g) {
		return groupEnded, ""
	}
	if why == "" {
		why = "members survived SIGKILL"
	}
	m.Log.Printf("job %s: %s: members survived SIGKILL; %s", id, what, why)
	return groupSurvived, why
}

// killAcceptanceGroupBySysctl requires the leader's marker while it leads the group. Once it is gone, a
// live marked member proves the group's ownership, as in the dead-wrapper path.
func (m *Manager) killAcceptanceGroupBySysctl(id string, g int) {
	if info, err := sysctlProc(g); err == nil && !info.zombie && info.pgid == g {
		if envMarker(info.env) != id {
			m.Log.Printf("job %s: refusing to kill acceptance process group %d: its leader carries no marker of the job", id, g)
			return
		}
	}
	m.endMarkedGroupBySysctl(id, fmt.Sprintf("acceptance process group %d", g), g, sysctlMarkedMember(id, g))
}
