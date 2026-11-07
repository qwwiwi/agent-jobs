package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The box-wide gate lock (#134, #208): one heavy step (a full test suite, a type check, a build,
// scripts/check.sh) at a time per box, for the main session's `agent-jobs gate run`, the jobs' own
// `agent-jobs gate run` (allowed inside a job since #208) and the wrapper's acceptance step alike:
//   - fair: a waiter joins a FIFO queue next to the lock (<lock>.queue, one line per waiter with a
//     heartbeat it refreshes every poll) and only the first live waiter tries the lock, so a gate that
//     came later never overtakes; a waiter that died or stopped polling (no heartbeat for
//     gateQueueStale) drops out of the queue. The holder is not queued;
//   - bounded: a waiter gives up after its lock wait with an error showing the holder line;
//   - not reentrant: a successful acquisition requires a successful exclusive flock on the shared
//     lock file, and nothing else grants entry. The holder line is in a file anyone can write, so it is
//     shown as untrusted text and never decides; a held lock is waited for, bounded, whoever holds it
//     (a gate of the same job, the caller's own parent). Nested gates (a gate inside a gate or inside
//     the acceptance command) are unsupported: they wait within their bound and time out without
//     running their command;
//   - released with its holder: flock belongs to the holder's open file, so a holder that is killed
//     (with its job's process group or scope) releases it, and the queue forgets a killed waiter;
//   - visible: a waiter inside a job records its wait in <jobdir>/gate-wait.json (gateWaitRecorder),
//     which job_list, status and the daemon's timeout read (GateWaitOf, creditGateWait).

// Gate lock environment and files.
const (
	// JobDirEnvVar is the job's folder, set with the job marker (JobEnv): a gate inside the job records
	// its lock wait there.
	JobDirEnvVar = "AGENT_JOBS_JOB_DIR"
	// GateWaitFile is a job's gate lock waits (gateWaitRecorder), in its job dir.
	GateWaitFile = "gate-wait.json"

	gateQueueSuffix     = ".queue"
	gateQueueStale      = 5 * time.Second
	gateQueueMaxBytes   = 64 << 10
	gateQueueMaxEntries = 256
	gateQueueLockTries  = 10
	gateQueueLockRetry  = 10 * time.Millisecond
	gateWaitFileMax     = 64 << 10
	gateWaitMaxEntries  = 32
)

// gateLockReq is one acquisition of the gate lock.
type gateLockReq struct {
	path   string
	wait   time.Duration
	job    string    // the caller's job id ("" = not in a job)
	detail string    // the holder line after "pid N job J since T" (where, what)
	notes  io.Writer // waiting notes
	// waits, when set, records the wait (a job's gate-wait.json).
	waits *gateWaitRecorder
}

// gateLease is a held gate lock.
type gateLease struct {
	release func()
	waited  time.Duration // time spent waiting for the lock (0 = it was free)
}

// acquireGateLock takes the box-wide gate lock for r (see the comment above), polling every
// gateLockPoll until r.wait or ctx ends.
func acquireGateLock(ctx context.Context, r gateLockReq) (*gateLease, error) {
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return nil, fmt.Errorf("gate lock dir: %w", err)
	}
	f, err := openGateLock(r.path)
	if err != nil {
		return nil, fmt.Errorf("open gate lock: %w", err)
	}
	if r.notes == nil {
		r.notes = io.Discard
	}
	q := openGateQueue(r.path + gateQueueSuffix)
	defer q.close()
	start := time.Now()
	deadline := start.Add(r.wait)
	var me *gateEntry
	waiting := false
	stopWaiting := func() {
		if me != nil {
			q.leave(*me)
		}
		if waiting && r.waits != nil {
			r.waits.end(time.Since(start))
		}
	}
	for {
		first := me == nil && !q.anyAhead(nil) || me != nil && !q.anyAhead(me)
		if first {
			err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
			if err == nil {
				break
			}
			if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
				stopWaiting()
				f.Close()
				return nil, fmt.Errorf("flock gate lock: %w", err)
			}
		}
		if !waiting {
			waiting = true
			holder := gateHolder(f)
			fmt.Fprintf(r.notes, "gate: waiting up to %s for the gate lock %s, untrusted holder line: %s\n", r.wait, r.path, holder)
			if r.waits != nil {
				r.waits.start(start, holder)
			}
			if me == nil {
				e := q.join()
				me = &e
			}
		}
		if time.Now().After(deadline) {
			holder := gateHolder(f)
			stopWaiting()
			f.Close()
			return nil, fmt.Errorf("the gate lock %s is still held after %s (untrusted holder line: %s): another gate runs on this box",
				r.path, r.wait, holder)
		}
		select {
		case <-ctx.Done():
			stopWaiting()
			f.Close()
			return nil, ctx.Err()
		case <-time.After(gateLockPoll):
		}
	}
	stopWaiting()
	lease := &gateLease{release: func() {
		_ = f.Truncate(0)
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}}
	if waiting {
		lease.waited = time.Since(start)
		fmt.Fprintf(r.notes, "gate: waited %s for the gate lock\n", lease.waited.Round(time.Millisecond))
	}
	writeGateHolderLine(r.path, gateHolderLine(r.job, r.detail))
	return lease, nil
}

