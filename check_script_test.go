package agentjobs

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Every `go test` run of scripts/check.sh sets its own -timeout (issue #97): internal/jobs takes longer
// than go's 10-minute default on a busy host, so the default killed the run with a panic before it ended.
func TestCheckScriptGoTestTimeout(t *testing.T) {
	b, err := os.ReadFile("scripts/check.sh")
	if err != nil {
		t.Fatal(err)
	}
	runs := 0
	for _, line := range strings.Split(string(b), "\n") {
		code := strings.TrimSpace(line)
		if strings.HasPrefix(code, "#") || strings.HasPrefix(code, "echo ") || !strings.Contains(code, "go test ") {
			continue
		}
		runs++
		if !strings.Contains(code, ` -timeout "$GO_TEST_TIMEOUT"`) {
			t.Errorf("go test without the explicit timeout: %s", code)
		}
	}
	if runs != 1 {
		t.Fatalf("found %d go test runs in scripts/check.sh, want 1 (one suite run that manifest-check reuses, #172)", runs)
	}
	m := regexp.MustCompile(`(?m)^GO_TEST_TIMEOUT="\$\{GO_TEST_TIMEOUT:-([0-9]+)m\}"$`).FindStringSubmatch(string(b))
	if m == nil {
		t.Fatal(`scripts/check.sh does not set GO_TEST_TIMEOUT="${GO_TEST_TIMEOUT:-<n>m}"`)
	}
	if n, _ := strconv.Atoi(m[1]); n < 30 {
		t.Fatalf("default go test timeout %sm, want at least 30m (internal/jobs alone takes ~14m)", m[1])
	}
}

// fakeCheckTree builds a throwaway tree with a copy of scripts/check.sh and stubs for everything it calls,
// so the script runs hermetically: fake go and gofmt on PATH (go logs each call; `go test` prints a fixed
// -json stream and exits with FAKE_GO_TEST_EXIT), a manifest-check stub (logs its call, keeps a copy of the
// file it is given, exits with FAKE_MANIFEST_EXIT).
func fakeCheckTree(t *testing.T, withChecklist bool) (root string, env []string) {
	t.Helper()
	root = t.TempDir()
	for _, f := range []string{"scripts/check.sh", "scripts/render-test-json.sh"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(root, f), string(b), 0o755)
	}
	writeFile(t, filepath.Join(root, "scripts/check-commit-messages.sh"), "#!/bin/sh\nexit 0\n", 0o755)
	writeFile(t, filepath.Join(root, "scripts/manifest-check.sh"), `#!/bin/sh
echo "manifest-check $1" >> "$FAKE_LOG"
cp "$1" "$FAKE_LOG.manifest-input"
exit "${FAKE_MANIFEST_EXIT:-0}"
`, 0o755)
	if withChecklist {
		writeFile(t, filepath.Join(root, "brief/CHECKLIST.md"), "# checklist\n", 0o644)
	}
	bin := filepath.Join(root, "fakebin")
	writeFile(t, filepath.Join(bin, "gofmt"), "#!/bin/sh\nexit 0\n", 0o755)
	writeFile(t, filepath.Join(bin, "go"), `#!/bin/sh
echo "go $*" >> "$FAKE_LOG"
if [ "$1" = test ]; then
  printf '%s\n' '{"Action":"output","Package":"p","Test":"TestA","Output":"    a_test.go:1: boom \u003c x\n"}'
  printf '%s\n' '{"Action":"fail","Package":"p","Test":"TestA"}'
  exit "${FAKE_GO_TEST_EXIT:-0}"
fi
exit 0
`, 0o755)
	env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "FAKE_LOG="+filepath.Join(root, "fake.log"))
	return root, env
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func runFakeCheck(t *testing.T, root string, env []string, extra ...string) (out string, code int, log string) {
	t.Helper()
	cmd := exec.Command("bash", filepath.Join(root, "scripts/check.sh"))
	cmd.Env = append(append([]string{}, env...), extra...)
	b, err := cmd.CombinedOutput()
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatal(err)
		}
		code = ee.ExitCode()
	}
	l, _ := os.ReadFile(filepath.Join(root, "fake.log"))
	return string(b), code, string(l)
}

