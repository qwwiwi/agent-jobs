package jobs

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

// maxPendingOutcomeItems is how many results the reminder lists before "+N more".
const maxPendingOutcomeItems = 5

// PendingOutcomeReminder surfaces recent unreviewed results to the bound orchestrator session.
// Delivery does not acknowledge a result; only job_outcome clears the reminder. The block is injected
// only when its semantic state (job ids, statuses and attempts) changed since the last emission, after
// hooks.repeat_after_seconds, or in a new context epoch (StartHookEpoch); otherwise "" (#166). It
// does not record what it returns: the caller prints Text and then calls Commit on the emission (or
// Release when printing failed), so a block that never reached the context is injected again. nil means
// nothing to inject. Call it once per hook run.
func (m *Manager) PendingOutcomeReminder(sessionID string) (*HookEmission, error) {
	bound, err := m.StopHookHolds(sessionID)
	if err != nil || !bound {
		return nil, err
	}
	now := m.Now()
	rows, err := m.Store.PendingOutcomes(m.Agent, now.Add(-7*24*time.Hour))
	if err != nil {
		return nil, err
	}
	hash := ""
	if len(rows) > 0 {
		h := sha256.New()
		for _, j := range rows {
			// The attempt is part of the state: a resumed job that fails again is a new obligation.
			fmt.Fprintf(h, "%s\x00%s\x00%d\n", j.ID, j.Status, j.Attempt)
		}
		hash = hex.EncodeToString(h.Sum(nil))
	}
	e := m.planHook(sessionID, hookKindOutcome, hash)
	if e == nil {
		return nil, nil
	}
	var items []string
	for _, j := range rows[:min(len(rows), maxPendingOutcomeItems)] {
		age := max(time.Duration(0), now.Sub(*j.FinishedAt))
		var text string
		switch {
		case age < time.Minute:
			text = fmt.Sprintf("%ds", int(age/time.Second))
		case age < time.Hour:
			text = fmt.Sprintf("%dm", int(age/time.Minute))
		case age < 24*time.Hour:
			text = fmt.Sprintf("%dh", int(age/time.Hour))
		default:
			text = fmt.Sprintf("%dd", int(age/(24*time.Hour)))
		}
		items = append(items, fmt.Sprintf("%s %s (finished %s ago)", Printable(j.ID, 64),
			RedactPrintable(j.Title, MaxTitleRunes), text))
	}
	head := fmt.Sprintf("[jobs:pending-outcome] %d finished jobs without job_outcome: ", len(rows))
	e.Text = capItems(head, items, len(rows), ", ", "", m.Config().Hooks.Max())
	return e, nil
}
