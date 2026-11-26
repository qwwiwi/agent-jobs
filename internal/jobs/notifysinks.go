package jobs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// NotifyConfig is the config block "notify": extra sinks for a job-done line, next to (or instead
// of) the tmux target. Both are off by default; with neither set nothing changes.
type NotifyConfig struct {
	// Command is an argv (program and arguments, no shell) run once per job-done line, with the line
	// and a newline on its stdin. The line is never part of the argv. It has NotifyCommandTimeout to
	// finish; a failure is logged and changes nothing for the job.
	Command []string `json:"command"`
	// InboxDir is an absolute directory that gets one JSON file (InboxEvent) per job-done line, for
	// `agent-jobs inbox pull` to print and acknowledge.
	InboxDir string `json:"inbox_dir"`
}

// enabled reports whether any sink is configured.
func (n NotifyConfig) enabled() bool { return len(n.Command) > 0 || n.InboxDir != "" }

// validate checks the block as config.json gives it.
func (n NotifyConfig) validate() error {
	if n.Command != nil && (len(n.Command) == 0 || strings.TrimSpace(n.Command[0]) == "") {
		return errors.New("notify.command must be a non-empty array whose first element is the program (omit the key for none)")
	}
	for _, a := range n.Command {
		if strings.ContainsRune(a, 0) {
			return errors.New("notify.command: an argument contains a NUL byte")
		}
	}
	if n.InboxDir != "" && (!filepath.IsAbs(n.InboxDir) || filepath.Clean(n.InboxDir) == "/") {
		return fmt.Errorf("notify.inbox_dir %q must be an absolute path other than /", n.InboxDir)
	}
	return nil
}

// InboxEvent is one job-done line in the file inbox: the file <id>.json of notify.inbox_dir.
type InboxEvent struct {
	// ID names the delivery (derived from the jobs, their attempts and finish times), so a delivery
	// repeated after a failed acknowledgement rewrites its own file instead of adding a second one.
	ID   string    `json:"id"`
	Line string    `json:"line"`
	TS   time.Time `json:"ts"`
}

// NotifyCommandTimeout bounds one run of notify.command; a variable so tests can shorten it.
var NotifyCommandTimeout = 10 * time.Second

// notifyCommandOutputMax caps how much of a failed command's output reaches the log.
const notifyCommandOutputMax = 300

const (
	inboxEventExt  = ".json"
	inboxTmpPrefix = ".tmp-"
)

// notifySinks hands a delivered job-done line to the configured sinks. ref is the delivery's
// identity (deliverDone). Each sink is on its own: a failure is logged and neither stops the other
// sink nor fails the delivery.
func (m *Manager) notifySinks(ref, line string) {
	n := m.Config().Notify
	if !n.enabled() {
		return
	}
	line = Printable(line, 0)
	if n.InboxDir != "" {
		ev := InboxEvent{ID: inboxEventID(ref), Line: line, TS: m.Now().UTC()}
		if err := WriteInboxEvent(n.InboxDir, ev); err != nil {
			m.Log.Printf("notify.inbox_dir failed, line not stored: %s", RedactText(err.Error()))
		}
	}
	if len(n.Command) > 0 {
		if err := runNotifyCommand(n.Command, line); err != nil {
			m.Log.Printf("notify.command failed: %s", RedactText(err.Error()))
		}
	}
}

// inboxEventID is the event id of a delivery: a short hash of its identity, safe as a file name.
func inboxEventID(ref string) string {
	sum := sha256.Sum256([]byte(ref))
	return hex.EncodeToString(sum[:8])
}

// runNotifyCommand runs argv directly (no shell) with line and a newline on stdin, in a process
// group of its own that is killed at NotifyCommandTimeout.
func runNotifyCommand(argv []string, line string) error {
	ctx, cancel := context.WithTimeout(context.Background(), NotifyCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	cmd.Stdin = strings.NewReader(line + "\n")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("timed out after %s, killed", NotifyCommandTimeout)
	}
	return fmt.Errorf("%w: %s", err, Printable(out.String(), notifyCommandOutputMax))
}

// WriteInboxEvent stores ev as <dir>/<id>.json, mode 0600, creating dir (0700) when missing. The
// file appears complete or not at all: it is written under a temporary name and renamed.
func WriteInboxEvent(dir string, ev InboxEvent) error {
	if ev.ID == "" || strings.ContainsAny(ev.ID, "/\x00") || strings.HasPrefix(ev.ID, ".") {
		return fmt.Errorf("inbox event id %q is not a file name", ev.ID)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, inboxTmpPrefix+"*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(append(b, '\n'))
	if err == nil {
		err = f.Chmod(0o600)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, filepath.Join(dir, ev.ID+inboxEventExt))
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

// InboxPull returns the pending events of dir, oldest first. With ack it then removes the files of
// exactly the events it returns, so an event written meanwhile stays for the next pull. A missing
// dir is an empty inbox. A file that is not a readable event (not a regular *.json file, not an
// event's JSON) is never removed: it is named in skipped. Temporary files of a write in progress
// are passed over silently.
func InboxPull(dir string, ack bool) (events []InboxEvent, skipped []string, err error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	files := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, inboxTmpPrefix) {
			continue
		}
		path := filepath.Join(dir, name)
		if !e.Type().IsRegular() || !strings.HasSuffix(name, inboxEventExt) {
			skipped = append(skipped, path+": not an event file")
			continue
		}
		b, rerr := os.ReadFile(path)
		if errors.Is(rerr, os.ErrNotExist) {
			continue // acknowledged by another pull meanwhile
		}
		var ev InboxEvent
		if rerr == nil {
			rerr = json.Unmarshal(b, &ev)
		}
		if rerr == nil && (ev.ID == "" || ev.Line == "") {
			rerr = errors.New("no id or line")
		}
		if rerr != nil {
			skipped = append(skipped, fmt.Sprintf("%s: %v", path, rerr))
			continue
		}
		if _, dup := files[ev.ID]; dup {
			skipped = append(skipped, path+": a second file of event "+ev.ID)
			continue
		}
		files[ev.ID] = path
		events = append(events, ev)
	}
	sort.SliceStable(events, func(a, b int) bool {
		if !events[a].TS.Equal(events[b].TS) {
			return events[a].TS.Before(events[b].TS)
		}
		return events[a].ID < events[b].ID
	})
	if ack {
		for _, ev := range events {
			if rerr := os.Remove(files[ev.ID]); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
				return events, skipped, fmt.Errorf("acknowledge event %s: %w", ev.ID, rerr)
			}
		}
	}
	return events, skipped, nil
}
