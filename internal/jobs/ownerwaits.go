package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// «Owner waits»: a job started at the owner's request (job_start owner_waits, start --owner-waits)
// ends its job-done line with OwnerWaitsMarker, and the delivered line records «owner reply pending»
// for the agent (store.MarkNotified). Until the orchestrator calls owner_replied, the Stop hook of
// the orchestrator session (`agent-jobs hook stop`) does not let the session end its turn silently.
// A record older than owner_waits_max_hours (default 6) expires on its own.

// OwnerWaitsMarker ends the job-done line of an owner-waits job.
const OwnerWaitsMarker = "owner waits"

// MaxHookInputBytes caps the Stop-hook JSON read from stdin.
const MaxHookInputBytes = 1 << 20

// StopHookInput is the part of Claude Code's Stop-hook input the hook reads.
type StopHookInput struct {
	HookEventName string `json:"hook_event_name"`
	// StopHookActive is true when the session is already continuing because a Stop hook blocked it.
	StopHookActive bool `json:"stop_hook_active"`
}

// StopHookOutput is Claude Code's Stop-hook block decision (printed as JSON, exit 0).
type StopHookOutput struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

// ParseStopHookInput reads the hook JSON (at most MaxHookInputBytes). Empty input is an empty event.
func ParseStopHookInput(r io.Reader) (StopHookInput, error) {
	var in StopHookInput
	b, err := io.ReadAll(io.LimitReader(r, MaxHookInputBytes+1))
	if err != nil {
		return in, fmt.Errorf("read hook input: %w", err)
	}
	if len(b) > MaxHookInputBytes {
		return in, fmt.Errorf("hook input is larger than %d bytes", MaxHookInputBytes)
	}
	if strings.TrimSpace(string(b)) == "" {
		return in, nil
	}
	if err := json.Unmarshal(b, &in); err != nil {
		return in, fmt.Errorf("hook input: %w", err)
	}
	return in, nil
}

// StopHookSkips reports whether the hook allows the stop without looking at the store: the session
// is already continuing after a block (stop_hook_active; one block per turn, never a loop), or the
// event is not Stop (the hook registered for another event).
func StopHookSkips(in StopHookInput) bool {
	return in.StopHookActive || (in.HookEventName != "" && in.HookEventName != "Stop")
}

// StopHookDecision is the hook's answer: nil allows the stop, otherwise the block with its reason.
func StopHookDecision(in StopHookInput, pending []*store.OwnerPending) *StopHookOutput {
	if StopHookSkips(in) || len(pending) == 0 {
		return nil
	}
	parts := make([]string, 0, len(pending))
	for _, p := range pending {
		parts = append(parts, fmt.Sprintf("job %s «%s» (%s)", Printable(p.JobID, 64), Printable(p.Title, MaxTitleRunes), p.Status))
	}
	return &StopHookOutput{
		Decision: "block",
		Reason: "Owner waits: the owner asked for " + strings.Join(parts, ", ") + ", and the job-done line arrived. " +
			"Write the result to the owner over your channel now (read the report with job_result first), " +
			"then call the agent-jobs tool owner_replied (with job_id, or without one for all). " +
			"If you already wrote to the owner, just call owner_replied.",
	}
}

// OwnerPending returns the «owner reply pending» records after dropping the expired ones, and how
// many expired now.
func (m *Manager) OwnerPending() ([]*store.OwnerPending, int, error) {
	return m.Store.OwnerPendingList(m.Now(), m.Cfg.OwnerWaitsMaxAge())
}

// OwnerReplied clears the «owner reply pending» record of job id, or all of them when id is "".
// A given id without a record is an error.
func (m *Manager) OwnerReplied(id string) ([]string, error) {
	id = strings.TrimSpace(id)
	cleared, err := m.Store.ClearOwnerPending(id)
	if err != nil {
		return nil, err
	}
	if id != "" && len(cleared) == 0 {
		return nil, errors.New("no owner reply pending for job " + Printable(id, 64))
	}
	return cleared, nil
}
