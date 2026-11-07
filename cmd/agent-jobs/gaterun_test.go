package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

// `agent-jobs gate run` passes the command's own flags through, takes the box lock (here the test's own,
// gateRunTestLock), never one under the agent-jobs home, has no flag to pick another lock and always
// ends stdout with exit=N, also on a usage error.
func TestGateRunCLI(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_JOBS_HOME", home)
	t.Setenv("TELEGRAM_BOT_TOKEN", "do-not-pass")
	lock := filepath.Join(t.TempDir(), "agent-jobs-gate.lock")
	gateRunTestLock = lock
	t.Cleanup(func() { gateRunTestLock = "" })
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.name=T", "-c", "user.email=t@example.com", "commit", "-q",
		"--allow-empty", "-m", "init"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	out, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	head := strings.TrimSpace(string(out))

	run := func(args ...string) (int, string, string) {
		var o, e bytes.Buffer
		code := cmdGateRun(args, &o, &e)
		return code, o.String(), e.String()
	}
	code, o, e := run("--repo", repo, "--head", head, "--", "/bin/sh", "-c", `echo "arg=$1 tg=${TELEGRAM_BOT_TOKEN:-none}"; exit 4`,
		"sh", "-x")
	if code != 4 || o != "arg=-x tg=none\nexit=4\n" {
		t.Fatalf("code %d, out %q, err %q", code, o, e)
	}
	if _, err := os.Stat(lock); err != nil {
		t.Fatalf("the gate did not take the box lock: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "gate.lock")); err == nil {
		t.Fatal("a gate lock under the agent-jobs home: the lock follows the state root")
	}
	for _, args := range [][]string{
		{"--repo", repo, "--head", head},          // no command
		{"--repo", repo, "--bogus", "--", "true"}, // unknown flag
		{"--repo", repo, "--", "true"},            // no --head
		{"--repo", repo, "--head", head, "--lock", filepath.Join(home, "other.lock"), "--", "true"}, // no other lock
	} {
		if code, o, _ := run(args...); code != 125 || !strings.HasSuffix(o, "exit=125\n") {
			t.Fatalf("%v: code %d, out %q", args, code, o)
		}
	}
}

// Inside a job `gate run` is the one control command that runs (#208): its heavy steps take the box-wide
// gate lock. Every other gate subcommand and every other command that manages jobs stays refused.
func TestGateRunAllowedInsideJob(t *testing.T) {
	t.Setenv(jobs.JobEnvVar, "261107-000000-gate")
	if reason := refuseInsideJob("gate", []string{"run", "--head", "HEAD", "--", "go", "test", "./..."}); reason != "" {
		t.Fatalf("gate run refused inside a job: %s", reason)
	}
	for _, c := range []struct {
		cmd  string
		args []string
	}{
		{"gate", []string{"retire", "--pr", "1"}},
		{"gate", nil},
		{"gate", []string{"--head", "HEAD", "run"}},
		{"start", []string{"--title", "t"}},
		{"daemon", nil},
		{"tick", nil},
	} {
		if reason := refuseInsideJob(c.cmd, c.args); reason == "" {
			t.Errorf("%s %v allowed inside a job", c.cmd, c.args)
		}
	}
}
