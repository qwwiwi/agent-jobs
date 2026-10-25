package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/checklist"
)

// The reminder is for the bound session's owner prompts only and speaks only when a checklist decision
// is pending in the repository at the session's cwd (owner decision 2026-10-25): a checklist not approved
// (with the reason) or a brief without a checklist. It prints nothing for an approved, unchanged
// checklist, a repository without a brief, no repository, no cwd or a daemon prompt («[jobs:…»). The
// size rule is added only while size_required is on.
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
	remindPrompt := func(cwd, prompt string) string {
		t.Helper()
		line, err := m.BriefReminder(UserPromptInput{SessionID: "s1", Cwd: cwd, Prompt: prompt})
		if err != nil {
			t.Fatal(err)
		}
		if line != "" && (!strings.HasPrefix(line, BriefReminderPrefix+"Checklist: ") || strings.Contains(line, "\n")) {
			t.Fatalf("not the one reminder line: %q", line)
		}
		return line
	}
	remind := func(cwd string) string { t.Helper(); return remindPrompt(cwd, "build the login page") }
	repo := sizeRepo(t, m)
	if line := remind(filepath.Join(repo, "brief")); line != "" {
		t.Errorf("approved: %q", line)
	}
	if err := os.WriteFile(checklist.BriefPath(repo), []byte("# Brief\n\nchanged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if line := remind(repo); !strings.Contains(line, "demo v2 in "+repo+" is not approved: ") || strings.Contains(line, SizeRuleNote) {
		t.Errorf("edited: %q", line)
	}
	if line := remindPrompt(repo, "  [jobs:job-done] job x succeeded"); line != "" {
		t.Errorf("daemon prompt: %q", line)
	}
	m.Cfg.SizeRequired = true
	if line := remind(repo); !strings.HasSuffix(line, " "+SizeRuleNote+".") {
		t.Errorf("size_required on: %q", line)
	}
	m.Cfg.SizeRequired = false
	if line := remind(newRepo(t)); line != "" {
		t.Errorf("no brief: %q", line)
	}
	briefOnly := newRepo(t)
	if err := os.MkdirAll(filepath.Dir(checklist.BriefPath(briefOnly)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(checklist.BriefPath(briefOnly), []byte("# Brief\n\nI want a page.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if line := remind(briefOnly); !strings.Contains(line, "but no "+checklist.ChecklistRel) {
		t.Errorf("brief without a checklist: %q", line)
	}
	if line := remind(t.TempDir()); line != "" {
		t.Errorf("no repo: %q", line)
	}
	if line := remind(""); line != "" {
		t.Errorf("no cwd: %q", line)
	}
}

// The hook input must be a UserPromptSubmit event with a session_id; cwd is optional.
func TestParseUserPromptInput(t *testing.T) {
	in, err := ParseUserPromptInput(strings.NewReader(`{"hook_event_name":"UserPromptSubmit","session_id":"s1","cwd":"/x","prompt":"hi"}`))
	if err != nil || in.SessionID != "s1" || in.Cwd != "/x" || in.Prompt != "hi" {
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
