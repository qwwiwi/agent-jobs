package jobs

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// recordKills replaces killFn with a recorder that delivers nothing.
func recordKills(t *testing.T) *[]int {
	t.Helper()
	var kills []int
	real := killFn
	killFn = func(pid int, _ syscall.Signal) error { kills = append(kills, pid); return nil }
	t.Cleanup(func() { killFn = real })
	return &kills
}

// #55: on Linux an unreadable /proc means the wrapper's identity cannot be shown, so nothing is
// signalled; the legacy (no-pidfd) path is chosen by OS, never by whether /proc happens to be there.
func TestKillGroupWithoutProcRefusesOnLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux only")
	}
	kills := recordKills(t)
	var sent []syscall.Signal
	realSend := pidfdSend
	pidfdSend = func(_ int, sig syscall.Signal) error { sent = append(sent, sig); return nil }
	t.Cleanup(func() { pidfdSend = realSend })

	other := exec.Command("sleep", "30")
	other.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Process.Kill(); _ = other.Wait() })
	g := other.Process.Pid

	m := newManager(t)
	m.KillGrace = 50 * time.Millisecond
	var logBuf bytes.Buffer
	m.Log = log.New(&logBuf, "", 0)
	old := procRoot
	procRoot = filepath.Join(t.TempDir(), "missing")
	t.Cleanup(func() { procRoot = old })

	j := &store.Job{ID: "261020-000000-abcd", LaunchToken: "tok"}
	m.killGroup(j, g, g)
	if len(*kills) != 0 || len(sent) != 0 {
		t.Fatalf("signalled without /proc: kills %v pidfd %v", *kills, sent)
	}
	if !strings.Contains(logBuf.String(), "/proc") {
		t.Fatalf("refusal does not name /proc: %q", logBuf.String())
	}
}

// #55: on Linux a genuine job's group is ended member by member through pidfds; no numeric
// kill(-pgid) is ever sent (the wrapper could exit and the id be reused right before it).
func TestCancelOnLinuxNeverSignalsGroupByNumber(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux only")
	}
	kills := recordKills(t)
	m := newManager(t)
	m.KillGrace = 500 * time.Millisecond
	j := enqueue(t, m, "FAKE_SLEEP=60", nil)
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "fake claude", 5*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(m.JobDir(j.ID), "argv.txt"))
		return err == nil
	})
	j = get(t, m, j.ID)
	if err := m.Cancel(j.ID); err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(*kills, func(p int) bool { return p < 0 }) {
		t.Fatalf("numeric group signal sent: %v", *kills)
	}
	waitFor(t, "group gone", 5*time.Second, func() bool { return !groupAlive(j.PGID) })
}

