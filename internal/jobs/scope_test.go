package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

func TestJobScopeIn(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"0::/user.slice/user-1000.slice/user@1000.service/app.slice/agent-jobs-thrall-261017-150405-ab12.scope\n",
			"agent-jobs-thrall-261017-150405-ab12.scope"},
		// A sub-cgroup the job created inside its scope is still the scope.
		{"0::/user.slice/user@1000.service/app.slice/agent-jobs-a-1.scope/escape\n", "agent-jobs-a-1.scope"},
		// cgroup v1 hybrid: any hierarchy line counts.
		{"12:pids:/\n1:name=systemd:/user.slice/agent-jobs-a-1.scope\n0::/\n", "agent-jobs-a-1.scope"},
		{"0::/user.slice/user@1000.service/app.slice/app-agent\\x2djobs.slice/agent-jobs@thrall.service\n", ""},
		{"0::/user.slice/user@1000.service/app.slice/agent-jobs-.scope\n", ""},
		{"0::/system.slice/channel-thrall.service\n", ""},
		{"", ""},
	} {
		if got := jobScopeIn(c.in); got != c.want {
			t.Errorf("jobScopeIn(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if u := ScopeUnit("thrall", "261017-150405-ab12"); u != "agent-jobs-thrall-261017-150405-ab12.scope" || !isJobScopeName(u) {
		t.Fatalf("ScopeUnit = %q", u)
	}
}

// Point 2 in isolation: with the marker gone and no wrapper in the ppid chain or as session leader,
// the job scope named in /proc/self/cgroup alone refuses control commands.
func TestCheckNotInJobRefusesInsideJobScope(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("cgroups are Linux")
	}
	f := filepath.Join(t.TempDir(), "cgroup")
	if err := os.WriteFile(f, []byte("0::/user.slice/user@1000.service/app.slice/agent-jobs-x-1.scope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := cgroupFile
	cgroupFile = f
	t.Cleanup(func() { cgroupFile = old })
	err := CheckNotInJob()
	if err == nil || !strings.Contains(err.Error(), "agent-jobs-x-1.scope") {
		t.Fatalf("CheckNotInJob = %v, want a scope refusal", err)
	}
	if err := os.WriteFile(f, []byte("0::/user.slice/user@1000.service/app.slice/other.scope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckNotInJob(); err != nil {
		t.Fatalf("outside a job scope: %v", err)
	}
}

func TestCgroupScopeConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"cgroup_scope":"sometimes"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(dir); err == nil {
		t.Fatal("an unknown cgroup_scope must be rejected")
	}
	t.Setenv("AGENT_JOBS_CGROUP_SCOPE", "")
	if m := (Config{}).scopeMode(); m != ScopeAuto {
		t.Fatalf("default mode %q", m)
	}
	t.Setenv("AGENT_JOBS_CGROUP_SCOPE", ScopeOff)
	if m := (Config{CgroupScope: ScopeRequired}).scopeMode(); m != ScopeOff {
		t.Fatalf("env must win: %q", m)
	}
}

// The wrapper drops the user bus variables from the job: no `systemd-run --user` out of the scope.
func TestJobEnvDropsUserBus(t *testing.T) {
	env, err := JobEnv(filepath.Join(t.TempDir(), "jobs", "j1"), []string{"XDG_RUNTIME_DIR=/run/user/1", "DBUS_SESSION_BUS_ADDRESS=unix:path=/x", "KEEP=1"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(env, " "); got != "KEEP=1 "+JobEnvVar+"=j1" {
		t.Fatalf("env %q", got)
	}
}

// requireUserScope skips unless this host can create transient systemd user scopes (Linux, systemd,
// a reachable user bus). It points XDG_RUNTIME_DIR at /run/user/<uid> when only that is missing.
func requireUserScope(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("cgroup scopes need Linux with systemd")
	}
	if os.Getenv("XDG_RUNTIME_DIR") == "" {
		dir := fmt.Sprintf("/run/user/%d", os.Getuid())
		if fi, err := os.Stat(filepath.Join(dir, "bus")); err == nil && fi.Mode()&fs.ModeSocket != 0 {
			t.Setenv("XDG_RUNTIME_DIR", dir)
		}
	}
	if err := ProbeScope(); err != nil {
		t.Skipf("no systemd user scope on this host: %v", err)
	}
}

// scopedManager is newManager with scopes required; every scope a test job got is stopped at the end.
func scopedManager(t *testing.T) *Manager {
	t.Helper()
	requireUserScope(t)
	m := newManager(t)
	t.Setenv("AGENT_JOBS_CGROUP_SCOPE", ScopeRequired)
	t.Cleanup(func() {
		if list, err := m.Store.List(100); err == nil {
			for _, j := range list {
				if j.ScopeUnit != "" {
					_ = m.stopScope(j.ID)
				}
			}
		}
	})
	return m
}

type orphanInfo struct{ pid, ppid, sid int }

func readOrphan(t *testing.T, dir string) orphanInfo {
	t.Helper()
	var o orphanInfo
	waitFor(t, "orphan.ready", 15*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(dir, "orphan.ready"))
		return err == nil
	})
	b, err := os.ReadFile(filepath.Join(dir, "orphan.info"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Sscan(string(b), &o.pid, &o.ppid, &o.sid); err != nil || o.pid <= 1 {
		t.Fatalf("orphan.info %q: %v", b, err)
	}
	return o
}

// gone reports a process that no longer exists or is a zombie (killed, not yet reaped by its subreaper).
func gone(pid int) bool {
	st, _, err := procStat(pid)
	return err != nil || st == "Z"
}

func requireSetsid(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid(1) not installed")
	}
}

// Point 5, launch path: a job process that drops the marker, calls setsid and is re-parented away from
// the wrapper (so the ancestry, session and store checks all miss it) is still refused by the cgroup
// check, and the wrapper's cgroup sweep ends it before exit_code is written.
func TestScopeRefusesAndKillsSetsidOrphan(t *testing.T) {
	requireSetsid(t)
	m := scopedManager(t)
	t.Setenv("AGENT_JOBS_SELF", selfBin)
	old := SweepGrace
	SweepGrace = time.Second
	t.Cleanup(func() { SweepGrace = old })
	j := enqueue(t, m, "FAKE_SETSID_ORPHAN", nil)
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	running := get(t, m, j.ID)
	if want := ScopeUnit("test", j.ID); running.ScopeUnit != want {
		t.Fatalf("scope unit %q, want %q", running.ScopeUnit, want)
	}
	dir := m.JobDir(j.ID)
	o := readOrphan(t, dir)
	t.Cleanup(func() { _ = syscall.Kill(o.pid, syscall.SIGKILL) })
	// The escape is real: another session (its setsid leader has exited), re-parented away from the wrapper.
	if o.sid == running.PGID || o.ppid == running.PID || alive(o.sid) && !gone(o.sid) {
		t.Fatalf("orphan did not escape session/ancestry: %+v (wrapper %d)", o, running.PID)
	}
	out, _ := os.ReadFile(filepath.Join(dir, "orphan.out"))
	if !strings.Contains(string(out), "refused") || !strings.Contains(string(out), "job scope "+running.ScopeUnit) ||
		!strings.Contains(string(out), "exit=1") {
		t.Fatalf("orphan's control command was not refused by the scope check: %s", out)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("AGENT_JOBS_HOME"), "agentb")); err == nil {
		t.Fatal("the orphan reached agent B's state")
	}
	waitFor(t, "exit_code", 20*time.Second, func() bool {
		_, ok := readExitCode(dir)
		return ok
	})
	// Before any collection: the wrapper's own sweep already ended the orphan.
	if !gone(o.pid) {
		t.Fatalf("orphan %d outlived the wrapper's exit_code", o.pid)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "stderr.log")); !strings.Contains(string(b), "leftover process") {
		t.Fatalf("stderr.log does not record the sweep: %s", b)
	}
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if done := get(t, m, j.ID); done.Status != store.StatusSucceeded {
		t.Fatalf("job: %s %q", done.Status, done.Error)
	}
	if st, _, err := scopeState(running.ScopeUnit); err != nil || st != "inactive" {
		t.Fatalf("scope after the job: %q %v", st, err)
	}
}

