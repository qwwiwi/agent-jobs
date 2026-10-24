package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

// `hook brief-reminder` prints one reminder line for the bound session and nothing for another; it
// never blocks and never fails: every error exits 0 with nothing on stdout.
func TestHookBriefReminder(t *testing.T) {
	orch := bindableAgent(t, "test")
	opener := func(agent string) (*jobs.Manager, error) {
		if agent != "test" {
			return nil, errors.New("wrong agent " + agent)
		}
		return jobs.Open(agent)
	}
	run := func(args []string, stdin string) (int, string, string) {
		var out, errb bytes.Buffer
		code := runHook(args, strings.NewReader(stdin), &out, &errb, opener)
		return code, out.String(), errb.String()
	}
	prompt := func(session string) string {
		b, _ := json.Marshal(map[string]string{"session_id": session, "hook_event_name": "UserPromptSubmit",
			"cwd": orch, "prompt": "hello"})
		return string(b)
	}
	reminder := []string{"brief-reminder", "--agent", "test"}
	if code, out, _ := run(reminder, prompt("s1")); code != 0 || out != "" {
		t.Fatalf("no session bound: %d %q", code, out)
	}
	if code, _, errs := run([]string{"bind", "--agent", "test"}, bindEvent("s1", orch)); code != 0 {
		t.Fatalf("bind: %d %q", code, errs)
	}
	code, out, _ := run(reminder, prompt("s1"))
	if code != 0 || strings.Count(out, "\n") != 1 || !strings.HasPrefix(out, jobs.BriefReminderPrefix+jobs.SizeRuleNote) ||
		!strings.Contains(out, "not a git work tree") {
		t.Fatalf("bound session: %d %q", code, out)
	}
	if code, out, _ := run(reminder, prompt("s2")); code != 0 || out != "" {
		t.Fatalf("another session: %d %q", code, out)
	}
	for name, c := range map[string]struct {
		args  []string
		stdin string
	}{
		"bad json":    {reminder, "{"},
		"empty":       {reminder, ""},
		"other event": {reminder, `{"hook_event_name":"Stop","session_id":"s1","stop_hook_active":false}`},
		"wrong agent": {[]string{"brief-reminder", "--agent", "nope"}, prompt("s1")},
		"positional":  {[]string{"brief-reminder", "--agent", "test", "x"}, prompt("s1")},
	} {
		code, out, errs := run(c.args, c.stdin)
		if code != 0 || out != "" || !strings.Contains(errs, "agent-jobs hook brief-reminder:") {
			t.Errorf("%s: %d %q %q", name, code, out, errs)
		}
	}
}
