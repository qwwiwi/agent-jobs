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
	"github.com/qwwiwi/agent-jobs/internal/testutil"
)

const acceptanceScript = "scripts/acceptance-clean-clone.sh"

// acceptanceRun runs the acceptance script with a minimal environment (go and git on PATH, a temp
// HOME, no agent-jobs variables) plus extra; argv0 is prepended when given (a fake wrapper).
func acceptanceRun(t *testing.T, extra []string, argv0 []string, args ...string) (string, int) {
	t.Helper()
	script, err := filepath.Abs(acceptanceScript)
	if err != nil {
		t.Fatal(err)
	}
	cmd := acceptanceCmd(t, script, extra, argv0, args...)
	out, err := cmd.CombinedOutput()
	return string(out), exitCode(t, err)
}

// exitCode is the exit status of a finished command (a start failure fails the test).
func exitCode(t *testing.T, err error) int {
	t.Helper()
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return 0
}

// acceptanceCmd is the command acceptanceRun runs, for the given script: a minimal environment plus
// extra (a later entry of the same key wins, so extra may override HOME, TMPDIR or PATH).
func acceptanceCmd(t *testing.T, script string, extra []string, argv0 []string, args ...string) *exec.Cmd {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go not on PATH")
	}
	// The caller's Go caches: a temp HOME would otherwise download every module into the test's dir.
	goEnv, err := exec.Command(goBin, "env", "GOCACHE", "GOMODCACHE", "GOPATH").Output()
	if err != nil {
		t.Fatal(err)
	}
	g := strings.Split(strings.TrimSpace(string(goEnv)), "\n")
	if len(g) != 3 {
		t.Fatalf("go env: %q", goEnv)
	}
	// A short TMPDIR: the daemon's unix sockets live under it (the script refuses a long one), and
	// the caller's TMPDIR may be long (a job's is <state>/jobs/<id>/tmp).
	tmp := testutil.ShortTempDir(t)
	argv := append(append(append([]string{}, argv0...), "bash", script), args...)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append([]string{
		"HOME=" + t.TempDir(),
		"TMPDIR=" + tmp,
		"PATH=" + filepath.Dir(goBin) + ":/usr/local/bin:/usr/bin:/bin",
		"GOCACHE=" + g[0], "GOMODCACHE=" + g[1], "GOPATH=" + g[2],
	}, extra...)
	return cmd
}

// jobArgs are the extra arguments and environment a full run needs when this test runs inside an
// agent-jobs job (--inside-job and the job marker).
func jobArgs() ([]string, []string) {
	if !insideJob() {
		return nil, nil
	}
	var extra []string
	if v := os.Getenv("AGENT_JOBS_JOB"); v != "" {
		extra = append(extra, "AGENT_JOBS_JOB="+v)
	}
	return []string{"--inside-job"}, extra
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

// A TMPDIR too long for the daemon's unix sockets is refused before anything is cloned, and the
// empty temp dir is removed again.
func TestAcceptanceScriptLongTmpdir(t *testing.T) {
	long := filepath.Join(t.TempDir(), strings.Repeat("d", 60))
	if err := os.MkdirAll(long, 0o755); err != nil {
		t.Fatal(err)
	}
	args := []string{"--from", filepath.Join(long, "nowhere")}
	extra := []string{"TMPDIR=" + long}
	if insideJob() {
		args = append(args, "--inside-job")
		if v := os.Getenv("AGENT_JOBS_JOB"); v != "" {
			extra = append(extra, "AGENT_JOBS_JOB="+v)
		}
	}
	out, code := acceptanceRun(t, extra, nil, args...)
	if code != 2 || !strings.Contains(out, "too long for its unix sockets") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if entries, err := os.ReadDir(long); err != nil || len(entries) != 0 {
		t.Fatalf("temp dir left behind: %v %v", entries, err)
	}
}

// TestAcceptanceScriptRun is the whole acceptance (clone of this checkout, build, install, daemon,
// test job) without the test suite, with the caller's Git, XDG, Claude and agent-jobs settings set to
// sentinels that must stay untouched; the real-model runs use a fake claude on PATH (with and without
// the marker). They wait for two daemon ticks, so they run only on request (in parallel):
// ACCEPTANCE_CLEAN_CLONE=1 go test -run TestAcceptanceScriptRun .
func TestAcceptanceScriptRun(t *testing.T) {
	if os.Getenv("ACCEPTANCE_CLEAN_CLONE") != "1" {
		t.Skip("set ACCEPTANCE_CLEAN_CLONE=1 to run the full acceptance (about 3 minutes)")
	}
	if runtime.GOOS != "linux" {
		t.Skip("Linux only")
	}
	root, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, acceptanceScript)
	for _, c := range []struct {
		name   string
		claude string // the fake claude's report summary on PATH ("" = the dry run, no fake claude)
		pass   bool
		step   string
	}{
		{"dry", "", true, "dry-job"},
		{"real-model", "ACCEPT-REAL-JOB-OK", true, "real-job"},
		{"real-model-no-marker", "SOMETHING-ELSE", false, "real-job"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			sent := newSentinels(t)
			jargs, extra := jobArgs()
			args := append([]string{"--no-tests", "--from", root}, jargs...)
			extra = append(extra, sent.env...)
			if c.claude != "" {
				bin := filepath.Join(t.TempDir(), "bin")
				writeFakeClaude(t, bin, c.claude)
				goBin, err := exec.LookPath("go")
				if err != nil {
					t.Fatal(err)
				}
				extra = append(extra, "PATH="+bin+":"+filepath.Dir(goBin)+":/usr/local/bin:/usr/bin:/bin")
				args = append(args, "--real-model", "sonnet", "--wait-minutes", "5")
			}
			cmd := acceptanceCmd(t, script, extra, nil, args...)
			b, err := cmd.CombinedOutput()
			out, code := string(b), exitCode(t, err)
			ok := code == 0 && strings.Contains(out, "RESULT: PASS") && strings.Contains(out, "PASS  "+c.step)
			if c.pass != ok {
				t.Fatalf("acceptance (want pass=%v): exit %d\n%s", c.pass, code, out)
			}
			if !c.pass && (!strings.Contains(out, "FAIL  "+c.step) || !strings.Contains(out, "lacks the marker")) {
				t.Fatalf("want %s to fail on the marker:\n%s", c.step, out)
			}
			sent.check(t)
		})
	}
}

// writeFakeClaude writes dir/claude: a runner for a --real-model job that answers with a report whose
// summary is summary.
func writeFakeClaude(t *testing.T, dir, summary string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' '{\"type\":\"system\",\"subtype\":\"init\"}'\n" +
		`printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"num_turns":1,"total_cost_usd":0,"result":"## Summary\n` +
		summary + `\n\n## Findings / Changes\nnothing\n\n## Evidence\nnothing\n\n## Not done / uncertain\nnothing\n\n## Progress\n- --:-- UTC replied"}'` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}
