//go:build !linux

package agentjobs_test

import "errors"

// setChildSubreaper is Linux only; elsewhere newReaper skips the tests before the reaper runs.
func setChildSubreaper() error {
	return errors.New("child subreaper is Linux only")
}
