//go:build !linux

package jobs

import (
	"errors"
	"os/exec"
)

// Without Landlock the key file cannot be kept from a job's processes: while providers.openrouter is
// configured, the wrapper refuses to start any job (wrapperKeySandbox).
func landlockAvailable() error {
	return errors.New("the key sandbox needs Linux Landlock: openrouter jobs (and, while providers.openrouter is configured, every job) run on Linux only")
}

func (s *keySandbox) start(cmd *exec.Cmd) error { return landlockAvailable() }
