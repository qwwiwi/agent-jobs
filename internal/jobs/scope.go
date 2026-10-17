package jobs

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Cgroup containment (Linux, systemd). The daemon launches every wrapper in its own transient systemd
// user scope, agent-jobs-<agent>-<id>.scope. A cgroup is inherited by every descendant and cannot be
// left with setsid, a double fork or re-parenting, so:
//
//   - control commands refuse when /proc/self/cgroup names an agent-jobs-* scope (CheckNotInJob);
//   - the wrapper ends every other process of its scope's cgroup before it writes exit_code;
//   - cancel, timeout and every collection stop the whole unit (systemctl --user stop), so no
//     descendant survives the job.
//
// The scope lives under the user manager's app.slice, not under the daemon's service cgroup, so a
// daemon restart (KillMode=process, or even control-group) never touches it.
//
// Fallback (no systemd-run --user: macOS, containers, no user bus): the session containment of the
// sweep and the marker, ancestry, session and store checks. It is weaker: a process that calls setsid
// leaves the session (docs/ARCHITECTURE.md, «Cgroup containment»).
//
// What a scope does not close: a process of the same user can write its pid into another cgroup's
// cgroup.procs under the user manager, or ask the user manager for a new unit over D-Bus. The wrapper
// removes the bus address from the job's environment and the guard rejects the direct spellings
// (systemd-run, systemctl --user, busctl, dbus-send, gdbus, writes under /sys/fs/cgroup), but a
// script the job writes is not inspected (not a sandbox).

// Containment modes (config cgroup_scope, env AGENT_JOBS_CGROUP_SCOPE).
const (
	ScopeAuto     = "auto"
	ScopeRequired = "required"
	ScopeOff      = "off"
)

// CgroupScopeModes are the accepted values of cgroup_scope ("" = auto).
var CgroupScopeModes = []string{"", ScopeAuto, ScopeRequired, ScopeOff}

// ScopePrefix starts the name of every job scope unit.
const ScopePrefix = "agent-jobs-"

// ScopeUnit is the transient scope unit of a job. Agent names ([a-z0-9_-]) and job ids
// (digits, '-', hex) are valid unit-name characters, so no escaping is needed.
func ScopeUnit(agent, id string) string { return ScopePrefix + agent + "-" + id + ".scope" }

// isJobScopeName reports a cgroup path component that is a job scope (agent-jobs-*.scope).
func isJobScopeName(c string) bool {
	return len(c) > len(ScopePrefix)+len(".scope") && strings.HasPrefix(c, ScopePrefix) && strings.HasSuffix(c, ".scope")
}

// cgroupFile is /proc/self/cgroup and cgroupRoot the cgroup v2 mount; variables so tests can stand in.
var (
	cgroupFile = "/proc/self/cgroup"
	cgroupRoot = "/sys/fs/cgroup"
)

// jobScopeIn returns the first job scope named in the content of a /proc/<pid>/cgroup file (every
// hierarchy line, every path component), or "".
func jobScopeIn(content string) string {
	for _, line := range strings.Split(content, "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) < 3 {
			continue
		}
		for _, c := range strings.Split(parts[2], "/") {
			if isJobScopeName(c) {
				return c
			}
		}
	}
	return ""
}

// JobScope returns the job scope this process runs in ("" = none). ok is false when the check could
// not run (not Linux, /proc/self/cgroup unreadable): the caller fails open, the other checks still apply.
func JobScope() (unit string, ok bool) {
	if runtime.GOOS != "linux" {
		return "", false
	}
	b, err := os.ReadFile(cgroupFile)
	if err != nil {
		return "", false
	}
	return jobScopeIn(string(b)), true
}

// unifiedPath returns the cgroup v2 path ("0::<path>") of a /proc/<pid>/cgroup content.
func unifiedPath(content string) (string, bool) {
	for _, line := range strings.Split(content, "\n") {
		if p, ok := strings.CutPrefix(line, "0::"); ok {
			return strings.TrimSpace(p), true
		}
	}
	return "", false
}

// procCgroup returns the cgroup v2 path of pid.
func procCgroup(pid int) (string, error) {
	b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return "", err
	}
	p, ok := unifiedPath(string(b))
	if !ok {
		return "", fmt.Errorf("/proc/%d/cgroup has no cgroup v2 path", pid)
	}
	return p, nil
}

// inSubtree reports whether cgroup path p is scope or a cgroup below it.
func inSubtree(p, scope string) bool { return p == scope || strings.HasPrefix(p, scope+"/") }

// ownJobScope returns this process's cgroup v2 path when its last component is want (the wrapper's
// expected scope), else "". Only then does the wrapper sweep by cgroup.
func ownJobScope(want string) string {
	if runtime.GOOS != "linux" || want == "" {
		return ""
	}
	b, err := os.ReadFile(cgroupFile)
	if err != nil {
		return ""
	}
	p, ok := unifiedPath(string(b))
	if !ok || filepath.Base(p) != want {
		return ""
	}
	return p
}

// cgroupMembers lists the pids in the cgroup subtree at path (cgroup v2), except this process. A
// sub-cgroup that vanishes during the walk is skipped; an unreadable root is an error.
func cgroupMembers(path string) ([]int, error) {
	root := filepath.Join(cgroupRoot, path)
	self := os.Getpid()
	seen := map[int]bool{}
	var out []int
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == root {
				return err
			}
			return nil // a sub-cgroup removed meanwhile
		}
		if !d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(filepath.Join(p, "cgroup.procs"))
		if err != nil {
			if p == root {
				return err
			}
			return nil
		}
		for _, f := range strings.Fields(string(b)) {
			if pid, err := strconv.Atoi(f); err == nil && pid != self && !seen[pid] {
				seen[pid] = true
				out = append(out, pid)
			}
		}
		return nil
	})
	return out, err
}

