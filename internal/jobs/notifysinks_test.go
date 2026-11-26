package jobs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// stdinSink is a notify.command that saves its stdin to <dir>/stdin and its arguments, one per
// line, to <dir>/argv. It returns the argv to configure.
func stdinSink(t *testing.T, extra ...string) (argv []string, dir string) {
	t.Helper()
	dir = t.TempDir()
	script := filepath.Join(dir, "sink.sh")
	body := "#!/bin/sh\nd=$(dirname \"$0\")\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done >> \"$d/argv\"\ncat >> \"$d/stdin\"\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return append([]string{script}, extra...), dir
}

// sinkFile reads a file a sink wrote; a sink that never ran left none, which reads as "".
func sinkFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func logBuffer(m *Manager) *bytes.Buffer {
	var buf bytes.Buffer
	m.Log = log.New(&buf, "", 0)
	return &buf
}

func pull(t *testing.T, dir string, ack bool) []InboxEvent {
	t.Helper()
	var evs []InboxEvent
	skipped, err := InboxPull(dir, ack, func(ev InboxEvent) error { evs = append(evs, ev); return nil })
	if err != nil || len(skipped) != 0 {
		t.Fatalf("InboxPull: %v, skipped %q", err, skipped)
	}
	return evs
}

// notify.command gets the job-done line the session got, on stdin, as one line; its argv is the
// configured one and nothing else.
func TestNotifyCommandGetsLineOnStdin(t *testing.T) {
	m := newManager(t)
	logf := notifyTo(t, m)
	argv, dir := stdinSink(t, "fixed-arg")
	m.Cfg.Notify.Command = argv
	now := coalesceClock(m)
	j := finishAt(t, m, now, *now, nil)
	notifyAt(t, m, now, *now)
	lines := tmuxLines(t, logf)
	if len(lines) != 1 {
		t.Fatalf("tmux lines: %q", lines)
	}
	if got := sinkFile(t, filepath.Join(dir, "stdin")); got != lines[0]+"\n" {
		t.Fatalf("stdin of notify.command:\n got %q\nwant %q", got, lines[0]+"\n")
	}
	if got := sinkFile(t, filepath.Join(dir, "argv")); got != "fixed-arg\n" {
		t.Fatalf("argv of notify.command: %q", got)
	}
	if get(t, m, j.ID).NotifiedAt == nil {
		t.Fatal("job not marked notified")
	}
}

// A line with shell syntax reaches the command byte for byte on stdin: no shell runs it and it is
// never part of argv.
func TestNotifyCommandInjectionSafe(t *testing.T) {
	m := newManager(t)
	argv, dir := stdinSink(t, "--flag")
	m.Cfg.Notify.Command = argv
	// A relative name keeps the title short enough not to be cut; a shell would create it in the
	// working directory of the command, which is this test's.
	const mark = "aj-notify-pwned-marker"
	t.Cleanup(func() { _ = os.Remove(mark) })
	title := fmt.Sprintf("$(touch %s) `touch %s` \"q\" 'x'; touch %s", mark, mark, mark)
	now := coalesceClock(m)
	j := finishAt(t, m, now, *now, func(r *StartRequest) { r.Title = title })
	notifyAt(t, m, now, *now)
	want := m.DoneLineFor(get(t, m, j.ID))
	if !strings.Contains(want, "$(touch "+mark+")") || !strings.Contains(want, `"q" 'x'; touch`) {
		t.Fatalf("the line lost the shell syntax under test: %q", want)
	}
	if got := sinkFile(t, filepath.Join(dir, "stdin")); got != want+"\n" {
		t.Fatalf("stdin:\n got %q\nwant %q", got, want+"\n")
	}
	if got := sinkFile(t, filepath.Join(dir, "argv")); got != "--flag\n" {
		t.Fatalf("the line leaked into argv: %q", got)
	}
	if _, err := os.Stat(mark); err == nil {
		t.Fatal("shell syntax of the line was executed")
	}
}

