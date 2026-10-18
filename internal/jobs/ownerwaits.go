package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// «Owner waits»: a job started at the owner's request (job_start owner_waits, start --owner-waits)
// ends its job-done line with OwnerWaitsMarker. When the job has finished, «owner reply pending» is
// recorded for it BEFORE its line is delivered (store.BeginOwnerPending; with notify off it is
// recorded in place of the line), and the delivered line marks it delivered (store.MarkNotified).
// Until the orchestrator calls owner_replied, the Stop hook of the orchestrator session
// (`agent-jobs hook stop`) does not let that session end its turn silently. The hook holds only the
// session bound by `agent-jobs hook bind` (the orchestrator's SessionStart hook, accepted only from
// the notify tmux pane with its cwd inside orchestrator_dir); with no session bound, or for any other
// session, it allows. Records are kept per (job, attempt); one older than owner_waits_max_hours
// (default 6) expires on its own and stays expired.

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
	Cwd            *string `json:"cwd"`
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

// SessionStartInput is the part of Claude Code's SessionStart-hook input `hook bind` reads.
type SessionStartInput struct {
	SessionID string
	// Cwd is the session's working directory; it must lie inside orchestrator_dir.
	Cwd string
}

// ParseSessionStartInput reads the SessionStart-hook JSON of `hook bind`: a SessionStart event with a
// non-empty session_id and cwd.
func ParseSessionStartInput(r io.Reader) (SessionStartInput, error) {
	raw, err := readHookInput(r)
	if err != nil {
		return SessionStartInput{}, err
	}
	if *raw.HookEventName != "SessionStart" {
		return SessionStartInput{}, fmt.Errorf("hook input: event %q, want SessionStart", Printable(*raw.HookEventName, 64))
	}
	if raw.SessionID == nil || *raw.SessionID == "" {
		return SessionStartInput{}, errors.New("hook input: SessionStart event without session_id")
	}
	if raw.Cwd == nil || *raw.Cwd == "" {
		return SessionStartInput{}, errors.New("hook input: SessionStart event without cwd")
	}
	return SessionStartInput{SessionID: *raw.SessionID, Cwd: *raw.Cwd}, nil
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

// BindHookSession makes in.SessionID the orchestrator session the Stop hook holds (`hook bind`, run
// from the orchestrator's SessionStart hook). pane is the caller's $TMUX_PANE. The session is bound
// only when (a) pane is the pane the configured notify tmux target resolves to (no target configured:
// refused), and (b) in.Cwd lies inside the configured orchestrator_dir (none configured: refused).
// (c) A job session is refused before this, by the CLI (refuseInsideJob, open). Replacing an existing
// binding obeys the same checks, so only a new session in the same pane and directory rebinds; a
// refused bind keeps the old one. A process the orchestrator starts itself in that pane and
// directory is inside its trust domain and is not told apart.
func (m *Manager) BindHookSession(in SessionStartInput, pane string) error {
	if m.Cfg.NotifyTmuxTarget == "" {
		return errors.New("bind refused: no notify_tmux_target configured (the Stop hook never blocks)")
	}
	if m.Cfg.OrchestratorDir == "" {
		return errors.New("bind refused: no orchestrator_dir configured (the Stop hook never blocks)")
	}
	if pane == "" {
		return errors.New("bind refused: not inside tmux ($TMUX_PANE is not set)")
	}
	want, err := m.tmuxPaneID(m.Cfg.NotifyTmuxTarget)
	if err != nil {
		return fmt.Errorf("bind refused: %w", err)
	}
	if pane != want {
		return fmt.Errorf("bind refused: pane %s is not the notify pane %s of notify_tmux_target", Printable(pane, 32), want)
	}
	if err := checkInsideDir(in.Cwd, m.Cfg.OrchestratorDir); err != nil {
		return fmt.Errorf("bind refused: %w", err)
	}
	return m.Store.BindHookSession(in.SessionID, m.Now())
}

// rePaneID is a tmux pane id (#{pane_id}).
var rePaneID = regexp.MustCompile(`^%[0-9]+$`)

// tmuxPaneID resolves target to its pane id with `tmux display-message -p -t target '#{pane_id}'`,
// on the configured socket, bounded by TmuxDeliveryTimeout.
func (m *Manager) tmuxPaneID(target string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), TmuxDeliveryTimeout)
	defer cancel()
	out, err := m.runTmuxOutput(ctx, "display-message", "-p", "-t", target, "#{pane_id}")
	if err != nil {
		return "", fmt.Errorf("resolve the pane of notify_tmux_target with tmux: %w", err)
	}
	id := strings.TrimSpace(out)
	if !rePaneID.MatchString(id) {
		return "", fmt.Errorf("resolve the pane of notify_tmux_target with tmux: got %q, want a pane id", Printable(id, 32))
	}
	return id, nil
}

// checkInsideDir accepts an absolute cwd that, with symlinks resolved, is dir or lies below it.
func checkInsideDir(cwd, dir string) error {
	if !filepath.IsAbs(cwd) {
		return fmt.Errorf("cwd %q is not absolute, so not inside orchestrator_dir", Printable(cwd, 200))
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("orchestrator_dir: %w", err)
	}
	realCwd, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return fmt.Errorf("cwd %q (orchestrator_dir check): %w", Printable(cwd, 200), err)
	}
	rel, err := filepath.Rel(realDir, realCwd)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("cwd %q is not inside orchestrator_dir", Printable(cwd, 200))
	}
	return nil
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

// OwnerReplied acknowledges every attempt of job id (delivered or not) and pre-acknowledges its
// latest finished attempt, or acknowledges every open record when id is "". A given id without
// anything to acknowledge is an error.
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
