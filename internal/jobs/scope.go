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
	"sync"
	"syscall"
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
// cgroup.procs under the user manager, ask the user manager for a new unit over D-Bus, or have a daemon
// outside the scope (cron, tmux, pm2, docker, ssh, …) start a process for it. The wrapper removes the
// bus address from the job's environment and the guard rejects the direct spellings (systemd-run,
// systemctl --user, D-Bus tools, crontab/at, tmux/screen, pm2, docker/podman, ssh, machinectl,
// loginctl, unshare/nsenter, writes under /sys/fs/cgroup), but a script the job writes is not
// inspected (not a sandbox).

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

// ownJobScope returns the cgroup v2 path of want (the wrapper's expected scope) when this process runs
// in it or in a sub-cgroup below it (a job can create one and move the wrapper there), else "". Only
// then does the wrapper sweep by cgroup, from the scope itself.
func ownJobScope(want string) string {
	if runtime.GOOS != "linux" || want == "" {
		return ""
	}
	b, err := os.ReadFile(cgroupFile)
	if err != nil {
		return ""
	}
	p, ok := unifiedPath(string(b))
	if !ok {
		return ""
	}
	parts := strings.Split(p, "/")
	for i, c := range parts {
		if c == want {
			return strings.Join(parts[:i+1], "/")
		}
	}
	return ""
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

// probeScope is ProbeScope; a variable so tests can make the user manager fail.
var probeScope = ProbeScope

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

// ScopeRetryWindow is how long a launch waits (requeued every tick) for the user manager on a host where
// a scope was created before, before auto falls back to session containment and required refuses.
const ScopeRetryWindow = 10 * time.Minute

// errScopeRetry asks launchQueued to put the job back into the queue: no scope right now, but this host
// had working scopes, so the failure is taken as transient (bus hiccup, user manager restarting).
var errScopeRetry = errors.New("no cgroup scope right now; job requeued")

// Host state of the probe, in the state dir: scope-ok once a scope was created on this host (never
// removed), scope-failing-since while probes fail after that (removed on the next success).
const (
	scopeOKFile      = "scope-ok"
	scopeFailingFile = "scope-failing-since"
)

// scopeFor decides the containment of a launch: the scope unit to launch in, or "" for session
// containment only. In required mode a missing scope refuses the launch. Once a scope worked on this
// host, a failing probe requeues the job (errScopeRetry) for up to ScopeRetryWindow instead of
// downgrading it (auto) or refusing it (required) on a transient failure.
func (m *Manager) scopeFor(id string) (string, error) {
	switch mode := m.Cfg.scopeMode(); mode {
	case ScopeOff:
		return "", nil
	case ScopeAuto, ScopeRequired:
		err := probeScope()
		if err == nil {
			_ = os.Remove(filepath.Join(m.StateDir, scopeFailingFile))
			if _, serr := os.Stat(filepath.Join(m.StateDir, scopeOKFile)); serr != nil {
				_ = os.WriteFile(filepath.Join(m.StateDir, scopeOKFile), []byte(m.Now().UTC().Format(time.RFC3339)+"\n"), 0o600)
			}
			return ScopeUnit(m.Agent, id), nil
		}
		if since, ok := m.scopeFailingSince(); ok && m.Now().Sub(since) < ScopeRetryWindow {
			return "", fmt.Errorf("%w (failing since %s): %v", errScopeRetry, since.UTC().Format(time.RFC3339), err)
		}
		if mode == ScopeRequired {
			return "", fmt.Errorf("cgroup_scope is required but no scope can be created: %w", err)
		}
		m.Log.Printf("job %s: no cgroup scope (%v); session containment only (weaker, see docs/ARCHITECTURE.md)", id, err)
		return "", nil
	default:
		return "", fmt.Errorf("cgroup_scope %q must be one of auto|required|off", mode)
	}
}

// scopeFailingSince returns when probes started failing on a host where a scope worked before (the
// first failure records it); ok is false on a host that never had a working scope.
func (m *Manager) scopeFailingSince() (time.Time, bool) {
	if _, err := os.Stat(filepath.Join(m.StateDir, scopeOKFile)); err != nil {
		return time.Time{}, false
	}
	f := filepath.Join(m.StateDir, scopeFailingFile)
	if b, err := os.ReadFile(f); err == nil {
		if t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(b))); err == nil {
			return t, true
		}
	}
	now := m.Now()
	_ = os.WriteFile(f, []byte(now.UTC().Format(time.RFC3339)+"\n"), 0o600)
	return now, true
}

// errScopeActive means the unit is still active (or its cgroup still has processes) after a stop.
var errScopeActive = errors.New("scope still active after stop")

// systemctlBin is the systemctl the daemon calls; a variable so tests can make a stop fail.
var systemctlBin = "systemctl"

// cgroupDirOwner returns the owner uid of a cgroup directory; a variable so tests can stand in.
var cgroupDirOwner = func(fi fs.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}

