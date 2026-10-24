package jobs

import (
	"errors"
	"fmt"
	"io"

	"github.com/qwwiwi/agent-jobs/internal/checklist"
)

// The brief reminder (Wave F, F9 (c)): `agent-jobs hook brief-reminder`, the UserPromptSubmit hook of the
// orchestrator session, prints one line before each prompt of the session bound by `hook bind`: the
// large/small rule and whether the repository at the session's cwd has an approved checklist. It is
// soft: it never blocks a prompt and never fails one (any error exits 0 with nothing on stdout).

// BriefReminderPrefix starts the reminder line.
const BriefReminderPrefix = "[agent-jobs] "

// UserPromptInput is the part of Claude Code's UserPromptSubmit-hook input the reminder reads.
type UserPromptInput struct {
	SessionID string
	Cwd       string
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
	return in, nil
}

// BriefReminder is the reminder line for the session in ("" for a session other than the bound one, or
// when no session is bound).
func (m *Manager) BriefReminder(in UserPromptInput) (string, error) {
	bound, err := m.StopHookHolds(in.SessionID)
	if err != nil || !bound {
		return "", err
	}
	return BriefReminderPrefix + SizeRuleNote + ". Checklist: " + m.checklistNote(in.Cwd), nil
}

// checklistNote says whether the repository at cwd has an approved, unchanged checklist (the check of
// checklist_get). It reads the brief files without the checklist lock: a read that races a checklist_set
// can misreport for one prompt, and the reminder must not wait for a lock.
func (m *Manager) checklistNote(cwd string) string {
	if cwd == "" {
		return "unknown (the hook input has no cwd)."
	}
	top, main, err := ChecklistRepo(cwd)
	if err != nil {
		return "none (the session's cwd is not a git work tree)."
	}
	f, err := checklist.Read(top)
	if err != nil {
		return "unknown (" + RedactPrintable(err.Error(), 200) + ")."
	}
	if f.Checklist == nil {
		return "none in " + Printable(top, 200) + " (a large task starts with checklist_init)."
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
		return fmt.Sprintf("%s v%d in %s is approved (%d live requirements).", Printable(c.ID, 80), c.Version,
			Printable(top, 200), len(c.LiveIDs()))
	}
	return fmt.Sprintf("%s v%d in %s is not approved: %s.", Printable(c.ID, 80), c.Version, Printable(top, 200),
		RedactPrintable(reason, 200))
}
