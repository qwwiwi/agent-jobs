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
// only when its semantic state (job ids and statuses) changed since the last emission, after
// hooks.repeat_after_seconds, or in a new context epoch (StartHookEpoch); otherwise "" (#166). It
// records what it returns, so call it once per hook run.
func (m *Manager) PendingOutcomeReminder(sessionID string) (string, error) {
	bound, err := m.StopHookHolds(sessionID)
	if err != nil || !bound {
		return "", err
	}
	now := m.Now()
	rows, err := m.Store.PendingOutcomes(m.Agent, now.Add(-7*24*time.Hour))
	if err != nil {
		return "", err
	}
	hash := ""
	if len(rows) > 0 {
		h := sha256.New()
		for _, j := range rows {
			fmt.Fprintf(h, "%s\x00%s\n", j.ID, j.Status)
		}
		hash = hex.EncodeToString(h.Sum(nil))
	}
	if !m.shouldEmitHook(sessionID, hookKindOutcome, hash) {
		return "", nil
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
	return capItems(head, items, len(rows), ", ", "", m.Config().Hooks.Max()), nil
}