// fakeGroupPidfd stands in for the kernel calls with per-process liveness: a send to a pid in dead
// fails with ESRCH, any other is recorded as "pid:sig" (signal 0 too) and delivers nothing; onSend
// runs after each recorded send. The kernel's group check (groupExists) sees every process of the fake
// /proc that is not dead, whether or not a listing saw it.
func fakeGroupPidfd(t *testing.T, dead map[int]bool, onSend func(pid int, sig syscall.Signal)) *[]string {
	t.Helper()
	var sent []string
	byFD := map[int]int{}
	oldOpen, oldSend, oldExists := pidfdOpen, pidfdSend, groupExists
	t.Cleanup(func() { pidfdOpen, pidfdSend, groupExists = oldOpen, oldSend, oldExists })
	groupExists = func(g int) bool {
		ents, _ := os.ReadDir(procRoot)
		for _, e := range ents {
			pid, err := strconv.Atoi(e.Name())
			if err != nil || dead[pid] {
				continue
			}
			if st, err := procStatFull(pid); err == nil && st.pgid == g {
				return true
			}
		}
		return false
	}
	pidfdOpen = func(pid int) (int, error) {
		if dead[pid] {
			return -1, unix.ESRCH
		}
		fd, err := unix.Open(os.DevNull, unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err == nil {
			byFD[fd] = pid
		}
		return fd, err
	}
	pidfdSend = func(fd int, sig syscall.Signal) error {
		pid := byFD[fd]
		if dead[pid] {
			return unix.ESRCH
		}
		sent = append(sent, fmt.Sprintf("%d:%d", pid, sig))
		if onSend != nil {
			onSend(pid, sig)
		}
		return nil
	}
	return &sent
}

// #55: endLedGroup proves members through the held leader while it exists; once the leader is gone
// and the id may be reused, a new member without the job's marker is never signalled (not even its
// SIGKILL), while a member proven earlier still gets SIGKILL through the descriptor that held it.
func TestEndLedGroupLeaderGoneNeverSignalsStranger(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux pidfd algorithm with fake /proc")
	}
	root := t.TempDir()
	old := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = old })
	const id = "261020-000000-abcd"
	const g = 50
	writeFakeProc(t, root, g, g, id, "")  // the leader (wrapper)
	writeFakeProc(t, root, 51, g, "", "") // a member without the marker: proven only by the live leader
	dead := map[int]bool{}
	sent := fakeGroupPidfd(t, dead, func(pid int, sig syscall.Signal) {
		if pid == 51 && sig == syscall.SIGTERM {
			// Right after the SIGTERM round the leader is gone, the group empty, and its id reused by
			// a stranger's process.
			dead[g] = true
			if err := os.RemoveAll(filepath.Join(root, "50")); err != nil {
				t.Fatal(err)
			}
			writeFakeProc(t, root, 53, g, "", "")
		}
	})
	leader, err := pidfdOpen(g)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(leader)
	m := newManager(t)
	m.KillGrace = 150 * time.Millisecond
	res, why := m.endLedGroup(id, "process group 50", g, leader, func(pid int) bool { return procMarker(pid) == id })
	if slices.ContainsFunc(*sent, func(s string) bool { return strings.HasPrefix(s, "53:") }) {
		t.Fatalf("the stranger reusing the group id was signalled: %v", *sent)
	}
	for _, want := range []string{"50:15", "51:15", "51:9"} {
		if !slices.Contains(*sent, want) {
			t.Fatalf("sent %v lacks %s", *sent, want)
		}
	}
	if res != groupSurvived || !strings.Contains(why, "member 53 is not proven") {
		t.Fatalf("result %v %q", res, why)
	}
}

// #55: a group whose members exit on SIGTERM is reported ended, and every signal went through a pidfd.
func TestEndLedGroupEndsGroup(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux pidfd algorithm with fake /proc")
	}
	root := t.TempDir()
	old := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = old })
	const g = 60
	writeFakeProc(t, root, g, g, "", "")
	writeFakeProc(t, root, 61, g, "", "")
	dead := map[int]bool{}
	sent := fakeGroupPidfd(t, dead, func(pid int, sig syscall.Signal) {
		if sig == syscall.SIGTERM {
			dead[pid] = true
			_ = os.RemoveAll(filepath.Join(root, fmt.Sprint(pid)))
		}
	})
	leader, err := pidfdOpen(g)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(leader)
	m := newManager(t)
	m.KillGrace = time.Second
	kills := recordKills(t)
	if res, why := m.endLedGroup("x", "process group 60", g, leader, func(int) bool { return false }); res != groupEnded {
		t.Fatalf("result %v %q, sent %v", res, why, *sent)
	}
	if !slices.Contains(*sent, "60:15") || !slices.Contains(*sent, "61:15") || len(*kills) != 0 {
		t.Fatalf("sent %v kills %v", *sent, *kills)
	}
}

// #55: an unreadable /proc mid-way is never taken for an ended group.
func TestEndLedGroupUnreadableProcIsNotEnded(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux pidfd algorithm with fake /proc")
	}
	old := procRoot
	procRoot = filepath.Join(t.TempDir(), "missing")
	t.Cleanup(func() { procRoot = old })
	sent := fakeGroupPidfd(t, map[int]bool{}, nil)
	leader, err := pidfdOpen(70)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(leader)
	m := newManager(t)
	m.KillGrace = 100 * time.Millisecond
	res, why := m.endLedGroup("x", "process group 70", 70, leader, func(int) bool { return true })
	if res == groupEnded || !strings.Contains(why, "members not checked") {
		t.Fatalf("result %v %q", res, why)
	}
	if slices.ContainsFunc(*sent, func(s string) bool { return !strings.HasSuffix(s, ":0") }) {
		t.Fatalf("signalled with /proc unreadable: %v", *sent)
	}
}

