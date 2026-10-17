//go:build linux

package agentjobs

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSystemctl logs every call and keeps is-active state in a file, so the install script can be
// exercised without touching the real user manager.
const fakeSystemctl = `#!/bin/sh
echo "$*" >> "$FAKE_DIR/systemctl.log"
case "$*" in
  *is-active*) [ -f "$FAKE_DIR/active" ] && exit 0; exit 3 ;;
  *restart*) touch "$FAKE_DIR/active" ;;
  *disable*) rm -f "$FAKE_DIR/active" ;;
esac
exit 0
`

// fakeBinary stands in for agent-jobs: the script only runs `init --agent X` with it.
const fakeBinary = `#!/bin/sh
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
	if got := e.read(filepath.Join(e.fake, "bin.log")); got != "init --agent t1\n" {
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

	// Last instance gone: template and binary are removed, env file and state are kept.
	if err := os.Remove(other); err != nil {
		t.Fatal(err)
	}
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
