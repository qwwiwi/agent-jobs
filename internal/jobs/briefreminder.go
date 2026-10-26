package jobs

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/qwwiwi/agent-jobs/internal/checklist"
)

// The brief reminder (Wave F, F9 (c)): `agent-jobs hook brief-reminder`, the UserPromptSubmit hook of the
// orchestrator session, prints one line before an owner prompt of the session bound by `hook bind` only
// when a checklist decision is pending in the repository at the session's cwd: a checklist that is not
// approved (a draft or a void approval), or a brief without a checklist. It prints nothing for a prompt
// the daemon typed («[jobs:…» lines), a repository with an approved, unchanged checklist, or one with no
// brief at all (owner decision 2026-10-25: a line on every prompt cost context and changed nothing). The
// large/small rule is added only while config size_required is on (otherwise size is inferred from
// covers). It is soft: it never blocks a prompt and never fails one (any error exits 0 with nothing on
// stdout).

// BriefReminderPrefix starts the reminder line.
const BriefReminderPrefix = "[agent-jobs] "

// UserPromptInput is the part of Claude Code's UserPromptSubmit-hook input the reminder reads.
type UserPromptInput struct {
	SessionID string
	Cwd       string
	// Prompt is the prompt text ("" when the input has none).
	Prompt string
}

// ParseUserPromptInput reads the UserPromptSubmit-hook JSON: a UserPromptSubmit event with a non-empty
// session_id (cwd may be missing; the line then says so).
func ParseUserPromptInput(r io.Reader) (UserPromptInput, error) {
	raw, err := readHookInput(r)
	if err != nil {
		return UserPromptInput{}, err
	}
	if *raw.HookEventName != "UserPromptSubmit" {
		return UserPromptInput{}, fmt.Errorf("hook input: event %q, want UserPromptSubmit", Printable(*raw.HookEventName, 64))
	}
	if raw.SessionID == nil || *raw.SessionID == "" {
		return UserPromptInput{}, errors.New("hook input: UserPromptSubmit event without session_id")
	}
	in := UserPromptInput{SessionID: *raw.SessionID}
	if raw.Cwd != nil {
		in.Cwd = *raw.Cwd
	}
	if raw.Prompt != nil {
		in.Prompt = *raw.Prompt
	}
	return in, nil
}

// daemonPromptPrefix starts every prompt the daemon types into the session (job-done, schedule,
// notify-owner and check lines).
const daemonPromptPrefix = "[jobs:"

// BriefReminder is the reminder line for the session in ("" for a session other than the bound one, when
// no session is bound, for a daemon prompt, or when no checklist decision is pending).
func (m *Manager) BriefReminder(in UserPromptInput) (string, error) {
	if strings.HasPrefix(strings.TrimSpace(in.Prompt), daemonPromptPrefix) {
		return "", nil
	}
	bound, err := m.StopHookHolds(in.SessionID)
	if err != nil || !bound {
		return "", err
	}
	note := m.checklistNote(in.Cwd)
	if note == "" {
		return "", nil
	}
	line := BriefReminderPrefix + "Checklist: " + note
	if m.Config().SizeRequired {
		line += " " + SizeRuleNote + "."
	}
	return line, nil
}

// checklistNote says what checklist decision is pending in the repository at cwd ("" = none: no brief
// at all, or an approved, unchanged checklist, the check of checklist_get). It reads the brief files
// without the checklist lock: a read that races a checklist_set can misreport for one prompt, and the
// reminder must not wait for a lock.
func (m *Manager) checklistNote(cwd string) string {
	if cwd == "" {
		return ""
	}
	top, main, err := ChecklistRepo(cwd)
	if err != nil {
		return ""
	}
	f, err := checklist.Read(top)
	if err != nil {
		return "unknown (" + RedactPrintable(err.Error(), 200) + ")."
	}
	if f.Checklist == nil && f.Brief == nil {
		return ""
	}
	if f.Checklist == nil {
		return fmt.Sprintf("%s has %s but no %s (derive the rows with checklist_set).", Printable(top, 200), checklist.BriefRel,
			checklist.ChecklistRel)
	}
	if f.Brief == nil {
		return fmt.Sprintf("%s has %s but no %s.", Printable(top, 200), checklist.ChecklistRel, checklist.BriefRel)
	}
	text, _ := Redact(string(f.Checklist))
	c, err := checklist.Parse(text)
	if err != nil {
		return "unreadable " + checklist.ChecklistRel + " (" + RedactPrintable(err.Error(), 200) + ")."
	}
	approved, _, reason, _, err := m.ChecklistStatus(main, c, f.Brief)
	if err != nil {
		return "unknown (" + RedactPrintable(err.Error(), 200) + ")."
	}
	if approved {
		return ""
	}
	return fmt.Sprintf("%s v%d in %s is not approved: %s.", Printable(c.ID, 80), c.Version, Printable(top, 200),
		RedactPrintable(reason, 200))
}
