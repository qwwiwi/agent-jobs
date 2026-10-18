package main

import (
	"strings"
	"testing"
)

// `http-api token` prints a token only with --rotate; without it, nothing is created or printed.
func TestHTTPAPITokenNeedsRotate(t *testing.T) {
	t.Setenv("AGENT_JOBS_HOME", t.TempDir())
	if err := cmdHTTPAPI(nil); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("no subcommand: %v", err)
	}
	if err := cmdHTTPAPI([]string{"show", "--agent", "test"}); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("unknown subcommand: %v", err)
	}
	out := captureStdoutErr(t, func() error { return cmdHTTPAPI([]string{"token", "--agent", "test"}) })
	if out.err == nil || !strings.Contains(out.err.Error(), "--rotate") || out.stdout != "" {
		t.Fatalf("token without --rotate: %v, stdout %q", out.err, out.stdout)
	}
}

type captured struct {
	stdout string
	err    error
}

// captureStdoutErr is captureStdout for a call that is expected to fail.
func captureStdoutErr(t *testing.T, f func() error) captured {
	t.Helper()
	var c captured
	c.stdout = captureStdout(t, func() error { c.err = f(); return nil })
	return c
}
