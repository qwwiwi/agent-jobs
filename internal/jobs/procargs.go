package jobs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

// Process identity where there is no /proc (macOS): sysctl kern.proc.pid gives a process's group and
// state, kern.procargs2 its argv and environment, kern.proc.pgrp the members of a process group. The
// calls live in procargs_darwin.go; elsewhere they fail (procargs_other.go), so nothing is signalled.

// sysctlInfo is what sysctl tells about one process.
type sysctlInfo struct {
	pgid   int
	zombie bool
	argv   []string
	env    []string
}

// errNoSysctlIdentity means this OS reads no process identity through sysctl.
var errNoSysctlIdentity = errors.New("no process identity check on this OS")

// parseProcArgs2 parses a kern.procargs2 buffer: a 32-bit argc (little-endian on every macOS
// architecture), the executable path NUL-terminated and NUL-padded, argc NUL-terminated arguments, then
// the environment up to the first empty string (Apple's own strings follow it).
func parseProcArgs2(b []byte) (argv, env []string, err error) {
	if len(b) < 4 {
		return nil, nil, fmt.Errorf("procargs2: %d bytes", len(b))
	}
	argc := int(binary.LittleEndian.Uint32(b))
	rest := b[4:]
	next := func() (string, bool) {
		i := bytes.IndexByte(rest, 0)
		if i < 0 {
			return "", false
		}
		s := string(rest[:i])
		rest = rest[i+1:]
		return s, true
	}
	if _, ok := next(); !ok {
		return nil, nil, errors.New("procargs2: no executable path")
	}
	for len(rest) > 0 && rest[0] == 0 {
		rest = rest[1:]
	}
	if argc < 0 || argc > len(rest) {
		return nil, nil, fmt.Errorf("procargs2: argc %d out of range", argc)
	}
	for range argc {
		s, ok := next()
		if !ok {
			return nil, nil, errors.New("procargs2: truncated argv")
		}
		argv = append(argv, s)
	}
	for {
		s, ok := next()
		if !ok || s == "" {
			return argv, env, nil
		}
		env = append(env, s)
	}
}
