package jobs

import (
	"fmt"
	"strings"
	"time"
)

// PendingOutcomeReminder surfaces recent unreviewed results to the bound orchestrator session.
// It is read-only: delivery does not acknowledge a result; only job_outcome clears the reminder.
func (m *Manager) PendingOutcomeReminder(sessionID string) (string, error) {
	bound, err := m.StopHookHolds(sessionID)
	if err != nil || !bound {
		return "", err
	}
	now := m.Now()
	rows, err := m.Store.PendingOutcomes(m.Agent, now.Add(-7*24*time.Hour))
	if err != nil || len(rows) == 0 {
		return "", err
	}
	var items []string
	for _, j := range rows[:min(len(rows), 5)] {
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
	if len(rows) > 5 {
		items = append(items, fmt.Sprintf("and %d more", len(rows)-5))
	}
	return fmt.Sprintf("[jobs:pending-outcome] %d finished jobs without job_outcome: %s", len(rows), strings.Join(items, ", ")), nil
}
