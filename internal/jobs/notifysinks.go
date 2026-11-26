package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
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

// notifyCommandBufferMax caps how much of a command's output is kept in memory (the rest is read
// and dropped); the log gets at most notifyCommandOutputMax of it.
const notifyCommandBufferMax = 4 << 10

const (
	inboxEventExt  = ".json"
	inboxTmpPrefix = ".tmp-"
)

// notifySinkWorkers bounds how many queued lines are handed to the sinks side by side.
const notifySinkWorkers = 2

// sinkStaleAfter is how long a claimed sink delivery may stay unfinished before it counts as
// interrupted (its run ended part way): far above one run, NotifyCommandTimeout plus an inbox write.
const sinkStaleAfter = 5 * time.Minute

// sinkRetention is how long a finished sink delivery is kept for inspection.
const sinkRetention = 7 * 24 * time.Hour

// sinkErrorMax caps the error text a failed sink delivery records.
const sinkErrorMax = 500

// Crash semantics of the sinks (notify_sink_outbox). A delivered job-done line is queued in the
// same transaction that marks its jobs notified, keyed by the batch identity (deliverDone's ref):
// the line is acknowledged and queued together or not at all, and a batch is queued once, so a
// retried batch never runs its sinks again. The sinks run later, outside the lifecycle lock
// (Tick): a run first claims its row (pending -> running) and only then writes the inbox file and
// runs the command. Delivery is therefore at most once per batch: a crash after the claim leaves
// the row running, and the next run past sinkStaleAfter records it failed («interrupted») instead
// of running the command again or bringing back an inbox file that may have been pulled and
// acknowledged meanwhile. That line may be lost to the sinks; the log and the row name it.

// sinkDelivery is the queue row of a delivered batch line, nil when no sink is configured. owner
// is set when the sinks alone carry the line (no tmux target).
func (m *Manager) sinkDelivery(ref, line string, batch []*store.Job, owner bool) *store.SinkDelivery {
	if !m.Config().Notify.enabled() {
		return nil
	}
	d := &store.SinkDelivery{Ref: ref, Line: Printable(line, 0), Owner: owner}
	for _, j := range batch {
		d.Jobs = append(d.Jobs, store.SinkJob{ID: j.ID, Attempt: j.Attempt})
	}
	return d
}

// deliverSinks hands the queued lines to the sinks of config notify, at most notifySinkWorkers side
// by side, until none is pending (or WaitNotifySinks asked it to stop). It never runs under the
// lifecycle lock or tmux.lock: a slow helper holds up neither cancellations nor tmux lines. First
// it records the runs that were interrupted (see the crash semantics above) and prunes old rows.
func (m *Manager) deliverSinks() {
	now := m.Now()
	refs, err := m.Store.InterruptStaleSinkDeliveries(now.Add(-sinkStaleAfter), now)
	if err != nil {
		m.Log.Printf("notify sinks: %v", err)
	}
	for _, ref := range refs {
		m.Log.Printf("notify sinks: delivery %s was interrupted before it finished; not run again", ref)
	}
	if err := m.Store.PruneSinkDeliveries(now.Add(-sinkRetention)); err != nil {
		m.Log.Printf("notify sinks: %v", err)
	}
	var wg sync.WaitGroup
	for range notifySinkWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !m.sinkStop.Load() {
				d, err := m.Store.ClaimSinkDelivery(m.Now())
				if err != nil {
					m.Log.Printf("notify sinks: %v", err)
					return
				}
				if d == nil {
					return
				}
				m.runSinkDelivery(d)
			}
		}()
	}
	wg.Wait()
}

// runSinkDelivery hands one claimed line to every configured sink and records the outcome: done
// when all of them took it, failed (with their errors) otherwise. Each sink is on its own: one
// failing never stops the other.
func (m *Manager) runSinkDelivery(d *store.SinkDelivery) {
	n := m.Config().Notify
	var errs []string
	if !n.enabled() {
		errs = append(errs, "no notify sink is configured any more")
	}
	if n.InboxDir != "" {
		ev := InboxEvent{ID: inboxEventID(d.Ref), Line: d.Line, TS: d.CreatedAt.UTC()}
		if err := WriteInboxEvent(n.InboxDir, ev); err != nil {
			msg := RedactText(err.Error())
			m.Log.Printf("notify.inbox_dir failed, line not stored: %s", msg)
			errs = append(errs, "notify.inbox_dir: "+msg)
		}
	}
	if len(n.Command) > 0 {
		if err := runNotifyCommand(n.Command, d.Line); err != nil {
			msg := RedactText(err.Error())
			m.Log.Printf("notify.command failed: %s", msg)
			errs = append(errs, "notify.command: "+msg)
		}
	}
	errText := Printable(strings.Join(errs, "; "), sinkErrorMax)
	if err := m.Store.FinishSinkDelivery(d.Ref, errText, m.Now()); err != nil {
		m.Log.Printf("notify sinks: %v", err)
	}
}