// systemctlCandidates are the only places the system-manager lookup takes systemctl from: never the
// daemon's PATH.
var systemctlCandidates = []string{"/usr/bin/systemctl", "/bin/systemctl"}

// systemManagerBin returns the systemctl that asks the system manager for the user manager's cgroup,
// resolved once from systemctlCandidates; apart from systemctlBin (the user-bus calls a test makes fail)
// so the lookup stays real in such a test. A variable so tests can stand in a stub.
var systemManagerBin = sync.OnceValues(func() (string, error) { return trustedBinary(systemctlCandidates) })

// systemManagerEnv is the whole environment of the lookup: nothing of the daemon's (no DBUS_* that could
// point it at another bus, no LD_*).
var systemManagerEnv = []string{"PATH=/usr/bin:/bin", "LANG=C"}

// systemManagerTimeout bounds the lookup; systemManagerWaitDelay bounds the wait for its pipes after the
// process exits or is killed (a descendant holding stdout would otherwise block Output). Variables for tests.
var (
	systemManagerTimeout   = 10 * time.Second
	systemManagerWaitDelay = 2 * time.Second
)

// trustedBinary returns the first candidate that resolves to a regular root-owned file, not group- or
// world-writable, in a root-owned directory that is not group- or world-writable.
func trustedBinary(candidates []string) (string, error) {
	var errs []error
	for _, c := range candidates {
		p, err := filepath.EvalSymlinks(c)
		if err == nil {
			err = rootOwnedUnwritable(p, false)
		}
		if err == nil {
			err = rootOwnedUnwritable(filepath.Dir(p), true)
		}
		if err == nil {
			return p, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", c, err))
	}
	return "", fmt.Errorf("no trusted systemctl: %w", errors.Join(errs...))
}

// rootOwnedUnwritable checks that p (not followed) is a directory (dir) or a regular file, owned by root
// and not writable by group or others.
func rootOwnedUnwritable(p string, dir bool) error {
	fi, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if dir && !fi.IsDir() || !dir && !fi.Mode().IsRegular() {
		return fmt.Errorf("%s: wrong file type %s", p, fi.Mode().Type())
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s: group or world writable", p)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || st.Uid != 0 {
		return fmt.Errorf("%s: not owned by root", p)
	}
	return nil
}

// userManagerUnitCgroup returns the ControlGroup of user@<uid>.service as the system manager reports it;
// a variable so tests can stand in. The system manager is asked (not the user bus, which may be the broken
// one): it placed the unit, and no process of the user can change its answer.
var userManagerUnitCgroup = systemManagerUnitCgroup

// systemManagerUnitCgroup asks the system manager through the trusted systemctl, with a fixed minimal
// environment, bounded by systemManagerTimeout plus systemManagerWaitDelay.
func systemManagerUnitCgroup(uid int) (string, error) {
	bin, err := systemManagerBin()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), systemManagerTimeout)
	defer cancel()
	unit := fmt.Sprintf("user@%d.service", uid)
	cmd := exec.CommandContext(ctx, bin, "--system", "show", unit, "--property=ControlGroup", "--value")
	cmd.Env = systemManagerEnv
	cmd.WaitDelay = systemManagerWaitDelay
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("systemctl --system show %s: %w", unit, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// scopeCgroupPath is the cgroup v2 path a job scope gets from `systemd-run --user --scope` (the user
// manager's app.slice): the daemon can reach it without the bus. The manager need not sit under user.slice
// (e.g. a custom system slice) and the caller need not run under it (the orchestrator's MCP server runs in
// a system service), so the manager's cgroup comes from the system manager. When that answer is missing
// or malformed, the default /user.slice/user-<uid>.slice/user@<uid>.service is used only if that cgroup
// exists and is owned by the uid; otherwise ok is false and the caller must not sweep anything: a cgroup
// path the user could have planted (e.g. a user@<uid>.service decoy inside a delegated subtree) is never
// guessed.
func scopeCgroupPath(unit string) (path string, ok bool) {
	uid := os.Getuid()
	if p, err := userManagerUnitCgroup(uid); err == nil && validManagerCgroup(p, uid) {
		return p + "/app.slice/" + unit, true
	}
	def := fmt.Sprintf("/user.slice/user-%d.slice/user@%d.service", uid, uid)
	fi, err := os.Lstat(filepath.Join(cgroupRoot, def))
	if err != nil || !fi.IsDir() {
		return "", false
	}
	if owner, ok := cgroupDirOwner(fi); !ok || owner != uid {
		return "", false
	}
	return def + "/app.slice/" + unit, true
}

// validManagerCgroup reports whether p is a usable cgroup v2 path of the user manager: absolute, no
// empty, "." or ".." component anywhere (so no trailing "/" either), the last component exactly
// user@<uid>.service.
func validManagerCgroup(p string, uid int) bool {
	if !strings.HasPrefix(p, "/") {
		return false
	}
	parts := strings.Split(p[1:], "/")
	for _, c := range parts {
		if c == "" || c == "." || c == ".." {
			return false
		}
	}
	return parts[len(parts)-1] == fmt.Sprintf("user@%d.service", uid)
}

// stopScope stops a job's scope: systemd sends SIGTERM to every process in the unit's cgroup, waits
// TimeoutStopSec, sends SIGKILL, and returns once the cgroup is empty. A unit that no longer exists
// (the wrapper exited and the scope was collected) is fine; a failed unit only when its cgroup is empty
// (a stop that timed out on unkillable processes leaves it failed with members). When systemctl cannot
// stop it (no user bus, the manager busy), the daemon sweeps the scope's cgroup itself (pidfd, membership
// re-checked) and accepts the stop only when the cgroup is then provably empty; without a trusted path to
// that cgroup it sweeps nothing and the stop fails (stays pending). The unit is always
// recomputed from the agent and the job id, never read from a file the job could write.
func (m *Manager) stopScope(id string) error {
	unit := ScopeUnit(m.Agent, id)
	ctx, cancel := context.WithTimeout(context.Background(), m.KillGrace+30*time.Second)
	defer cancel()
	var stderr bytes.Buffer
	cmd := systemdCmd(ctx, systemctlBin, "--user", "stop", unit)
	cmd.Stderr = &stderr
	err := cmd.Run()
	state, cg, serr := scopeState(unit)
	if serr == nil && (state == "inactive" || state == "") {
		return nil
	}
	if serr == nil && state == "failed" && cg == "" {
		return nil // a failed unit whose cgroup is gone
	}
	if cg == "" {
		cg, _ = scopeCgroupPath(unit)
	}
	var ferr error
	if cg == "" {
		ferr = errors.New("no trusted cgroup path for the user manager: not swept")
	} else {
		ferr = m.sweepScopeCgroup(id, cg)
	}
	if ferr == nil {
		return nil
	} else if serr == nil && state == "failed" {
		return fmt.Errorf("%s: failed with processes left: %w", unit, ferr)
	} else if err != nil {
		return fmt.Errorf("systemctl --user stop %s: %w: %s; %w", unit, err, strings.TrimSpace(stderr.String()), ferr)
	} else if serr != nil {
		return errors.Join(serr, ferr)
	}
	return fmt.Errorf("%s: %w (%s)", unit, errScopeActive, state)
}

// sweepScopeCgroup ends every process of a job's scope cgroup (path relative to cgroupRoot) without the
// user manager. nil only when the cgroup is gone or empty afterwards. A missing cgroup counts as gone only
// when its parent slice exists (the layout is the expected one); otherwise it proves nothing.
func (m *Manager) sweepScopeCgroup(id, path string) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("no cgroup sweep on %s", runtime.GOOS)
	}
	full := filepath.Join(cgroupRoot, path)
	if _, err := os.Stat(full); errors.Is(err, fs.ErrNotExist) {
		if fi, perr := os.Stat(filepath.Dir(full)); perr == nil && fi.IsDir() {
			return nil
		}
		return fmt.Errorf("scope cgroup %s not found (and no parent slice): %w", path, err)
	} else if err != nil {
		return fmt.Errorf("scope cgroup %s: %w", path, err)
	}
	r := sweepCgroup(path, m.Log.Writer())
	if r.Signalled > 0 {
		m.Log.Printf("job %s: ended %d process(es) of the scope cgroup directly", id, r.Signalled)
	}
	if r.Incomplete() {
		return fmt.Errorf("scope cgroup %s: %d process(es) left: %v", path, len(r.Survivors), r.Err)
	}
	// The sweep drops pids it cannot verify as members; the cgroup itself must list no live process.
	pids, err := cgroupMembers(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("scope cgroup %s: %w", path, err)
	}
	var left []int
	for _, p := range pids {
		if st, _, err := procStat(p); err == nil && st != "Z" {
			left = append(left, p)
		}
	}
	if len(left) > 0 {
		return fmt.Errorf("scope cgroup %s still lists live processes %v", path, left)
	}
	return nil
}

// scopeState returns the ActiveState ("inactive" for a unit that is not loaded) and the ControlGroup
// ("" when it has none) of a unit.
func scopeState(unit string) (state, cgroup string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := systemdCmd(ctx, systemctlBin, "--user", "show", "--property=ActiveState", "--property=ControlGroup", unit).Output()
	if err != nil {
		return "", "", fmt.Errorf("systemctl --user show %s: %w", unit, err)
	}
	for _, l := range strings.Split(string(out), "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(l), "=")
		switch k {
		case "ActiveState":
			state = v
		case "ControlGroup":
			cgroup = v
		}
	}
	return state, cgroup, nil
}