// Issue #172: check.sh runs the suite once; manifest-check reads that run's -json artifact.
func TestCheckScriptRunsSuiteOnce(t *testing.T) {
	root, env := fakeCheckTree(t, true)
	out, code, log := runFakeCheck(t, root, env)
	if code != 0 {
		t.Fatalf("exit %d, want 0:\n%s", code, out)
	}
	if n := strings.Count(log, "go test "); n != 1 {
		t.Errorf("%d go test runs, want 1:\n%s", n, log)
	}
	if !strings.Contains(log, "-json") {
		t.Errorf("go test was not run with -json:\n%s", log)
	}
	if !strings.Contains(log, "manifest-check ") {
		t.Errorf("manifest-check did not run:\n%s", log)
	}
	in, err := os.ReadFile(filepath.Join(root, "fake.log.manifest-input"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(in), `"Test":"TestA"`) {
		t.Errorf("manifest-check was not given the suite's json:\n%s", in)
	}
	if !strings.Contains(out, "a_test.go:1: boom < x") {
		t.Errorf("the log lacks the readable test output:\n%s", out)
	}
	if !strings.Contains(out, "check.sh: OK") {
		t.Errorf("no final OK:\n%s", out)
	}
}

func TestCheckScriptFailingTestFailsBeforeManifestCheck(t *testing.T) {
	root, env := fakeCheckTree(t, true)
	out, code, log := runFakeCheck(t, root, env, "FAKE_GO_TEST_EXIT=1")
	if code == 0 {
		t.Fatalf("a failing test passed the gate:\n%s", out)
	}
	if strings.Contains(log, "manifest-check ") || strings.Contains(out, "check.sh: OK") {
		t.Errorf("the gate went on after a failing test:\n%s\n%s", out, log)
	}
	if !strings.Contains(out, "a_test.go:1: boom < x") {
		t.Errorf("the failure output is not in the log:\n%s", out)
	}
	if n := strings.Count(log, "go test "); n != 1 {
		t.Errorf("%d go test runs, want 1", n)
	}
}

func TestCheckScriptManifestMismatchFails(t *testing.T) {
	root, env := fakeCheckTree(t, true)
	out, code, _ := runFakeCheck(t, root, env, "FAKE_MANIFEST_EXIT=1")
	if code == 0 || strings.Contains(out, "check.sh: OK") {
		t.Fatalf("a manifest mismatch passed the gate (exit %d):\n%s", code, out)
	}
}

func TestCheckScriptWithoutChecklistRunsSuiteOnce(t *testing.T) {
	root, env := fakeCheckTree(t, false)
	out, code, log := runFakeCheck(t, root, env)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if n := strings.Count(log, "go test "); n != 1 || strings.Contains(log, "manifest-check ") {
		t.Errorf("want one go test and no manifest-check:\n%s", log)
	}
	if !strings.Contains(out, "skipped: no brief/CHECKLIST.md") {
		t.Errorf("no skip line:\n%s", out)
	}
}

func TestRenderTestJSON(t *testing.T) {
	in := `{"Action":"output","Output":"=== RUN   TestA\n"}
{"Action":"output","Output":"\tx: got \"a\" \\ <\n"}
{"Action":"pass","Test":"TestA"}
# pkg [setup failed]
`
	cmd := exec.Command("scripts/render-test-json.sh")
	cmd.Stdin = strings.NewReader(in)
	b, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	want := "=== RUN   TestA\n\tx: got \"a\" \\ <\n# pkg [setup failed]\n"
	if string(b) != want {
		t.Fatalf("got %q, want %q", b, want)
	}
}
