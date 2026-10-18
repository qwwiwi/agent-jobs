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
	write(`{"max_running": 1}`)
	if c, err := LoadConfig(dir); err != nil || c.MaxRunningJobs() != 1 {
		t.Fatalf("floor: %d %v", c.MaxRunningJobs(), err)
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
	// Explicit null is refused, not read as the default; so is every non-integer value.
	write(`{"max_running": null}`)
	if _, err := LoadConfig(dir); err == nil || !strings.Contains(err.Error(), "max_running must be an integer 1..8, not null") {
		t.Fatalf("max_running null: %v", err)
	}
	for _, v := range []string{`"3"`, "true", "1.5", `{"n": 3}`, "[3]"} {
		write(`{"max_running": ` + v + `}`)
		if _, err := LoadConfig(dir); err == nil || !strings.Contains(err.Error(), "max_running") {
			t.Fatalf("max_running %s: %v", v, err)
		}
	}
}

// The cap is read once when a manager opens: an open manager (the daemon, the MCP server) keeps its
// value after config.json changes; a newly opened one gets the new value.
func TestMaxRunningReadAtOpen(t *testing.T) {
	m := newManager(t)
	if err := os.WriteFile(filepath.Join(m.StateDir, "config.json"), []byte(`{"max_running": 4}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m2, err := Open("test")
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	q1, err1 := m.Queue()
	q2, err2 := m2.Queue()
	if err1 != nil || err2 != nil || q1.ConfiguredMaxRunning != DefaultMaxRunningPerAgent || q2.ConfiguredMaxRunning != 4 {
		t.Fatalf("open manager %+v %v, new manager %+v %v", q1, err1, q2, err2)
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
	if err != nil || qs.ConfiguredMaxRunning != 3 || qs.Running != 3 || qs.Queued != 1 {
		t.Fatalf("queue: %+v %v", qs, err)
	}
}
