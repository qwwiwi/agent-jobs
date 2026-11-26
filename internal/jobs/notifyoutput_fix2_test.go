package jobs

import (
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// A failing helper's output never reaches notify_sink_outbox.error: an opaque credential it prints
// (no label, no known prefix, so redaction cannot spot it) stays out of SQLite. The row records a
// structured reason: the exit code and how many bytes of output there were.
func TestNotifyCommandFailureOutputNotPersisted(t *testing.T) {
	const secret = "Zq8xT3vLm9KpR2wYb7NcF4hJ6dSa1GeU5oIiXyQ0"
	m := newManager(t)
	m.Cfg.Notify.Command = []string{"/bin/sh", "-c", "cat >/dev/null; printf '%s' " + secret + " >&2; exit 3"}
	now := coalesceClock(m)
	if err := m.Store.MarkNotifiedSinks(nil, *now, nil, &store.SinkDelivery{Ref: "job-done:x@1.1", Line: "[jobs:job-done] job x"}); err != nil {
		t.Fatal(err)
	}
	m.deliverSinks()
	d, err := m.Store.GetSinkDelivery("job-done:x@1.1")
	if err != nil || d.State != store.SinkFailed {
		t.Fatalf("row: %+v %v", d, err)
	}
	if strings.Contains(d.Error, secret) || strings.Contains(d.Error, secret[:12]) {
		t.Fatalf("helper output persisted: %q", d.Error)
	}
	want := "notify.command: exit status 3, " + "40 bytes of output"
	if d.Error != want {
		t.Fatalf("error %q, want %q", d.Error, want)
	}
}
