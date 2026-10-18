package agentjobs_test

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The tests here drive the acceptance script against a tiny fake repository (the script itself, a
// fake agent-jobs binary, a recording scripts/check.sh and an installer) so that isolation, cleanup
// and the required checks are exercised in seconds, without the daemon's minute ticks.

// fakeMain is the fake agent-jobs binary; %q is the record dir. It logs every call, answers what the
// script asks, and behaves as the record dir's flag files say: hang-status (status blocks, with a
// child), stubborn (the daemon ignores TERM).
const fakeMain = `package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

var version = "dev"

const rec = %q

func note(name, text string) { _ = os.WriteFile(filepath.Join(rec, name), []byte(text), 0o644) }

func flag(name string) bool { _, err := os.Stat(filepath.Join(rec, name)); return err == nil }

func main() {
	args := os.Args[1:]
	if f, err := os.OpenFile(filepath.Join(rec, "calls.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		fmt.Fprintln(f, strings.Join(args, " "))
		f.Close()
	}
	if len(args) == 0 {
		os.Exit(2)
	}
	agent := ""
	for i, a := range args {
		if a == "--agent" && i+1 < len(args) {
			agent = args[i+1]
		}
	}
	st := filepath.Join(os.Getenv("AGENT_JOBS_HOME"), agent)
	switch args[0] {
	case "version":
		fmt.Println("agent-jobs", version)
	case "init":
		_ = os.MkdirAll(st, 0o700)
		_ = os.WriteFile(filepath.Join(st, "owner-rules.md"), []byte("rules\n"), 0o600)
		_ = os.WriteFile(filepath.Join(st, "orchestrator.md"), []byte("orch\n"), 0o600)
	case "http-api":
		_ = os.WriteFile(filepath.Join(st, "http-api.token"), []byte("tok\n"), 0o600)
	case "daemon":
		daemon(st)
	case "status":
		if flag("hang-status") {
			c := exec.Command("sleep", "300")
			_ = c.Start()
			note("status.pid", strconv.Itoa(os.Getpid()))
			note("status-child.pid", strconv.Itoa(c.Process.Pid))
			note("status.started", "1")
			select {}
		}
		fmt.Println("http api:  on")
	case "doctor", "cancel":
	case "list":
		fmt.Println("ID  STATUS")
	case "start":
		fmt.Fprintln(os.Stderr, "fake: no jobs here")
		os.Exit(1)
	default:
		os.Exit(2)
	}
}

func daemon(st string) {
	note("daemon.env", "HOME="+os.Getenv("HOME")+"\nCLAUDE_CONFIG_DIR="+os.Getenv("CLAUDE_CONFIG_DIR")+"\n")
	if flag("stubborn") {
		ch := make(chan os.Signal, 8)
		signal.Notify(ch, syscall.SIGTERM)
		go func() {
			for range ch {
				note("daemon.term", "1")
			}
		}()
	}
	l, err := net.Listen("unix", filepath.Join(st, "http-api.sock"))
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	note("daemon.pid", strconv.Itoa(os.Getpid()))
	fmt.Println("daemon started")
	_ = http.Serve(l, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "{}") }))
}
`

// fakeCheck is the fake repository's scripts/check.sh: it records the environment the clone's code
// runs with and, with the flag hang-check, blocks with a child.
const fakeCheck = `#!/bin/sh
env > %[1]q/check.env
if [ -f %[1]q/hang-check ]; then
  sleep 300 &
  echo $! > %[1]q/check-child.pid
  echo $$ > %[1]q/check.pid
  wait
fi
exit 0
`

// brokenInstaller installs a binary other than the one it is given (and everything else the
// acceptance checks for).
const brokenInstaller = `#!/bin/sh
set -e
while [ $# -gt 0 ]; do
  case "$1" in
    --instance) inst="$2"; shift 2 ;;
    --binary) bin="$2"; shift 2 ;;
    --skills-dir) skills="$2"; shift 2 ;;
    *) shift ;;
  esac
done
skills="${skills:-${CLAUDE_CONFIG_DIR:-$HOME/.claude}/skills}"
mkdir -p "$HOME/.local/bin" "$HOME/.config/systemd/user" "$skills/agent-jobs"
cp "$bin" "$HOME/.local/bin/agent-jobs"
printf '\n# not the build\n' >> "$HOME/.local/bin/agent-jobs"
chmod 0755 "$HOME/.local/bin/agent-jobs"
: > "$HOME/.config/systemd/user/agent-jobs@.service"
: > "$skills/agent-jobs/SKILL.md"
"$bin" init --agent "$inst"
"$AGENT_JOBS_SYSTEMCTL" --user enable "agent-jobs@$inst.service"
`

// fakeRepo is a committed fake repository and its record dir.
type fakeRepo struct {
	dir, rec, script string
}

// cleanGitEnv is this process's environment without any GIT_* variable.
func cleanGitEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	return env
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.invalid"}, args...)...)
	cmd.Env = cleanGitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func copyFile(t *testing.T, src, dst string, mode os.FileMode) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, dst, string(b), mode)
}

