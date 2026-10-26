package agentjobs

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// manifestRepo lays out a copy of a repository the way the judge's wrapper does (a plain directory, no
// .git) with agent-jobs' scripts/manifest-check.sh, the reference script it runs, a checklist with R01
// live and the given go.mod and test file.
func manifestRepo(t *testing.T, gomod, test string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"go.mod":             gomod,
		"app_test.go":        test,
		"brief/CHECKLIST.md": "# Checklist\n\n| ID | Brief quote | Contract | Rationale |\n|---|---|---|---|\n| R01 | «a login» | live | brief |\n",
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

// runRepoManifest runs ./scripts/manifest-check.sh in dir with env added to this process's environment.
func runRepoManifest(t *testing.T, dir string, env []string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("./scripts/manifest-check.sh", args...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "TMPDIR="+t.TempDir()), env...)
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