// deliverSinksInBackground is deliverSinks in the daemon: in a goroutine of its own, so the tick
// returns at once. A run still in progress usually picks up what this tick queued; a line it just
// missed goes out with the next tick.
func (m *Manager) deliverSinksInBackground() {
	if !m.sinking.CompareAndSwap(false, true) {
		return
	}
	m.sinkWG.Add(1)
	go func() {
		defer m.sinkWG.Done()
		defer m.sinking.Store(false)
		m.deliverSinks()
	}()
}

// WaitNotifySinks stops a background sink run from claiming more lines and waits for the ones in
// flight (each bounded by NotifyCommandTimeout). Lines still pending go out after the next start.
func (m *Manager) WaitNotifySinks() {
	m.sinkStop.Store(true)
	m.sinkWG.Wait()
}

// inboxEventID is the event id of a delivery: a short hash of its identity, safe as a file name.
func inboxEventID(ref string) string {
	sum := sha256.Sum256([]byte(ref))
	return hex.EncodeToString(sum[:8])
}

// runNotifyCommand runs argv directly (no shell) with line and a newline on stdin, in a process
// group of its own. The group is killed when the program exits (a background child it left behind
// ends with it) or at NotifyCommandTimeout. Its output is read into a capped buffer
// (notifyCommandBufferMax), so a loud helper costs no memory; the program's own exit decides the
// outcome, never a child that still holds the output open.
func runNotifyCommand(argv []string, line string) error {
	ctx, cancel := context.WithTimeout(context.Background(), NotifyCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	cmd.Stdin = strings.NewReader(line + "\n")
	// An *os.File as stdout and stderr: Wait returns when the program exits, not when the last
	// holder of the pipe closes it; the copy below drains the pipe on its own.
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	defer r.Close()
	cmd.Stdout, cmd.Stderr = w, w
	err = cmd.Start()
	_ = w.Close()
	if err != nil {
		return err
	}
	out := &limitedBuffer{max: notifyCommandBufferMax}
	copied := make(chan struct{})
	go func() {
		defer close(copied)
		_, _ = io.Copy(out, r)
	}()
	err = cmd.Wait()
	timedOut := ctx.Err() != nil
	// Whatever the program left running in its group ends with it.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	// A child that left the group may still hold the pipe: stop reading after a short grace.
	_ = r.SetReadDeadline(time.Now().Add(time.Second))
	<-copied
	if timedOut {
		return fmt.Errorf("timed out after %s, killed", NotifyCommandTimeout)
	}
	if err == nil {
		return nil
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
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if err := checkInboxDir(dir, info); err != nil {
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

// checkInboxDir refuses an inbox directory that someone else could plant or remove events in: one
// writable by group or others, or not owned by this user. info is the directory's own (followed)
// stat.
func checkInboxDir(dir string, info os.FileInfo) error {
	if !info.IsDir() {
		return fmt.Errorf("notify.inbox_dir %s is not a directory", dir)
	}
	if perm := info.Mode().Perm(); perm&0o022 != 0 {
		return fmt.Errorf("notify.inbox_dir %s is writable by group or others (mode %o): refused, make it 0700", dir, perm)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("notify.inbox_dir %s: cannot read its owner", dir)
	}
	if uid := os.Getuid(); int(st.Uid) != uid {
		return fmt.Errorf("notify.inbox_dir %s is not owned by this user (owner uid %d, this user %d): refused", dir, st.Uid, uid)
	}
	return nil
}

// InboxPull hands the pending events of dir to emit, oldest first. Pulls of one inbox are serialized
// by an exclusive flock on the directory, so two pulls never hand out the same event. With ack an
// event's file is removed only after emit returned nil for it: when emit fails (stdout closed) the
// pull stops with that error and the event and the ones after it stay for the next pull. A missing
// dir is an empty inbox; a dir writable by others or not owned by this user is refused
// (checkInboxDir). A file that is not a readable event (not a regular *.json file, not an event's
// JSON) is never removed: it is named in skipped. Temporary files of a write in progress are passed
// over silently.
func InboxPull(dir string, ack bool, emit func(InboxEvent) error) (skipped []string, err error) {
	d, err := os.Open(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer d.Close()
	info, err := d.Stat()
	if err != nil {
		return nil, err
	}
	if err := checkInboxDir(dir, info); err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(d.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("lock inbox %s: %w", dir, err)
	}
	defer func() { _ = syscall.Flock(int(d.Fd()), syscall.LOCK_UN) }()
	// Read under the lock: a pull that waited sees what the one before it left.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var events []InboxEvent
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
			continue // removed by hand meanwhile
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
	for _, ev := range events {
		if err := emit(ev); err != nil {
			return skipped, fmt.Errorf("output of event %s failed, it and the later ones stay in the inbox: %w", ev.ID, err)
		}
		if !ack {
			continue
		}
		if rerr := os.Remove(files[ev.ID]); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			return skipped, fmt.Errorf("acknowledge event %s: %w", ev.ID, rerr)
		}
	}
	return skipped, nil
}