// newFakeRepo builds the fake repository: installer "" is this repository's install-linux.sh (with
// its unit and skill), else that script; flags are record-dir flag files (hang-status, ...).
func newFakeRepo(t *testing.T, installer string, flags ...string) fakeRepo {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("Linux only (the installer and /proc)")
	}
	base := t.TempDir()
	r := fakeRepo{dir: filepath.Join(base, "repo"), rec: filepath.Join(base, "rec")}
	r.script = filepath.Join(r.dir, acceptanceScript)
	if err := os.MkdirAll(r.rec, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range flags {
		writeFile(t, filepath.Join(r.rec, f), "1\n", 0o644)
	}
	copyFile(t, acceptanceScript, r.script, 0o755)
	writeFile(t, filepath.Join(r.dir, "go.mod"), "module fakeagentjobs\n\ngo 1.21\n", 0o644)
	writeFile(t, filepath.Join(r.dir, "cmd/agent-jobs/main.go"), fmt.Sprintf(fakeMain, r.rec), 0o644)
	writeFile(t, filepath.Join(r.dir, "scripts/check.sh"), fmt.Sprintf(fakeCheck, r.rec), 0o755)
	if installer == "" {
		copyFile(t, "scripts/install-linux.sh", filepath.Join(r.dir, "scripts/install-linux.sh"), 0o755)
		copyFile(t, "deploy/systemd/agent-jobs@.service", filepath.Join(r.dir, "deploy/systemd/agent-jobs@.service"), 0o644)
		copyFile(t, "skills/agent-jobs/SKILL.md", filepath.Join(r.dir, "skills/agent-jobs/SKILL.md"), 0o644)
	} else {
		writeFile(t, filepath.Join(r.dir, "scripts/install-linux.sh"), installer, 0o755)
	}
	gitIn(t, r.dir, "init", "-q", "-b", "main")
	gitIn(t, r.dir, "add", "-A")
	gitIn(t, r.dir, "commit", "-qm", "fake")
	return r
}

// args are the script arguments for a run of the fake repository, plus the job environment.
func (r fakeRepo) args(extra ...string) ([]string, []string) {
	jargs, jenv := jobArgs()
	return append(append([]string{"--from", r.dir}, jargs...), extra...), jenv
}

func (r fakeRepo) read(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(r.rec, name))
	if err != nil {
		t.Fatalf("record %s: %v", name, err)
	}
	return string(b)
}

func (r fakeRepo) pid(t *testing.T, name string) int {
	t.Helper()
	n, err := strconv.Atoi(strings.TrimSpace(r.read(t, name)))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// waitFor waits up to d for the record file name.
func (r fakeRepo) waitFor(t *testing.T, name string, d time.Duration) {
	t.Helper()
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(r.rec, name)); err == nil {
			return
		}
	}
	t.Fatalf("no %s after %s", name, d)
}

// sentinels are a caller's Git repository (GIT_DIR, GIT_WORK_TREE, GIT_INDEX_FILE), XDG config,
// Claude config and agent-jobs home that an acceptance run must leave as they were.
type sentinels struct {
	repo, xdg, claude, ajHome string
	env                       []string
	head, ref, index, status  string
}

