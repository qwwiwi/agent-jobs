package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// writeMarkerModule writes a CommonJS module at file that leaves a marker file when it is loaded and
// exports a chromium whose launch fails (so a run that loads it ends at exit 2 without a browser).
func writeMarkerModule(t *testing.T, file, marker string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	src := "require('fs').writeFileSync(" + strconv.Quote(marker) + ", 'loaded');\n" +
		"module.exports = { chromium: { launch: async () => { throw new Error('fake chromium'); } } };\n"
	if err := os.WriteFile(file, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
}

// #135: the orchestrator's `agent-jobs ui-check` runs the embedded script, never a job's planted
// <job dir>/tools/ui-check.mjs, and never resolves Playwright from a node_modules under the job worktree
// it stands in (nor through a relative NODE_PATH entry or a relative --playwright).
func TestUICheckTrustedIgnoresPlantedCode(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed")
	}
	tmp := t.TempDir()
	marks := filepath.Join(tmp, "marks")
	if err := os.Mkdir(marks, 0o755); err != nil {
		t.Fatal(err)
	}
	jobDir := filepath.Join(tmp, "job")
	worktree := filepath.Join(jobDir, "work")
	plantedTool := filepath.Join(marks, "planted-tool")
	plantedPW := filepath.Join(marks, "planted-playwright")
	// A job-controlled copy of the tool and a planted Playwright in the worktree.
	tool := filepath.Join(jobDir, "tools", "ui-check.mjs")
	if err := os.MkdirAll(filepath.Dir(tool), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tool, []byte("import { writeFileSync } from 'node:fs';\nwriteFileSync("+
		strconv.Quote(plantedTool)+", 'ran');\nprocess.exit(0);\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"playwright", "playwright-core", "@playwright/test"} {
		pkg := filepath.Join(worktree, "node_modules", filepath.FromSlash(name))
		writeMarkerModule(t, filepath.Join(pkg, "index.js"), plantedPW)
		if err := os.WriteFile(filepath.Join(pkg, "package.json"),
			[]byte(`{"name":"`+name+`","main":"index.js"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The trusted Playwright the orchestrator names explicitly.
	trustedMark := filepath.Join(marks, "trusted-playwright")
	trustedPW := filepath.Join(tmp, "trusted", "playwright", "index.js")
	writeMarkerModule(t, trustedPW, trustedMark)

	t.Chdir(worktree)
	t.Setenv("NODE_PATH", "node_modules")                             // relative: resolves to the worktree
	t.Setenv("NODE_OPTIONS", "--require ./node_modules/playwright")   // would preload the planted module
	t.Setenv("npm_config_prefix", filepath.Join(tmp, "empty-prefix")) // npm root -g finds nothing

	run := func(args ...string) (int, string) {
		var out, errb bytes.Buffer
		code := runUICheck(args, &out, &errb)
		return code, out.String() + errb.String()
	}
	planted := func(t *testing.T) {
		t.Helper()
		for _, m := range []string{plantedTool, plantedPW} {
			if _, err := os.Stat(m); err == nil {
				t.Fatalf("job-controlled code ran: %s exists", filepath.Base(m))
			}
		}
	}

	t.Run("no playwright flag", func(t *testing.T) {
		code, out := run("http://127.0.0.1:9", "/", "--out", filepath.Join(tmp, "out1"))
		planted(t)
		if code != 2 || !strings.Contains(out, "BROWSER CHECK NOT RUN: Playwright not found") ||
			!strings.Contains(out, "trusted mode") {
			t.Fatalf("exit %d, want 2 with Playwright not found in trusted mode:\n%s", code, out)
		}
	})
	t.Run("relative playwright flag", func(t *testing.T) {
		code, out := run("http://127.0.0.1:9", "/", "--playwright", "node_modules/playwright")
		planted(t)
		if code != 2 || !strings.Contains(out, "must be absolute") {
			t.Fatalf("exit %d, want 2 with must be absolute:\n%s", code, out)
		}
	})
	t.Run("absolute playwright flag", func(t *testing.T) {
		code, out := run("http://127.0.0.1:9", "/", "--playwright", filepath.Dir(trustedPW))
		planted(t)
		if code != 2 || !strings.Contains(out, "cannot launch Chromium: fake chromium") {
			t.Fatalf("exit %d, want 2 from the trusted fake's launch:\n%s", code, out)
		}
		if _, err := os.Stat(trustedMark); err != nil {
			t.Fatalf("the trusted --playwright was not loaded: %v", err)
		}
	})
	t.Run("help", func(t *testing.T) {
		code, out := run("--help")
		if code != 0 || !strings.Contains(out, "embedded in this binary") {
			t.Fatalf("exit %d:\n%s", code, out)
		}
	})
}

func TestUICheckEnv(t *testing.T) {
	sep := string(os.PathListSeparator)
	got := uiCheckEnv([]string{"A=1", "NODE_OPTIONS=--require x", "NODE_PATH=rel" + sep + "/abs/one" + sep + "./x", "B=2"})
	want := []string{"A=1", "NODE_PATH=/abs/one", "B=2"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got := uiCheckEnv([]string{"NODE_PATH=rel"}); len(got) != 0 {
		t.Fatalf("an all-relative NODE_PATH stays: %q", got)
	}
}
