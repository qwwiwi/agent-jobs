package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// `agent-jobs gate run` passes the command's own flags through, takes the lock under the agent-jobs home
// and always ends stdout with exit=N, also on a usage error.
func TestGateRunCLI(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_JOBS_HOME", home)
	t.Setenv("TELEGRAM_BOT_TOKEN", "do-not-pass")
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
	if _, err := os.Stat(filepath.Join(home, "gate.lock")); err != nil {
		t.Fatalf("the default gate lock is not under the agent-jobs home: %v", err)
	}
	for _, args := range [][]string{
		{"--repo", repo, "--head", head},          // no command
		{"--repo", repo, "--bogus", "--", "true"}, // unknown flag
		{"--repo", repo, "--", "true"},            // no --head
	} {
		if code, o, _ := run(args...); code != 125 || !strings.HasSuffix(o, "exit=125\n") {
			t.Fatalf("%v: code %d, out %q", args, code, o)
		}
	}
}
