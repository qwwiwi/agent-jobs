package agentjobs

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const manifestChecklist = "# Checklist\n\n| ID | Brief quote | Contract | Rationale |\n|---|---|---|---|\n| R01 | «a login» | live | brief |\n"

// manifestRepo lays out a copy of a repository the way the judge's wrapper does (a plain directory, no
// .git) with agent-jobs' scripts/manifest-check.sh, the reference script it runs, a checklist with R01
// live and the given go.mod and test file.
func manifestRepo(t *testing.T, gomod, test string) string {
	t.Helper()
	return manifestRepoWithChecklist(t, gomod, test, manifestChecklist)
}

// manifestRepoWithChecklist is manifestRepo with the given checklist.
func manifestRepoWithChecklist(t *testing.T, gomod, test, checklist string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"go.mod":             gomod,
		"app_test.go":        test,
		"brief/CHECKLIST.md": checklist,
	}
	for _, rel := range []string{"scripts/manifest-check.sh", "examples/manifest-check/manifest-check.sh"} {
		b, err := os.ReadFile(rel)
		if err != nil {
			t.Fatal(err)
		}
		files[rel] = string(b)
	}
	for rel, body := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

var (
	goEnvOnce sync.Once
	goEnv     []string
	goEnvErr  error
)

// scrubbedEnv is the environment the judge gives the script: no inherited variables, a throwaway HOME
// and TMPDIR, PATH with the Go toolchain and the system directories only. The Go build and module caches
// stay this process's own, so the runs do not rebuild the standard library.
func scrubbedEnv(t *testing.T) []string {
	t.Helper()
	goEnvOnce.Do(func() {
		var out []byte
		out, goEnvErr = exec.Command("go", "env", "GOROOT", "GOCACHE", "GOMODCACHE").Output()
		if goEnvErr != nil {
			return
		}
		v := strings.Split(strings.TrimSpace(string(out)), "\n")
		if len(v) != 3 {
			goEnvErr = errors.New("go env: unexpected output: " + string(out))
			return
		}
		goEnv = []string{
			"PATH=" + filepath.Join(v[0], "bin") + ":/usr/bin:/bin",
			"GOCACHE=" + v[1],
			"GOMODCACHE=" + v[2],
		}
	})
	if goEnvErr != nil {
		t.Fatal(goEnvErr)
	}
	return append(append([]string{}, goEnv...), "HOME="+t.TempDir(), "TMPDIR="+t.TempDir())
}

