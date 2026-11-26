package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

// `inbox pull` prints the pending job-done lines of notify.inbox_dir oldest first and leaves them;
// `--ack` prints and removes them; an empty inbox prints nothing; without notify.inbox_dir the
// command says what is missing.
func TestInboxPullCLI(t *testing.T) {
	t.Setenv("AGENT_JOBS_HOME", t.TempDir())
	state, err := jobs.StateDir("cli")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := cmdInbox([]string{"pull", "--agent", "cli"}); err == nil || !strings.Contains(err.Error(), "notify.inbox_dir") {
		t.Fatalf("pull without notify.inbox_dir: %v", err)
	}
	inbox := filepath.Join(t.TempDir(), "inbox")
	cfg := `{"notify": {"inbox_dir": "` + inbox + `"}}`
	if err := os.WriteFile(filepath.Join(state, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := captureStdout(t, func() error { return cmdInbox([]string{"pull", "--agent", "cli", "--ack"}) }); out != "" {
		t.Fatalf("empty inbox printed %q", out)
	}
	t0 := time.Unix(1_800_000_000, 0)
	for i, line := range []string{"[jobs:job-done] job b later", "[jobs:job-done] job a earlier"} {
		ev := jobs.InboxEvent{ID: string(rune('a' + i)), Line: line, TS: t0.Add(-time.Duration(i) * time.Minute)}
		if err := jobs.WriteInboxEvent(inbox, ev); err != nil {
			t.Fatal(err)
		}
	}
	want := "[jobs:job-done] job a earlier\n[jobs:job-done] job b later\n"
	for _, args := range [][]string{{"pull", "--agent", "cli"}, {"pull", "--ack", "--agent", "cli"}} {
		if out := captureStdout(t, func() error { return cmdInbox(args) }); out != want {
			t.Fatalf("inbox %v:\n got %q\nwant %q", args, out, want)
		}
	}
	if out := captureStdout(t, func() error { return cmdInbox([]string{"pull", "--agent", "cli"}) }); out != "" {
		t.Fatalf("events left after --ack: %q", out)
	}
	if err := cmdInbox([]string{"push", "--agent", "cli"}); err == nil {
		t.Fatal("unknown subcommand accepted")
	}
}
