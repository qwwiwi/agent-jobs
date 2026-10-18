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
// ends its job-done line with OwnerWaitsMarker. When the job has finished, «owner reply pending» is
// recorded for it BEFORE its line is delivered (store.BeginOwnerPending; with notify off it is
// recorded in place of the line), and the delivered line marks it delivered (store.MarkNotified).
// Until the orchestrator calls owner_replied, the Stop hook of the orchestrator session
// (`agent-jobs hook stop`) does not let that session end its turn silently. The hook holds only the
// session bound by `agent-jobs hook bind` (the orchestrator's SessionStart hook); with no session
// bound, or for any other session, it allows. A record older than owner_waits_max_hours (default 6)
// expires on its own.

// OwnerWaitsMarker ends the job-done line of an owner-waits job.
const OwnerWaitsMarker = "owner waits"

// MaxHookInputBytes caps the hook JSON read from stdin.
const MaxHookInputBytes = 1 << 20

// StopHookInput is the part of Claude Code's Stop-hook input the hook reads.
type StopHookInput struct {
	HookEventName string `json:"hook_event_name"`
	SessionID     string `json:"session_id"`
	// StopHookActive is true when the session is already continuing because a Stop hook blocked it.
	StopHookActive bool `json:"stop_hook_active"`
}

// StopHookOutput is Claude Code's Stop-hook block decision (printed as JSON, exit 0).
type StopHookOutput struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

// hookInput is the hook JSON with every field optional, to tell a missing field from a zero one.
type hookInput struct {
	HookEventName  *string `json:"hook_event_name"`
	SessionID      *string `json:"session_id"`
	StopHookActive *bool   `json:"stop_hook_active"`
}

// readHookInput reads one JSON object (at most MaxHookInputBytes) with a non-empty hook_event_name.
func readHookInput(r io.Reader) (hookInput, error) {
	var in hookInput
	b, err := io.ReadAll(io.LimitReader(r, MaxHookInputBytes+1))
	if err != nil {
		return in, fmt.Errorf("read hook input: %w", err)
	}
	if len(b) > MaxHookInputBytes {
		return in, fmt.Errorf("hook input is larger than %d bytes", MaxHookInputBytes)
	}
	if strings.TrimSpace(string(b)) == "" {
		return in, errors.New("empty hook input")
	}
	if err := json.Unmarshal(b, &in); err != nil {
		return in, fmt.Errorf("hook input: %w", err)
	}
	if in.HookEventName == nil || *in.HookEventName == "" {
		return in, errors.New("hook input: no hook_event_name")
	}
	return in, nil
}

// ParseStopHookInput reads the Stop-hook JSON. Invalid or incomplete input is an error (the hook then
// allows the stop): a Stop event needs a non-empty session_id and a boolean stop_hook_active (a
// missing one is not false); another event needs only its name (the hook skips it).
func ParseStopHookInput(r io.Reader) (StopHookInput, error) {
	raw, err := readHookInput(r)
	if err != nil {
		return StopHookInput{}, err
	}
	in := StopHookInput{HookEventName: *raw.HookEventName}
	if raw.SessionID != nil {
		in.SessionID = *raw.SessionID
	}
	if in.HookEventName != "Stop" {
		return in, nil
	}
	if in.SessionID == "" {
		return in, errors.New("hook input: Stop event without session_id")
	}
	if raw.StopHookActive == nil {
		return in, errors.New("hook input: Stop event without stop_hook_active")
	}
	in.StopHookActive = *raw.StopHookActive
	return in, nil
}

// ParseSessionStartInput reads the SessionStart-hook JSON of `hook bind` and returns its session id.
func ParseSessionStartInput(r io.Reader) (string, error) {
	raw, err := readHookInput(r)
	if err != nil {
		return "", err
	}
	if *raw.HookEventName != "SessionStart" {
		return "", fmt.Errorf("hook input: event %q, want SessionStart", Printable(*raw.HookEventName, 64))
	}
	if raw.SessionID == nil || *raw.SessionID == "" {
		return "", errors.New("hook input: SessionStart event without session_id")
	}
	return *raw.SessionID, nil
}

// StopHookSkips reports whether the hook allows the stop without looking at the store: the session
// is already continuing after a block (stop_hook_active; one block per turn, never a loop), or the
// event is not Stop (the hook registered for another event).
func StopHookSkips(in StopHookInput) bool {
	return in.StopHookActive || in.HookEventName != "Stop"
}

// ownerPendingState says, for the hook's reason, how far a pending job's line got.
func ownerPendingState(state string) string {
	switch state {
	case store.OwnerDelivering:
		return "its job-done line is being delivered or its delivery is retried"
	case store.OwnerNotifyOff:
		return "notify is off, so no job-done line arrives: check it with job_status"
	}
	return "its job-done line arrived"
}

// StopHookDecision is the hook's answer for the bound session: nil allows the stop, otherwise the
// block with its reason.
func StopHookDecision(in StopHookInput, pending []*store.OwnerPending) *StopHookOutput {
	if StopHookSkips(in) || len(pending) == 0 {
		return nil
	}
	parts := make([]string, 0, len(pending))
	for _, p := range pending {
		parts = append(parts, fmt.Sprintf("job %s «%s» (%s; %s)", Printable(p.JobID, 64),
			Printable(p.Title, MaxTitleRunes), p.Status, ownerPendingState(p.State)))
	}
	return &StopHookOutput{
		Decision: "block",
		Reason: "Owner waits: the owner asked for " + strings.Join(parts, ", ") + ", and the job has finished. " +
			"Write the result to the owner over your channel now (read the report with job_result first), " +
			"then call the agent-jobs tool owner_replied (with job_id, or without one for all). " +
			"If you already wrote to the owner, just call owner_replied.",
	}
}

// BindHookSession makes sessionID the orchestrator session the Stop hook holds (`hook bind`, run
// from the orchestrator's SessionStart hook; the last bind wins).
func (m *Manager) BindHookSession(sessionID string) error {
	return m.Store.BindHookSession(sessionID, m.Now())
}

// StopHookHolds reports whether the Stop hook may hold sessionID: it is the bound orchestrator
// session. No bound session, or another one, is never held.
func (m *Manager) StopHookHolds(sessionID string) (bool, error) {
	bound, _, err := m.Store.HookSession()
	if err != nil {
		return false, err
	}
	return bound != "" && bound == sessionID, nil
}

// OwnerPending returns the «owner reply pending» records after dropping the expired ones, and how
// many expired now.
func (m *Manager) OwnerPending() ([]*store.OwnerPending, int, error) {
	return m.Store.OwnerPendingList(m.Now(), m.Cfg.OwnerWaitsMaxAge())
}

// OwnerReplied clears the «owner reply pending» record of job id (delivered or not), or all of them
// when id is "". A finished owner-waits job whose line is not out yet is cleared ahead. A given id
// without anything to clear is an error.
func (m *Manager) OwnerReplied(id string) ([]string, error) {
	id = strings.TrimSpace(id)
	cleared, err := m.Store.ClearOwnerPending(id, m.Now())
	if err != nil {
		return nil, err
	}
	if id != "" && len(cleared) == 0 {
		return nil, errors.New("no owner reply pending for job " + Printable(id, 64))
	}
	return cleared, nil
}