// The timeout of notify.command is 10 s.
func TestNotifyCommandTimeoutDefault(t *testing.T) {
	if NotifyCommandTimeout != 10*time.Second {
		t.Fatalf("NotifyCommandTimeout = %s, want 10s", NotifyCommandTimeout)
	}
}

// A command that hangs is killed at the timeout; the failure is logged and the job is still
// marked notified.
func TestNotifyCommandTimeoutKilled(t *testing.T) {
	old := NotifyCommandTimeout
	NotifyCommandTimeout = 300 * time.Millisecond
	t.Cleanup(func() { NotifyCommandTimeout = old })
	m := newManager(t)
	buf := logBuffer(m)
	dir := t.TempDir()
	script := filepath.Join(dir, "hang.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch \"$(dirname \"$0\")/started\"\nsleep 30\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	m.Cfg.Notify.Command = []string{script}
	now := coalesceClock(m)
	j := finishAt(t, m, now, *now, nil)
	start := time.Now()
	notifyAt(t, m, now, *now)
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("notify took %s with a 300ms command timeout", d)
	}
	if _, err := os.Stat(filepath.Join(dir, "started")); err != nil {
		t.Fatalf("the command never ran: %v", err)
	}
	if !strings.Contains(buf.String(), "notify.command") || !strings.Contains(buf.String(), "timed out") {
		t.Fatalf("timeout not logged: %q", buf.String())
	}
	if got := get(t, m, j.ID); got.NotifiedAt == nil || got.Status != store.StatusCancelled {
		t.Fatalf("job after a timed out command: notified=%v status=%s", got.NotifiedAt, got.Status)
	}
}

// A failing command (non-zero exit, or one that cannot be started) is logged and never fails the
// job or its tmux delivery.
func TestNotifyCommandFailureLogged(t *testing.T) {
	for name, argv := range map[string][]string{
		"exit 3":  {"/bin/sh", "-c", "echo boom >&2; exit 3"},
		"missing": {filepath.Join(t.TempDir(), "no-such-binary")},
	} {
		t.Run(name, func(t *testing.T) {
			m := newManager(t)
			logf := notifyTo(t, m)
			buf := logBuffer(m)
			m.Cfg.Notify.Command = argv
			now := coalesceClock(m)
			j := finishAt(t, m, now, *now, nil)
			notifyAt(t, m, now, *now)
			if len(tmuxLines(t, logf)) != 1 || get(t, m, j.ID).NotifiedAt == nil {
				t.Fatal("a failing notify.command blocked the tmux delivery")
			}
			if !strings.Contains(buf.String(), "notify.command failed") {
				t.Fatalf("failure not logged: %q", buf.String())
			}
		})
	}
}

// notify.inbox_dir: every delivered line is one JSON file (id, line, ts), mode 0600; pull lists the
// events oldest first and leaves them; pull with ack removes exactly what it returned.
func TestNotifyInboxWritePullAck(t *testing.T) {
	m := newManager(t)
	logf := notifyTo(t, m)
	inbox := filepath.Join(t.TempDir(), "inbox")
	m.Cfg.Notify.InboxDir = inbox
	now := coalesceClock(m)
	t0 := *now
	finishAt(t, m, now, t0, nil)
	notifyAt(t, m, now, t0)
	finishAt(t, m, now, t0.Add(time.Minute), func(r *StartRequest) { r.Title = "second" })
	notifyAt(t, m, now, t0.Add(time.Minute))
	lines := tmuxLines(t, logf)
	if len(lines) != 2 {
		t.Fatalf("tmux lines: %q", lines)
	}
	entries, err := os.ReadDir(inbox)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("inbox files: %v", entries)
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 || !strings.HasSuffix(e.Name(), ".json") {
			t.Fatalf("inbox file %s has mode %o", e.Name(), info.Mode().Perm())
		}
		var raw map[string]any
		if err := json.Unmarshal([]byte(readFile(t, filepath.Join(inbox, e.Name()))), &raw); err != nil {
			t.Fatal(err)
		}
		if len(raw) != 3 || raw["id"] == "" || raw["line"] == "" || raw["ts"] == "" {
			t.Fatalf("event fields: %v", raw)
		}
	}
	if info, err := os.Stat(inbox); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("inbox dir: %v %v", info, err)
	}
	evs := pull(t, inbox, false)
	if len(evs) != 2 || evs[0].Line != lines[0] || evs[1].Line != lines[1] {
		t.Fatalf("pull: %+v\nwant lines %q", evs, lines)
	}
	if !evs[0].TS.Equal(t0) || !evs[1].TS.Equal(t0.Add(time.Minute)) || evs[0].ID == evs[1].ID {
		t.Fatalf("pull ids/ts: %+v", evs)
	}
	if again := pull(t, inbox, false); len(again) != 2 {
		t.Fatalf("pull without ack consumed events: %+v", again)
	}
	if acked := pull(t, inbox, true); len(acked) != 2 {
		t.Fatalf("pull --ack: %+v", acked)
	}
	if left := pull(t, inbox, false); len(left) != 0 {
		t.Fatalf("events left after ack: %+v", left)
	}
	if entries, _ := os.ReadDir(inbox); len(entries) != 0 {
		t.Fatalf("files left after ack: %v", entries)
	}
}