func newSentinels(t *testing.T) *sentinels {
	t.Helper()
	return newSentinelsGit(t, true)
}

// newSentinelsGit: with all false only GIT_INDEX_FILE points into the caller's repository (git
// clone then succeeds, and a redirected checkout would rewrite the caller's index).
func newSentinelsGit(t *testing.T, all bool) *sentinels {
	t.Helper()
	base := t.TempDir()
	s := &sentinels{
		repo: filepath.Join(base, "caller-repo"), xdg: filepath.Join(base, "xdg"),
		claude: filepath.Join(base, "claude"), ajHome: filepath.Join(base, "aj"),
	}
	for _, d := range []string{s.repo, s.xdg, s.claude, s.ajHome} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(t, s.repo, "init", "-q", "-b", "main")
	writeFile(t, filepath.Join(s.repo, "caller.txt"), "caller\n", 0o644)
	gitIn(t, s.repo, "add", "caller.txt")
	gitIn(t, s.repo, "commit", "-qm", "caller")
	s.head, s.ref, s.index, s.status = s.snapshot(t)
	s.env = []string{
		"GIT_INDEX_FILE=" + filepath.Join(s.repo, ".git", "index"),
		"XDG_CONFIG_HOME=" + s.xdg, "CLAUDE_CONFIG_DIR=" + s.claude, "AGENT_JOBS_HOME=" + s.ajHome,
	}
	if all {
		s.env = append(s.env, "GIT_DIR="+filepath.Join(s.repo, ".git"), "GIT_WORK_TREE="+s.repo)
	}
	return s
}

func (s *sentinels) snapshot(t *testing.T) (head, ref, index, status string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(s.repo, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	return gitIn(t, s.repo, "rev-parse", "HEAD"), gitIn(t, s.repo, "symbolic-ref", "HEAD"),
		fmt.Sprintf("%x", sha256.Sum256(b)), gitIn(t, s.repo, "status", "--porcelain")
}

// check: the caller's repository (HEAD, branch, index, work tree) and dirs are untouched.
func (s *sentinels) check(t *testing.T) {
	t.Helper()
	head, ref, index, status := s.snapshot(t)
	if head != s.head || ref != s.ref || index != s.index || status != s.status {
		t.Errorf("caller repo changed: HEAD %s->%s, ref %s->%s, index %s->%s, status %q->%q",
			s.head, head, s.ref, ref, s.index, index, s.status, status)
	}
	entries, err := os.ReadDir(s.repo)
	if err != nil || len(entries) != 2 {
		t.Errorf("caller work tree changed: %v %v", entries, err)
	}
	for _, d := range []string{s.xdg, s.claude, s.ajHome} {
		if entries, err := os.ReadDir(d); err != nil || len(entries) != 0 {
			t.Errorf("%s touched: %v %v", d, entries, err)
		}
	}
}

// envOf parses `env` output.
func envOf(text string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			m[k] = v
		}
	}
	return m
}

