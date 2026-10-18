package agentjobs_test

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// reaperEnv makes the test binary a reaper (TestMain): the value is the report file, the arguments
// are the command. The reaper is a child subreaper, so every process the command leaves behind ends
// up as its own child once its parent is gone: it ends only those (direct, unreaped children, whose
// PIDs cannot have been reused), and never infers ownership from a process group or a start time.
const reaperEnv = "AGENT_JOBS_TEST_REAPER"

func TestMain(m *testing.M) {
	if report := os.Getenv(reaperEnv); report != "" {
		os.Exit(reaperMain(report, os.Args[1:]))
	}
	os.Exit(m.Run())
}

// reaperMain runs argv as its child and forwards TERM and INT to it (HUP: KILL, the test gave up).
// Once the child exited it waits briefly for the dying, then KILLs every process still alive among
// its children (the orphans of the run), again for their orphans, until it has none (at most 10 s);
// the ones it found are written to report, one per line. The exit code is the child's (128+N for a signal).
// Everything runs on one goroutine: a child is reaped only between the scans, never while it is
// being signalled.
func reaperMain(report string, argv []string) int {
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "reaper: no command")
		return 125
	}
	if err := setChildSubreaper(); err != nil {
		fmt.Fprintln(os.Stderr, "reaper: PR_SET_CHILD_SUBREAPER:", err)
		return 125
	}
	sigs := make(chan os.Signal, 16)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	path, err := exec.LookPath(argv[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "reaper:", err)
		return 125
	}
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, reaperEnv+"=") {
			env = append(env, kv)
		}
	}
	p, err := os.StartProcess(path, argv, &os.ProcAttr{Env: env, Files: []*os.File{os.Stdin, os.Stdout, os.Stderr}})
	if err != nil {
		fmt.Fprintln(os.Stderr, "reaper:", err)
		return 125
	}
	child, code := p.Pid, -1
	for code < 0 {
		select {
		case s := <-sigs:
			sig := s.(syscall.Signal)
			if sig == syscall.SIGHUP {
				sig = syscall.SIGKILL
			}
			_ = syscall.Kill(child, sig) // not reaped yet: reaping happens only below
		case <-time.After(20 * time.Millisecond):
		}
		code = reapAll(child, code)
	}
	// The members the run itself KILLed may still be on their way out; a signal now (the test gave
	// up) skips the wait. The signals stay caught: a default HUP or TERM would end the reaper and
	// hand the orphans to init.
	for end := time.Now().Add(300 * time.Millisecond); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		select {
		case <-sigs:
			end = time.Now()
		default:
		}
		reapAll(0, 0)
	}
	var left []string
	for end := time.Now().Add(10 * time.Second); ; {
		for _, pid := range liveChildren() {
			left = append(left, describe(pid))
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		time.Sleep(20 * time.Millisecond)
		reapAll(0, 0)
		if _, err := syscall.Wait4(-1, nil, syscall.WNOHANG, nil); err == syscall.ECHILD {
			break
		}
		if time.Now().After(end) {
			left = append(left, "children still not reaped after 10 s (unkillable)")
			break
		}
	}
	if err := os.WriteFile(report, []byte(strings.Join(left, "\n")), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "reaper:", err)
		return 125
	}
	return code
}

// reapAll reaps every exited child; it returns the exit code of main once main is among them, else
// code unchanged.
func reapAll(main, code int) int {
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		if err != nil || pid <= 0 {
			return code
		}
		if pid == main {
			if ws.Signaled() {
				code = 128 + int(ws.Signal())
			} else {
				code = ws.ExitStatus()
			}
		}
	}
}

// liveChildren are this process's children that are not zombies.
func liveChildren() []int {
	self := strconv.Itoa(os.Getpid())
	var kids []int
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if f := procStat(pid); len(f) > 1 && f[1] == self && f[0] != "Z" {
			kids = append(kids, pid)
		}
	}
	return kids
}

func describe(pid int) string {
	b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	return fmt.Sprintf("%d %s", pid, strings.TrimSpace(strings.ReplaceAll(string(b), "\x00", " ")))
}

// procStat is /proc/PID/stat after the comm: state (field 3), ppid, pgrp, ...
func procStat(pid int) []string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return nil
	}
	s := string(b)
	return strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
}

// reaper is a run under the reaper: argv0 to put in front of the command, its environment entry and
// the report file.
type reaper struct {
	argv0  []string
	env    string
	report string
}

func newReaper(t *testing.T) reaper {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("Linux only (child subreaper)")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	report := filepath.Join(t.TempDir(), "leftovers")
	return reaper{argv0: []string{self}, env: reaperEnv + "=" + report, report: report}
}

// leftovers are the processes the run left behind (the reaper has ended them); the run must be over.
func (r reaper) leftovers(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(r.report)
	if err != nil {
		t.Fatalf("reaper report: %v", err)
	}
	if s := strings.TrimSpace(string(b)); s != "" {
		return strings.Split(s, "\n")
	}
	return nil
}

// The reaper ends what a command left behind, reports it, keeps the exit code, and touches nothing
// that is not its own: a process of this test outside the run survives.
func TestReaperEndsOnlyItsOrphans(t *testing.T) {
	r := newReaper(t)
	bystander := exec.Command("sleep", "300")
	if err := bystander.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bystander.Process.Kill(); _ = bystander.Wait() })
	cmd := exec.Command(r.argv0[0], "sh", "-c", "sleep 300 & ( sleep 301 & ); exit 3")
	cmd.Env = append(os.Environ(), r.env)
	if code := exitCode(t, cmd.Run()); code != 3 {
		t.Fatalf("exit %d, want 3", code)
	}
	left := strings.Join(r.leftovers(t), "\n")
	if strings.Count(left, "\n") != 1 || !strings.Contains(left, "sleep 300") || !strings.Contains(left, "sleep 301") {
		t.Fatalf("leftovers %q, want both sleeps", left)
	}
	if err := bystander.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("bystander ended: %v", err)
	}
}