// fakeSysctl stands in for the macOS sysctl reads.
func fakeSysctl(t *testing.T, proc func(pid int) (sysctlInfo, error), group func(g int) ([]int, error)) {
	t.Helper()
	oldP, oldG := sysctlProc, sysctlGroup
	t.Cleanup(func() { sysctlProc, sysctlGroup = oldP, oldG })
	sysctlProc, sysctlGroup = proc, group
}

// sigRecorder replaces killFn with one that records "pid:sig" and delivers nothing.
func sigRecorder(t *testing.T) *[]string {
	t.Helper()
	var sent []string
	real := killFn
	killFn = func(pid int, sig syscall.Signal) error {
		sent = append(sent, fmt.Sprintf("%d:%d", pid, sig))
		return nil
	}
	t.Cleanup(func() { killFn = real })
	return &sent
}

// #55 (macOS): the wrapper's identity comes from kern.procargs2 before every group signal. A live
// group led by a process that is not the job's wrapper is never signalled; a proven wrapper's group
// gets SIGTERM and, still proven, SIGKILL; once the wrapper is gone no member is signalled by pid
// (nothing pins it between the check and the kill), but the group still gets SIGKILL while a live
// member carries the job's marker (fix loop 2: a marked child that ignores SIGTERM must not survive),
// and none when no member does; the ones left are reported.
func TestKillGroupBySysctl(t *testing.T) {
	leader := exec.Command("sleep", "30")
	leader.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := leader.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = leader.Process.Kill(); _ = leader.Wait() })
	g := leader.Process.Pid

	m := newManager(t)
	m.KillGrace = 50 * time.Millisecond
	j := &store.Job{ID: "261020-000000-abcd", LaunchToken: "tok"}
	wrapper := sysctlInfo{pgid: g, argv: []string{"/usr/local/bin/agent-jobs", "run-wrapper", m.JobDir(j.ID), "--token", "tok", "--", "claude"}}
	marked := sysctlInfo{pgid: g, argv: []string{"claude"}, env: []string{JobEnvVar + "=" + j.ID}}
	unmarked := sysctlInfo{pgid: g, argv: []string{"sleep"}}
	members := func(int) ([]int, error) { return []int{g, 777, 778}, nil }

	for _, tc := range []struct {
		name        string
		proc        func(calls int, pid int) (sysctlInfo, error)
		want        []string
		wantRefused bool
		wantRes     groupEnd
	}{
		{"not the wrapper", func(_, pid int) (sysctlInfo, error) { return unmarked, nil }, nil, true, groupRefused},
		{"other token", func(_, pid int) (sysctlInfo, error) {
			w := wrapper
			w.argv = slices.Clone(w.argv)
			w.argv[4] = "other"
			return w, nil
		}, nil, true, groupRefused},
		{"zombie wrapper", func(_, pid int) (sysctlInfo, error) { w := wrapper; w.zombie = true; return w, nil }, nil, true, groupRefused},
		{"unreadable", func(_, pid int) (sysctlInfo, error) { return sysctlInfo{}, errNoSysctlIdentity }, nil, true, groupRefused},
		{"wrapper stays", func(_, pid int) (sysctlInfo, error) { return wrapper, nil }, []string{fmt.Sprintf("-%d:15", g), fmt.Sprintf("-%d:9", g)}, false, groupSurvived},
		{"wrapper gone, members marked", func(calls, pid int) (sysctlInfo, error) {
			switch {
			case pid == g && calls == 1:
				return wrapper, nil
			case pid == g:
				return sysctlInfo{}, errNoSysctlIdentity
			case pid == 777:
				return marked, nil
			}
			return sysctlInfo{}, errNoSysctlIdentity // 778 gone
		}, []string{fmt.Sprintf("-%d:15", g), fmt.Sprintf("-%d:9", g)}, false, groupSurvived},
		{"wrapper gone, a member unmarked", func(calls, pid int) (sysctlInfo, error) {
			switch {
			case pid == g && calls == 1:
				return wrapper, nil
			case pid == g:
				return sysctlInfo{}, errNoSysctlIdentity
			case pid == 777:
				return marked, nil
			}
			return unmarked, nil
		}, []string{fmt.Sprintf("-%d:15", g), fmt.Sprintf("-%d:9", g)}, false, groupSurvived},
		{"wrapper gone, no member marked", func(calls, pid int) (sysctlInfo, error) {
			switch {
			case pid == g && calls == 1:
				return wrapper, nil
			case pid == g:
				return sysctlInfo{}, errNoSysctlIdentity
			}
			return unmarked, nil
		}, []string{fmt.Sprintf("-%d:15", g)}, false, groupSurvived},
		{"wrapper gone, marked member is a zombie or elsewhere", func(calls, pid int) (sysctlInfo, error) {
			switch {
			case pid == g && calls == 1:
				return wrapper, nil
			case pid == g:
				return sysctlInfo{}, errNoSysctlIdentity
			case pid == 777:
				z := marked
				z.zombie = true
				return z, nil
			}
			moved := marked
			moved.pgid = g + 1
			return moved, nil
		}, []string{fmt.Sprintf("-%d:15", g)}, false, groupSurvived},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := map[int]int{}
			fakeSysctl(t, func(pid int) (sysctlInfo, error) { calls[pid]++; return tc.proc(calls[pid], pid) }, members)
			sent := sigRecorder(t)
			var logBuf bytes.Buffer
			m.Log = log.New(&logBuf, "", 0)
			res, why := m.killGroupBySysctl(j, g, g)
			// SIGKILL is resent each round while the wrapper stays proven (the fake group never ends).
			if got := slices.Compact(slices.Clone(*sent)); !slices.Equal(got, tc.want) {
				t.Fatalf("sent %v, want %v (log %q)", *sent, tc.want, logBuf.String())
			}
			if tc.wantRefused != strings.Contains(logBuf.String(), "refusing to kill process group") {
				t.Fatalf("refusal logged %v, want %v: %q", !tc.wantRefused, tc.wantRefused, logBuf.String())
			}
			if res != tc.wantRes {
				t.Fatalf("result %v %q, want %v", res, why, tc.wantRes)
			}
			if res == groupSurvived && !strings.Contains(logBuf.String(), "members survived: [") {
				t.Fatalf("survivors not reported: %q", logBuf.String())
			}
		})
	}
	// A scoped job cannot be checked without cgroups: refused.
	fakeSysctl(t, func(int) (sysctlInfo, error) { return wrapper, nil }, members)
	sent := sigRecorder(t)
	m.killGroupBySysctl(&store.Job{ID: j.ID, LaunchToken: "tok", ScopeUnit: "agent-jobs-x.scope"}, g, g)
	if len(*sent) != 0 {
		t.Fatalf("scoped job signalled without a scope check: %v", *sent)
	}
}

