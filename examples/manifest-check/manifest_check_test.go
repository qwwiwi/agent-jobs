// Package manifestcheck tests the reference scripts/manifest-check.sh (Wave F, F6) against fixtures
// and one real `go test -json` run.
package manifestcheck

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// run runs manifest-check.sh with POSIX /bin/sh in dir, the checklist at checklist ("" = the script's
// default), with args; it returns the output and the exit code.
func run(t *testing.T, dir, checklist string, args ...string) (string, int) {
	t.Helper()
	script, err := filepath.Abs("manifest-check.sh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", append([]string{script}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "TMPDIR="+t.TempDir())
	if checklist != "" {
		cmd.Env = append(cmd.Env, "MANIFEST_CHECKLIST="+checklist)
	}
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return string(out), 0
	case errors.As(err, &ee):
		return string(out), ee.ExitCode()
	}
	t.Fatalf("run: %v", err)
	return "", -1
}

func abs(t *testing.T, rel string) string {
	t.Helper()
	p, err := filepath.Abs(rel)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Every live requirement (R, Ri, G with contract live; not D, not deferred) with a passing tagged test:
// exit 0. A name may carry two tags; escaped pipes in a cell and quotes in a test name are read right;
// a tag of a non-live row is only a note.
func TestManifestCheckPasses(t *testing.T) {
	out, code := run(t, t.TempDir(), abs(t, "testdata/CHECKLIST.md"), abs(t, "testdata/pass.json"))
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for _, want := range []string{
		"ok       R01  TestLogin/[R01]_login_page_renders",
		"ok       R02  TestLogin/[R02][R03i]_logout_keeps_the_session",
		"ok       R03i  TestLogin/[R02][R03i]_logout_keeps_the_session",
		`ok       G01  TestAudit/[G01]_writes_\"quoted\"_lines`,
		"note     [R04] tags TestReport/[R04]_draft but is not a live requirement",
		"manifest-check: 4 of 4 live requirements have a passing tagged test",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "D01") {
		t.Errorf("a D row counted:\n%s", out)
	}
}

// A live requirement whose tagged tests failed, were skipped or only appear inside another test's output
// has no passing tagged test: exit 1, and the line names it.
func TestManifestCheckMissing(t *testing.T) {
	out, code := run(t, t.TempDir(), abs(t, "testdata/CHECKLIST.md"), abs(t, "testdata/missing.json"))
	if code != 1 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for _, want := range []string{
		"ok       R01  TestLogin/[R01]_login_page_renders",
		"MISSING  R02  no passing test tagged [R02] (failed: TestLogin/[R02]_logout)",
		"MISSING  R03i  no passing test tagged [R03i]",
		"manifest-check: 2 of 4 live requirements have a passing tagged test",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

// Bad input exits 2: no checklist, a checklist without live requirements, no test output.
func TestManifestCheckBadInput(t *testing.T) {
	dir := t.TempDir()
	if out, code := run(t, dir, "", abs(t, "testdata/pass.json")); code != 2 || !strings.Contains(out, "brief/CHECKLIST.md not found") {
		t.Fatalf("no checklist: exit %d\n%s", code, out)
	}
	noLive := filepath.Join(dir, "none.md")
	if err := os.WriteFile(noLive, []byte("| ID | Brief quote | Contract | Rationale |\n|---|---|---|---|\n| R01 | x | deferred | later |\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, code := run(t, dir, noLive, abs(t, "testdata/pass.json")); code != 2 || !strings.Contains(out, "no live requirements") {
		t.Fatalf("no live rows: exit %d\n%s", code, out)
	}
	empty := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if out, code := run(t, dir, abs(t, "testdata/CHECKLIST.md"), empty); code != 2 || !strings.Contains(out, "no go test output") {
		t.Fatalf("empty output: exit %d\n%s", code, out)
	}
}

// Without a file argument the script runs `go test -json ./...` from the repository root itself, with
// brief/CHECKLIST.md there: a real module with a tagged subtest passes, and fails once that test fails.
func TestManifestCheckRunsGoTest(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go on PATH")
	}
	repo := t.TempDir()
	files := map[string]string{
		"go.mod":             "module example.com/app\n\ngo 1.22\n",
		"brief/CHECKLIST.md": "| ID | Brief quote | Contract | Rationale |\n|---|---|---|---|\n| R01 | «a login page» | live | brief |\n",
		"app_test.go": "package app\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\nfunc TestLogin(t *testing.T) {\n" +
			"\tt.Run(\"[R01] login page renders\", func(t *testing.T) {\n" +
			"\t\tif _, err := os.Stat(\"broken\"); err == nil {\n\t\t\tt.Fatal(\"broken\")\n\t\t}\n\t})\n}\n",
	}
	for rel, body := range files {
		p := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out, code := run(t, repo, "")
	if code != 0 || !strings.Contains(out, "ok       R01  TestLogin/[R01]_login_page_renders") ||
		!strings.Contains(out, "manifest-check: go test exit 0") {
		t.Fatalf("passing run: exit %d\n%s", code, out)
	}
	if err := os.WriteFile(filepath.Join(repo, "broken"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	out, code = run(t, repo, "")
	if code != 1 || !strings.Contains(out, "MISSING  R01  no passing test tagged [R01] (failed: TestLogin/[R01]_login_page_renders)") {
		t.Fatalf("failing run: exit %d\n%s", code, out)
	}
}

// The live IDs are read before the tests run: a test that rewrites brief/CHECKLIST.md (here: down to
// R01) does not shrink the set the script checks, so R02 without a passing tagged test still fails it.
func TestManifestCheckReadsLiveIDsBeforeTheTests(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go on PATH")
	}
	repo := t.TempDir()
	files := map[string]string{
		"go.mod": "module example.com/app\n\ngo 1.22\n",
		"brief/CHECKLIST.md": "| ID | Brief quote | Contract | Rationale |\n|---|---|---|---|\n| R01 | «a login page» | live | brief |\n" +
			"| R02 | «a logout button» | live | brief |\n",
		"app_test.go": "package app\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\nfunc TestLogin(t *testing.T) {\n" +
			"\tt.Run(\"[R01] login page renders\", func(t *testing.T) {\n" +
			"\t\tshrunk := \"| ID | Brief quote | Contract | Rationale |\\n|---|---|---|---|\\n| R01 | «a login page» | live | brief |\\n\"\n" +
			"\t\tif err := os.WriteFile(\"brief/CHECKLIST.md\", []byte(shrunk), 0o644); err != nil {\n\t\t\tt.Fatal(err)\n\t\t}\n\t})\n}\n",
	}
	for rel, body := range files {
		p := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out, code := run(t, repo, "")
	if code != 1 || !strings.Contains(out, "MISSING  R02  no passing test tagged [R02]") ||
		!strings.Contains(out, "manifest-check: 1 of 2 live requirements have a passing tagged test") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if b, err := os.ReadFile(filepath.Join(repo, "brief", "CHECKLIST.md")); err != nil || strings.Contains(string(b), "R02") {
		t.Fatalf("the fixture test did not rewrite the checklist: %v\n%s", err, b)
	}
}
