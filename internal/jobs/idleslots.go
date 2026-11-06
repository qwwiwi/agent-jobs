package jobs

import (
	"context"
	"fmt"
	"strings"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// maxIdleLineTasks bounds how many ready tasks one idle-slot line names.
const maxIdleLineTasks = 20

// IdleSlotsLine is the line the daemon sends the orchestrator when job slots stand empty while tasks of
// open milestones are ready to start (#119): "3 slots free, ready: T8, T9 (milestone 1)". free is
// max_running minus the running jobs; a milestone that already runs its max_parallel jobs adds no ready
// task, as a job on it could not start. "" when no slot is free or no task is ready.
func (m *Manager) IdleSlotsLine(ctx context.Context) (line string, ready []string, err error) {
	running, err := m.Store.CountRunning()
	if err != nil {
		return "", nil, err
	}
	free := m.Config().MaxRunningJobs() - running
	if free <= 0 {
		return "", nil, nil
	}
	list, err := m.Store.ListMilestones(ctx, store.MilestoneFilter{Status: store.MilestoneOpen})
	if err != nil {
		return "", nil, err
	}
	var groups []string
	for _, ms := range list {
		tasks, err := m.Store.MilestoneTasks(ctx, ms.ID)
		if err != nil {
			return "", nil, err
		}
		jobs, err := m.Store.MilestoneJobs(ctx, ms.ID)
		if err != nil {
			return "", nil, err
		}
		if ms.MaxParallel != nil && runningJobs(jobs) >= *ms.MaxParallel {
			continue
		}
		st := TaskStatuses(tasks, jobs)
		var keys []string
		for _, t := range tasks {
			if st[t.Key] == TaskReady {
				keys = append(keys, t.Key)
				ready = append(ready, fmt.Sprintf("%d/%s", ms.ID, t.Key))
			}
		}
		if len(keys) > 0 {
			groups = append(groups, fmt.Sprintf("%s (milestone %d)", strings.Join(keys, ", "), ms.ID))
		}
	}
	if len(groups) == 0 {
		return "", nil, nil
	}
	if len(ready) > maxIdleLineTasks {
		return fmt.Sprintf("%d slots free, ready: %d tasks (milestone_list tree=true)", free, len(ready)), ready, nil
	}
	return fmt.Sprintf("%d slots free, ready: %s", free, strings.Join(groups, "; ")), ready, nil
}

func runningJobs(jobs []*store.Job) int {
	n := 0
	for _, j := range jobs {
		if j.Status == store.StatusRunning {
			n++
		}
	}
	return n
}

// nudgeIdleSlots sends the idle-slot line once per distinct ready list: while the same tasks stay ready
// and unstarted the orchestrator is not told again, and a quiet spell (no free slot or no ready task)
// forgets the list, so the next idle spell is announced. Without notify_tmux_target the line is only logged.
func (m *Manager) nudgeIdleSlots() error {
	line, ready, err := m.IdleSlotsLine(context.Background())
	if err != nil {
		return err
	}
	key := strings.Join(ready, ",")
	m.idleMu.Lock()
	defer m.idleMu.Unlock()
	if line == "" {
		m.idleSent = ""
		return nil
	}
	if key == m.idleSent {
		return nil
	}
	target := m.Config().NotifyTmuxTarget
	if target == "" {
		m.Log.Printf("notify off (notify_tmux_target not configured): %s", line)
		m.idleSent = key
		return nil
	}
	if err := m.sendTmuxLocked(target, line); err != nil {
		m.Log.Printf("idle slots: notify failed, retry next tick: %s", RedactText(err.Error()))
		return nil
	}
	m.idleSent = key
	return nil
}
