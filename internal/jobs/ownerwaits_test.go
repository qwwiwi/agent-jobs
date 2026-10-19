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
	if line := DoneLine(j, 0); strings.Contains(line, OwnerWaitsMarker) {
		t.Fatalf("plain job carries the marker: %q", line)
	}
	j.OwnerWaits, j.Attempt = true, 2
	if line := DoneLine(j, 0); !strings.HasSuffix(line, " · resumed (attempt 2) · owner waits") {
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

	// A cleared record stays cleared when the same attempt's line is marked again.
	if err := m.Store.MarkNotified(w.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if list, _, _ := m.OwnerPending(); len(list) != 0 {
		t.Fatalf("a cleared record came back: %+v", list)
	}

	// Max age: a record older than owner_waits_max_hours expires on the next look.
	w2 := enqueue(t, m, "brief", func(r *StartRequest) { r.OwnerWaits = true })
	if err := m.Store.MarkNotified(w2.ID, time.Now()); err != nil {
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
	if out == nil || out.Decision != "block" || !strings.Contains(out.Reason, "job j1 «the[2J report» (succeeded; its job-done line arrived)") ||
		!strings.Contains(out.Reason, "owner_replied") || hasControl(out.Reason) {
		t.Fatalf("block: %+v", out)
	}
	if out := StopHookDecision(in, nil); out != nil {
		t.Fatalf("nothing pending must allow: %+v", out)
	}
	active, err := ParseStopHookInput(strings.NewReader(`{"session_id":"s","hook_event_name":"Stop","stop_hook_active":true}`))
	if err != nil || !StopHookSkips(active) {
		t.Fatalf("stop_hook_active: %+v %v", active, err)
	}
	if out := StopHookDecision(active, pending); out != nil {
		t.Fatalf("stop_hook_active must allow: %+v", out)
	}
	if out := StopHookDecision(StopHookInput{HookEventName: "SubagentStop"}, pending); out != nil {
		t.Fatalf("another event must allow: %+v", out)
	}
	if _, err := ParseStopHookInput(strings.NewReader("{not json")); err == nil {
		t.Fatal("bad JSON must be an error")
	}
	if _, err := ParseStopHookInput(strings.NewReader(strings.Repeat(" ", MaxHookInputBytes+1))); err == nil {
		t.Fatal("oversized input must be an error")
	}
}

// Incomplete or invalid hook input is an error (the hook then allows the stop), never an inactive
// Stop event: a missing stop_hook_active is not false, a missing session_id cannot match the bound
// session. Another event needs only its name.
func TestStopHookInputValidation(t *testing.T) {
	for _, bad := range []string{
		"", "  \n", "null", "{}", "[]", `"Stop"`, "{not json",
		`{"session_id":"s","stop_hook_active":false}`,
		`{"hook_event_name":"","session_id":"s","stop_hook_active":false}`,
		`{"hook_event_name":"Stop","stop_hook_active":false}`,
		`{"hook_event_name":"Stop","session_id":"","stop_hook_active":false}`,
		`{"hook_event_name":"Stop","session_id":"s"}`,
		`{"hook_event_name":"Stop","session_id":"s","stop_hook_active":null}`,
		`{"hook_event_name":"Stop","session_id":"s","stop_hook_active":"false"}`,
		`{"hook_event_name":"Stop","session_id":1,"stop_hook_active":false}`,
	} {
		if in, err := ParseStopHookInput(strings.NewReader(bad)); err == nil {
			t.Fatalf("%q must be an error, got %+v", bad, in)
		}
	}
	in, err := ParseStopHookInput(strings.NewReader(`{"hook_event_name":"SubagentStop"}`))
	if err != nil || !StopHookSkips(in) {
		t.Fatalf("another event: %+v %v", in, err)
	}
	in, err = ParseStopHookInput(strings.NewReader(`{"session_id":"s","hook_event_name":"Stop","stop_hook_active":false,"transcript_path":"/x"}`))
	if err != nil || StopHookSkips(in) || in.SessionID != "s" {
		t.Fatalf("complete Stop event: %+v %v", in, err)
	}
}

// The pending record exists before the job-done line reaches the session: a Stop hook in the gap
// between delivery and the notified mark already blocks, and an owner_replied in that gap clears the
// record for good (the mark does not bring it back).
func TestOwnerRepliedInDeliveryGap(t *testing.T) {
	m := newManager(t)
	notifyTo(t, m)
	w := enqueue(t, m, "brief", func(r *StartRequest) { r.OwnerWaits = true })
	if err := m.Cancel(w.ID); err != nil {
		t.Fatal(err)
	}
	var gapPending []*store.OwnerPending
	var gapCleared []string
	var gapErr error
	afterDoneLineSent = func(j *store.Job) {
		gapPending, _, _ = m.OwnerPending()
		gapCleared, gapErr = m.OwnerReplied(j.ID)
	}
	t.Cleanup(func() { afterDoneLineSent = nil })
	if err := m.notifyPending(); err != nil {
		t.Fatal(err)
	}
	if len(gapPending) != 1 || gapPending[0].JobID != w.ID {
		t.Fatalf("in the gap the record must already be pending: %+v", gapPending)
	}
	if gapErr != nil || len(gapCleared) != 1 || gapCleared[0] != w.ID {
		t.Fatalf("owner_replied in the gap: %v %v", gapCleared, gapErr)
	}
	if list, _, _ := m.OwnerPending(); len(list) != 0 {
		t.Fatalf("the reply in the gap was lost, still pending: %+v", list)
	}
	if j, _ := m.Store.Get(w.ID); j.NotifiedAt == nil {
		t.Fatal("the job is not marked notified")
	}
}

// owner_replied with the id of a finished owner-waits job whose line is not out yet acknowledges it:
// the later line records nothing pending.
func TestOwnerRepliedBeforeDelivery(t *testing.T) {
	m := newManager(t)
	notifyTo(t, m)
	w := enqueue(t, m, "brief", func(r *StartRequest) { r.OwnerWaits = true })
	queued := enqueue(t, m, "brief", func(r *StartRequest) { r.OwnerWaits = true })
	if _, err := m.OwnerReplied(queued.ID); err == nil {
		t.Fatal("owner_replied on an unfinished job must be an error")
	}
	if err := m.Cancel(w.ID); err != nil {
		t.Fatal(err)
	}
	if cleared, err := m.OwnerReplied(w.ID); err != nil || len(cleared) != 1 {
		t.Fatalf("owner_replied before the line: %v %v", cleared, err)
	}
	if err := m.notifyPending(); err != nil {
		t.Fatal(err)
	}
	if list, _, _ := m.OwnerPending(); len(list) != 0 {
		t.Fatalf("pending after an early reply: %+v", list)
	}
}

// With notify off (no notify_tmux_target) the job still records the pending reply, and the hook's
// reason does not claim a job-done line arrived.
func TestOwnerWaitsNotifyOffReason(t *testing.T) {
	m := newManager(t)
	m.Cfg.NotifyTmuxTarget = ""
	w := enqueue(t, m, "brief", func(r *StartRequest) { r.OwnerWaits = true })
	if err := m.Cancel(w.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.notifyPending(); err != nil {
		t.Fatal(err)
	}
	list, _, err := m.OwnerPending()
	if err != nil || len(list) != 1 {
		t.Fatalf("pending with notify off: %+v %v", list, err)
	}
	out := StopHookDecision(StopHookInput{HookEventName: "Stop", SessionID: "s"}, list)
	if out == nil || strings.Contains(out.Reason, "arrived") || !strings.Contains(out.Reason, "notify is off") {
		t.Fatalf("notify-off reason: %+v", out)
	}
}

// A job-done line whose delivery keeps failing holds the session at most owner_waits_max_hours:
// once expired, the retries of every later tick never block again.
func TestOwnerWaitsFailingDeliveryExpiresOnce(t *testing.T) {
	m := newManager(t)
	m.Cfg.NotifyTmuxTarget = "nowhere"
	t.Setenv("AGENT_JOBS_TMUX_BIN", "/usr/bin/false")
	w := enqueue(t, m, "brief", func(r *StartRequest) { r.OwnerWaits = true })
	if err := m.Cancel(w.ID); err != nil {
		t.Fatal(err)
	}
	m.Cfg.OwnerWaitsMaxHours = 1
	base := time.Now()
	for i := range 5 {
		at := base.Add(time.Duration(i) * 50 * time.Minute)
		m.Now = func() time.Time { return at }
		if err := m.notifyPending(); err != nil {
			t.Fatal(err)
		}
		list, _, err := m.OwnerPending()
		if err != nil {
			t.Fatal(err)
		}
		if blocks := len(list) == 1; blocks != (i < 2) {
			t.Fatalf("tick %d (+%d min): pending %v", i, i*50, list)
		}
	}
	if j, _ := m.Store.Get(w.ID); j.NotifiedAt != nil {
		t.Fatal("the line was never delivered")
	}
}

// orchestrator_dir must be absolute and not /.
func TestOrchestratorDirConfig(t *testing.T) {
	for _, bad := range []string{`"rel/dir"`, `"/"`} {
		dir := t.TempDir()
		if err := os.WriteFile(dir+"/config.json", []byte(`{"orchestrator_dir": `+bad+`}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(dir); err == nil || !strings.Contains(err.Error(), "orchestrator_dir") {
			t.Fatalf("%s: %v", bad, err)
		}
	}
}