// gateHolderLine is what a holder writes into the lock: "pid N job J since T<detail>" (job "-" when not
// in a job), shown to the waiters as untrusted text (gateHolder), never parsed.
func gateHolderLine(job, detail string) string {
	if job == "" {
		job = "-"
	}
	return fmt.Sprintf("pid %d job %s since %s%s", os.Getpid(), job, time.Now().UTC().Format(time.RFC3339), detail)
}

// writeGateHolderLine replaces the holder line of the lock at path.
func writeGateHolderLine(path, line string) {
	f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return
	}
	defer f.Close()
	_ = f.Truncate(0)
	if len(line) > gateHolderBytes-1 {
		line = line[:gateHolderBytes-1]
	}
	_, _ = f.WriteString(line + "\n")
}

// envValue is the value of key in env ("" when absent).
func envValue(env []string, key string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if k, v, ok := strings.Cut(env[i], "="); ok && k == key {
			return v
		}
	}
	return ""
}

// gateEntry is one waiter of the gate queue: its ticket (nanoseconds at join, unique in the file), its
// pid and its last heartbeat (unix milliseconds).
type gateEntry struct {
	seq int64
	pid int
	hb  int64
}

// gateQueue is the FIFO of the gate lock's waiters, a file next to the lock shared by every user like the
// lock itself (openGateLock). Without it (it cannot be opened) waiters only poll the lock.
type gateQueue struct {
	f *os.File
}

func openGateQueue(path string) *gateQueue {
	f, err := openGateLock(path)
	if err != nil {
		return &gateQueue{}
	}
	return &gateQueue{f: f}
}

func (q *gateQueue) close() {
	if q.f != nil {
		q.f.Close()
	}
}

// update runs fn on the live entries (oldest ticket first) under the queue's own flock and writes back
// what it returns. Entries of dead processes or without a heartbeat for gateQueueStale are dropped.
func (q *gateQueue) update(fn func([]gateEntry) []gateEntry) []gateEntry {
	if q.f == nil {
		return nil
	}
	fd := int(q.f.Fd())
	// Every user can open the queue file, so anyone can hold its flock: never block on it. A queue that
	// stays locked (gateQueueLockTries) is skipped this poll; the caller then just polls the gate lock.
	if !tryFlock(fd, gateQueueLockTries, gateQueueLockRetry) {
		return nil
	}
	defer func() { _ = syscall.Flock(fd, syscall.LOCK_UN) }()
	b := make([]byte, gateQueueMaxBytes)
	n, _ := q.f.ReadAt(b, 0)
	now := time.Now().UnixMilli()
	var entries []gateEntry
	for _, line := range strings.Split(string(b[:n]), "\n") {
		f := strings.Fields(line)
		if len(f) != 3 {
			continue
		}
		seq, e1 := strconv.ParseInt(f[0], 10, 64)
		pid, e2 := strconv.Atoi(f[1])
		hb, e3 := strconv.ParseInt(f[2], 10, 64)
		// A heartbeat from the future or pid 1 (planted lines) never goes stale on its own: dropped.
		if e1 != nil || e2 != nil || e3 != nil || pid <= 1 || now-hb > gateQueueStale.Milliseconds() ||
			hb-now > gateQueueStale.Milliseconds() || !alive(pid) {
			continue
		}
		entries = append(entries, gateEntry{seq, pid, hb})
	}
	if len(entries) > gateQueueMaxEntries {
		entries = entries[:gateQueueMaxEntries]
	}
	slices.SortFunc(entries, func(a, b gateEntry) int { return int(min(max(a.seq-b.seq, -1), 1)) })
	entries = fn(entries)
	var buf bytes.Buffer
	for _, e := range entries {
		fmt.Fprintf(&buf, "%d %d %d\n", e.seq, e.pid, e.hb)
	}
	if buf.Len() <= gateQueueMaxBytes {
		_ = q.f.Truncate(0)
		_, _ = q.f.WriteAt(buf.Bytes(), 0)
	}
	return entries
}

