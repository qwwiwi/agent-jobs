package mcp

import (
	"io"
	"os"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

// errNotRegular marks a job file that is not a regular file of this user with one link (FIFO, device,
// socket, directory, symlink, hard link, another owner), or a job dir reached through a symlink.
var errNotRegular = jobs.ErrNotRegular

// getuid is os.Getuid; a test swaps it to see a file of another owner refused.
var getuid = os.Getuid

// ReadJobReport reads at most max bytes of <stateDir>/jobs/<id>/report.md; truncated reports whether
// there was more. The job dir is writable by the job, so it is opened with jobs.OpenJobFile: the walk
// never leaves the state dir or follows a symlink, and only a regular file of this user with exactly
// one link is read.
func ReadJobReport(stateDir, id string, max int) (string, bool, error) {
	f, _, err := jobs.OpenJobFile(stateDir, id, "report.md", getuid())
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, int64(max)+1))
	if err != nil {
		return "", false, err
	}
	if len(b) > max {
		return string(b[:max]), true, nil
	}
	return string(b), false, nil
}
