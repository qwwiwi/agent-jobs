//go:build !linux

package jobs

import "os/exec"

// parentDeathSignalWorks: no parent death signal outside Linux; only t.Cleanup ends a test's child.
const parentDeathSignalWorks = false

func setParentDeathSignal(*exec.Cmd) {}