// The caller's Git overrides, XDG config, Claude config and agent-jobs home reach neither git nor the
// clone's code nor the installer, in both modes; with --real-model only the daemon gets the caller's
// HOME and CLAUDE_CONFIG_DIR back.
func TestAcceptanceScriptIsolation(t *testing.T) {
	for _, c := range []struct{ real, allGit bool }{{false, true}, {true, true}, {false, false}} {
		real := c.real
		t.Run(fmt.Sprintf("real-model=%v,all-git-vars=%v", c.real, c.allGit), func(t *testing.T) {
			t.Parallel()
			r := newFakeRepo(t, "")
			sent := newSentinelsGit(t, c.allGit)
			home := t.TempDir()
			var more []string
			if real {
				more = []string{"--real-model", "sonnet"}
			}
			args, extra := r.args(more...)
			extra = append(extra, sent.env...)
			extra = append(extra, "HOME="+home)
			out, code := runFake(t, r, extra, args...)
			sent.check(t)
			// The fake binary refuses `start`: the run fails at the job, after install and status.
			if code != 1 || !strings.Contains(out, "PASS  install") || !strings.Contains(out, "PASS  status") {
				t.Fatalf("exit %d\n%s", code, out)
			}
			env := envOf(r.read(t, "check.env"))
			for _, k := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "XDG_CONFIG_HOME", "CLAUDE_CONFIG_DIR"} {
				if v, ok := env[k]; ok {
					t.Errorf("clone code ran with %s=%s", k, v)
				}
			}
			if env["HOME"] == home || !strings.HasPrefix(env["AGENT_JOBS_HOME"], env["HOME"]+"/") {
				t.Errorf("clone code ran with HOME=%s AGENT_JOBS_HOME=%s (caller HOME %s)", env["HOME"], env["AGENT_JOBS_HOME"], home)
			}
			denv := envOf(r.read(t, "daemon.env"))
			if real && (denv["HOME"] != home || denv["CLAUDE_CONFIG_DIR"] != sent.claude) {
				t.Errorf("real-model daemon: %v, want HOME=%s CLAUDE_CONFIG_DIR=%s", denv, home, sent.claude)
			}
			if !real && (denv["HOME"] == home || denv["CLAUDE_CONFIG_DIR"] != "") {
				t.Errorf("dry daemon: %v", denv)
			}
		})
	}
}

