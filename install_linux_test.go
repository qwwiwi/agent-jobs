//go:build linux

package agentjobs

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeSystemctl logs every call and keeps is-active state in a file, so the install script can be
// exercised without touching the real user manager.
const fakeSystemctl = `#!/bin/sh
echo "$*" >> "$FAKE_DIR/systemctl.log"
case "$*" in
  *is-active*) [ -f "$FAKE_DIR/active" ] && exit 0; exit 3 ;;
  *list-units*) [ -f "$FAKE_DIR/units" ] && cat "$FAKE_DIR/units"; exit 0 ;;
  *restart*) touch "$FAKE_DIR/active" ;;
  *disable*) rm -f "$FAKE_DIR/active" ;;
esac
exit 0
`

// fakeBinary stands in for agent-jobs: the script only runs `init --agent X` with it; the tests run
// `run-wrapper` to fake a running job.
const fakeBinary = `#!/bin/sh
if [ "$1" = run-wrapper ]; then sleep 60; exit 0; fi
echo "$*" >> "$FAKE_DIR/bin.log"
echo "state: fake"
`

type installEnv struct {
	t    *testing.T
	home string
	fake string
	bin  string
}

func newInstallEnv(t *testing.T) *installEnv {
	t.Helper()
	root := t.TempDir()
	e := &installEnv{t: t, home: filepath.Join(root, "home"), fake: filepath.Join(root, "fake")}
	for _, d := range []string{e.home, e.fake} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	e.write(filepath.Join(e.fake, "systemctl"), fakeSystemctl, 0o755)
	e.bin = filepath.Join(e.fake, "agent-jobs")
	e.write(e.bin, fakeBinary, 0o755)
	return e
}

func (e *installEnv) write(p, s string, mode os.FileMode) {
	e.t.Helper()
	if err := os.WriteFile(p, []byte(s), mode); err != nil {
		e.t.Fatal(err)
	}
}

func (e *installEnv) read(p string) string {
	e.t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		e.t.Fatal(err)
	}
	return string(b)
}

// run runs scripts/install-linux.sh with a sandboxed HOME and the fake systemctl.
func (e *installEnv) run(args ...string) (string, error) {
	e.t.Helper()
	cmd := exec.Command("bash", append([]string{"scripts/install-linux.sh"}, args...)...)
	cmd.Env = []string{
		"HOME=" + e.home,
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"FAKE_DIR=" + e.fake,
		"AGENT_JOBS_SYSTEMCTL=" + filepath.Join(e.fake, "systemctl"),
		"XDG_RUNTIME_DIR=" + e.fake,
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (e *installEnv) calls() string {
	b, err := os.ReadFile(filepath.Join(e.fake, "systemctl.log"))
	if err != nil && !os.IsNotExist(err) {
		e.t.Fatal(err)
	}
	return string(b)
}

func (e *installEnv) resetCalls() {
	_ = os.Remove(filepath.Join(e.fake, "systemctl.log"))
}

func TestUnitTemplate(t *testing.T) {
	b, err := os.ReadFile("deploy/systemd/agent-jobs@.service")
	if err != nil {
		t.Fatal(err)
	}
	unit := string(b)
	for _, want := range []string{
		"\nExecStart=%h/.local/bin/agent-jobs daemon --agent %i\n",
		"\nKillMode=process\n", // a restart must never kill the jobs' process groups
		"\nRestart=on-failure\n",
		"\nEnvironmentFile=-%E/agent-jobs/%i.env\n",
		"\nWantedBy=default.target\n",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit template lacks %q", strings.TrimSpace(want))
		}
	}
	// Exactly one KillMode directive (comments aside), so a later line cannot override it.
	n := 0
	for _, l := range strings.Split(unit, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "KillMode=") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("unit template has %d KillMode directives, want exactly 1 (process)", n)
	}

	// Let systemd itself parse the unit when it is available: sandboxed HOME (%h) with a fake binary.
	analyze, err := exec.LookPath("systemd-analyze")
	if err != nil {
		t.Skip("systemd-analyze not found; static checks only")
	}
	runtime := os.Getenv("XDG_RUNTIME_DIR")
	if runtime == "" {
		runtime = fmt.Sprintf("/run/user/%d", os.Getuid())
	}
	if _, err := os.Stat(filepath.Join(runtime, "systemd")); err != nil {
		t.Skipf("no systemd user manager runtime dir (%s); static checks only", runtime)
	}
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".local/bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".local/bin/agent-jobs"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	unitCopy := filepath.Join(home, "agent-jobs@.service")
	if err := os.WriteFile(unitCopy, b, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(analyze, "--user", "verify", unitCopy)
	cmd.Env = []string{"HOME=" + home, "XDG_RUNTIME_DIR=" + runtime, "PATH=/usr/local/bin:/usr/bin:/bin"}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("systemd-analyze --user verify: %v\n%s", err, out)
	}
}