// A pull of a directory that does not exist yet is empty, not an error; a file that is not an
// event is named as skipped and survives an ack.
func TestInboxPullMissingDirAndForeignFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "inbox")
	var evs []InboxEvent
	collect := func(ev InboxEvent) error { evs = append(evs, ev); return nil }
	if skipped, err := InboxPull(dir, true, collect); err != nil || len(evs) != 0 || len(skipped) != 0 {
		t.Fatalf("missing dir: %v %v %v", evs, skipped, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	skipped, err := InboxPull(dir, true, collect)
	if err != nil || len(evs) != 0 || len(skipped) != 1 || !strings.Contains(skipped[0], "broken.json") {
		t.Fatalf("foreign file: %v %q %v", evs, skipped, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "broken.json")); err != nil {
		t.Fatalf("a skipped file was removed by ack: %v", err)
	}
}

// Without notify_tmux_target the sinks alone carry the line, and notify_coalesce_seconds holds and
// joins the lines for them as it does for tmux.
func TestNotifySinksCoalesceWithoutTmux(t *testing.T) {
	m := newManager(t)
	inbox := filepath.Join(t.TempDir(), "inbox")
	argv, dir := stdinSink(t)
	m.Cfg.Notify = NotifyConfig{Command: argv, InboxDir: inbox}
	m.Cfg.NotifyCoalesceSeconds = 120
	now := coalesceClock(m)
	t0 := *now
	a := finishAt(t, m, now, t0, nil)
	b := finishAt(t, m, now, t0.Add(30*time.Second), func(r *StartRequest) { r.Title = "second" })
	notifyAt(t, m, now, t0.Add(149*time.Second))
	if evs := pull(t, inbox, false); len(evs) != 0 || get(t, m, a.ID).NotifiedAt != nil {
		t.Fatalf("delivered inside the window: %+v", evs)
	}
	want := fmt.Sprintf("[jobs:job-done] 2 jobs: %s; %s", donePart(t, m, a.ID), donePart(t, m, b.ID))
	notifyAt(t, m, now, t0.Add(150*time.Second))
	evs := pull(t, inbox, false)
	if len(evs) != 1 || evs[0].Line != want {
		t.Fatalf("coalesced inbox event:\n got %+v\nwant %q", evs, want)
	}
	if got := sinkFile(t, filepath.Join(dir, "stdin")); got != want+"\n" {
		t.Fatalf("coalesced stdin: %q", got)
	}
	for _, id := range []string{a.ID, b.ID} {
		if get(t, m, id).NotifiedAt == nil {
			t.Fatalf("job %s not marked notified", id)
		}
	}
	notifyAt(t, m, now, t0.Add(10*time.Minute))
	if evs := pull(t, inbox, false); len(evs) != 1 {
		t.Fatalf("delivered again: %+v", evs)
	}
}

// With tmux and coalescing, the sinks get the same single coalesced line as the session.
func TestNotifySinksCoalesceWithTmux(t *testing.T) {
	m := newManager(t)
	logf := notifyTo(t, m)
	inbox := filepath.Join(t.TempDir(), "inbox")
	m.Cfg.Notify.InboxDir = inbox
	m.Cfg.NotifyCoalesceSeconds = 60
	now := coalesceClock(m)
	t0 := *now
	finishAt(t, m, now, t0, nil)
	finishAt(t, m, now, t0.Add(10*time.Second), func(r *StartRequest) { r.Title = "second" })
	notifyAt(t, m, now, t0.Add(30*time.Second))
	if evs := pull(t, inbox, false); len(evs) != 0 {
		t.Fatalf("inbox event inside the window: %+v", evs)
	}
	notifyAt(t, m, now, t0.Add(70*time.Second))
	lines, evs := tmuxLines(t, logf), pull(t, inbox, false)
	if len(lines) != 1 || len(evs) != 1 || evs[0].Line != lines[0] || !strings.HasPrefix(lines[0], "[jobs:job-done] 2 jobs: ") {
		t.Fatalf("tmux %q, inbox %+v", lines, evs)
	}
}

// A line tmux did not accept is not handed to the sinks: they get it with the retry that tmux
// accepts, once.
func TestNotifySinksWaitForTmux(t *testing.T) {
	m := newManager(t)
	logf := notifyTo(t, m)
	inbox := filepath.Join(t.TempDir(), "inbox")
	m.Cfg.Notify.InboxDir = inbox
	now := coalesceClock(m)
	finishAt(t, m, now, *now, nil)
	good := os.Getenv("AGENT_JOBS_TMUX_BIN")
	t.Setenv("AGENT_JOBS_TMUX_BIN", "/usr/bin/false")
	notifyAt(t, m, now, *now)
	if evs := pull(t, inbox, false); len(evs) != 0 {
		t.Fatalf("inbox event for a line tmux refused: %+v", evs)
	}
	t.Setenv("AGENT_JOBS_TMUX_BIN", good)
	notifyAt(t, m, now, now.Add(time.Minute))
	if lines, evs := tmuxLines(t, logf), pull(t, inbox, false); len(lines) != 1 || len(evs) != 1 {
		t.Fatalf("after the retry: tmux %q, inbox %+v", lines, evs)
	}
}

// Off by default: an empty config has no sinks, and with only notify_tmux_target set nothing but
// tmux is used; with no target and no sinks the job is marked notified at once as before.
func TestNotifySinksOffByDefault(t *testing.T) {
	c, err := parseConfig(t.TempDir(), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Notify.Command) != 0 || c.Notify.InboxDir != "" {
		t.Fatalf("default notify config: %+v", c.Notify)
	}
	m := newManager(t)
	buf := logBuffer(m)
	m.Cfg.NotifyCoalesceSeconds = 120
	now := coalesceClock(m)
	j := finishAt(t, m, now, *now, nil)
	notifyAt(t, m, now, *now)
	if get(t, m, j.ID).NotifiedAt == nil || !strings.Contains(buf.String(), "notify off") {
		t.Fatalf("notify-off behaviour changed: %q", buf.String())
	}
}

// config.json: notify.command is a non-empty argv with a program, notify.inbox_dir an absolute path.
func TestNotifyConfigValidation(t *testing.T) {
	good := `{"notify": {"command": ["/usr/local/bin/wake", "--now"], "inbox_dir": "/var/tmp/inbox"}}`
	c, err := parseConfig(t.TempDir(), []byte(good))
	if err != nil || len(c.Notify.Command) != 2 || c.Notify.InboxDir != "/var/tmp/inbox" {
		t.Fatalf("valid config: %+v %v", c.Notify, err)
	}
	for _, bad := range []string{
		`{"notify": {"command": [""]}}`,
		`{"notify": {"command": "curl http://x"}}`,
		`{"notify": {"inbox_dir": "relative/inbox"}}`,
		`{"notify": {"inbox_dir": "/"}}`,
	} {
		if _, err := parseConfig(t.TempDir(), []byte(bad)); err == nil {
			t.Fatalf("config accepted: %s", bad)
		}
	}
}