// The documented weaker fallback: without a scope the same orphan is neither refused nor swept. This
// pins why the scope exists (if this ever starts failing, the fallback got stronger: update the docs).
func TestSessionFallbackMissesSetsidOrphan(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("session sweep reads /proc (Linux)")
	}
	requireSetsid(t)
	m := newManager(t) // AGENT_JOBS_CGROUP_SCOPE=off
	t.Setenv("AGENT_JOBS_SELF", selfBin)
	old := SweepGrace
	SweepGrace = 200 * time.Millisecond
	t.Cleanup(func() { SweepGrace = old })
	j := enqueue(t, m, "FAKE_SETSID_ORPHAN", nil)
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if get(t, m, j.ID).ScopeUnit != "" {
		t.Fatal("scope off must launch without a scope")
	}
	o := readOrphan(t, m.JobDir(j.ID))
	t.Cleanup(func() { _ = syscall.Kill(o.pid, syscall.SIGKILL) })
	waitFor(t, "exit_code", 20*time.Second, func() bool {
		_, ok := readExitCode(m.JobDir(j.ID))
		return ok
	})
	out, _ := os.ReadFile(filepath.Join(m.JobDir(j.ID), "orphan.out"))
	if strings.Contains(string(out), "refused") {
		t.Fatalf("fallback refused the setsid orphan (stronger than documented?): %s", out)
	}
	if gone(o.pid) {
		t.Fatal("fallback swept the setsid orphan (stronger than documented?)")
	}
}