func TestInstallLinuxIdempotentAndUninstall(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found")
	}
	e := newInstallEnv(t)
	unitDst := filepath.Join(e.home, ".config/systemd/user/agent-jobs@.service")
	binDst := filepath.Join(e.home, ".local/bin/agent-jobs")
	envFile := filepath.Join(e.home, ".config/agent-jobs/t1.env")

	// First install: everything is written, the unit is enabled and started.
	out, err := e.run("--instance", "t1", "--binary", e.bin)
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if e.read(unitDst) != e.read("deploy/systemd/agent-jobs@.service") {
		t.Error("installed unit differs from the template")
	}
	if fi, err := os.Stat(binDst); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("binary not installed executable: %v %v", fi, err)
	}
	if fi, err := os.Stat(envFile); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("env file not created 0600: %v %v", fi, err)
	}
	if got := e.read(filepath.Join(e.fake, "bin.log")); got != "init --agent t1 --pin-scope\n" {
		t.Errorf("init call = %q", got)
	}
	calls := e.calls()
	for _, want := range []string{"--user daemon-reload\n", "--user enable agent-jobs@t1.service\n", "--user restart agent-jobs@t1.service\n"} {
		if !strings.Contains(calls, want) {
			t.Errorf("systemctl calls lack %q:\n%s", want, calls)
		}
	}
	matches, _ := filepath.Glob(filepath.Join(e.home, ".local/bin/.agent-jobs.new.*"))
	if len(matches) != 0 {
		t.Errorf("temp binary left behind: %v", matches)
	}

	// Second install, nothing changed, daemon active: no restart, the edited env file survives.
	e.write(envFile, "AGENT_JOBS_TMUX_BIN=/x/tmux\n", 0o600)
	e.resetCalls()
	out, err = e.run("--instance", "t1", "--binary", e.bin)
	if err != nil {
		t.Fatalf("re-install: %v\n%s", err, out)
	}
	if strings.Contains(e.calls(), "restart") {
		t.Errorf("unchanged re-install restarted the daemon:\n%s", e.calls())
	}
	if e.read(envFile) != "AGENT_JOBS_TMUX_BIN=/x/tmux\n" {
		t.Error("re-install overwrote the env file")
	}

	// A changed binary restarts the daemon.
	e.write(e.bin, fakeBinary+"# v2\n", 0o755)
	e.resetCalls()
	if out, err = e.run("--instance", "t1", "--binary", e.bin); err != nil {
		t.Fatalf("upgrade: %v\n%s", err, out)
	}
	if !strings.Contains(e.calls(), "--user restart agent-jobs@t1.service") {
		t.Errorf("changed binary did not restart:\n%s", e.calls())
	}

	// Uninstall while another instance is enabled: shared template and binary stay.
	wants := filepath.Join(e.home, ".config/systemd/user/default.target.wants")
	if err := os.MkdirAll(wants, 0o755); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(wants, "agent-jobs@t2.service")
	if err := os.Symlink(unitDst, other); err != nil {
		t.Fatal(err)
	}
	e.resetCalls()
	if out, err = e.run("--instance", "t1", "--uninstall"); err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	if !strings.Contains(e.calls(), "--user disable --now agent-jobs@t1.service") {
		t.Errorf("uninstall did not disable --now:\n%s", e.calls())
	}
	for _, p := range []string{unitDst, binDst, envFile} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s removed while another instance is enabled: %v", p, err)
		}
	}
	e.assertOtherUntouched("agent-jobs@t2")

	// Another instance active but not enabled (list-units only): shared files stay too.
	if err := os.Remove(other); err != nil {
		t.Fatal(err)
	}
	e.write(filepath.Join(e.fake, "units"), "agent-jobs@t1.service loaded deactivating stop x\nagent-jobs@t3.service loaded active running agent-jobs daemon (agent t3)\n", 0o644)
	e.resetCalls()
	if out, err = e.run("--instance", "t1", "--uninstall"); err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	for _, p := range []string{unitDst, binDst} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s removed while another instance is active: %v", p, err)
		}
	}
	if !strings.Contains(out, "agent-jobs@t3.service") {
		t.Errorf("uninstall did not name the active instance:\n%s", out)
	}
	e.assertOtherUntouched("agent-jobs@t3")
	if err := os.Remove(filepath.Join(e.fake, "units")); err != nil {
		t.Fatal(err)
	}

	// A job still running from this binary: the binary is its guard hook and must stay.
	wrapper := exec.Command(binDst, "run-wrapper", "/fake/jobdir", "--token", "x")
	wrapper.Env = []string{"PATH=/usr/bin:/bin"}
	wrapper.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // own group: the kill takes its sleep too
	if err := wrapper.Start(); err != nil {
		t.Fatal(err)
	}
	stopWrapper := func() { _ = syscall.Kill(-wrapper.Process.Pid, syscall.SIGKILL); _ = wrapper.Wait() }
	t.Cleanup(stopWrapper)
	waitForCmdline(t, wrapper.Process.Pid, "run-wrapper")
	out, err = e.run("--instance", "t1", "--uninstall")
	if err == nil {
		t.Errorf("uninstall with a running job succeeded:\n%s", out)
	}
	if !strings.Contains(out, "1 jobs still running") {
		t.Errorf("uninstall did not report the running job:\n%s", out)
	}
	if _, err := os.Stat(binDst); err != nil {
		t.Errorf("binary removed while a job runs: %v", err)
	}
	stopWrapper()

	// Last instance gone, no job running: template and binary are removed, env file and state are kept.
	if out, err = e.run("--instance", "t1", "--uninstall"); err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	for _, p := range []string{unitDst, binDst} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s still present after the last uninstall", p)
		}
	}
	if _, err := os.Stat(envFile); err != nil {
		t.Errorf("env file removed by uninstall: %v", err)
	}
}