// wrapperScope returns the scope unit the wrapper for jobDir (<state>/jobs/<id>, state = <home>/<agent>)
// is launched in by the daemon.
func wrapperScope(jobDir string) string {
	abs, err := filepath.Abs(jobDir)
	if err != nil {
		return ""
	}
	return ScopeUnit(filepath.Base(filepath.Dir(filepath.Dir(abs))), filepath.Base(abs))
}

// userBusEnv returns env with XDG_RUNTIME_DIR pointing at /run/user/<uid> when neither it nor
// DBUS_SESSION_BUS_ADDRESS is set and the user bus socket exists there (a manual `tick` from a login
// shell without the variable, or a daemon started outside the user manager).
func userBusEnv(env []string) []string {
	for _, kv := range env {
		if k, v, _ := strings.Cut(kv, "="); (k == "XDG_RUNTIME_DIR" || k == "DBUS_SESSION_BUS_ADDRESS") && v != "" {
			return env
		}
	}
	dir := fmt.Sprintf("/run/user/%d", os.Getuid())
	if fi, err := os.Stat(filepath.Join(dir, "bus")); err == nil && fi.Mode()&fs.ModeSocket != 0 {
		return append(append([]string{}, env...), "XDG_RUNTIME_DIR="+dir)
	}
	return env
}

// systemdCmd builds a systemd-run/systemctl call against the user manager.
func systemdCmd(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = userBusEnv(os.Environ())
	return cmd
}

// scopeMode is the effective containment mode (env AGENT_JOBS_CGROUP_SCOPE wins over config).
func (c Config) scopeMode() string {
	m := c.CgroupScope
	if e := os.Getenv("AGENT_JOBS_CGROUP_SCOPE"); e != "" {
		m = e
	}
	if m == "" {
		return ScopeAuto
	}
	return m
}

// ProbeScope checks that a transient user scope can be created right now: it runs `true` in a
// throwaway scope. Linux only.
func ProbeScope() error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("cgroup scopes need Linux with systemd (this is %s)", runtime.GOOS)
	}
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return fmt.Errorf("systemd-run not found: %w", err)
	}
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := systemdCmd(ctx, "systemd-run", "--user", "--scope", "--quiet", "--collect",
		"--unit="+ScopePrefix+"probe-"+hex.EncodeToString(b)+".scope", "--", "true")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("systemd-run --user --scope: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// scopeArgv is the systemd-run prefix that puts the wrapper into its scope. systemd-run --scope
// creates the unit, moves itself into it and then execs the wrapper, so the spawned pid is the
// wrapper's. TimeoutStopSec is the TERM→KILL grace of a stop.
func scopeArgv(unit, description string, grace time.Duration) []string {
	secs := int(grace.Round(time.Second) / time.Second)
	if secs < 1 {
		secs = 1
	}
	return []string{"systemd-run", "--user", "--scope", "--quiet", "--collect",
		"--unit=" + unit, "--description=" + description,
		fmt.Sprintf("--property=TimeoutStopSec=%ds", secs), "--"}
}

// scopeFor decides the containment of a launch: the scope unit to launch in, or "" for session
// containment only. In required mode a missing scope refuses the launch.
func (m *Manager) scopeFor(id string) (string, error) {
	switch mode := m.Cfg.scopeMode(); mode {
	case ScopeOff:
		return "", nil
	case ScopeAuto, ScopeRequired:
		if err := ProbeScope(); err != nil {
			if mode == ScopeRequired {
				return "", fmt.Errorf("cgroup_scope is required but no scope can be created: %w", err)
			}
			m.Log.Printf("job %s: no cgroup scope (%v); session containment only (weaker, see docs/ARCHITECTURE.md)", id, err)
			return "", nil
		}
		return ScopeUnit(m.Agent, id), nil
	default:
		return "", fmt.Errorf("cgroup_scope %q must be one of auto|required|off", mode)
	}
}

// errScopeActive means the unit is still active after a stop.
var errScopeActive = errors.New("scope still active after stop")

// stopScope stops a job's scope: systemd sends SIGTERM to every process in the unit's cgroup, waits
// TimeoutStopSec, sends SIGKILL, and returns once the cgroup is empty. A unit that no longer exists
// (the wrapper exited and the scope was collected) is fine. The unit is always recomputed from the
// agent and the job id, never read from a file the job could write.
func (m *Manager) stopScope(id string) error {
	unit := ScopeUnit(m.Agent, id)
	ctx, cancel := context.WithTimeout(context.Background(), m.KillGrace+30*time.Second)
	defer cancel()
	var stderr bytes.Buffer
	cmd := systemdCmd(ctx, "systemctl", "--user", "stop", unit)
	cmd.Stderr = &stderr
	err := cmd.Run()
	state, serr := scopeState(unit)
	if serr == nil && (state == "inactive" || state == "failed" || state == "") {
		return nil
	}
	if err != nil {
		return fmt.Errorf("systemctl --user stop %s: %w: %s", unit, err, strings.TrimSpace(stderr.String()))
	}
	if serr != nil {
		return serr
	}
	return fmt.Errorf("%s: %w (%s)", unit, errScopeActive, state)
}

// scopeState returns the ActiveState of a unit ("inactive" for a unit that is not loaded).
func scopeState(unit string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := systemdCmd(ctx, "systemctl", "--user", "show", "--property=ActiveState", "--value", unit).Output()
	if err != nil {
		return "", fmt.Errorf("systemctl --user show %s: %w", unit, err)
	}
	return strings.TrimSpace(string(out)), nil
}
