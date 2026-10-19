package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIsOrchestratorMCP(t *testing.T) {
	for _, c := range []struct {
		argv []string
		want bool
	}{
		{[]string{"/bin/agent-jobs", "mcp", "--role", "orchestrator", "--agent", "a"}, true},
		{[]string{"agent-jobs", "mcp", "--agent=a", "-role=orchestrator"}, true},
		{[]string{"agent-jobs", "mcp", "--role", "job", "--agent", "a"}, false},
		{[]string{"agent-jobs", "mcp", "--role", "orchestrator", "--agent", "b"}, false},
		{[]string{"agent-jobs", "daemon", "--role", "orchestrator", "--agent", "a"}, false},
		{[]string{"agent-jobs"}, false},
	} {
		if got := isOrchestratorMCP(c.argv, "a"); got != c.want {
			t.Errorf("%v: %v, want %v", c.argv, got, c.want)
		}
	}
}

// daemon.json round-trips; another version, or another build of the same path, is a mismatch.
func TestDaemonInfoMismatch(t *testing.T) {
	m := newManager(t)
	bin := filepath.Join(t.TempDir(), "agent-jobs")
	if err := os.WriteFile(bin, []byte("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	m.SelfBin = bin
	if s := m.DaemonMismatch(FileIdentity{}); s != "" {
		t.Fatalf("no daemon.json: %q", s)
	}
	self, err := StatIdentity(bin)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.WriteDaemonInfo(); err != nil {
		t.Fatal(err)
	}
	if s := m.DaemonMismatch(self); s != "" {
		t.Fatalf("same build: %q", s)
	}
	if err := os.WriteFile(bin, []byte("v2 longer"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(bin, time.Now().Add(time.Hour), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteDaemonInfo(); err != nil { // the daemon restarted on the new build
		t.Fatal(err)
	}
	if s := m.DaemonMismatch(self); !strings.Contains(s, "different builds") {
		t.Fatalf("other build: %q", s)
	}
	old := Version
	Version = "0.0.1-test"
	defer func() { Version = old }()
	if s := m.DaemonMismatch(self); !strings.Contains(s, "the daemon runs agent-jobs "+old) {
		t.Fatalf("other version: %q", s)
	}
}
