package jobs

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The outer-wrapper hook exists only in the agentjobs_testhooks build: under a (fake) job wrapper that
// AGENT_JOBS_TEST_OUTER_WRAPPER names, a release build still refuses control commands, the hooked
// build under test does not. Inside a job scope (the suite run by a job) the release build refuses
// earlier, on the scope check, before it looks at the wrapper ancestry.
func TestOuterWrapperHookOnlyInTestBuild(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("ancestry check is Linux-only (/proc)")
	}
	dir := t.TempDir()
	release := filepath.Join(dir, "release")
	if out, err := exec.Command("go", "build", "-o", release, "../../cmd/agent-jobs").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	script := filepath.Join(dir, "agent-jobs")
	body := "#!/bin/bash\nAGENT_JOBS_TEST_OUTER_WRAPPER=\"$OUTER${OUTER:+,}$$\" \"$BIN\" list --agent test\necho \"exit=$?\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(bin string) string {
		cmd := exec.Command(script, "run-wrapper", filepath.Join(dir, "jobs", "x"), "--token", "t", "--", "claude")
		cmd.Env = append(os.Environ(), "AGENT_JOBS_HOME="+dir, "BIN="+bin, "OUTER="+os.Getenv(outerWrapperEnv))
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("fake wrapper: %v %s", err, out)
		}
		return string(out)
	}
	want := "refused: running under the job wrapper"
	if unit, _ := JobScope(); unit != "" {
		want = "refused: running in the cgroup of job scope " + unit
	}
	if out := run(release); !strings.Contains(out, want) || strings.Contains(out, "exit=0") {
		t.Fatalf("release build honoured the test hook: %s", out)
	}
	if out := run(selfBin); !strings.Contains(out, "exit=0") {
		t.Fatalf("hooked build under an outer wrapper: %s", out)
	}
}