// #55: off Linux the path is chosen by OS; an OS with no sysctl identity check signals nothing.
func TestKillGroupWithoutIdentityCheckRefuses(t *testing.T) {
	fakeSysctl(t, func(int) (sysctlInfo, error) { return sysctlInfo{}, errNoSysctlIdentity },
		func(int) ([]int, error) { return nil, errNoSysctlIdentity })
	sent := sigRecorder(t)
	leader := exec.Command("sleep", "30")
	leader.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := leader.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = leader.Process.Kill(); _ = leader.Wait() })
	m := newManager(t)
	m.killGroupBySysctl(&store.Job{ID: "x", LaunchToken: "tok"}, leader.Process.Pid, leader.Process.Pid)
	if len(*sent) != 0 {
		t.Fatalf("signalled without an identity check: %v", *sent)
	}
}

// procargs2 parsing: argc, the executable path and its padding, argv, then env up to the empty string.
func TestParseProcArgs2(t *testing.T) {
	var b []byte
	b = binary.LittleEndian.AppendUint32(b, 3)
	b = append(b, "/usr/local/bin/agent-jobs\x00\x00\x00\x00"...)
	b = append(b, "agent-jobs\x00run-wrapper\x00/j\x00"...)
	b = append(b, "PATH=/bin\x00"+JobEnvVar+"=id-1\x00\x00executable_path=/x\x00"...)
	argv, env, err := parseProcArgs2(b)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(argv, []string{"agent-jobs", "run-wrapper", "/j"}) || !slices.Equal(env, []string{"PATH=/bin", JobEnvVar + "=id-1"}) {
		t.Fatalf("argv %q env %q", argv, env)
	}
	if envMarker(env) != "id-1" {
		t.Fatalf("marker %q", envMarker(env))
	}
	for name, bad := range map[string][]byte{
		"short":        {1, 0},
		"no exec path": binary.LittleEndian.AppendUint32(nil, 1),
		"argc too big": append(binary.LittleEndian.AppendUint32(nil, 99), "/x\x00a\x00"...),
		"truncated":    append(binary.LittleEndian.AppendUint32(nil, 2), "/x\x00a\x00b"...),
	} {
		if _, _, err := parseProcArgs2(bad); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}

// #55 review: a member forked during the grace still gets SIGTERM, one forked after the SIGKILL round
// still gets SIGKILL (a numeric kill(-pgid) reached both atomically; the per-member rounds must too).
func TestEndLedGroupSignalsLateMembers(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux pidfd algorithm with fake /proc")
	}
	root := t.TempDir()
	old := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = old })
	const g = 80
	writeFakeProc(t, root, g, g, "", "")
	writeFakeProc(t, root, 81, g, "", "")
	sent := fakeGroupPidfd(t, map[int]bool{}, func(pid int, sig syscall.Signal) {
		switch {
		case pid == 81 && sig == syscall.SIGTERM:
			writeFakeProc(t, root, 82, g, "", "") // forked during the grace
		case pid == 81 && sig == syscall.SIGKILL:
			writeFakeProc(t, root, 83, g, "", "") // forked right after the SIGKILL round
		}
	})
	leader, err := pidfdOpen(g)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(leader)
	m := newManager(t)
	m.KillGrace = 300 * time.Millisecond
	if res, _ := m.endLedGroup("x", "process group 80", g, leader, func(int) bool { return false }); res != groupSurvived {
		t.Fatalf("result %v", res)
	}
	for _, want := range []string{"80:15", "81:15", "82:15", "80:9", "81:9", "82:9", "83:9"} {
		if !slices.Contains(*sent, want) {
			t.Fatalf("sent %v lacks %s", *sent, want)
		}
	}
	// Each held process gets each signal once, not once per round.
	if n := len(slices.DeleteFunc(slices.Clone(*sent), func(s string) bool { return s != "81:15" })); n != 1 {
		t.Fatalf("81 got SIGTERM %d times: %v", n, *sent)
	}
}

