package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

// `hook stop` prints the block decision while an owner reply is pending, nothing otherwise; with
// stop_hook_active it allows without opening the store; every error fails open with exit 1 (never
// exit 2, which would block the stop).
func TestHookStop(t *testing.T) {
	t.Setenv("AGENT_JOBS_HOME", t.TempDir())
	opens := 0
	opener := func(agent string) (*jobs.Manager, error) {
		opens++
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
	stop := []string{"stop", "--agent", "test"}
	const event = `{"session_id":"s1","hook_event_name":"Stop","stop_hook_active":false}`

	if code, out, _ := run(stop, event); code != 0 || out != "" {
		t.Fatalf("nothing pending: %d %q", code, out)
	}

	m, err := jobs.Open("test")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	j, err := m.Enqueue(jobs.StartRequest{Title: "owner job", Kind: "general", Brief: "b", Model: "sonnet", OwnerWaits: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Store.MarkNotified(j.ID, time.Now()); err != nil {
		t.Fatal(err)
	}

	if code, _, errs := run([]string{"bind", "--agent", "test"}, bindEvent("s1")); code != 0 {
		t.Fatalf("bind: %d %q", code, errs)
	}
	code, out, _ := run(stop, event)
	var dec jobs.StopHookOutput
	if code != 0 || json.Unmarshal([]byte(out), &dec) != nil || dec.Decision != "block" ||
		!strings.Contains(dec.Reason, "job "+j.ID+" «owner job»") {
		t.Fatalf("pending: %d %q", code, out)
	}

	before := opens
	if code, out, _ := run(stop, `{"session_id":"s1","hook_event_name":"Stop","stop_hook_active":true}`); code != 0 || out != "" || opens != before {
		t.Fatalf("stop_hook_active: %d %q opens %d→%d", code, out, before, opens)
	}

	for name, c := range map[string]struct {
		args  []string
		stdin string
	}{
		"bad json":       {stop, "{"},
		"empty input":    {stop, ""},
		"null":           {stop, "null"},
		"empty object":   {stop, "{}"},
		"no session":     {stop, `{"hook_event_name":"Stop","stop_hook_active":false}`},
		"no active flag": {stop, `{"session_id":"s1","hook_event_name":"Stop"}`},
		"no event":       {stop, `{"session_id":"s1","stop_hook_active":false}`},
		"no subcommand":  {nil, event},
		"wrong agent":    {[]string{"stop", "--agent", "other"}, event},
		"positional arg": {append(stop, "x"), event},
	} {
		if code, out, errs := run(c.args, c.stdin); code != 1 || out != "" || !strings.Contains(errs, "the stop is allowed") {
			t.Fatalf("%s: %d %q %q", name, code, out, errs)
		}
	}

	if _, err := m.OwnerReplied(""); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := run(stop, event); code != 0 || out != "" {
		t.Fatalf("after owner_replied: %d %q", code, out)
	}
}

func bindEvent(session string) string {
	return `{"session_id":"` + session + `","hook_event_name":"SessionStart","source":"startup"}`
}

// The Stop hook holds only the orchestrator session bound by `hook bind` (its SessionStart hook):
// before any bind every session is allowed, after it only the bound one is blocked, and a later bind
// (a new session, /clear) moves the binding. A bind with incomplete input fails and keeps the binding.
func TestHookStopSessionBinding(t *testing.T) {
	t.Setenv("AGENT_JOBS_HOME", t.TempDir())
	m, err := jobs.Open("test")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	j, err := m.Enqueue(jobs.StartRequest{Title: "owner job", Kind: "general", Brief: "b", Model: "sonnet", OwnerWaits: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Store.MarkNotified(j.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	run := func(args []string, stdin string) (int, string, string) {
		var out, errb bytes.Buffer
		code := runHook(args, strings.NewReader(stdin), &out, &errb, jobs.Open)
		return code, out.String(), errb.String()
	}
	stopEvent := func(session string) string {
		return `{"session_id":"` + session + `","hook_event_name":"Stop","stop_hook_active":false}`
	}
	stop := []string{"stop", "--agent", "test"}
	bind := []string{"bind", "--agent", "test"}
	blocks := func(session string) bool {
		t.Helper()
		code, out, errs := run(stop, stopEvent(session))
		if code != 0 {
			t.Fatalf("stop %s: %d %q", session, code, errs)
		}
		return strings.Contains(out, `"decision":"block"`)
	}

	if blocks("s1") || blocks("s2") {
		t.Fatal("no session bound: every stop must be allowed")
	}
	if code, out, errs := run(bind, bindEvent("s1")); code != 0 || out != "" {
		t.Fatalf("bind s1: %d %q %q", code, out, errs)
	}
	if !blocks("s1") || blocks("s2") {
		t.Fatal("bound to s1: only s1 is held")
	}
	for name, in := range map[string]string{
		"empty":      "",
		"no session": `{"hook_event_name":"SessionStart"}`,
		"stop event": stopEvent("s2"),
	} {
		if code, out, errs := run(bind, in); code != 1 || out != "" || !strings.Contains(errs, "agent-jobs hook bind") {
			t.Fatalf("bind %s: %d %q %q", name, code, out, errs)
		}
	}
	if !blocks("s1") || blocks("s2") {
		t.Fatal("a failed bind must keep the binding")
	}
	if code, _, errs := run(bind, bindEvent("s2")); code != 0 {
		t.Fatalf("bind s2: %d %q", code, errs)
	}
	if blocks("s1") || !blocks("s2") {
		t.Fatal("rebound to s2: only s2 is held")
	}
}

// `status` without a job id ends with the owner replies pending.
func TestStatusShowsOwnerPending(t *testing.T) {
	t.Setenv("AGENT_JOBS_HOME", t.TempDir())
	m, err := jobs.Open("test")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if out := captureStdout(t, func() error { return printOwnerPending(m) }); !strings.Contains(out, "no owner replies pending") {
		t.Fatalf("none:\n%s", out)
	}
	j, err := m.Enqueue(jobs.StartRequest{Title: "owner job", Kind: "general", Brief: "b", Model: "sonnet", OwnerWaits: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Store.MarkNotified(j.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() error { return printOwnerPending(m) })
	if !strings.Contains(out, "1 owner replies pending (expire after 6h0m0s") || !strings.Contains(out, "job "+j.ID+" queued, line delivered, since") ||
		!strings.Contains(out, "no orchestrator session bound") {
		t.Fatalf("pending:\n%s", out)
	}
	if err := m.BindHookSession("sess-1"); err != nil {
		t.Fatal(err)
	}
	if out := captureStdout(t, func() error { return printOwnerPending(m) }); !strings.Contains(out, "the Stop hook holds session sess-1") {
		t.Fatalf("bound:\n%s", out)
	}
}
