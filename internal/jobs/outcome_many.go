package jobs

import (
	"errors"
	"fmt"
	"strings"
)

// MaxOutcomeBatch caps the items of one SetOutcomes call (job_outcome_many, `agent-jobs outcome`).
const MaxOutcomeBatch = 500

// OutcomeItem is one job's outcome in a batch.
type OutcomeItem struct {
	// ID and Outcome are omitempty so the MCP schema does not require them: a missing one is refused
	// for that item alone (SetOutcome), not for the whole batch.
	ID      string `json:"id,omitempty"`
	Outcome string `json:"outcome,omitempty"`
	Note    string `json:"note,omitempty"`
}

// OutcomeResult is the answer for one OutcomeItem, in the order of the request.
type OutcomeResult struct {
	ID          string `json:"id"`
	OK          bool   `json:"ok"`
	Outcome     string `json:"outcome,omitempty"`
	OutcomeNote string `json:"outcome_note,omitempty"`
	Error       string `json:"error,omitempty"`
}

// SetOutcomes records the outcomes of several jobs, each exactly as SetOutcome would: an item that is
// refused (unknown id, another agent's job, an unfinished job, a bad outcome or note) gets its error in
// its own result and never stops the others. Items run in order, so a repeated id ends with its last
// valid item; an outcome is only ever the one named in an item, never inferred from the job's status.
// The error is for a batch that is empty or too large.
func (m *Manager) SetOutcomes(items []OutcomeItem) ([]OutcomeResult, error) {
	if len(items) == 0 {
		return nil, errors.New("at least one item {id, outcome, note?} is required")
	}
	if len(items) > MaxOutcomeBatch {
		return nil, fmt.Errorf("%d items, at most %d per call", len(items), MaxOutcomeBatch)
	}
	res := make([]OutcomeResult, len(items))
	for i, it := range items {
		id := strings.TrimSpace(it.ID)
		res[i].ID = id
		outcome, note, err := m.setOutcome(id, it.Outcome, it.Note)
		if err != nil {
			res[i].Error = err.Error()
			continue
		}
		if m.afterOutcome != nil {
			m.afterOutcome(id)
		}
		// The write is acknowledged with what it committed: a re-read could show another request's
		// overwrite, or fail after the outcome was already recorded.
		res[i].OK, res[i].Outcome, res[i].OutcomeNote = true, outcome, note
	}
	return res, nil
}
