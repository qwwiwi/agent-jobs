//go:build linux

package jobs

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// B6 (#136 R3 review): a process started in the key sandbox, and everything it starts, cannot read a
// file in the key directory by any route: cat, a script it wrote, another interpreter, a copy, a hard
// link, /proc/self/root, or the directory renamed. Everything else stays readable, and the starting
// process itself (the proxy's) is not restricted.
func TestKeySandboxDeniesKeyDirToJobProcesses(t *testing.T) {
	if err := landlockAvailable(); err != nil {
		t.Skip(err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	keys := filepath.Join(root, "keys")
	work := filepath.Join(root, "work")
	for _, d := range []string{keys, work} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	key := filepath.Join(keys, "openrouter.env")
	other := filepath.Join(root, "other.txt")
	if err := os.WriteFile(key, []byte("OPENROUTER_API_KEY="+orFakeKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("readable\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &keySandbox{noRead: []string{keys}}
	run := func(script string) (string, error) {
		t.Helper()
		cmd := exec.Command("/bin/bash", "-c", script)
		cmd.Dir = work
		cmd.Env = append(os.Environ(), "KEY="+key, "KEYS="+keys, "OTHER="+other)
		var out strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := s.start(cmd); err != nil {
			t.Fatalf("start: %v", err)
		}
		err := cmd.Wait()
		return out.String(), err
	}
	if out, err := run(`cat "$OTHER" && ls "$KEYS"`); err != nil || !strings.Contains(out, "readable") || !strings.Contains(out, "openrouter.env") {
		t.Fatalf("outside the key dir: %v %q", err, out)
	}
	probes := map[string]string{
		"cat":            `cat "$KEY"`,
		"own script":     `printf 'cat "%s"\n' "$KEY" > ./probe.sh && chmod +x ./probe.sh && ./probe.sh`,
		"head in a pipe": `head -c 100 < "$KEY"`,
		"cp":             `cp "$KEY" ./copy && cat ./copy`,
		"hard link":      `ln "$KEY" ./link && cat ./link`,
		"proc root":      `cat "/proc/self/root$KEY"`,
		"grandchild":     `bash -c 'bash -c "cat \"$KEY\""'`,
	}
	if _, err := exec.LookPath("python3"); err == nil {
		probes["python"] = `python3 -I -c 'import os; print(open(os.environ["KEY"]).read())'`
	}
	for name, script := range probes {
		out, err := run(script)
		if err == nil || strings.Contains(out, orFakeKey) {
			t.Errorf("%s read the key: err %v, output %q", name, err, out)
		}
	}
	if out, _ := run(`cat "$KEY"`); !strings.Contains(out, "Permission denied") {
		t.Errorf("cat of the key: %q, want EACCES (Permission denied)", out)
	}
	// Renamed in place, the directory keeps no grant.
	if out, err := run(`mv "$KEYS" "$KEYS.moved" && cat "$KEYS.moved/openrouter.env"`); err == nil || strings.Contains(out, orFakeKey) {
		t.Errorf("renamed key dir read: err %v, output %q", err, out)
	}
	_ = os.Rename(keys+".moved", keys)
	// The starting process (the wrapper, its proxy) is not restricted.
	if b, err := os.ReadFile(key); err != nil || !strings.Contains(string(b), orFakeKey) {
		t.Fatalf("the test process lost the key: %v", err)
	}
}

// The sandbox grants the read right beneath every sibling along the path to the key directory, never
// on the path itself or the key directory.
func TestLandlockGrants(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	keys := filepath.Join(root, "a", "keys")
	for _, d := range []string{keys, filepath.Join(root, "a", "sib"), filepath.Join(root, "b")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "a", "f"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(keys, filepath.Join(root, "a", "link")); err != nil {
		t.Fatal(err)
	}
	paths, dirs, err := landlockGrants([]string{keys})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for i, p := range paths {
		got[p] = dirs[i]
		if p == "/" || pathWithin(keys, p) || pathWithin(p, keys) {
			t.Errorf("grant on %s covers the key dir", p)
		}
	}
	for p, dir := range map[string]bool{filepath.Join(root, "a", "sib"): true, filepath.Join(root, "a", "f"): false,
		filepath.Join(root, "b"): true} {
		if d, ok := got[p]; !ok || d != dir {
			t.Errorf("grant %s: present %t dir %t", p, ok, d)
		}
	}
	if _, ok := got[filepath.Join(root, "a", "link")]; ok {
		t.Error("a symlink got a grant")
	}
}

// The key directory must not overlap a directory jobs create and read files in.
func TestKeySandboxPlacement(t *testing.T) {
	busy := []string{"/home/u", "/tmp", "/var/lib/agent-jobs/a"}
	for dir, ok := range map[string]bool{
		"/":                            false,
		"/home/u":                      false,
		"/home/u/.secrets":             false,
		"/home/u/.secrets/openrouter":  false,
		"/home":                        false,
		"/tmp/keys":                    false,
		"/var/lib/agent-jobs":          false,
		"/var/lib/agent-jobs-keys/a":   true,
		"/etc/agent-jobs/openrouter":   true,
		"/home/u2/.secrets/openrouter": true,
	} {
		err := keySandboxPlacement(dir, busy, expectedKeyDir("/home/u/.agent-jobs/thrall"))
		if (err == nil) != ok {
			t.Errorf("%s: %v, want ok %t", dir, err, ok)
		}
		// D1: a refused placement names where the key is expected.
		if err != nil && !strings.Contains(err.Error(), "/var/lib/agent-jobs-keys/thrall/") {
			t.Errorf("%s: error does not name the expected key dir: %v", dir, err)
		}
	}
}

// D2 (#136 fix loop 2): codex's native sandbox (bwrap) cannot start in a Landlock domain (mounts are
// forbidden there). While the key sandbox is on, a codex job on it runs with its runner outside the
// sandbox under codex_policy allow_unprotected (the default), with a warning, and is refused under
// refuse; worktree-full-access codex jobs, claude jobs and a wrapper without the sandbox are unaffected.
func TestCodexKeySandbox(t *testing.T) {
	defer setJobSandbox(nil)
	native := []string{"codex", "exec", "--sandbox", CodexSandboxWrite, "--", "prompt"}
	full := []string{"codex", "exec", "--sandbox", CodexSandboxFullAccess, "--", "prompt"}
	for _, policy := range []string{CodexPolicyAllowUnprotected, CodexPolicyRefuse} {
		s := &keySandbox{noRead: []string{"/nonexistent-keys"}, codexPolicy: policy}
		setJobSandbox(s)
		for _, c := range []struct {
			runner string
			argv   []string
		}{{"codex", full}, {"claude", native}} {
			if fail, warn := codexKeySandbox(c.runner, c.argv); fail != "" || warn != "" || s.runnerOutside {
				t.Fatalf("%s %s %v: fail %q warn %q outside %t", policy, c.runner, c.argv, fail, warn, s.runnerOutside)
			}
		}
		fail, warn := codexKeySandbox("codex", native)
		switch policy {
		case CodexPolicyRefuse:
			if !strings.Contains(fail, "bwrap") || !strings.Contains(fail, "codex_policy is refuse") || warn != "" || s.runnerOutside {
				t.Fatalf("refuse: fail %q warn %q outside %t", fail, warn, s.runnerOutside)
			}
		default:
			if fail != "" || !strings.HasPrefix(warn, "WARNING: openrouter key protection is OFF") || !s.runnerOutside {
				t.Fatalf("allow_unprotected: fail %q warn %q outside %t", fail, warn, s.runnerOutside)
			}
		}
	}
	setJobSandbox(nil)
	if fail, warn := codexKeySandbox("codex", native); fail != "" || warn != "" {
		t.Fatalf("no sandbox: %q %q", fail, warn)
	}
}

// The runner of an allow_unprotected codex job starts outside the key sandbox; every other process
// of the job (startJobCmd), and any runner otherwise, inside it.
func TestStartRunnerCmdOutsideOnlyWhenAllowed(t *testing.T) {
	if err := landlockAvailable(); err != nil {
		t.Skip(err)
	}
	keys := filepath.Join(t.TempDir(), "keys")
	if err := os.Mkdir(keys, 0o700); err != nil {
		t.Fatal(err)
	}
	keys, _ = filepath.EvalSymlinks(keys)
	key := filepath.Join(keys, "k.env")
	if err := os.WriteFile(key, []byte("SECRET\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	defer setJobSandbox(nil)
	read := func(start func(*exec.Cmd) error) bool {
		t.Helper()
		cmd := exec.Command("cat", key)
		var out strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := start(cmd); err != nil {
			t.Fatal(err)
		}
		return cmd.Wait() == nil && strings.Contains(out.String(), "SECRET")
	}
	s := &keySandbox{noRead: []string{keys}}
	setJobSandbox(s)
	if read(startRunnerCmd) || read(startJobCmd) {
		t.Fatal("the key was readable inside the sandbox")
	}
	s.runnerOutside = true
	if !read(startRunnerCmd) {
		t.Fatal("an allow_unprotected runner could not read outside the sandbox")
	}
	if read(startJobCmd) {
		t.Fatal("a job step left the sandbox with the runner")
	}
}

// The enumerated ruleset for the key root layout (a temp stand-in for /, never the real /var/lib):
// every top-level entry but var, every entry of var but lib, every entry of var/lib but
// agent-jobs-keys, every entry of agent-jobs-keys but the agent's key dir; never the key dir, the
// path to it or a symlink.
func TestLandlockGrantsKeyRootLayout(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	keys := filepath.Join(root, "var", "lib", "agent-jobs-keys", "thrall")
	for _, d := range []string{keys, "etc", "home/u", "proc", "dev", "tmp", "run", "sys", "usr/bin", "var/cache", "var/tmp",
		"var/lib/dpkg", "var/lib/agent-jobs-keys/sylvanas"} {
		if !filepath.IsAbs(d) {
			d = filepath.Join(root, d)
		}
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(keys, "openrouter.env"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "var", "lib", "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(keys, filepath.Join(root, "var", "lib", "keys-link")); err != nil {
		t.Fatal(err)
	}
	paths, dirs, err := landlockGrantsFrom(root, []string{keys})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for i, p := range paths {
		rel, _ := filepath.Rel(root, p)
		got[rel] = dirs[i]
	}
	want := map[string]bool{"etc": true, "home": true, "proc": true, "dev": true, "tmp": true, "run": true, "sys": true,
		"usr": true, "var/cache": true, "var/tmp": true, "var/lib/dpkg": true, "var/lib/file": false,
		"var/lib/agent-jobs-keys/sylvanas": true}
	if len(got) != len(want) {
		t.Errorf("grants %v, want %v", got, want)
	}
	for p, dir := range want {
		if d, ok := got[p]; !ok || d != dir {
			t.Errorf("grant %s: present %t dir %t", p, ok, d)
		}
	}
}

// Why codex_policy exists (D2): bwrap starts outside Landlock but not inside the key sandbox, nor
// inside a ruleset that restricts nothing (only the execute right, granted beneath /), while the key
// stays unreadable in the sandbox. Set KEYSANDBOX_TEST_BWRAP to a bwrap that runs on this host (on
// Ubuntu 24.04, one an AppArmor profile allows user namespaces, e.g. codex's bundled one); skipped
// otherwise. If this test fails because bwrap now starts in the sandbox, the codex refusal can go.
func TestBwrapCannotStartInLandlockDomain(t *testing.T) {
	bwrap := os.Getenv("KEYSANDBOX_TEST_BWRAP")
	if bwrap == "" {
		t.Skip("KEYSANDBOX_TEST_BWRAP is not set")
	}
	if err := landlockAvailable(); err != nil {
		t.Skip(err)
	}
	if out, err := exec.Command(bwrap, "--ro-bind", "/", "/", "true").CombinedOutput(); err != nil {
		t.Skipf("bwrap does not start outside Landlock here: %v %s", err, out)
	}
	keys := filepath.Join(t.TempDir(), "keys")
	if err := os.Mkdir(keys, 0o700); err != nil {
		t.Fatal(err)
	}
	keys, _ = filepath.EvalSymlinks(keys)
	key := filepath.Join(keys, "k.env")
	if err := os.WriteFile(key, []byte("SECRET\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(restrict func() error, argv ...string) (string, error) {
		t.Helper()
		cmd := exec.Command(argv[0], argv[1:]...)
		var out strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &out
		errc := make(chan error, 1)
		go func() {
			runtime.LockOSThread() // never unlocked: the restricted thread ends with the goroutine
			if err := restrict(); err != nil {
				errc <- err
				return
			}
			errc <- cmd.Start()
		}()
		if err := <-errc; err != nil {
			t.Fatal(err)
		}
		err := cmd.Wait()
		return strings.TrimSpace(out.String()), err
	}
	s := &keySandbox{noRead: []string{keys}}
	if out, err := run(s.restrictThread, "cat", key); err == nil || !strings.Contains(out, "Permission denied") {
		t.Fatalf("key read in the sandbox: %v %q", err, out)
	}
	for name, restrict := range map[string]func() error{"key sandbox": s.restrictThread, "execute-only ruleset": executeOnlyRuleset} {
		out, err := run(restrict, bwrap, "--ro-bind", "/", "/", "true")
		if err == nil || !strings.Contains(out, "Operation not permitted") {
			t.Fatalf("%s: bwrap started (%v %q): the codex_policy refusal is no longer needed", name, err, out)
		}
		t.Logf("%s: bwrap: %v %q", name, err, out)
	}
}

// executeOnlyRuleset restricts the calling thread with a Landlock ruleset that handles only the
// execute right and grants it beneath /: a domain that restricts no access.
func executeOnlyRuleset() error {
	attr := unix.LandlockRulesetAttr{Access_fs: unix.LANDLOCK_ACCESS_FS_EXECUTE}
	fd, _, e := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr.Access_fs), 0)
	if e != 0 {
		return e
	}
	defer unix.Close(int(fd))
	root, err := unix.Open("/", unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(root)
	rule := unix.LandlockPathBeneathAttr{Allowed_access: unix.LANDLOCK_ACCESS_FS_EXECUTE, Parent_fd: int32(root)}
	if _, _, e := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, fd, unix.LANDLOCK_RULE_PATH_BENEATH, uintptr(unsafe.Pointer(&rule)), 0, 0, 0); e != 0 {
		return e
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return err
	}
	if _, _, e := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, fd, 0, 0); e != 0 {
		return e
	}
	return nil
}
