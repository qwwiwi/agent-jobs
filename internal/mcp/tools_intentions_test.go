package mcp

import (
	"encoding/json"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// The intention tools are orchestrator-only, keep the agent's goals in its store, edit in place,
// filter by status and refuse empty or control-character text, unknown ids and bad statuses.
func TestIntentionTools(t *testing.T) {
	for _, n := range []string{"intention_add", "intention_list", "intention_set"} {
		if !slices.Contains(OrchestratorOnly, n) || slices.Contains(JobAllowed, n) {
			t.Fatalf("%s must be orchestrator-only", n)
		}
	}
	m := newManager(t)
	cs := connect(t, mustServer(t, m))

	var a, b IntentionOutput
	call(t, cs, "intention_add", map[string]any{"text": "  ship   the release ", "due": "2026-11-01", "priority": "high"}, &a)
	if a.Intention == nil || a.Intention.ID == 0 || a.Intention.Text != "ship the release" ||
		a.Intention.Status != store.IntentionOpen || a.Intention.Due != "2026-11-01" || a.Intention.Priority != "high" {
		t.Fatalf("intention_add: %+v", a.Intention)
	}
	call(t, cs, "intention_add", map[string]any{"text": strings.Repeat("я", jobs.MaxIntentionRunes+50)}, &b)
	if b.Intention.Priority != jobs.DefaultIntentionPriority || len([]rune(b.Intention.Text)) != jobs.MaxIntentionRunes {
		t.Fatalf("default priority / cap: %q %d", b.Intention.Priority, len([]rune(b.Intention.Text)))
	}

	for _, c := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"empty text", map[string]any{"text": "   "}, "text is required"},
		{"newline", map[string]any{"text": "one\ntwo"}, "control character"},
		{"escape", map[string]any{"text": "x\x1b[2J"}, "control character"},
		{"bidi override", map[string]any{"text": "a‮b"}, "control character"},
		{"bad due", map[string]any{"text": "x", "due": "tomorrow"}, "YYYY-MM-DD"},
		{"bad priority", map[string]any{"text": "x", "priority": "urgent"}, "priority"},
	} {
		if msg := callErr(t, cs, "intention_add", c.args); !strings.Contains(msg, c.want) {
			t.Fatalf("intention_add %s: %q does not mention %q", c.name, msg, c.want)
		}
	}
	for _, c := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"unknown id", map[string]any{"id": 999, "status": "done"}, "not found"},
		{"no id", map[string]any{"status": "done"}, `missing properties: ["id"]`},
		{"zero id", map[string]any{"id": 0, "status": "done"}, "id is required"},
		{"bad status", map[string]any{"id": a.Intention.ID, "status": "deleted"}, "must be one of"},
		{"nothing to set", map[string]any{"id": a.Intention.ID}, "status, a text or both"},
		{"control text", map[string]any{"id": a.Intention.ID, "text": "a\tb"}, "control character"},
	} {
		if msg := callErr(t, cs, "intention_set", c.args); !strings.Contains(msg, c.want) {
			t.Fatalf("intention_set %s: %q does not mention %q", c.name, msg, c.want)
		}
	}

	var set IntentionOutput
	call(t, cs, "intention_set", map[string]any{"id": a.Intention.ID, "status": "done", "text": "ship v0.4"}, &set)
	if set.Intention.ID != a.Intention.ID || set.Intention.Status != store.IntentionDone ||
		set.Intention.Text != "ship v0.4" || set.Intention.ClosedAt == nil {
		t.Fatalf("intention_set: %+v", set.Intention)
	}

	var all, open, done IntentionsOutput
	call(t, cs, "intention_list", map[string]any{}, &all)
	call(t, cs, "intention_list", map[string]any{"status": "open"}, &open)
	call(t, cs, "intention_list", map[string]any{"status": "done"}, &done)
	if len(all.Intentions) != 2 || len(open.Intentions) != 1 || open.Intentions[0].ID != b.Intention.ID ||
		len(done.Intentions) != 1 || done.Intentions[0].ID != a.Intention.ID {
		t.Fatalf("list: all %d, open %+v, done %+v", len(all.Intentions), open.Intentions, done.Intentions)
	}
	var none IntentionsOutput
	call(t, cs, "intention_list", map[string]any{"status": "dropped"}, &none)
	if none.Intentions == nil || len(none.Intentions) != 0 {
		t.Fatalf("empty filter must be an empty list: %+v", none.Intentions)
	}
	if msg := callErr(t, cs, "intention_list", map[string]any{"status": "closed"}); !strings.Contains(msg, "must be one of") {
		t.Fatalf("bad filter: %q", msg)
	}
}

// CLI parity: `agent-jobs intentions` lists what the tools wrote, with the same status filter and
// the same JSON as intention_list.
func TestIntentionsCommand(t *testing.T) {
	m := newManager(t) // sets AGENT_JOBS_HOME for the binary too
	cs := connect(t, mustServer(t, m))
	var a, b IntentionOutput
	call(t, cs, "intention_add", map[string]any{"text": "ship the release", "due": "2026-11-01"}, &a)
	call(t, cs, "intention_add", map[string]any{"text": "write the guide"}, &b)
	call(t, cs, "intention_set", map[string]any{"id": b.Intention.ID, "status": "dropped"}, &b)

	out, err := exec.Command(selfBin, "intentions", "--agent", "test").CombinedOutput()
	if err != nil {
		t.Fatalf("intentions: %v\n%s", err, out)
	}
	for _, want := range []string{"ID", "STATUS", "ship the release", "2026-11-01", "write the guide", "dropped"} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("table lacks %q:\n%s", want, out)
		}
	}
	out, err = exec.Command(selfBin, "intentions", "--agent", "test", "--status", "open", "--json").Output()
	if err != nil {
		t.Fatalf("intentions --json: %v", err)
	}
	var got IntentionsOutput
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	if len(got.Intentions) != 1 || got.Intentions[0].ID != a.Intention.ID || got.Intentions[0].Text != "ship the release" {
		t.Fatalf("--status open --json: %s", out)
	}
	if out, err := exec.Command(selfBin, "intentions", "--agent", "test", "--status", "later").CombinedOutput(); err == nil ||
		!strings.Contains(string(out), "must be one of") {
		t.Fatalf("bad status: %v %s", err, out)
	}
}
