package jobs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// paneModeTmux installs a fake tmux that logs every call to FAKE_TMUX_LOG and answers
// display-message from replies in order (the last reply repeats). exitCode is what display-message
// exits with: non-zero stands for a tmux that cannot answer the format. The poll interval is
// shortened for the test.
func paneModeTmux(t *testing.T, exitCode int, replies ...string) string {
	t.Helper()
	dir := t.TempDir()
	logf := filepath.Join(dir, "tmux.log")
	t.Setenv("FAKE_TMUX_LOG", logf)
	replyFile := filepath.Join(dir, "replies")
	if err := os.WriteFile(replyFile, []byte(strings.Join(replies, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "tmux")
	script := fmt.Sprintf(`#!/bin/bash
printf '%%s|' "$@" >> "$FAKE_TMUX_LOG"
printf '\n' >> "$FAKE_TMUX_LOG"
for a in "$@"; do
  [[ $a == display-message ]] || continue
  n=$(cat %[1]s/n 2>/dev/null || echo 1)
  echo $((n+1)) > %[1]s/n
  line=$(sed -n "${n}p" %[2]s)
  [[ -n $line ]] || line=$(tail -n 1 %[2]s)
  printf '%%s\n' "$line"
  exit %[3]d
done
exit 0
`, dir, replyFile, exitCode)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_JOBS_TMUX_BIN", bin)
	oldPoll := PaneModePoll
	PaneModePoll = 10 * time.Millisecond
	t.Cleanup(func() { PaneModePoll = oldPoll })
	return logf
}

// tmuxCalls are the fake tmux's logged calls, oldest first.
func tmuxCalls(t *testing.T, logf string) []string {
	t.Helper()
	b, err := os.ReadFile(logf)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// A pane in a tmux mode (copy-mode swallows send-keys -l and still exits 0, so the line is lost
// silently) is polled until it leaves the mode; the line is typed only after that.
func TestNotifyWaitsForPaneToLeaveMode(t *testing.T) {
	m := newManager(t)
	logf := paneModeTmux(t, 0, "1", "1", "0")
	m.Cfg.NotifyTmuxTarget = "fake-session"
	n, err := m.NotifyOwner("job-1", "hello", "")
	if err != nil || n == nil || !n.Delivered {
		t.Fatalf("notify: %+v %v", n, err)
	}
	probes, firstSend := 0, -1
	for i, c := range tmuxCalls(t, logf) {
		switch {
		case strings.Contains(c, "display-message"):
			probes++
			if firstSend >= 0 {
				t.Fatalf("pane probed after the line was typed, call %d", i)
			}
		case strings.Contains(c, "send-keys") && firstSend < 0:
			firstSend = i
		}
	}
	if probes != 3 || firstSend != 3 {
		t.Fatalf("probes=%d firstSend=%d, want 3 and 3", probes, firstSend)
	}
}

// A pane that never leaves the mode is a failed delivery, retried on the next tick: not one
// keystroke is typed into it and the row carries the mode it was stuck in.
func TestNotifyFailsWhilePaneStaysInMode(t *testing.T) {
	m := newManager(t)
	logf := paneModeTmux(t, 0, "1")
	old := TmuxDeliveryTimeout
	TmuxDeliveryTimeout = 300 * time.Millisecond
	t.Cleanup(func() { TmuxDeliveryTimeout = old })
	m.Cfg.NotifyTmuxTarget = "fake-session"
	n, err := m.NotifyOwner("job-1", "hello", "")
	if err == nil || n == nil || n.Delivered || !strings.Contains(n.Error, `pane_in_mode="1"`) {
		t.Fatalf("notify: %+v %v", n, err)
	}
	for _, c := range tmuxCalls(t, logf) {
		if strings.Contains(c, "send-keys") {
			t.Fatalf("typed into a pane in a mode: %s", c)
		}
	}
}

// A tmux that cannot answer the format (an old one, or no such pane for display-message) must not
// block delivery: the line is typed as before.
func TestNotifyTypesWhenPaneModeIsUnknown(t *testing.T) {
	m := newManager(t)
	logf := paneModeTmux(t, 1, "")
	m.Cfg.NotifyTmuxTarget = "fake-session"
	n, err := m.NotifyOwner("job-1", "hello", "")
	if err != nil || n == nil || !n.Delivered {
		t.Fatalf("notify: %+v %v", n, err)
	}
	if calls := tmuxCalls(t, logf); !strings.Contains(strings.Join(calls, "\n"), "send-keys") {
		t.Fatalf("nothing typed: %v", calls)
	}
}