// assertOtherUntouched fails if any systemctl call named the other instance.
func (e *installEnv) assertOtherUntouched(unit string) {
	e.t.Helper()
	if c := e.calls(); strings.Contains(c, unit) {
		e.t.Errorf("systemctl call touched %s:\n%s", unit, c)
	}
}

// waitForCmdline waits until /proc/<pid>/cmdline contains want (the fake wrapper has exec'd).
func waitForCmdline(t *testing.T, pid int, want string) {
	t.Helper()
	for range 100 {
		b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if strings.Contains(string(b), want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pid %d never showed %q", pid, want)
}

func TestInstallLinuxEnvFileParsedNotSourced(t *testing.T) {
	e := newInstallEnv(t)
	envFile := filepath.Join(e.home, ".config/agent-jobs/t1.env")
	if err := os.MkdirAll(filepath.Dir(envFile), 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(e.fake, "sourced")
	e.write(envFile, "# comment\ntouch "+marker+"\nAGENT_JOBS_HOME=\""+filepath.Join(e.home, "state")+"\"\n$(touch "+marker+")\n", 0o600)
	// The fake binary records AGENT_JOBS_HOME as seen by init.
	e.write(e.bin, "#!/bin/sh\necho \"$* home=$AGENT_JOBS_HOME\" >> \"$FAKE_DIR/bin.log\"\n", 0o755)
	if out, err := e.run("--instance", "t1", "--binary", e.bin); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("env file was executed as shell")
	}
	want := "init --agent t1 --pin-scope home=" + filepath.Join(e.home, "state") + "\n"
	if got := e.read(filepath.Join(e.fake, "bin.log")); got != want {
		t.Errorf("init saw %q, want %q", got, want)
	}
}

func TestInstallLinuxRejectsBadInstance(t *testing.T) {
	e := newInstallEnv(t)
	for _, inst := range []string{"", "Thrall", "../x", "a b", strings.Repeat("a", 33)} {
		out, err := e.run("--instance", inst, "--binary", e.bin)
		if err == nil {
			t.Errorf("instance %q accepted:\n%s", inst, out)
		}
	}
	if c := e.calls(); c != "" {
		t.Errorf("systemctl called for a rejected instance:\n%s", c)
	}
}
