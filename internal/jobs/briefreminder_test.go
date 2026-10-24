package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/checklist"
)

// The reminder is for the bound session only and names the size rule and the checklist state of the
// repository at the session's cwd: approved, not approved (with the reason), none, or no repository.
func TestBriefReminder(t *testing.T) {
	m := newManager(t)
	if line, err := m.BriefReminder(UserPromptInput{SessionID: "s1", Cwd: t.TempDir()}); err != nil || line != "" {
		t.Fatalf("no session bound: %q %v", line, err)
	}
	if err := m.Store.BindHookSession("s1", m.Now()); err != nil {
		t.Fatal(err)
	}
	if line, err := m.BriefReminder(UserPromptInput{SessionID: "other", Cwd: t.TempDir()}); err != nil || line != "" {
		t.Fatalf("another session: %q %v", line, err)
	}
	remind := func(cwd string) string {
		t.Helper()
		line, err := m.BriefReminder(UserPromptInput{SessionID: "s1", Cwd: cwd})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(line, BriefReminderPrefix+SizeRuleNote+". Checklist: ") || strings.Contains(line, "\n") {
			t.Fatalf("not the one reminder line: %q", line)
		}
		return line
	}
	repo := sizeRepo(t, m)
	if line := remind(filepath.Join(repo, "brief")); !strings.Contains(line, "demo v2 in "+repo+" is approved (3 live requirements).") {
		t.Errorf("approved: %q", line)
	}
	if err := os.WriteFile(checklist.BriefPath(repo), []byte("# Brief\n\nchanged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if line := remind(repo); !strings.Contains(line, "demo v2 in "+repo+" is not approved: ") {
		t.Errorf("edited: %q", line)
	}
	if line := remind(newRepo(t)); !strings.Contains(line, "Checklist: none in ") {
		t.Errorf("no checklist: %q", line)
	}
	if line := remind(t.TempDir()); !strings.Contains(line, "not a git work tree") {
		t.Errorf("no repo: %q", line)
	}
	if line := remind(""); !strings.Contains(line, "no cwd") {
		t.Errorf("no cwd: %q", line)
	}
}

// The hook input must be a UserPromptSubmit event with a session_id; cwd is optional.
func TestParseUserPromptInput(t *testing.T) {
	in, err := ParseUserPromptInput(strings.NewReader(`{"hook_event_name":"UserPromptSubmit","session_id":"s1","cwd":"/x","prompt":"hi"}`))
	if err != nil || in.SessionID != "s1" || in.Cwd != "/x" {
		t.Fatalf("%+v %v", in, err)
	}
	for name, raw := range map[string]string{
		"other event": `{"hook_event_name":"Stop","session_id":"s1"}`,
		"no session":  `{"hook_event_name":"UserPromptSubmit"}`,
		"bad json":    `{`,
		"empty":       ``,
	} {
		if _, err := ParseUserPromptInput(strings.NewReader(raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
