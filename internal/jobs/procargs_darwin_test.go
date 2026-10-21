//go:build darwin

package jobs

import (
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestDarwinSysctlReadsJobMarker(t *testing.T) {
	const id = "261021-000000-test"
	for _, tc := range []struct {
		name string
		env  func(string, []string) ([]string, error)
	}{{"job", JobEnv}, {"acceptance", acceptanceEnv}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), id)
			env, err := tc.env(dir, []string{JobEnvVar + "=inherited"})
			if err != nil {
				t.Fatal(err)
			}
			c := liveChild(t, env...)
			pid := c.Process.Pid
			info, err := sysctlProc(pid)
			if err != nil || info.pgid != pid || info.zombie || envMarker(info.env) != id {
				t.Fatalf("pgid=%d zombie=%v marker=%q error=%v", info.pgid, info.zombie, envMarker(info.env), err)
			}
			if !sysctlMarkedMember(id, pid)(pid) || sysctlMarkedMember("inherited", pid)(pid) {
				t.Fatal("membership did not compare the current job ID")
			}
		})
	}
}

// On SIP-enabled Macs, kern.procargs2 returns argv but omits restricted executables' env.
// An unreadable marker must never be replaced with an assumption of ownership.
func TestDarwinSysctlRestrictedEnvironment(t *testing.T) {
	m := newManager(t)
	m.KillGrace = 0
	const id = "261021-000000-test"
	c := exec.Command("/bin/sleep", "30")
	c.Env = []string{JobEnvVar + "=" + id}
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Process.Kill(); _ = c.Wait() })
	g := c.Process.Pid
	info, err := sysctlProc(g)
	if err != nil {
		t.Fatal(err)
	}
	if envMarker(info.env) == id {
		t.Skip("this Mac permits reading restricted process environments")
	}
	if len(info.argv) != 2 || info.argv[1] != "30" {
		t.Fatalf("restricted process argv %q", info.argv)
	}
	sent := sigRecorder(t)
	for _, end := range []func() (groupEnd, string){
		func() (groupEnd, string) { return m.killAcceptanceGroup(id, g) },
		func() (groupEnd, string) {
			return m.endMarkedGroup(id, "restricted leftover group", g, sysctlMarkedMember(id, g))
		},
	} {
		res, why := end()
		if res != groupRefused || why == "" || len(*sent) != 0 || exited(g) {
			t.Fatalf("result=%v reason=%q signals=%v exited=%v", res, why, *sent, exited(g))
		}
	}
	t.Log("SIP omitted the marker; live restricted group was refused without signalling")
}

func TestDarwinSysctlZombie(t *testing.T) {
	c := exec.Command(testSleep, "0")
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Process.Kill(); _ = c.Wait() })
	pid := c.Process.Pid
	waitFor(t, "unreaped child", 3*time.Second, func() bool {
		kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
		return err == nil && kp.Proc.P_stat == sZomb
	})
	info, err := sysctlProc(pid)
	if err != nil || !info.zombie || info.pgid != pid || !exited(pid) {
		t.Fatalf("pgid=%d zombie=%v error=%v exited=%v", info.pgid, info.zombie, err, exited(pid))
	}
}