// tryFlock takes an exclusive flock on fd without blocking: up to tries attempts, retry apart. false
// when someone else kept it locked (or flock failed) all along.
func tryFlock(fd, tries int, retry time.Duration) bool {
	for i := range tries {
		if syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB) == nil {
			return true
		}
		if i < tries-1 {
			time.Sleep(retry)
		}
	}
	return false
}

// join adds a waiter and returns its entry.
func (q *gateQueue) join() gateEntry {
	e := gateEntry{seq: time.Now().UnixNano(), pid: os.Getpid(), hb: time.Now().UnixMilli()}
	q.update(func(es []gateEntry) []gateEntry {
		for slices.ContainsFunc(es, func(o gateEntry) bool { return o.seq == e.seq }) {
			e.seq++
		}
		return append(es, e)
	})
	return e
}

// anyAhead refreshes me's heartbeat (re-adding it with its ticket when it was dropped) and reports
// whether a live waiter is ahead of it; with me nil, whether anyone waits at all.
func (q *gateQueue) anyAhead(me *gateEntry) bool {
	ahead := false
	q.update(func(es []gateEntry) []gateEntry {
		if me == nil {
			ahead = len(es) > 0
			return es
		}
		found := false
		for i := range es {
			if es[i].seq == me.seq && es[i].pid == me.pid {
				es[i].hb, found = time.Now().UnixMilli(), true
			} else if es[i].seq < me.seq {
				ahead = true
			}
		}
		if !found {
			es = append(es, gateEntry{me.seq, me.pid, time.Now().UnixMilli()})
		}
		return es
	})
	return ahead
}

// leave removes e from the queue.
func (q *gateQueue) leave(e gateEntry) {
	q.update(func(es []gateEntry) []gateEntry {
		return slices.DeleteFunc(es, func(o gateEntry) bool { return o.seq == e.seq && o.pid == e.pid })
	})
}

// GateWaitState is a job's gate-wait.json: its current waits for the gate lock, its last finished ones
// and the total of all of them (every attempt).
type GateWaitState struct {
	Waits     []GateWaitEntry `json:"waits,omitempty"`
	Done      []GateWaitDone  `json:"done,omitempty"`
	WaitedSec int64           `json:"waited_sec"`
}

// GateWaitDone is one finished wait: when it started and how long it took.
type GateWaitDone struct {
	Since time.Time `json:"since"`
	Sec   int64     `json:"sec"`
}

// Credit is how long the job waited for the gate lock since start (its current attempt) until now: the
// union of its wait intervals since start, finished ones and current ones (up to now), each cut at
// start, so waits that overlap (two gates of the job waiting at once) count once. It is what the job's
// deadline moves by (capped by the caller).
func (s GateWaitState) Credit(start, now time.Time) time.Duration {
	type span struct{ from, to time.Time }
	var spans []span
	add := func(from, to time.Time) {
		if from.Before(start) {
			from = start
		}
		if to.After(now) {
			to = now
		}
		if to.After(from) {
			spans = append(spans, span{from, to})
		}
	}
	for _, w := range s.Done {
		if w.Sec > 0 {
			add(w.Since, w.Since.Add(time.Duration(w.Sec)*time.Second))
		}
	}
	for _, w := range s.Waits {
		add(w.Since, now)
	}
	slices.SortFunc(spans, func(a, b span) int { return a.from.Compare(b.from) })
	var d time.Duration
	var end time.Time
	for _, sp := range spans {
		switch {
		case !sp.from.Before(end):
			d += sp.to.Sub(sp.from)
			end = sp.to
		case sp.to.After(end):
			d += sp.to.Sub(end)
			end = sp.to
		}
	}
	return d
}