// An installer that puts other bytes in place fails the install step; later steps never run.
func TestAcceptanceScriptInstalledBinary(t *testing.T) {
	r := newFakeRepo(t, brokenInstaller)
	args, extra := r.args("--no-tests")
	out, code := runFake(t, r, extra, args...)
	if code != 1 || !strings.Contains(out, "FAIL  install") || !strings.Contains(out, "installed binary differs") {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

// Without curl the HTTP check cannot run: the script refuses to start instead of passing without it.
func TestAcceptanceScriptCurlRequired(t *testing.T) {
	bin := t.TempDir()
	for _, tool := range []string{"git", "go", "dirname", "grep"} {
		p, err := exec.LookPath(tool)
		if err != nil {
			t.Skip(tool, " not found")
		}
		if err := os.Symlink(p, filepath.Join(bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	out, code := acceptanceRun(t, []string{"PATH=" + bin}, nil, "--from", t.TempDir())
	if code != 1 || !strings.Contains(out, "curl not found (required)") {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

// TERM while a step runs: the step and its children are stopped, nothing of the run remains.
func TestAcceptanceScriptInterruptStep(t *testing.T) {
	r := newFakeRepo(t, "", "hang-check")
	args, extra := r.args()
	cmd, out, since, done := startFake(t, r, extra, args...)
	r.waitFor(t, "check.pid", 60*time.Second)
	procs := watch(t, since, r.pid(t, "check.pid"), r.pid(t, "check-child.pid"))
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	code := waitDone(t, done, out, 60*time.Second)
	assertGone(t, out, procs)
	if code != 143 || !strings.Contains(out.String(), "interrupted (exit 143)") {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

// TERM while the status step runs, then TERM and INT again while the teardown waits for a daemon that
// ignores TERM: the teardown still ends the step, the daemon and their groups.
func TestAcceptanceScriptInterruptTeardown(t *testing.T) {
	r := newFakeRepo(t, "", "hang-status", "stubborn")
	args, extra := r.args("--no-tests")
	cmd, out, since, done := startFake(t, r, extra, args...)
	r.waitFor(t, "status.started", 60*time.Second)
	procs := watch(t, since, r.pid(t, "status.pid"), r.pid(t, "status-child.pid"), r.pid(t, "daemon.pid"))
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	r.waitFor(t, "daemon.term", 30*time.Second)
	for _, sig := range []os.Signal{syscall.SIGTERM, syscall.SIGINT} {
		if err := cmd.Process.Signal(sig); err != nil {
			t.Fatal(err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	code := waitDone(t, done, out, 60*time.Second)
	assertGone(t, out, procs)
	if code != 143 || !strings.Contains(out.String(), "interrupted (exit 143)") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	// The jobs were looked for after the step was stopped.
	if calls := r.read(t, "calls.log"); !strings.Contains(calls, "list --agent") || !strings.Contains(calls, "--status running") {
		t.Errorf("no job cancel pass:\n%s", calls)
	}
}

func runFake(t *testing.T, r fakeRepo, extra []string, args ...string) (string, int) {
	t.Helper()
	b, err := acceptanceCmd(t, r.script, extra, nil, args...).CombinedOutput()
	return string(b), exitCode(t, err)
}

// startFake starts the script with its output in a file (no pipe a leftover process could hold);
// since is the script's start time (clock ticks since boot).
func startFake(t *testing.T, r fakeRepo, extra []string, args ...string) (*exec.Cmd, *outFile, uint64, chan error) {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	cmd := acceptanceCmd(t, r.script, extra, nil, args...)
	cmd.Stdout, cmd.Stderr = f, f
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	since := startTicks(procStat(cmd.Process.Pid))
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return cmd, &outFile{f.Name()}, since, done
}

type outFile struct{ path string }

func (o *outFile) String() string {
	b, _ := os.ReadFile(o.path)
	return string(bytes.TrimSpace(b))
}

func waitDone(t *testing.T, done chan error, out *outFile, d time.Duration) int {
	t.Helper()
	select {
	case err := <-done:
		return exitCode(t, err)
	case <-time.After(d):
		t.Fatalf("still running after %s\n%s", d, out)
	}
	return -1
}

// procStat is /proc/PID/stat after the comm: state (field 3), ppid, pgrp, ..., starttime (field 22).
func procStat(pid int) []string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return nil
	}
	s := string(b)
	return strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
}

func startTicks(f []string) uint64 {
	if len(f) < 20 {
		return 0
	}
	n, _ := strconv.ParseUint(f[19], 10, 64)
	return n
}

// watched are processes of one run (pid and start time) and the process groups they were in.
type watched struct {
	since  uint64
	pids   map[int]uint64
	groups []int
}

// watch records pids (with their start times and groups) and reaps whatever of them and of their
// groups is left when the test ends, also on a failure path.
func watch(t *testing.T, since uint64, pids ...int) *watched {
	t.Helper()
	w := &watched{since: since, pids: map[int]uint64{}}
	for _, p := range pids {
		f := procStat(p)
		if len(f) < 20 {
			t.Fatalf("no /proc stat for %d", p)
		}
		w.pids[p] = startTicks(f)
		g, _ := strconv.Atoi(f[2])
		w.groups = append(w.groups, g)
	}
	t.Cleanup(func() { w.leftovers(true) })
	return w
}

// leftovers lists (and with kill, SIGKILLs) the live processes of the run: a recorded pid with its
// recorded start time, or a member of a recorded group started no earlier than the script. Zombies
// are gone already (a subreaper may not have reaped them yet); this test's own group is never touched.
func (w *watched) leftovers(kill bool) []string {
	var left []string
	add := func(pid int, why string) {
		left = append(left, fmt.Sprintf("%d (%s)", pid, why))
		if kill {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	for p, st := range w.pids {
		if f := procStat(p); len(f) > 19 && f[0] != "Z" && startTicks(f) == st {
			add(p, "recorded")
		}
	}
	own := syscall.Getpgrp()
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		f := procStat(pid)
		if len(f) < 20 || f[0] == "Z" || startTicks(f) < w.since {
			continue
		}
		for _, g := range w.groups {
			if g != own && f[2] == strconv.Itoa(g) {
				add(pid, "group "+f[2])
			}
		}
	}
	return left
}

// assertGone: nothing of the run is left, and each step had a process group of its own.
func assertGone(t *testing.T, out *outFile, w *watched) {
	t.Helper()
	own := syscall.Getpgrp()
	for _, g := range w.groups {
		if g == own {
			t.Errorf("process group %d is this test's own: a step ran without a group of its own\n%s", g, out)
		}
	}
	if left := w.leftovers(true); len(left) > 0 {
		t.Errorf("processes survived the run: %v\n%s", left, out)
	}
}
