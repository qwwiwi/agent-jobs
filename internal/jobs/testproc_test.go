package jobs

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// startOwned starts cmd for test t and registers its end in t.Cleanup right away (SIGKILL, then the
// wait), so no path out of the test, a t.Fatal before the test's own kill included, leaves it running
// (#105). On Linux the kernel also kills it when the test process itself dies (go test -timeout, a
// job's containment), when no cleanup runs. stop ends it earlier; it may be called more than once and
// returns the wait's error.
func startOwned(t testing.TB, cmd *exec.Cmd) (stop func() error) {
	t.Helper()
	setParentDeathSignal(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	var werr error
	stop = func() error {
		once.Do(func() {
			_ = cmd.Process.Kill()
			werr = cmd.Wait()
		})
		return werr
	}
	t.Cleanup(func() { _ = stop() })
	return stop
}

// The helper mode of this test binary (TestMain): the path of the agent-jobs binary under test, the
// file the helper writes the daemon's pid and its home to, and what it does after the start.
const (
	ownedHelperEnv     = "AGENT_JOBS_TEST_OWNED_HELPER"
	ownedHelperPIDFile = "AGENT_JOBS_TEST_OWNED_PIDFILE"
	ownedHelperMode    = "AGENT_JOBS_TEST_OWNED_MODE"
)

// TestOwnedHelperDaemon runs only as the child of TestStartedDaemonNeverOutlivesItsTest: it starts a
// daemon, records its pid and then fails mid-way (mode fail) or hangs until it is killed (mode hang).
func TestOwnedHelperDaemon(t *testing.T) {
	if os.Getenv(ownedHelperEnv) == "" {
		t.Skip("helper of TestStartedDaemonNeverOutlivesItsTest")
	}
	newManager(t)
	// No pipe on its output: a daemon that writes to a closed pipe dies of SIGPIPE, which would hide
	// the leak (a quiet daemon writes nothing for hours).
	d := exec.Command(selfBin, "daemon", "--agent", "test")
	startOwned(t, d)
	rec := strconv.Itoa(d.Process.Pid) + "\n" + os.Getenv("AGENT_JOBS_HOME") + "\n"
	if err := os.WriteFile(os.Getenv(ownedHelperPIDFile), []byte(rec), 0o600); err != nil {
		t.Fatal(err)
	}
	if os.Getenv(ownedHelperMode) == "hang" {
		time.Sleep(2 * time.Minute)
	}
	t.Fatal("planned failure mid-way, before the test's own kill")
}

// #105: a test that starts a daemon and then fails mid-way (t.Fatal before its own kill), or whose
// process is killed outright (go test -timeout, a job's containment), leaves no daemon behind.
func TestStartedDaemonNeverOutlivesItsTest(t *testing.T) {
	modes := []string{"fail"}
	if parentDeathSignalWorks {
		modes = append(modes, "hang")
	}
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "daemon.pid")
			c := exec.Command(os.Args[0], "-test.run=^TestOwnedHelperDaemon$", "-test.count=1")
			c.Env = append(os.Environ(), ownedHelperEnv+"="+selfBin, ownedHelperPIDFile+"="+pidFile, ownedHelperMode+"="+mode)
			c.Stdout, c.Stderr = io.Discard, io.Discard
			if err := c.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- c.Wait() }()
			var pid int
			var home string
			waitFor(t, "daemon pid", 30*time.Second, func() bool {
				b, err := os.ReadFile(pidFile)
				f := strings.Split(string(b), "\n")
				if err != nil || len(f) < 2 {
					return false
				}
				pid, _ = strconv.Atoi(f[0])
				home = f[1]
				return pid > 0
			})
			t.Cleanup(func() {
				if alive(pid) && procHasArg(pid, "daemon") {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
				if home != "" && mode == "hang" {
					_ = os.RemoveAll(home) // the killed helper ran no cleanup of its own
				}
			})
			if mode == "hang" {
				_ = c.Process.Kill()
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("the helper test passed; it must fail mid-way")
				}
			case <-time.After(30 * time.Second):
				_ = c.Process.Kill()
				t.Fatal("helper test did not end")
			}
			waitFor(t, "daemon gone", 5*time.Second, func() bool { return !alive(pid) || !procHasArg(pid, "daemon") })
		})
	}
}

// procHasArg reports whether pid's command line has the argument arg (false when unreadable).
func procHasArg(pid int, arg string) bool {
	argv, err := procCmdline(pid)
	if err != nil {
		b, _ := exec.Command("ps", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
		argv = strings.Fields(string(b))
	}
	for _, a := range argv {
		if a == arg {
			return true
		}
	}
	return false
}
