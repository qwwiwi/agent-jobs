package agentjobs_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

const acceptanceScript = "scripts/acceptance-clean-clone.sh"

// acceptanceRun runs the acceptance script with a minimal environment (go and git on PATH, a temp
// HOME, no agent-jobs variables) plus extra; argv0 is prepended when given (a fake wrapper).
func acceptanceRun(t *testing.T, extra []string, argv0 []string, args ...string) (string, int) {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go not on PATH")
	}
	script, err := filepath.Abs(acceptanceScript)
	if err != nil {
		t.Fatal(err)
	}
	argv := append(append(append([]string{}, argv0...), "bash", script), args...)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append([]string{
		"HOME=" + t.TempDir(),
		"TMPDIR=" + t.TempDir(),
		"PATH=" + filepath.Dir(goBin) + ":/usr/local/bin:/usr/bin:/bin",
	}, extra...)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), code
}

// insideJob reports whether this test process runs inside an agent-jobs job (marker or wrapper).
func insideJob() bool {
	if _, ok := jobs.InsideJob(); ok {
		return true
	}
	if pid, _ := jobs.WrapperAncestor(); pid != 0 {
		return true
	}
	sid, _ := jobs.WrapperSession()
	return sid != 0
}

func TestAcceptanceScriptUsage(t *testing.T) {
	out, code := acceptanceRun(t, nil, nil, "--help")
	if code != 0 || !strings.Contains(out, "usage: acceptance-clean-clone.sh") || !strings.Contains(out, "--real-model") {
		t.Fatalf("--help: exit %d\n%s", code, out)
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--bogus"}, "unknown argument: --bogus"},
		{[]string{"HEAD", "HEAD"}, "at most one REF"},
		{[]string{"--wait-minutes", "0"}, "--wait-minutes wants 1..999"},
		{[]string{"--wait-minutes", "x"}, "--wait-minutes wants 1..999"},
		{[]string{"--real-model", "a b"}, "invalid --real-model"},
		{[]string{"--from"}, "usage:"},
		{[]string{"no-such-ref-xyz"}, "ref 'no-such-ref-xyz' is not a commit"},
	} {
		out, code := acceptanceRun(t, nil, nil, c.args...)
		if code != 2 || !strings.Contains(out, c.want) {
			t.Errorf("%v: exit %d, want 2 with %q\n%s", c.args, code, c.want, out)
		}
	}
}

// Inside a job the binary refuses control commands: the script stops before it creates anything
// unless --inside-job is given, and refuses --inside-job outside a job.
func TestAcceptanceScriptInsideJob(t *testing.T) {
	tmp := t.TempDir()
	from := filepath.Join(tmp, "nowhere") // never reached: the check comes first
	out, code := acceptanceRun(t, []string{"AGENT_JOBS_JOB=job-1", "TMPDIR=" + tmp}, nil, "--from", from)
	if code != 2 || !strings.Contains(out, "running inside an agent-jobs job") {
		t.Fatalf("marker set: exit %d\n%s", code, out)
	}
	if runtime.GOOS == "linux" {
		// No marker, but an ancestor whose argv is a job wrapper's (`sh <script> run-wrapper D --token T`).
		wrap := filepath.Join(tmp, "fakewrap")
		if err := os.WriteFile(wrap, []byte("#!/bin/sh\necho \"wrapper=$$\"\nshift 4\n\"$@\"\nexit $?\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		out, code = acceptanceRun(t, []string{"TMPDIR=" + tmp}, []string{"sh", wrap, "run-wrapper", tmp, "--token", "T"}, "--from", from)
		pid, _, _ := strings.Cut(strings.TrimPrefix(out, "wrapper="), "\n")
		if code != 2 || !strings.HasPrefix(out, "wrapper=") || !regexp.MustCompile(`wrapper pids '([0-9]+,)*`+pid+`(,[0-9]+)*'`).MatchString(out) {
			t.Fatalf("wrapper ancestor %s not detected: exit %d\n%s", pid, code, out)
		}
	}
	if entries, err := os.ReadDir(tmp); err != nil {
		t.Fatal(err)
	} else {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "agent-jobs-accept.") {
				t.Fatalf("temp dir created before the inside-job check: %s", e.Name())
			}
		}
	}
	if insideJob() {
		t.Log("inside a job: the --inside-job refusal outside a job is not checked here")
		return
	}
	out, code = acceptanceRun(t, nil, nil, "--inside-job", "--from", from)
	if code != 2 || !strings.Contains(out, "no job around this process") {
		t.Fatalf("--inside-job outside a job: exit %d\n%s", code, out)
	}
}

// TestAcceptanceScriptRun is the whole acceptance (clone of this checkout, build, install, daemon,
// dry job) without the test suite; it waits for two daemon ticks, so it runs only on request:
// AGENT_JOBS_TEST_ACCEPTANCE=1 go test -run TestAcceptanceScriptRun .
func TestAcceptanceScriptRun(t *testing.T) {
	if os.Getenv("AGENT_JOBS_TEST_ACCEPTANCE") != "1" {
		t.Skip("set AGENT_JOBS_TEST_ACCEPTANCE=1 to run the full acceptance (about 3 minutes)")
	}
	if runtime.GOOS != "linux" {
		t.Skip("Linux only")
	}
	root, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"--no-tests", "--from", root}
	var extra []string
	if insideJob() {
		args = append(args, "--inside-job")
		if v := os.Getenv("AGENT_JOBS_JOB"); v != "" {
			extra = append(extra, "AGENT_JOBS_JOB="+v)
		}
	}
	out, code := acceptanceRun(t, extra, nil, args...)
	if code != 0 || !strings.Contains(out, "RESULT: PASS") || !strings.Contains(out, "PASS  dry-job") {
		t.Fatalf("acceptance: exit %d\n%s", code, out)
	}
}
