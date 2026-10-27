package jobs

import (
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// Processes that run a binary from a finished job's temp dir (#105, doctor procs): the test daemons
// and systemd units a job's `go test` built there and left running. Report only; the temp dir itself
// stays until prune removes it with the job folder.

// deletedSuffix is what the kernel appends to a /proc link whose file was unlinked.
const deletedSuffix = " (deleted)"

// pathVariants returns dir cleaned and, when it differs, with its symlinks resolved: /proc names the
// real path.
func pathVariants(dir string) []string {
	out := []string{filepath.Clean(dir)}
	if r, err := filepath.EvalSymlinks(dir); err == nil && r != out[0] {
		out = append(out, r)
	}
	return out
}

// procLink reads the /proc/<pid>/<name> link ("" when unreadable).
func procLink(pid int, name string) string {
	p, err := os.Readlink(filepath.Join(procRoot, strconv.Itoa(pid), name))
	if err != nil {
		return ""
	}
	return p
}

// tmpBinaryJob returns the id of the job whose temp dir (<root>/<id>/tmp/…, root one of the variants
// of this agent's jobs dir) holds the executable pid runs: its /proc exe link (also once the file is
// deleted) or an absolute argv[0]. "" when none.
func tmpBinaryJob(jobsRoots []string, pid int, argv []string) string {
	cands := []string{strings.TrimSuffix(procLink(pid, "exe"), deletedSuffix)}
	if len(argv) > 0 && filepath.IsAbs(argv[0]) {
		cands = append(cands, argv[0])
	}
	for _, root := range jobsRoots {
		for _, p := range cands {
			if !filepath.IsAbs(p) {
				continue
			}
			rel, err := filepath.Rel(root, filepath.Clean(p))
			if err != nil {
				continue
			}
			parts := strings.Split(rel, string(filepath.Separator))
			if len(parts) >= 3 && parts[0] != ".." && parts[0] != "." && parts[1] == JobTmpDir {
				return parts[0]
			}
		}
	}
	return ""
}

// procServiceUnit returns the systemd service unit pid runs in (the last element of its cgroup v2
// path when it is a .service), or "".
func procServiceUnit(pid int) string {
	cg, err := procCgroup(pid)
	if err != nil {
		return ""
	}
	if b := path.Base(cg); strings.HasSuffix(b, ".service") {
		return Printable(b, 120)
	}
	return ""
}
