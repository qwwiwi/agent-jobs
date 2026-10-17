package jobs

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// The session sweep: once claude has exited, the wrapper ends every other process of its session
// before it writes exit_code, so nothing the job started outlives it (a markerless orphan could
// otherwise start a nested job once the session leader is gone; ghost preview servers and browsers
// go too).
//
// Every signal goes through a pidfd: the descriptor is opened FIRST, session membership is checked
// AFTER it is open, and the signal is sent through the descriptor. A pid that was reused by an
// unrelated process is therefore never signalled: either its /proc entry already shows the new,
// foreign session, or the process the descriptor pins has died and the send fails with ESRCH. When
// pidfds are unavailable (ENOSYS: old kernel), nothing is signalled and the members count as
// survivors; there is no fallback to a bare kill(pid).
//
// Fail closed: if members survive the last round, or the session cannot be enumerated, the wrapper
// still writes exit_code but also writes SweepMarker listing the survivors, and the daemon fails the
// job with ReasonSurvivors.
//
// Residual gap: enumerating /proc is not atomic. A member that forks between the readdir and the
// stat of the child can leave a child the round did not see; the next round sees it, but a process
// that forks faster than the rounds run can outlast them (it then shows up as a survivor). The
// cgroup plan item (docs/PLAN.md, Wave A) closes this: a scope is stopped as a whole.

// SweepMarker is the job-dir file the wrapper writes when the sweep could not end the session.
const SweepMarker = "sweep_incomplete"

// ReasonSurvivors is the failure reason of a job whose sweep was incomplete.
const ReasonSurvivors = "processes survived the job"

// SweepGrace is the wait between SIGTERM and SIGKILL of a finished job's leftover processes.
var SweepGrace = 3 * time.Second

// sweepRounds bounds the SIGTERM/SIGKILL rounds.
const sweepRounds = 5

// errPidfdUnsupported means the kernel has no pidfd_open/pidfd_send_signal.
var errPidfdUnsupported = errors.New("pidfd unsupported")

// pidfdOpen and pidfdSend live in pidfd_linux.go and pidfd_other.go.

// signalIgnore is signal.Ignore; a variable so the non-Linux sweep stays testable.
var signalIgnore = signal.Ignore

// sessionMembers lists the live (non-zombie) processes of session sid other than this process.
// A process that vanishes during the scan is skipped; an unreadable /proc is an error.
func sessionMembers(sid int) ([]int, error) {
	ents, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, err
	}
	self := os.Getpid()
	var out []int
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		if st, s, err := procStat(pid); err == nil && s == sid && st != "Z" {
			out = append(out, pid)
		}
	}
	return out, nil
}

// signalMember sends sig to pid only if, with a pidfd already open on it, pid is still a live
// member of session sid. It reports whether the signal was sent.
func signalMember(pid, sid int, sig syscall.Signal) (bool, error) {
	fd, err := pidfdOpen(pid)
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, errPidfdUnsupported) {
		return false, errPidfdUnsupported
	}
	if errors.Is(err, unix.ESRCH) {
		return false, nil // gone already
	}
	if err != nil {
		return false, fmt.Errorf("pidfd_open %d: %w", pid, err)
	}
	defer unix.Close(fd)
	// Checked only now: the descriptor pins one process, so this stat is of that process (or, if it
	// died meanwhile, of a stranger, and then the send below fails with ESRCH).
	if st, s, err := procStat(pid); err != nil || s != sid || st == "Z" {
		return false, nil
	}
	err = pidfdSend(fd, sig)
	if errors.Is(err, unix.ESRCH) {
		return false, nil
	}
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, errPidfdUnsupported) {
		return false, errPidfdUnsupported
	}
	if err != nil {
		return false, fmt.Errorf("pidfd_send_signal %d: %w", pid, err)
	}
	return true, nil
}

// SweepResult is what a sweep did and what it could not do.
type SweepResult struct {
	Signalled int   // distinct processes that got a signal
	Survivors []int // members still alive after the last round
	Err       error // enumeration or signalling failure; the sweep is then incomplete too
}

