package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

func TestConfigMaxRunning(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) {
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{}`)
	if c, err := LoadConfig(dir); err != nil || c.MaxRunningJobs() != DefaultMaxRunningPerAgent || DefaultMaxRunningPerAgent != 2 {
		t.Fatalf("absent: %d %v", c.MaxRunningJobs(), err)
	}
	write(`{"max_running": 5}`)
	if c, err := LoadConfig(dir); err != nil || c.MaxRunningJobs() != 5 {
		t.Fatalf("5: %d %v", c.MaxRunningJobs(), err)
	}
	write(`{"max_running": 8}`)
	if c, err := LoadConfig(dir); err != nil || c.MaxRunningJobs() != CeilingMaxRunningPerAgent {
		t.Fatalf("ceiling: %d %v", c.MaxRunningJobs(), err)
	}
	for _, v := range []string{"0", "9", "-1"} {
		write(`{"max_running": ` + v + `}`)
		if _, err := LoadConfig(dir); err == nil || !strings.Contains(err.Error(), "max_running "+v+" must be 1..8") {
			t.Fatalf("max_running %s: %v", v, err)
		}
	}
}

func TestConcurrencyCapConfigured(t *testing.T) {
	m := newManager(t)
	three := 3
	m.Cfg.MaxRunning = &three
	var ids []string
	for i := 0; i < 4; i++ {
		ids = append(ids, enqueue(t, m, "FAKE_SLEEP=20", nil).ID)
	}
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if err := m.Tick(); err != nil { // a second pass must not exceed the cap either
		t.Fatal(err)
	}
	n, _ := m.Store.CountRunning()
	q, _ := m.Store.ListByStatus(store.StatusQueued)
	if n != 3 || len(q) != 1 || q[0].ID != ids[3] {
		t.Fatalf("running=%d queued=%d", n, len(q))
	}
	qs, err := m.Queue()
	if err != nil || qs.MaxRunning != 3 || qs.Running != 3 || qs.Queued != 1 {
		t.Fatalf("queue: %+v %v", qs, err)
	}
}