// GateWaitEntry is one gate of the job waiting for the lock.
type GateWaitEntry struct {
	PID    int       `json:"pid"`
	Since  time.Time `json:"since"`
	Holder string    `json:"holder"`
}

// gateWaitRecorder records one waiter's wait in a job's gate-wait.json, under that file's flock.
type gateWaitRecorder struct {
	path  string
	pid   int
	since time.Time
}

// newGateWaitRecorder records into <jobDir>/gate-wait.json; nil when jobDir is "".
func newGateWaitRecorder(jobDir string) *gateWaitRecorder {
	if jobDir == "" {
		return nil
	}
	return &gateWaitRecorder{path: filepath.Join(jobDir, GateWaitFile), pid: os.Getpid()}
}

func (w *gateWaitRecorder) start(since time.Time, holder string) {
	w.since = since.UTC()
	w.edit(func(s *GateWaitState) {
		s.Waits = slices.DeleteFunc(s.Waits, func(e GateWaitEntry) bool { return e.PID == w.pid || !alive(e.PID) })
		if len(s.Waits) < gateWaitMaxEntries {
			s.Waits = append(s.Waits, GateWaitEntry{PID: w.pid, Since: since.UTC(), Holder: Printable(holder, 300)})
		}
	})
}

func (w *gateWaitRecorder) end(waited time.Duration) {
	w.edit(func(s *GateWaitState) {
		s.Waits = slices.DeleteFunc(s.Waits, func(e GateWaitEntry) bool { return e.PID == w.pid || !alive(e.PID) })
		sec := int64(waited.Round(time.Second) / time.Second)
		s.WaitedSec += sec
		s.Done = append(s.Done, GateWaitDone{Since: w.since, Sec: sec})
		if n := len(s.Done) - gateWaitMaxEntries; n > 0 {
			s.Done = s.Done[n:] // the oldest: they started before the current attempt (or count in WaitedSec only)
		}
	})
}

// edit changes the file under its own flock, taken without blocking (tryFlock); a file that cannot be
// locked, opened or read is left alone (the record is diagnostic and a timeout credit, never a condition
// of the gate).
func (w *gateWaitRecorder) edit(fn func(*GateWaitState)) {
	f, err := os.OpenFile(w.path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return
	}
	// The job can lock its own gate-wait.json; never wait on it (end runs while the box lock is held):
	// a few tries, then the record is skipped.
	if !tryFlock(int(f.Fd()), gateQueueLockTries, gateQueueLockRetry) {
		return
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	b, err := io.ReadAll(io.LimitReader(f, gateWaitFileMax+1))
	if err != nil || len(b) > gateWaitFileMax {
		return
	}
	var s GateWaitState
	if len(bytes.TrimSpace(b)) > 0 && json.Unmarshal(b, &s) != nil {
		s = GateWaitState{}
	}
	fn(&s)
	out, err := json.Marshal(s)
	if err != nil {
		return
	}
	_ = f.Truncate(0)
	_, _ = f.WriteAt(append(out, '\n'), 0)
}

// ReadGateWait reads a job's gate-wait.json (OpenJobFile: a regular file of this user in the job's
// folder) and keeps only the waits of live processes. A missing file is a zero state.
func ReadGateWait(stateDir, id string) (GateWaitState, error) {
	f, size, err := OpenJobFile(stateDir, id, GateWaitFile, os.Getuid())
	if errors.Is(err, os.ErrNotExist) {
		return GateWaitState{}, nil
	}
	if err != nil {
		return GateWaitState{}, err
	}
	defer f.Close()
	if size > gateWaitFileMax {
		return GateWaitState{}, fmt.Errorf("%s has %d bytes, at most %d", GateWaitFile, size, gateWaitFileMax)
	}
	b, err := io.ReadAll(io.LimitReader(f, gateWaitFileMax))
	if err != nil {
		return GateWaitState{}, err
	}
	var s GateWaitState
	if err := json.Unmarshal(b, &s); err != nil {
		return GateWaitState{}, fmt.Errorf("%s: %w", GateWaitFile, err)
	}
	s.Waits = slices.DeleteFunc(s.Waits, func(e GateWaitEntry) bool { return !alive(e.PID) })
	if s.WaitedSec < 0 {
		s.WaitedSec = 0
	}
	return s, nil
}