// Incomplete reports whether the job must not count as a clean finish.
func (r SweepResult) Incomplete() bool { return len(r.Survivors) > 0 || r.Err != nil }

// sweepSid runs the bounded rounds against session sid (Linux, /proc).
func sweepSid(sid int, log io.Writer) SweepResult {
	signalled := map[int]bool{}
	var errs []error
	send := func(pids []int, sig syscall.Signal) bool {
		for _, p := range pids {
			ok, err := signalMember(p, sid, sig)
			if errors.Is(err, errPidfdUnsupported) {
				fmt.Fprintln(log, "run-wrapper: pidfd unsupported by this kernel: leftover processes are not signalled")
				errs = append(errs, err)
				return false
			}
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if ok {
				signalled[p] = true
			}
		}
		return true
	}
	members := func() []int {
		pids, err := sessionMembers(sid)
		if err != nil {
			errs = append(errs, fmt.Errorf("enumerate session %d: %w", sid, err))
		}
		return pids
	}
	result := func() SweepResult {
		left := members()
		return SweepResult{Signalled: len(signalled), Survivors: left, Err: errors.Join(errs...)}
	}
	for round := 0; round < sweepRounds; round++ {
		pids := members()
		if len(errs) > 0 || len(pids) == 0 {
			return result()
		}
		if !send(pids, syscall.SIGTERM) {
			return result()
		}
		deadline := time.Now().Add(SweepGrace)
		for time.Now().Before(deadline) {
			if left := members(); len(left) == 0 || len(errs) > 0 {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if len(errs) > 0 {
			return result()
		}
		if !send(members(), syscall.SIGKILL) {
			return result()
		}
		time.Sleep(50 * time.Millisecond)
	}
	return result()
}

// sweepSession sweeps the wrapper's own session. The daemon starts the wrapper with setsid, so the
// wrapper leads it; run by hand (not a leader) there is nothing of the job's to sweep. Elsewhere than
// Linux there is no /proc and no pidfd: the wrapper ignores SIGTERM and sends it to its own process
// group (its own live pid, so no reuse risk), and the result is unverified (weaker; documented).
func sweepSession(log io.Writer) SweepResult {
	sid, err := unix.Getsid(0)
	if err != nil {
		return SweepResult{Err: fmt.Errorf("getsid: %w", err)}
	}
	if sid != os.Getpid() {
		return SweepResult{}
	}
	if runtime.GOOS != "linux" {
		signalIgnore(syscall.SIGTERM)
		_ = syscall.Kill(-sid, syscall.SIGTERM)
		return SweepResult{}
	}
	return sweepSid(sid, log)
}

// writeSweepMarker records an incomplete sweep in the job dir: the survivor pids, one per line,
// then the error if there was one.
func writeSweepMarker(jobDir string, r SweepResult) error {
	var b strings.Builder
	for _, p := range r.Survivors {
		fmt.Fprintf(&b, "%d\n", p)
	}
	if r.Err != nil {
		fmt.Fprintf(&b, "error: %s\n", Printable(r.Err.Error(), 300))
	}
	return writeAtomic(filepath.Join(jobDir, SweepMarker), []byte(b.String()))
}

// sweepFailure returns the failure reason when the job dir holds a sweep marker, or "". The job dir is
// writable by the job, so the marker is opened without following links or blocking (a FIFO in its
// place cannot hang the daemon) and read only if it is a regular file, capped; anything odd in its
// place fails the job too.
func sweepFailure(dir string) string {
	f, err := os.OpenFile(filepath.Join(dir, SweepMarker), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		return ReasonSurvivors + " (sweep marker unreadable)"
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return ReasonSurvivors + " (sweep marker is not a regular file)"
	}
	b, _ := io.ReadAll(io.LimitReader(f, 4<<10))
	var pids []string
	for _, l := range strings.Split(string(b), "\n") {
		if _, err := strconv.Atoi(strings.TrimSpace(l)); err == nil {
			pids = append(pids, strings.TrimSpace(l))
		}
	}
	if len(pids) == 0 {
		return ReasonSurvivors + " (sweep incomplete, see stderr.log)"
	}
	return Printable(ReasonSurvivors+": pids "+strings.Join(pids, ", "), 200)
}
