package jobs

import (
	"path/filepath"
	"strings"
	"testing"
)

// With sinks only (no tmux target), an SLO breach note rides on the queued job-done line. When the
// outbox insert is rejected, the breach stays pending, so the next tick's line carries the notice:
// exactly once, and the breach is marked noted only with the line that carried it.
func TestSinkOnlyBreachNoteSurvivesFailedEnqueue(t *testing.T) {
	m := newManager(t)
	db := rawDB(t, m)
	inbox := filepath.Join(t.TempDir(), "inbox")
	m.Cfg.Notify = NotifyConfig{InboxDir: inbox}
	m.Cfg.SLO.Targets = map[string]float64{"deploy": 600}
	now := coalesceClock(m)
	if _, err := m.RecordSLO("deploy", 700); err != nil {
		t.Fatal(err)
	}
	j := finishAt(t, m, now, *now, nil)
	mustExec(t, db, `CREATE TRIGGER fail_outbox BEFORE INSERT ON notify_sink_outbox BEGIN SELECT RAISE(ABORT, 'planted fault'); END`)
	_ = m.Tick() // the insert is rejected; the tick may report it
	if get(t, m, j.ID).NotifiedAt != nil {
		t.Fatal("job marked notified although its line was not queued")
	}
	if bs, err := m.Store.UnnotedSLOBreaches(); err != nil || len(bs) != 1 {
		t.Fatalf("breach marked noted by a rolled-back enqueue: %+v %v", bs, err)
	}
	mustExec(t, db, `DROP TRIGGER fail_outbox`)
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	evs := pullAck(t, inbox)
	if len(evs) != 1 {
		t.Fatalf("inbox events: %+v", evs)
	}
	if got := strings.Count(evs[0].Line, "slo breach"); got != 1 {
		t.Fatalf("breach notice appears %d times in %q", got, evs[0].Line)
	}
	if bs, err := m.Store.UnnotedSLOBreaches(); err != nil || len(bs) != 0 {
		t.Fatalf("breach not marked noted with the queued line: %+v %v", bs, err)
	}
}
