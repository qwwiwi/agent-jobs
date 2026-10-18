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

	code, out, _ := run(stop, event)
	var dec jobs.StopHookOutput
	if code != 0 || json.Unmarshal([]byte(out), &dec) != nil || dec.Decision != "block" ||
		!strings.Contains(dec.Reason, "job "+j.ID+" «owner job»") {
		t.Fatalf("pending: %d %q", code, out)
	}

	before := opens
	if code, out, _ := run(stop, `{"hook_event_name":"Stop","stop_hook_active":true}`); code != 0 || out != "" || opens != before {
		t.Fatalf("stop_hook_active: %d %q opens %d→%d", code, out, before, opens)
	}

	for name, c := range map[string]struct {
		args  []string
		stdin string
	}{
		"bad json":       {stop, "{"},
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
	if !strings.Contains(out, "1 owner replies pending (expire after 6h0m0s") || !strings.Contains(out, "job "+j.ID+" queued since") {
		t.Fatalf("pending:\n%s", out)
	}
}
