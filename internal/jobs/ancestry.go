package jobs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// The second, process-based layer of «no nested jobs». The env marker (JobEnvVar) can be dropped by
// a script the job writes; the process tree cannot: every process of a job descends from the
// wrapper (`agent-jobs run-wrapper <jobdir> --token T -- …`) and, unless it calls setsid itself,
// stays in the wrapper's session, whose id is the job's recorded pgid.
//
// Linux only (/proc). On other systems, or when /proc cannot be read, the ancestry check is skipped
// (fail open) and only the env marker and the session check apply.

// procRoot is /proc; a variable so tests can point it elsewhere.
var procRoot = "/proc"

// maxAncestry bounds the ppid walk (a loop in a corrupt /proc must not hang the check).
const maxAncestry = 256

// IsWrapperCmdline reports an argv of the wrapper: `run-wrapper <jobdir> --token <T>` at position 1,
// or at position 2 behind an interpreter running an agent-jobs script. The binary itself is matched by
// structure, not by name (it may be installed under any name).
func IsWrapperCmdline(argv []string) bool {
	if len(argv) > 3 && argv[1] == "run-wrapper" && argv[3] == "--token" {
		return true
	}
	// Behind an interpreter the script path comes first: it must be an agent-jobs script.
	return len(argv) > 4 && strings.HasPrefix(strings.ToLower(filepath.Base(argv[1])), "agent-jobs") &&
		argv[2] == "run-wrapper" && argv[4] == "--token"
}

// procParent reads the ppid of pid from /proc/<pid>/stat (the field after the parenthesised comm,
// which may itself contain spaces and parentheses).
func procParent(pid int) (int, error) {
	b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, err
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, fmt.Errorf("parse /proc/%d/stat", pid)
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 2 {
		return 0, fmt.Errorf("parse /proc/%d/stat", pid)
	}
	return strconv.Atoi(f[1])
}

func procCmdline(pid int) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimRight(string(b), "\x00"), "\x00"), nil
}

// WrapperAncestor returns the pid of an ancestor of this process that is a job wrapper (0 = none).
// ok is false when the check could not run (not Linux, /proc unreadable): the caller fails open.
func WrapperAncestor() (pid int, ok bool) {
	if runtime.GOOS != "linux" {
		return 0, false
	}
	if _, err := os.Stat(filepath.Join(procRoot, "self", "stat")); err != nil {
		return 0, false
	}
	p := os.Getppid()
	for n := 0; n < maxAncestry && p > 1; n++ {
		argv, err := procCmdline(p)
		if err != nil {
			return 0, true // the ancestor exited meanwhile: the chain above it is unknown, nothing found
		}
		if IsWrapperCmdline(argv) {
			return p, true
		}
		if p, err = procParent(p); err != nil {
			return 0, true
		}
	}
	return 0, true
}

// CheckNotInJob is the agent-independent part: the env marker, the job scope cgroup (setsid, double
// forks and re-parenting stay inside it), then the wrapper ancestry and session (the fallback where
// there is no scope).
func CheckNotInJob() error {
	if id, inside := InsideJob(); inside {
		return fmt.Errorf("refused: running inside job %q (%s is set); a job never starts, cancels or manages jobs, "+
			"only the orchestrator does", id, JobEnvVar)
	}
	if unit, _ := JobScope(); unit != "" {
		return fmt.Errorf("refused: running in the cgroup of job scope %s; a job never starts, cancels or manages jobs, "+
			"only the orchestrator does", unit)
	}
	if pid, _ := WrapperAncestor(); pid != 0 {
		return fmt.Errorf("refused: running under the job wrapper (pid %d); a job never starts, cancels or manages jobs, "+
			"only the orchestrator does", pid)
	}
	if sid, _ := WrapperSession(); sid != 0 {
		return fmt.Errorf("refused: running in the session of a job wrapper (session %d); a job never starts, cancels "+
			"or manages jobs, only the orchestrator does", sid)
	}
	return nil
}

// WrapperSession returns this process's session id when the session leader is a job wrapper (0 =
// not). The wrapper is started with setsid, so it leads the session of every job process that did not
// call setsid itself – also of an orphan re-parented away from the wrapper, whose ppid chain no longer
// reaches it. Independent of the agent and the state dir (a job could target another agent's store).
// ok is false when the check could not run (not Linux, /proc unreadable): the caller fails open.
func WrapperSession() (sid int, ok bool) {
	if runtime.GOOS != "linux" {
		return 0, false
	}
	if _, err := os.Stat(filepath.Join(procRoot, "self", "stat")); err != nil {
		return 0, false
	}
	sid, err := unix.Getsid(0)
	if err != nil || sid <= 1 || sid == os.Getpid() {
		return 0, true
	}
	argv, err := procCmdline(sid)
	if err != nil {
		return 0, true // the leader exited: the store check (CheckNotInJobSession) still applies
	}
	if IsWrapperCmdline(argv) {
		return sid, true
	}
	return 0, true
}

// ErrInJobSession is returned when this process is in the session of a running job.
var ErrInJobSession = errors.New("refused: running inside a job's session; a job never starts, cancels or manages jobs")

// CheckNotInJobSession is the fallback when the session leader is gone: it refuses when this process's session id is the recorded pgid of one of the
// agent's running jobs (the wrapper starts with setsid, so its pgid is also the session id).
func (m *Manager) CheckNotInJobSession() error {
	sid, err := unix.Getsid(0)
	if err != nil {
		return nil // fail open: no session id to compare
	}
	running, err := m.Store.ListByStatus(store.StatusRunning)
	if err != nil {
		return err
	}
	for _, j := range running {
		if j.PGID > 1 && j.PGID == sid {
			return fmt.Errorf("%w (job %s, session %d)", ErrInJobSession, j.ID, sid)
		}
	}
	return nil
}

// procStat returns the state and session id of pid from /proc/<pid>/stat.
func procStat(pid int) (state string, sid int, err error) {
	b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "stat"))
	if err != nil {
		return "", 0, err
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return "", 0, fmt.Errorf("parse /proc/%d/stat", pid)
	}
	f := strings.Fields(s[i+1:]) // state ppid pgrp session …
	if len(f) < 4 {
		return "", 0, fmt.Errorf("parse /proc/%d/stat", pid)
	}
	sid, err = strconv.Atoi(f[3])
	return f[0], sid, err
}