// runRepoManifest runs ./scripts/manifest-check.sh in dir with env added to a scrubbed environment (a
// later entry overrides an earlier one).
func runRepoManifest(t *testing.T, dir string, env []string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("./scripts/manifest-check.sh", args...)
	cmd.Dir = dir
	cmd.Env = append(scrubbedEnv(t), env...)
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

const manifestTest = "package app\n\nimport \"testing\"\n\nfunc TestLogin(t *testing.T) {\n" +
	"\tt.Run(\"[R01] login works\", func(t *testing.T) {})\n}\n"

// agent-jobs' own scripts/manifest-check.sh fetches the modules, then runs the reference check, which
// runs go test itself: a passing tagged test is exit 0.
func TestRepoManifestCheckRunsReference(t *testing.T) {
	dir := manifestRepo(t, "module example.com/app\n\ngo 1.27\n", manifestTest)
	out, code := runRepoManifest(t, dir, nil)
	if code != 0 || !strings.Contains(out, "ok       R01  TestLogin/[R01]_login_works") ||
		!strings.Contains(out, "manifest-check: 1 of 1 live requirements have a passing tagged test") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

// Only the tagged tests run first: an untagged failing test does not run. A tag the scan misses (here
// in a package-level variable) is found by the whole suite, which runs when the tagged run leaves a
// requirement missing.
func TestRepoManifestCheckTaggedTestsFirst(t *testing.T) {
	failing := manifestTest + "\nfunc TestUntagged(t *testing.T) { t.Fatal(\"must not run\") }\n"
	out, code := runRepoManifest(t, manifestRepo(t, "module example.com/app\n\ngo 1.27\n", failing), nil)
	if code != 0 || !strings.Contains(out, "manifest-check: go test exit 0") || strings.Contains(out, "whole suite") {
		t.Fatalf("tagged run: exit %d:\n%s", code, out)
	}

	hidden := "package app\n\nimport \"testing\"\n\nvar loginName = \"[R01] login works\"\n\n" +
		"func TestLogin(t *testing.T) { t.Run(loginName, func(t *testing.T) {}) }\n\n" +
		"func TestOther(t *testing.T) {\n\tt.Run(\"[R02] other\", func(t *testing.T) {})\n}\n"
	out, code = runRepoManifest(t, manifestRepo(t, "module example.com/app\n\ngo 1.27\n", hidden), nil)
	if code != 0 || !strings.Contains(out, "running the whole suite") || !strings.Contains(out, "ok       R01  TestLogin/[R01]_login_works") {
		t.Fatalf("fallback: exit %d:\n%s", code, out)
	}
}

// Modules that cannot be fetched (a cold cache without network; here the proxy is off) fail fast with
// exit 2 and one line, not as build failures that read as missing requirements; with a go test -json
// file the script fetches nothing.
func TestRepoManifestCheckColdCacheWithoutNetwork(t *testing.T) {
	gomod := "module example.com/app\n\ngo 1.27\n\nrequire example.invalid/nothing v1.0.0\n"
	dir := manifestRepo(t, gomod, manifestTest)
	offline := []string{"GOPROXY=off", "GOMODCACHE=" + t.TempDir(), "GOFLAGS=-mod=mod"}
	out, code := runRepoManifest(t, dir, offline)
	if code != 2 || !strings.Contains(out, "manifest-check: go mod download failed") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	json, err := filepath.Abs("examples/manifest-check/testdata/pass.json")
	if err != nil {
		t.Fatal(err)
	}
	checklist, err := filepath.Abs("examples/manifest-check/testdata/CHECKLIST.md")
	if err != nil {
		t.Fatal(err)
	}
	out, code = runRepoManifest(t, dir, append(offline, "MANIFEST_CHECKLIST="+checklist), json)
	if code != 0 || strings.Contains(out, "go mod download") {
		t.Fatalf("with a file: exit %d:\n%s", code, out)
	}
}

// The checklist is read once, before any test runs: a tagged test that removes a row from the working
// tree's checklist (here R01's test drops R02) cannot make the whole-suite run pass without R02.
func TestRepoManifestCheckChecklistSnapshot(t *testing.T) {
	test := `package app

import (
	"os"
	"strings"
	"testing"
)

func TestLogin(t *testing.T) {
	t.Run("[R01] login works", func(t *testing.T) {
		b, err := os.ReadFile("brief/CHECKLIST.md")
		if err != nil {
			return
		}
		var keep []string
		for _, l := range strings.Split(string(b), "\n") {
			if !strings.Contains(l, "| R02 |") {
				keep = append(keep, l)
			}
		}
		if err := os.WriteFile("brief/CHECKLIST.md", []byte(strings.Join(keep, "\n")), 0o644); err != nil {
			t.Fatal(err)
		}
	})
}
`
	checklist := manifestChecklist + "| R02 | «a logout» | live | brief |\n"
	dir := manifestRepoWithChecklist(t, "module example.com/app\n\ngo 1.27\n", test, checklist)
	out, code := runRepoManifest(t, dir, nil)
	if code != 1 || !strings.Contains(out, "MISSING  R02  no passing test tagged [R02]") ||
		!strings.Contains(out, "manifest-check: 1 of 2 live requirements have a passing tagged test") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	b, err := os.ReadFile(filepath.Join(dir, "brief", "CHECKLIST.md"))
	if err != nil || strings.Contains(string(b), "R02") {
		t.Fatalf("the R01 test did not remove R02 from the checklist (%v):\n%s", err, b)
	}
}

// Without a timeout binary the module download is still bounded by MANIFEST_DOWNLOAD_TIMEOUT: a go that
// hangs is stopped after the limit and the script exits 2 with one line.
func TestRepoManifestCheckDownloadBoundedWithoutTimeout(t *testing.T) {
	bin := t.TempDir()
	for _, tool := range []string{"awk", "cat", "cp", "dirname", "find", "mktemp", "paste", "rm", "sleep", "sort"} {
		p, err := exec.LookPath(tool)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(p, filepath.Join(bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte("#!/bin/sh\nexec sleep 20\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := manifestRepo(t, "module example.com/app\n\ngo 1.27\n", manifestTest)
	start := time.Now()
	out, code := runRepoManifest(t, dir, []string{"PATH=" + bin, "MANIFEST_DOWNLOAD_TIMEOUT=1"})
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("took %v with a limit of 1s:\n%s", elapsed, out)
	}
	if code != 2 || !strings.Contains(out, "manifest-check: go mod download did not finish within 1s") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}
