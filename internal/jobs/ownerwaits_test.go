package jobs

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// The job-done line of an owner-waits job ends with the marker; other jobs' lines do not carry it.
func TestDoneLineOwnerWaitsMarker(t *testing.T) {
	j := &store.Job{ID: "id1", Title: "t", Status: store.StatusSucceeded}
	if line := DoneLine(j); strings.Contains(line, OwnerWaitsMarker) {
		t.Fatalf("plain job carries the marker: %q", line)
	}
	j.OwnerWaits, j.Attempt = true, 2
	if line := DoneLine(j); !strings.HasSuffix(line, " · resumed (attempt 2) · owner waits") {
		t.Fatalf("owner-waits line: %q", line)
	}
}

// An owner-waits job stores the flag; its delivered job-done line carries the marker and records
// «owner reply pending»; a plain job's line records nothing; OwnerReplied clears it; the record
// expires after owner_waits_max_hours.
func TestOwnerWaitsPendingOnDelivery(t *testing.T) {
	m := newManager(t)
	logf := notifyTo(t, m)
	w := enqueue(t, m, "brief", func(r *StartRequest) { r.OwnerWaits = true; r.Title = "owner job" })
	p := enqueue(t, m, "brief", nil)
	if got, err := m.Store.Get(w.ID); err != nil || !got.OwnerWaits {
		t.Fatalf("stored flag: %+v %v", got, err)
	}
	for _, id := range []string{w.ID, p.ID} {
		if err := m.Cancel(id); err != nil {
			t.Fatal(err)
		}
	}
	if list, _, err := m.OwnerPending(); err != nil || len(list) != 0 {
		t.Fatalf("pending before the line: %v %v", list, err)
	}
	if err := m.notifyPending(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(logf)
	if err != nil {
		t.Fatal(err)
	}
	var wLine, pLine string
	for _, l := range strings.Split(string(b), "\n") {
		switch {
		case strings.Contains(l, "[jobs:job-done] job "+w.ID):
			wLine = l
		case strings.Contains(l, "[jobs:job-done] job "+p.ID):
			pLine = l
		}
	}
	if !strings.Contains(wLine, "cancelled · 0 turns") || !strings.Contains(wLine, "· owner waits|") {
		t.Fatalf("owner-waits line: %q\n%s", wLine, b)
	}
	if pLine == "" || strings.Contains(pLine, OwnerWaitsMarker) {
		t.Fatalf("plain line: %q", pLine)
	}
	list, _, err := m.OwnerPending()
	if err != nil || len(list) != 1 || list[0].JobID != w.ID || list[0].Title != "owner job" || list[0].Status != store.StatusCancelled {
		t.Fatalf("pending: %+v %v", list, err)
	}

	if _, err := m.OwnerReplied(p.ID); err == nil || !strings.Contains(err.Error(), "no owner reply pending") {
		t.Fatalf("owner_replied on a job without a record: %v", err)
	}
	if cleared, err := m.OwnerReplied(" " + w.ID + " "); err != nil || len(cleared) != 1 || cleared[0] != w.ID {
		t.Fatalf("owner_replied: %v %v", cleared, err)
	}
	if list, _, _ := m.OwnerPending(); len(list) != 0 {
		t.Fatalf("still pending: %+v", list)
	}
	if cleared, err := m.OwnerReplied(""); err != nil || len(cleared) != 0 {
		t.Fatalf("owner_replied all with none: %v %v", cleared, err)
	}

	// Max age: a record older than owner_waits_max_hours expires on the next look.
	if err := m.Store.MarkNotified(w.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	m.Cfg.OwnerWaitsMaxHours = 2
	m.Now = func() time.Time { return time.Now().Add(time.Hour) }
	if list, expired, _ := m.OwnerPending(); len(list) != 1 || expired != 0 {
		t.Fatalf("within max age: %v %d", list, expired)
	}
	m.Now = func() time.Time { return time.Now().Add(2*time.Hour + time.Minute) }
	if list, expired, _ := m.OwnerPending(); len(list) != 0 || expired != 1 {
		t.Fatalf("after max age: %v %d", list, expired)
	}
}

// The default max age is 6 h; a negative owner_waits_max_hours is refused.
func TestOwnerWaitsMaxAgeConfig(t *testing.T) {
	if got := (Config{}).OwnerWaitsMaxAge(); got != 6*time.Hour {
		t.Fatalf("default: %s", got)
	}
	if got := (Config{OwnerWaitsMaxHours: 1}).OwnerWaitsMaxAge(); got != time.Hour {
		t.Fatalf("1 h: %s", got)
	}
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/config.json", []byte(`{"owner_waits_max_hours": -1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(dir); err == nil || !strings.Contains(err.Error(), "owner_waits_max_hours") {
		t.Fatalf("negative max hours: %v", err)
	}
}

// The Stop hook blocks while a record is pending, allows without one, and allows once the session
// already continues after a block (stop_hook_active: one block per turn, never a loop) or for
// another event.
func TestStopHookDecision(t *testing.T) {
	pending := []*store.OwnerPending{{JobID: "j1", Title: "the\x1b[2J report", Status: store.StatusSucceeded}}
	in, err := ParseStopHookInput(strings.NewReader(`{"session_id":"s","hook_event_name":"Stop","stop_hook_active":false}`))
	if err != nil {
		t.Fatal(err)
	}
	out := StopHookDecision(in, pending)
	if out == nil || out.Decision != "block" || !strings.Contains(out.Reason, "job j1 «the[2J report» (succeeded)") ||
		!strings.Contains(out.Reason, "owner_replied") || hasControl(out.Reason) {
		t.Fatalf("block: %+v", out)
	}
	if out := StopHookDecision(in, nil); out != nil {
		t.Fatalf("nothing pending must allow: %+v", out)
	}
	active, err := ParseStopHookInput(strings.NewReader(`{"hook_event_name":"Stop","stop_hook_active":true}`))
	if err != nil || !StopHookSkips(active) {
		t.Fatalf("stop_hook_active: %+v %v", active, err)
	}
	if out := StopHookDecision(active, pending); out != nil {
		t.Fatalf("stop_hook_active must allow: %+v", out)
	}
	if out := StopHookDecision(StopHookInput{HookEventName: "SubagentStop"}, pending); out != nil {
		t.Fatalf("another event must allow: %+v", out)
	}
	if in, err := ParseStopHookInput(strings.NewReader("")); err != nil || StopHookDecision(in, pending) == nil {
		t.Fatalf("empty input blocks while pending: %v", err)
	}
	if _, err := ParseStopHookInput(strings.NewReader("{not json")); err == nil {
		t.Fatal("bad JSON must be an error")
	}
	if _, err := ParseStopHookInput(strings.NewReader(strings.Repeat(" ", MaxHookInputBytes+1))); err == nil {
		t.Fatal("oversized input must be an error")
	}
}