// Point 3 for cancel and timeout: the daemon stops the whole scope, so a setsid orphan the job left
// dies with it before the row turns terminal.
func TestScopeStoppedOnCancelAndTimeout(t *testing.T) {
	requireSetsid(t)
	for _, how := range []string{"cancel", "timeout"} {
		t.Run(how, func(t *testing.T) {
			m := scopedManager(t)
			noAutoContinue(m)
			t.Setenv("AGENT_JOBS_SELF", selfBin)
			j := enqueue(t, m, "FAKE_SETSID_ORPHAN FAKE_SLEEP=60", func(r *StartRequest) { r.TimeoutMinutes = 1 })
			if err := m.Tick(); err != nil {
				t.Fatal(err)
			}
			unit := get(t, m, j.ID).ScopeUnit
			o := readOrphan(t, m.JobDir(j.ID))
			t.Cleanup(func() { _ = syscall.Kill(o.pid, syscall.SIGKILL) })
			want := store.StatusCancelled
			if how == "cancel" {
				if err := m.Cancel(j.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				want = store.StatusTimeout
				real := time.Now
				m.Now = func() time.Time { return real().Add(2 * time.Minute) }
				if err := m.Tick(); err != nil {
					t.Fatal(err)
				}
			}
			if got := get(t, m, j.ID); got.Status != want {
				t.Fatalf("status %s %q, want %s", got.Status, got.Error, want)
			}
			if !gone(o.pid) {
				t.Fatalf("setsid orphan %d survived the %s", o.pid, how)
			}
			if st, _, err := scopeState(unit); err != nil || st != "inactive" {
				t.Fatalf("scope after %s: %q %v", how, st, err)
			}
		})
	}
}

// testUnitMaxRuntime caps a systemd unit a test starts (RuntimeMaxSec): far above the test's own
// waits, so only a unit whose test died without its cleanup ever reaches it.
const testUnitMaxRuntime = "10min"

// Point 5, daemon restarts: the real daemon runs as a systemd user service with the KillMode of the
// shipped unit template; restarting it leaves the job's scope (outside the service's cgroup) running,
// and the job is collected normally afterwards.
func TestDaemonUnitRestartLeavesJobScope(t *testing.T) {
	m := scopedManager(t)
	tmpl, err := os.ReadFile("../../deploy/systemd/agent-jobs@.service")
	if err != nil {
		t.Fatal(err)
	}
	km := regexp.MustCompile(`(?m)^KillMode=(\S+)$`).FindSubmatch(tmpl)
	if km == nil || string(km[1]) != "process" {
		t.Fatalf("unit template KillMode: %q", km)
	}
	j := enqueue(t, m, "FAKE_SLEEP=8", nil)
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	svc := "agent-jobs-testd-" + hex.EncodeToString(b) + ".service"
	// The unit runs outside the job's scope, so no containment of a job running this suite ends it:
	// a recognisable name and description, stopped in t.Cleanup, and a runtime cap that has systemd
	// stop it even when the test process dies before its cleanup runs (#105).
	args := []string{"--user", "--quiet", "--collect", "--unit=" + svc, "--property=KillMode=" + string(km[1]),
		"--description=agent-jobs test daemon (" + t.Name() + ")", "--property=RuntimeMaxSec=" + testUnitMaxRuntime}
	for _, k := range []string{"AGENT_JOBS_HOME", "AGENT_JOBS_CLAUDE_BIN", "AGENT_JOBS_TMUX_BIN", "AGENT_JOBS_CGROUP_SCOPE", "PATH"} {
		args = append(args, "--setenv="+k+"="+os.Getenv(k))
	}
	args = append(args, "--", selfBin, "daemon", "--agent", "test")
	if out, err := systemdCmd(t.Context(), "systemd-run", args...).CombinedOutput(); err != nil {
		t.Fatalf("start daemon unit: %v %s", err, out)
	}
	t.Cleanup(func() { _ = systemdCmd(context.Background(), "systemctl", "--user", "stop", svc).Run() })
	waitFor(t, "job running in its scope", 20*time.Second, func() bool {
		g := get(t, m, j.ID)
		return g.Status == store.StatusRunning && g.PID > 0 && g.ScopeUnit != ""
	})
	// The pid is recorded while it is still systemd-run; the wrapper registers after the exec, in the scope.
	waitFor(t, "wrapper registered", 20*time.Second, func() bool {
		_, err := ReadWrapperInfo(m.JobDir(j.ID))
		return err == nil
	})
	running := get(t, m, j.ID)
	wcg, err := procCgroup(running.PID)
	if err != nil {
		t.Fatal(err)
	}
	out, err := systemdCmd(t.Context(), "systemctl", "--user", "show", "--property=ControlGroup", "--value", svc).Output()
	if err != nil {
		t.Fatal(err)
	}
	svcCG := strings.TrimSpace(string(out))
	if svcCG == "" || inSubtree(wcg, svcCG) || filepath.Base(wcg) != running.ScopeUnit {
		t.Fatalf("wrapper cgroup %q, service cgroup %q, scope %q", wcg, svcCG, running.ScopeUnit)
	}
	if out, err := systemdCmd(t.Context(), "systemctl", "--user", "restart", svc).CombinedOutput(); err != nil {
		t.Fatalf("restart daemon unit: %v %s", err, out)
	}
	if st, _, err := scopeState(running.ScopeUnit); err != nil || st != "active" || !alive(running.PID) {
		t.Fatalf("daemon restart touched the job: scope %q %v, wrapper alive %v", st, err, alive(running.PID))
	}
	waitFor(t, "exit_code", 30*time.Second, func() bool {
		_, ok := readExitCode(m.JobDir(j.ID))
		return ok
	})
	if out, err := exec.Command(selfBin, "tick", "--agent", "test").CombinedOutput(); err == nil || !strings.Contains(string(out), "manual tick refused") {
		t.Fatalf("manual tick while daemon is active: %v %s", err, out)
	}
	// Collection belongs to the restarted daemon; a fresh CLI manager must not launch or collect
	// with its independently loaded config. Wait for the daemon's next scheduled tick.
	waitFor(t, "restarted daemon collected the job", TickInterval+15*time.Second, func() bool {
		return get(t, m, j.ID).Status == store.StatusSucceeded
	})
	if got := get(t, m, j.ID); got.Status != store.StatusSucceeded || got.Turns != 3 {
		t.Fatalf("after the restart: %s %q", got.Status, got.Error)
	}
}

// fakeSystemctl points systemctlBin at a script that fails every call until ok() is called; after that
// `show` reports the unit gone (not loaded). The sweep fallback finds no cgroup (cgroupRoot is empty).
func fakeSystemctl(t *testing.T) (ok func()) {
	t.Helper()
	dir := t.TempDir()
	okFile := filepath.Join(dir, "ok")
	body := "#!/bin/bash\n[ -e " + okFile + " ] || { echo 'Failed to connect to bus' >&2; exit 1; }\n" +
		"case \" $* \" in *' show '*) printf 'ActiveState=inactive\\nControlGroup=\\n' ;; esac\n"
	bin := filepath.Join(dir, "systemctl")
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	oldBin, oldRoot := systemctlBin, cgroupRoot
	systemctlBin, cgroupRoot = bin, t.TempDir()
	t.Cleanup(func() { systemctlBin, cgroupRoot = oldBin, oldRoot })
	return func() {
		if err := os.WriteFile(okFile, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// scopedRow is a claimed running job that records a scope unit (no process behind it).
func scopedRow(t *testing.T, m *Manager) *store.Job {
	t.Helper()
	j := enqueue(t, m, "x", nil)
	if err := m.Store.Claim(j.ID, "tok", m.Now()); err != nil {
		t.Fatal(err)
	}
	if err := m.Store.SetScope(j.ID, "tok", ScopeUnit(m.Agent, j.ID)); err != nil {
		t.Fatal(err)
	}
	return get(t, m, j.ID)
}

// A scope stop that fails does not leave a terminal row with nothing retrying: the stop is recorded as
// pending before the row turns terminal, shown on the job-done line, retried every tick, and cleared only
// once the unit is gone. Covers finish (collect), timeout and cancel.
func TestFailedScopeStopIsRetried(t *testing.T) {
	m := newManager(t)
	ok := fakeSystemctl(t)

	done := scopedRow(t, m)
	m.finish(done, store.StatusSucceeded, StreamResult{}, false, 0, "", m.Now())
	timedOut := scopedRow(t, m)
	m.finish(timedOut, store.StatusTimeout, StreamResult{}, true, 0, "timeout after 1 minutes", m.Now())
	cancelled := scopedRow(t, m)
	if err := m.Cancel(cancelled.ID); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		j      *store.Job
		status string
	}{{done, store.StatusFailed}, {timedOut, store.StatusTimeout}, {cancelled, store.StatusCancelled}} {
		got := get(t, m, c.j.ID)
		if got.Status != c.status || !got.ScopePending || !strings.Contains(got.Error, ReasonSurvivors) {
			t.Fatalf("after a failed stop: %s pending=%v %q, want %s pending", got.Status, got.ScopePending, got.Error, c.status)
		}
		if line := DoneLine(got, 0); !strings.Contains(line, "stop pending") || !strings.Contains(line, ReasonSurvivors) {
			t.Fatalf("job-done line hides the pending stop: %q", line)
		}
	}
	// Still failing: the next tick keeps it pending.
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if !get(t, m, done.ID).ScopePending {
		t.Fatal("a tick whose stop failed cleared the pending mark")
	}
	ok()
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	for _, j := range []*store.Job{done, timedOut, cancelled} {
		if got := get(t, m, j.ID); got.ScopePending {
			t.Fatalf("job %s: pending after a successful retry", j.ID)
		}
	}
}

// A unit that is failed but still has processes in its cgroup is not stopped (L3 of the PR #10 review).
func TestFailedUnitWithMembersIsNotStopped(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("cgroups are Linux")
	}
	m := newManager(t)
	dir := t.TempDir()
	body := "#!/bin/bash\ncase \" $* \" in *' show '*) printf 'ActiveState=failed\\nControlGroup=/x.scope\\n' ;; esac\n"
	bin := filepath.Join(dir, "systemctl")
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "x.scope"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A member the sweep cannot verify (its /proc/<pid>/cgroup is not /x.scope): it is left, so the
	// cgroup is not provably empty.
	if err := os.WriteFile(filepath.Join(root, "x.scope", "cgroup.procs"), []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldBin, oldRoot := systemctlBin, cgroupRoot
	systemctlBin, cgroupRoot = bin, root
	t.Cleanup(func() { systemctlBin, cgroupRoot = oldBin, oldRoot })
	if err := m.stopScope("261017-000000-0000"); err == nil {
		t.Fatal("a failed unit with processes in its cgroup counted as stopped")
	}
}

// Cancel with a broken user bus does not rely on the wrapper's process group: the daemon sweeps the
// scope's cgroup itself, so a setsid orphan (outside that group) dies too. Only the user-bus systemctl
// (systemctlBin) is broken: the manager's cgroup still comes from the real system manager
// (systemManagerBin), so the swept path is the real one. The caller's own cgroup plays no part: the
// cancel also works from a caller outside the user manager (the orchestrator's MCP server).
func TestCancelWithoutBusSweepsScopeCgroup(t *testing.T) {
	requireSetsid(t)
	for _, caller := range []string{"own", "outside the user manager"} {
		t.Run(caller, func(t *testing.T) {
			if caller != "own" {
				f := filepath.Join(t.TempDir(), "cgroup")
				if err := os.WriteFile(f, []byte("0::/agents.slice/channel-thrall.service\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				old := cgroupFile
				cgroupFile = f
				t.Cleanup(func() { cgroupFile = old })
			}
			cancelWithoutBus(t)
		})
	}
}

func cancelWithoutBus(t *testing.T) {
	m := scopedManager(t)
	t.Setenv("AGENT_JOBS_SELF", selfBin)
	j := enqueue(t, m, "FAKE_SETSID_ORPHAN FAKE_SLEEP=60", nil)
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	o := readOrphan(t, m.JobDir(j.ID))
	t.Cleanup(func() { _ = syscall.Kill(o.pid, syscall.SIGKILL) })
	dir := t.TempDir()
	bin := filepath.Join(dir, "systemctl")
	if err := os.WriteFile(bin, []byte("#!/bin/bash\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := systemctlBin
	systemctlBin = bin
	t.Cleanup(func() { systemctlBin = old })
	if err := m.Cancel(j.ID); err != nil {
		t.Fatal(err)
	}
	if !gone(o.pid) {
		t.Fatalf("setsid orphan %d survived a cancel without the user bus", o.pid)
	}
	if got := get(t, m, j.ID); got.Status != store.StatusCancelled || got.ScopePending {
		t.Fatalf("cancel: %s pending=%v %q", got.Status, got.ScopePending, got.Error)
	}
}

// fakeManagerCgroup makes the system manager answer cgroup (or fail with err) for user@<uid>.service,
// and points cgroupRoot at a temp dir holding dirs (relative paths), for one test.
func fakeManagerCgroup(t *testing.T, cgroup string, err error, dirs ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	oldLookup, oldRoot := userManagerUnitCgroup, cgroupRoot
	userManagerUnitCgroup = func(int) (string, error) { return cgroup, err }
	cgroupRoot = root
	t.Cleanup(func() { userManagerUnitCgroup, cgroupRoot = oldLookup, oldRoot })
	return root
}

// The scope's cgroup path follows the user manager's cgroup as the system manager reports it, wherever
// the manager runs (user.slice or a custom slice) and wherever the caller runs. A missing or malformed
// answer falls back to the default user.slice layout only when that cgroup exists and is owned by the
// uid; otherwise there is no path and nothing is swept.
func TestScopeCgroupPathFromSystemManager(t *testing.T) {
	uid := os.Getuid()
	mgr := fmt.Sprintf("user@%d.service", uid)
	def := fmt.Sprintf("/user.slice/user-%d.slice/%s", uid, mgr)
	unit := ScopeUnit("t", "261018-000000-0000")
	failed := errors.New("exit status 1")
	for _, tc := range []struct {
		name, answer string
		err          error
		dirs         []string
		want         string // "" = no path
	}{
		{"user.slice", def, nil, nil, def},
		{"agents.slice", "/agents.slice/" + mgr, nil, nil, "/agents.slice/" + mgr},
		// The answer wins over a planted decoy, and over the default layout.
		{"decoy ignored", "/agents.slice/" + mgr, nil, []string{"deleg.slice/x.service/" + mgr, def[1:]}, "/agents.slice/" + mgr},
		{"garbage, default exists", "not a cgroup", nil, []string{def[1:]}, def},
		{"garbage, no default", "not a cgroup", nil, nil, ""},
		{"empty answer (unit not loaded)", "", nil, nil, ""},
		{"dot-dot", "/agents.slice/../" + mgr, nil, nil, ""},
		{"wrong last component", "/agents.slice/" + mgr + "/app.slice", nil, nil, ""},
		{"other uid", fmt.Sprintf("/agents.slice/user@%d.service", uid+1), nil, nil, ""},
		{"command failure, default exists", "", failed, []string{def[1:]}, def},
		// A decoy elsewhere is never guessed: no answer and no default means no sweep.
		{"command failure, decoy only", "", failed, []string{"deleg.slice/x.service/" + mgr}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeManagerCgroup(t, tc.answer, tc.err, tc.dirs...)
			got, ok := scopeCgroupPath(unit)
			if tc.want == "" {
				if ok || got != "" {
					t.Fatalf("scopeCgroupPath = %q, %v, want no path", got, ok)
				}
				return
			}
			if want := tc.want + "/app.slice/" + unit; !ok || got != want {
				t.Fatalf("scopeCgroupPath = %q, %v, want %q", got, ok, want)
			}
		})
	}
	t.Run("default owned by another uid", func(t *testing.T) {
		fakeManagerCgroup(t, "", failed, def[1:])
		old := cgroupDirOwner
		cgroupDirOwner = func(fs.FileInfo) (int, bool) { return uid + 1, true }
		t.Cleanup(func() { cgroupDirOwner = old })
		if got, ok := scopeCgroupPath(unit); ok {
			t.Fatalf("scopeCgroupPath = %q: a default cgroup of another uid was used", got)
		}
	})
	t.Run("default is a symlink", func(t *testing.T) {
		root := fakeManagerCgroup(t, "", failed, "elsewhere/"+mgr, fmt.Sprintf("user.slice/user-%d.slice", uid))
		if err := os.Symlink(filepath.Join(root, "elsewhere", mgr), filepath.Join(root, def)); err != nil {
			t.Fatal(err)
		}
		if got, ok := scopeCgroupPath(unit); ok {
			t.Fatalf("scopeCgroupPath = %q: a symlinked default was followed", got)
		}
	})
}

// Every component of the manager's path is checked, not only those before user@<uid>.service.
func TestValidManagerCgroup(t *testing.T) {
	uid := os.Getuid()
	mgr := fmt.Sprintf("user@%d.service", uid)
	for _, tc := range []struct {
		p  string
		ok bool
	}{
		{"/user.slice/user-1.slice/" + mgr, true},
		{"/agents.slice/" + mgr, true},
		{"/" + mgr, true},
		{"", false},
		{"/", false},
		{"agents.slice/" + mgr, false},
		{"/agents.slice/" + mgr + "/", false},
		{"/agents.slice/" + mgr + "/..", false},
		{"/agents.slice/" + mgr + "/.", false},
		{"/agents.slice/" + mgr + "//", false},
		{"/agents.slice//" + mgr, false},
		{"/agents.slice/./" + mgr, false},
		{"/a/../" + mgr, false},
		{"/agents.slice/" + mgr + "/app.slice", false},
		{"/agents.slice/" + mgr + "x", false},
		{fmt.Sprintf("/agents.slice/user@%d.service", uid+1), false},
	} {
		if got := validManagerCgroup(tc.p, uid); got != tc.ok {
			t.Errorf("validManagerCgroup(%q) = %v, want %v", tc.p, got, tc.ok)
		}
	}
}

// With the user bus broken and no trusted systemctl for the system manager (so no answer) and no default
// cgroup, the stop refuses: nothing is swept (not even a uid-owned user@<uid>.service decoy holding the
// scope's name), the stop fails, and a cancelled job stays scope-pending.
func TestStopScopeRefusesWithoutTrustedManagerCgroup(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("cgroups are Linux")
	}
	m := newManager(t)
	untrusted := stubSystemManager(t, "echo /agents.slice/user@0.service\n")
	oldBin := systemManagerBin
	systemManagerBin = func() (string, error) { return trustedBinary([]string{untrusted}) }
	t.Cleanup(func() { systemManagerBin = oldBin })
	if _, err := userManagerUnitCgroup(os.Getuid()); err == nil || !strings.Contains(err.Error(), "no trusted systemctl") {
		t.Fatalf("lookup with only a user-owned systemctl = %v, want a refusal", err)
	}
	fakeSystemctl(t) // user bus broken, cgroupRoot an empty dir (no default cgroup)
	root := cgroupRoot
	mgr := fmt.Sprintf("user@%d.service", os.Getuid())
	j := scopedRow(t, m)
	unit := ScopeUnit(m.Agent, j.ID)
	decoy := "deleg.slice/x.service/" + mgr + "/app.slice/" + unit
	if err := os.MkdirAll(filepath.Join(root, decoy), 0o700); err != nil {
		t.Fatal(err)
	}
	// An empty decoy would count as swept; refusing must not depend on looking at it at all.
	if err := os.WriteFile(filepath.Join(root, decoy, "cgroup.procs"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err := m.stopScope(j.ID)
	if err == nil || !strings.Contains(err.Error(), "no trusted cgroup path") {
		t.Fatalf("stopScope = %v, want a refusal", err)
	}
	if err := m.Cancel(j.ID); err != nil {
		t.Fatal(err)
	}
	if got := get(t, m, j.ID); got.Status != store.StatusCancelled || !got.ScopePending {
		t.Fatalf("cancel without a trusted path: %s pending=%v %q, want cancelled and scope-pending", got.Status, got.ScopePending, got.Error)
	}
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	if !get(t, m, j.ID).ScopePending {
		t.Fatal("a tick without a trusted path cleared the pending mark")
	}
}

// stubSystemManager writes a bash stub with body and points systemManagerBin at it for one test.
func stubSystemManager(t *testing.T, body string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "systemctl")
	if err := os.WriteFile(bin, []byte("#!/bin/bash\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	old := systemManagerBin
	systemManagerBin = func() (string, error) { return bin, nil }
	t.Cleanup(func() { systemManagerBin = old })
	return bin
}

// killPidFile kills the pid a stub wrote to f (a sleeper it left behind).
func killPidFile(t *testing.T, f string) {
	t.Cleanup(func() {
		if b, err := os.ReadFile(f); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
}

// At the command boundary the lookup passes exactly the documented arguments and an environment without
// the daemon's DBUS_* or LD_* variables.
func TestSystemManagerLookupBoundary(t *testing.T) {
	dir := t.TempDir()
	args, env := filepath.Join(dir, "args"), filepath.Join(dir, "env")
	stubSystemManager(t, "printf '%s\\n' \"$@\" > "+args+"\nenv > "+env+"\necho ' /agents.slice/user@7.service '\n")
	t.Setenv("DBUS_SYSTEM_BUS_ADDRESS", "unix:path=/nonexistent/bus")
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/nonexistent/bus")
	t.Setenv("LD_LIBRARY_PATH", "/nonexistent")
	got, err := systemManagerUnitCgroup(7)
	if err != nil || got != "/agents.slice/user@7.service" {
		t.Fatalf("lookup = %q, %v", got, err)
	}
	b, err := os.ReadFile(args)
	if err != nil {
		t.Fatal(err)
	}
	if want := "--system\nshow\nuser@7.service\n--property=ControlGroup\n--value\n"; string(b) != want {
		t.Fatalf("args = %q, want %q", b, want)
	}
	b, err = os.ReadFile(env)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	for _, l := range lines {
		if strings.HasPrefix(l, "DBUS_") || strings.HasPrefix(l, "LD_") {
			t.Fatalf("lookup inherited %q; env:\n%s", l, b)
		}
	}
	for _, want := range systemManagerEnv {
		if !slices.Contains(lines, want) {
			t.Fatalf("env lacks %q:\n%s", want, b)
		}
	}
}

// The lookup is bounded even when a descendant holds its stdout: after the process exits (WaitDelay) and
// when it hangs (timeout, then WaitDelay).
func TestSystemManagerLookupBounded(t *testing.T) {
	for _, tc := range []struct{ name, tail string }{
		{"exits, sleeper holds stdout", "echo /agents.slice/user@7.service\nexit 0\n"},
		{"hangs, sleeper holds stdout", "exec sleep 30\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pid := filepath.Join(t.TempDir(), "pid")
			killPidFile(t, pid)
			stubSystemManager(t, "sleep 30 &\necho $! > "+pid+"\n"+tc.tail)
			oldT, oldW := systemManagerTimeout, systemManagerWaitDelay
			systemManagerTimeout, systemManagerWaitDelay = 500*time.Millisecond, 500*time.Millisecond
			t.Cleanup(func() { systemManagerTimeout, systemManagerWaitDelay = oldT, oldW })
			start := time.Now()
			_, err := systemManagerUnitCgroup(7)
			if d := time.Since(start); d > 5*time.Second {
				t.Fatalf("lookup took %s (err %v), want it bounded", d, err)
			}
			if err == nil {
				t.Fatal("lookup whose pipes stayed held succeeded")
			}
		})
	}
	if systemManagerWaitDelay <= 0 {
		t.Fatal("systemManagerWaitDelay must be set")
	}
}

// Only a root-owned, not group/world-writable regular file in such a directory is taken; the daemon's
// PATH plays no part.
func TestTrustedBinary(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux paths")
	}
	own := filepath.Join(t.TempDir(), "systemctl")
	if err := os.WriteFile(own, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "systemctl")
	if err := os.Symlink(own, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(own))
	for _, c := range [][]string{{own}, {link}, {"/nonexistent/systemctl"}, {"/usr/bin"}, nil} {
		if p, err := trustedBinary(c); err == nil {
			t.Errorf("trustedBinary(%q) = %q, want an error", c, p)
		}
	}
	real, err := trustedBinary(systemctlCandidates)
	if err != nil {
		t.Skipf("no trusted systemctl on this host: %v", err)
	}
	if p, err := trustedBinary(append([]string{own, link, "/nonexistent/systemctl"}, systemctlCandidates...)); err != nil || p != real {
		t.Fatalf("trustedBinary skipped the untrusted ones = %q, %v, want %q", p, err, real)
	}
}

// fakeProbe makes probeScope fail (or succeed) for one test.
func fakeProbe(t *testing.T, err error) {
	t.Helper()
	old := probeScope
	probeScope = func() error { return err }
	t.Cleanup(func() { probeScope = old })
}

// In auto (and required), a failing probe on a host where a scope worked before requeues the job for
// up to ScopeRetryWindow instead of downgrading it; after the window auto falls back (visibly) and
// required refuses. On a host that never had a scope, auto falls back at once.
func TestScopeProbeFailureRequeuesOnceScopesWorked(t *testing.T) {
	for _, mode := range []string{ScopeAuto, ScopeRequired} {
		t.Run(mode, func(t *testing.T) {
			m := newManager(t)
			t.Setenv("AGENT_JOBS_CGROUP_SCOPE", mode)
			fakeProbe(t, errors.New("bus timeout"))
			if err := os.WriteFile(filepath.Join(m.StateDir, scopeOKFile), []byte("x\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			j := enqueue(t, m, "x", nil)
			if err := m.Tick(); err != nil {
				t.Fatal(err)
			}
			if got := get(t, m, j.ID); got.Status != store.StatusQueued || got.StartedAt != nil || got.LaunchToken != "" {
				t.Fatalf("within the window: %s started=%v token=%q, want requeued", got.Status, got.StartedAt, got.LaunchToken)
			}
			if _, err := os.Stat(m.JobDir(j.ID)); err == nil {
				t.Fatal("a requeued job was prepared")
			}
			real := time.Now
			m.Now = func() time.Time { return real().Add(ScopeRetryWindow + time.Minute) }
			if mode == ScopeRequired {
				if err := m.Tick(); err != nil {
					t.Fatal(err)
				}
				if got := get(t, m, j.ID); got.Status != store.StatusFailed || !strings.Contains(got.Error, "required") {
					t.Fatalf("required after the window: %s %q", got.Status, got.Error)
				}
				return
			}
			got := runToEnd(t, m, j.ID)
			if got.Status != store.StatusSucceeded || Containment(got) != "session only" ||
				!strings.Contains(DoneLine(got, 0), "session only") {
				t.Fatalf("auto after the window: %s %q %q", got.Status, Containment(got), DoneLine(got, 0))
			}
		})
	}
	t.Run("never worked", func(t *testing.T) {
		m := newManager(t)
		t.Setenv("AGENT_JOBS_CGROUP_SCOPE", ScopeAuto)
		fakeProbe(t, errors.New("no systemd"))
		got := runToEnd(t, m, enqueue(t, m, "x", nil).ID)
		if got.Status != store.StatusSucceeded || Containment(got) != "session only" {
			t.Fatalf("auto on a host without scopes: %s %q", got.Status, Containment(got))
		}
	})
}

// The installer's pin: cgroup_scope=required only when unset and a scope works; other keys stay.
func TestPinScopeRequired(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfg, []byte(`{"notify_tmux_target":"s:0"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeProbe(t, errors.New("no bus"))
	if msg, err := PinScopeRequired(dir); err != nil || !strings.Contains(msg, "left at auto") {
		t.Fatalf("probe failing: %q %v", msg, err)
	}
	fakeProbe(t, nil)
	if msg, err := PinScopeRequired(dir); err != nil || !strings.Contains(msg, "set to required") {
		t.Fatalf("probe ok: %q %v", msg, err)
	}
	c, err := LoadConfig(dir)
	if err != nil || c.CgroupScope != ScopeRequired || c.NotifyTmuxTarget != "s:0" {
		t.Fatalf("config after pin: %+v %v", c, err)
	}
	if err := os.WriteFile(cfg, []byte(`{"cgroup_scope":"off"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if msg, err := PinScopeRequired(dir); err != nil || !strings.Contains(msg, "left as is") {
		t.Fatalf("explicit setting: %q %v", msg, err)
	}
	if c, _ := LoadConfig(dir); c.CgroupScope != ScopeOff {
		t.Fatalf("pin overwrote an explicit cgroup_scope: %q", c.CgroupScope)
	}
}

// A wrapper moved into a sub-cgroup of its scope still sweeps from the scope itself (L1 of the PR #10
// review).
func TestOwnJobScopeFromSubCgroup(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("cgroups are Linux")
	}
	f := filepath.Join(t.TempDir(), "cgroup")
	old := cgroupFile
	cgroupFile = f
	t.Cleanup(func() { cgroupFile = old })
	for _, c := range []struct{ content, want string }{
		{"0::/u/app.slice/agent-jobs-a-1.scope\n", "/u/app.slice/agent-jobs-a-1.scope"},
		{"0::/u/app.slice/agent-jobs-a-1.scope/sub/deeper\n", "/u/app.slice/agent-jobs-a-1.scope"},
		{"0::/u/app.slice/agent-jobs-a-2.scope/sub\n", ""},
	} {
		if err := os.WriteFile(f, []byte(c.content), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := ownJobScope("agent-jobs-a-1.scope"); got != c.want {
			t.Errorf("ownJobScope for %q = %q, want %q", c.content, got, c.want)
		}
	}
}
