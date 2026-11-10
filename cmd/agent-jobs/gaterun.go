package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

const gateRunUsage = "usage: agent-jobs gate run --head REV [--repo DIR] [--timeout D] [--lock-wait D] -- CMD [ARGS...]"

// gateRunTestLock is set only by this package's tests (a lock in their temp dir); every real run takes the
// one box-wide lock, jobs.GateLockPath(): no flag or environment variable picks another, so no two gates on
// the box can lock different files.
var gateRunTestLock string

// cmdGateRun is `agent-jobs gate run` (#134): one heavy local gate at a time per repository, at most
// jobs.GateMaxParallelEnvVar (default 2) per box (v0.8.2), on the asserted
// HEAD, in a process group of its own, without TELEGRAM_*, always ending with exit=N on stdout
// (jobs.RunGate). It returns the exit code; a usage error is GateExitRunner and prints the marker too.
func cmdGateRun(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gate run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repo := fs.String("repo", ".", "the git work tree the command runs in")
	head := fs.String("head", "", "the commit HEAD must be at (required)")
	// 0 (not given): the job's gate timeout (jobs.GateTimeoutEnvVar, from its card or config), else
	// jobs.DefaultGateTimeout. An explicit value wins.
	timeout := fs.Duration("timeout", 0, "the command's time limit, then its process group is killed (default: $"+
		jobs.GateTimeoutEnvVar+", else "+jobs.DefaultGateTimeout.String()+")")
	lockWait := fs.Duration("lock-wait", jobs.DefaultGateLockWait, "how long to wait for another gate on this box")
	// Flags stop at the command: what follows (its own flags too) is passed through untouched.
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(stderr, gateRunUsage)
		}
		fmt.Fprintf(stdout, "exit=%d\n", jobs.GateExitRunner)
		return jobs.GateExitRunner
	}
	if fs.NArg() == 0 || *timeout < 0 || *lockWait < 0 {
		fmt.Fprintln(stderr, gateRunUsage)
		fmt.Fprintf(stdout, "exit=%d\n", jobs.GateExitRunner)
		return jobs.GateExitRunner
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer stop()
	return jobs.RunGate(ctx, jobs.GateRun{Dir: *repo, Head: *head, Argv: fs.Args(), Timeout: *timeout, LockWait: *lockWait,
		LockPath: gateRunTestLock, Env: os.Environ(), Stdout: stdout, Stderr: stderr})
}