// #55 fix loop: a round enumerates the group before it checks the held processes, so the last live
// member can fork a child after the enumeration and exit before its check. The round that retires it
// must scan the group afresh rather than report it ended while the child lives.
func TestEndLedGroupRescansAfterLastHeldExits(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux pidfd algorithm with fake /proc")
	}
	root := t.TempDir()
	old := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = old })
	const id = "261020-000000-abcd"
	const g = 90
	writeFakeProc(t, root, g, g, id, "")
	writeFakeProc(t, root, 91, g, id, "")
	dead := map[int]bool{}
	sent := fakeGroupPidfd(t, dead, func(pid int, sig syscall.Signal) {
		if pid == g && sig == syscall.SIGTERM { // the leader exits on SIGTERM
			dead[g] = true
			_ = os.RemoveAll(filepath.Join(root, fmt.Sprint(g)))
		}
	})
	// Between the enumeration and the check of 91 in a round after the first: 91 forks 92 and exits.
	byFD := map[int]int{}
	open, send := pidfdOpen, pidfdSend
	pidfdOpen = func(pid int) (int, error) {
		fd, err := open(pid)
		if err == nil {
			byFD[fd] = pid
		}
		return fd, err
	}
	pidfdSend = func(fd int, sig syscall.Signal) error {
		if byFD[fd] == 91 && sig == 0 && slices.Contains(*sent, "91:15") && !dead[91] {
			writeFakeProc(t, root, 92, g, id, "")
			dead[91] = true
			_ = os.RemoveAll(filepath.Join(root, "91"))
		}
		return send(fd, sig)
	}
	leader, err := pidfdOpen(g)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(leader)
	m := newManager(t)
	m.KillGrace = 200 * time.Millisecond
	res, why := m.endLedGroup(id, "process group 90", g, leader, func(pid int) bool { return procMarker(pid) == id })
	if res == groupEnded {
		t.Fatalf("reported ended while child 92 lives (sent %v)", *sent)
	}
	for _, want := range []string{"92:15", "92:9"} {
		if !slices.Contains(*sent, want) {
			t.Fatalf("sent %v lacks %s (%v %q)", *sent, want, res, why)
		}
	}
}

