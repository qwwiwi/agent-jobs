//go:build darwin

package jobs

import "github.com/qwwiwi/agent-jobs/internal/store"

// groupMembers lists group candidates through kern.proc.pgrp. Their current group, state and marker
// are checked through sysctlProc immediately before signalling, never through /proc.
func groupMembers(g int) ([]int, error) {
	return sysctlGroup(g)
}

func acceptanceMember(id string, g int) func(int) bool {
	return sysctlMarkedMember(id, g)
}

func jobGroupMember(j *store.Job, g int) func(int) bool {
	return func(pid int) bool {
		// A Linux cgroup cannot be proven through sysctl.
		return j.ScopeUnit == "" && sysctlMarkedMember(j.ID, g)(pid)
	}
}

func (m *Manager) endMarkedGroup(id, what string, g int, isMember func(int) bool) (groupEnd, string) {
	return m.endMarkedGroupBySysctl(id, what, g, isMember)
}

func (m *Manager) killAcceptanceGroup(id string, g int) {
	m.killAcceptanceGroupBySysctl(id, g)
}
