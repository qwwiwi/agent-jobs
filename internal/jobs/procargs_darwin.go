//go:build darwin

package jobs

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// sZomb is SZOMB in <sys/proc.h>: a zombie's p_stat.
const sZomb = 5

// sysctlProc and sysctlGroup are the sysctl reads; variables so tests can stand in for them.
var (
	sysctlProc = func(pid int) (sysctlInfo, error) {
		before, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
		if err != nil {
			return sysctlInfo{}, err
		}
		if int(before.Proc.P_pid) != pid {
			return sysctlInfo{}, fmt.Errorf("pid %d: no such process", pid)
		}
		raw, err := unix.SysctlRaw("kern.procargs2", pid)
		if err != nil {
			return sysctlInfo{}, fmt.Errorf("kern.procargs2 %d: %w", pid, err)
		}
		argv, env, err := parseProcArgs2(raw)
		if err != nil {
			return sysctlInfo{}, err
		}
		// Read again: the same start time shows argv and env were read of the same process.
		after, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
		if err != nil || int(after.Proc.P_pid) != pid || after.Proc.P_starttime != before.Proc.P_starttime {
			return sysctlInfo{}, fmt.Errorf("pid %d changed while it was read", pid)
		}
		return sysctlInfo{pgid: int(after.Eproc.Pgid), zombie: after.Proc.P_stat == sZomb, argv: argv, env: env}, nil
	}
	sysctlGroup = func(g int) ([]int, error) {
		kps, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", g)
		if err != nil {
			return nil, err
		}
		pids := make([]int, 0, len(kps))
		for _, kp := range kps {
			pids = append(pids, int(kp.Proc.P_pid))
		}
		return pids, nil
	}
)