// #55 fix loop 2: two generations hand off between the listings. Held 91 forks 92 and retires; the
// rescan lists 92, which forks 93 and exits before it is opened, so no listing of that scan shows 93
// and no held process retired in it. The group must not be reported ended while the kernel still has
// a process in it: 93 is found by a later listing and signalled.
func TestEndLedGroupTwoGenerationHandoff(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux pidfd algorithm with fake /proc")
	}
	root := t.TempDir()
	old := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = old })
	const id = "261020-000000-abcd"
	const g = 95
	writeFakeProc(t, root, g, g, id, "")
	writeFakeProc(t, root, 91, g, id, "")
	dead := map[int]bool{}
	sent := fakeGroupPidfd(t, dead, func(pid int, sig syscall.Signal) {
		if pid == g && sig == syscall.SIGTERM { // the leader exits on SIGTERM
			dead[g] = true
			_ = os.RemoveAll(filepath.Join(root, fmt.Sprint(g)))
		}
	})
	exitProc := func(pid int) {
		dead[pid] = true
		_ = os.RemoveAll(filepath.Join(root, fmt.Sprint(pid)))
	}
	byFD := map[int]int{}
	open, send := pidfdOpen, pidfdSend
	pidfdOpen = func(pid int) (int, error) {
		if pid == 92 && !dead[92] {
			// Listed, then forks 93 and exits before it is opened.
			writeFakeProc(t, root, 93, g, id, "")
			exitProc(92)
		}
		fd, err := open(pid)
		if err == nil {
			byFD[fd] = pid
		}
		return fd, err
	}
	pidfdSend = func(fd int, sig syscall.Signal) error {
		// After its SIGTERM, between a listing and its check: 91 forks 92 and exits.
		if byFD[fd] == 91 && sig == 0 && slices.Contains(*sent, "91:15") && !dead[91] {
			writeFakeProc(t, root, 92, g, id, "")
			exitProc(91)
		}
		return send(fd, sig)
	}
	leader, err := pidfdOpen(g)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(leader)
	m := newManager(t)
	m.KillGrace = 200 * time.Millisecond
	res, why := m.endLedGroup(id, "process group 95", g, leader, func(pid int) bool { return procMarker(pid) == id })
	if res == groupEnded {
		t.Fatalf("reported ended while grandchild 93 lives (sent %v)", *sent)
	}
	for _, want := range []string{"93:15", "93:9"} {
		if !slices.Contains(*sent, want) {
			t.Fatalf("sent %v lacks %s (%v %q)", *sent, want, res, why)
		}
	}
}

// #55 fix loop 2: members that survive the kill are not dropped silently. The cancelled row names
// them with ReasonSurvivors, and the group recorded at the spawn stays recorded while it lives (its
// cleanup, prune and resume are held up).
func TestCancelRecordsSurvivingGroup(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux only")
	}
	m := newManager(t)
	m.KillGrace = 100 * time.Millisecond
	j := enqueue(t, m, "FAKE_SLEEP=60", nil)
	if err := m.Tick(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "fake claude", 5*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(m.JobDir(j.ID), "argv.txt"))
		return err == nil
	})
	j = get(t, m, j.ID)
	g := j.PGID
	t.Cleanup(func() {
		_ = syscall.Kill(-g, syscall.SIGKILL)
		waitFor(t, "group gone", 5*time.Second, func() bool { return !groupAlive(g) })
	})
	// The signals are not delivered: every member survives.
	realSend := pidfdSend
	pidfdSend = func(fd int, sig syscall.Signal) error {
		if sig == 0 {
			return realSend(fd, 0)
		}
		return nil
	}
	t.Cleanup(func() { pidfdSend = realSend })
	if err := m.Cancel(j.ID); err != nil {
		t.Fatal(err)
	}
	got := get(t, m, j.ID)
	if got.Status != store.StatusCancelled || !strings.Contains(got.Error, ReasonSurvivors) ||
		!strings.Contains(got.Error, fmt.Sprintf("process group %d survived", g)) {
		t.Fatalf("status %s error %q", got.Status, got.Error)
	}
	if !m.spawnedGroupAlive(got) || !got.PGIDSpawned {
		t.Fatal("the surviving group is no longer recorded")
	}
}
