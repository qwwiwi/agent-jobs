package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	agentjobs "github.com/qwwiwi/agent-jobs"
)

const uiCheckUsage = `usage: agent-jobs ui-check <base-url> <path>... [--out dir] [--schemes auto|light|dark|light,dark]
                         [--timeout ms] [--dwell ms] [--limit n] [--playwright path]
  runs the browser check embedded in this binary (scripts/ui-check.mjs) in trusted mode: never a job's
  <job dir>/tools/ui-check.mjs, never Playwright from the current directory (absolute --playwright,
  absolute NODE_PATH entries or npm root -g). Exit 0 pass, 1 fail, 2 not run.
`

// runUICheck is the orchestrator's browser check (#135). A job can write its own job dir and worktree, so
// neither <job dir>/tools/ui-check.mjs nor a node_modules under the current directory may run with the
// orchestrator's privileges: the script embedded in the binary is written to a fresh private temp dir
// (0700, removed afterwards) and run with --trusted, which resolves Playwright only from an absolute
// --playwright, absolute NODE_PATH entries or the global npm root. Relative NODE_PATH entries are also
// dropped from node's environment, so Playwright's own requires cannot fall back to the current directory.
func runUICheck(args []string, stdout, stderr io.Writer) int {
	for _, a := range args {
		if a == "-h" || a == "--help" {
			fmt.Fprint(stdout, uiCheckUsage)
			return 0
		}
	}
	node, err := exec.LookPath("node")
	if err != nil {
		// exec.LookPath refuses a node found through a relative PATH entry (exec.ErrDot) as well.
		fmt.Fprintf(stdout, "BROWSER CHECK NOT RUN: node not found: %v\n", err)
		return 2
	}
	script, err := agentjobs.Templates.ReadFile("scripts/ui-check.mjs")
	if err != nil {
		fmt.Fprintf(stdout, "BROWSER CHECK NOT RUN: embedded ui-check.mjs: %v\n", err)
		return 2
	}
	dir, err := os.MkdirTemp("", "agent-jobs-ui-check-")
	if err != nil {
		fmt.Fprintf(stdout, "BROWSER CHECK NOT RUN: temp dir: %v\n", err)
		return 2
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "ui-check.mjs")
	if err := os.WriteFile(path, script, 0o600); err != nil {
		fmt.Fprintf(stdout, "BROWSER CHECK NOT RUN: write ui-check.mjs: %v\n", err)
		return 2
	}
	cmd := exec.Command(node, append([]string{path, "--trusted"}, args...)...)
	cmd.Env = uiCheckEnv(os.Environ())
	cmd.Stdin = nil
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() >= 0 {
			return ee.ExitCode()
		}
		fmt.Fprintf(stdout, "BROWSER CHECK NOT RUN: node: %v\n", err)
		return 2
	}
	return 0
}

// uiCheckEnv keeps only the absolute entries of NODE_PATH (a relative one resolves against the current
// directory) and clears NODE_OPTIONS, which could preload a module (--require, --import) from it.
func uiCheckEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		switch {
		case strings.HasPrefix(kv, "NODE_OPTIONS="):
			continue
		case strings.HasPrefix(kv, "NODE_PATH="):
			var keep []string
			for _, p := range filepath.SplitList(strings.TrimPrefix(kv, "NODE_PATH=")) {
				if filepath.IsAbs(p) {
					keep = append(keep, p)
				}
			}
			if len(keep) == 0 {
				continue
			}
			kv = "NODE_PATH=" + strings.Join(keep, string(os.PathListSeparator))
		}
		out = append(out, kv)
	}
	return out
}
