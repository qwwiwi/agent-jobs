package jobs

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"syscall"
	"testing"
)

// processGone reports whether process pid has ended: it is gone, or a zombie that waits for its parent
// (a killed child of the fake git is reparented, and a container may have no reaper for it). Linux reads
// the state from /proc.
func processGone(t *testing.T, pid int) bool {
	t.Helper()
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return true
	}
	if err != nil {
		t.Fatal(err)
	}
	// The state follows the command name, which is in parentheses and may hold any character.
	i := bytes.LastIndexByte(b, ')')
	if i < 0 || i+2 >= len(b) {
		t.Fatalf("/proc/%d/stat: %q", pid, b)
	}
	return b[i+2] == 'Z' || b[i+2] == 'X'
}
