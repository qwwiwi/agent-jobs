package jobs

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// The key sandbox (issue #136, R3 review B6): while providers.openrouter is configured, no process of a
// job may read the key file, whatever tool it uses (a script it wrote, another interpreter, a codex
// runner without the guard). The wrapper reads the key into the proxy's memory before anything of the
// job starts; every process the wrapper starts for the job (the runner, its fallback, the acceptance
// command, the prepare and manifest-check steps, the wrapper's own git) is started on an OS thread
// restricted with Linux Landlock, and inherits that restriction with all its descendants: reading a
// file in the key file's directory fails with EACCES. The wrapper's other threads (the proxy) are not
// restricted.
//
// Landlock only grants: there is no "everything but this directory" rule. The sandbox handles the
// read-file right (and, on Landlock ABI 2+, the refer right, so the key cannot be linked or moved out to
// a readable place) and grants it beneath every entry of every ancestor of the key directory except the
// next step towards it. A file created later directly inside one of those ancestors is not covered: it
// is unreadable to the job until its next launch. So the key directory must not be inside a directory
// jobs create files in and read them back: not under $HOME (claude rewrites ~/.claude.json there), the
// temp dirs, or the agent-jobs state dir (keySandboxPlacement).

// keySandbox is the set of directories a job's processes cannot read files in.
type keySandbox struct {
	noRead []string // absolute, symlinks resolved
	// codexPolicy is providers.openrouter.codex_policy (codexKeySandbox).
	codexPolicy string
	// runnerOutside: the runner (and so all its descendants) starts outside the sandbox. Set only for a
	// codex runner on codex's native sandbox under codex_policy allow_unprotected (codexKeySandbox).
	runnerOutside bool
}

var (
	jobSandboxMu sync.Mutex
	jobSandbox   *keySandbox // the wrapper process's sandbox (nil = none: the daemon, a job without openrouter)
)

// setJobSandbox makes every job process this process starts from now on (startJobCmd) run in s.
func setJobSandbox(s *keySandbox) {
	jobSandboxMu.Lock()
	defer jobSandboxMu.Unlock()
	jobSandbox = s
}

// startJobCmd starts cmd, a process of the job, inside the wrapper's key sandbox when there is one.
func startJobCmd(cmd *exec.Cmd) error {
	jobSandboxMu.Lock()
	s := jobSandbox
	jobSandboxMu.Unlock()
	if s == nil {
		return cmd.Start()
	}
	return s.start(cmd)
}

// startRunnerCmd starts the job's runner: in the key sandbox like any job process (startJobCmd), unless
// the sandbox lets the runner run outside it (codexKeySandbox).
func startRunnerCmd(cmd *exec.Cmd) error {
	jobSandboxMu.Lock()
	outside := jobSandbox != nil && jobSandbox.runnerOutside
	jobSandboxMu.Unlock()
	if outside {
		return cmd.Start()
	}
	return startJobCmd(cmd)
}

// codexKeySandbox decides how a codex runner runs in this wrapper's key sandbox. bwrap, codex's native
// sandbox, cannot start inside any Landlock domain: a restricted process may not mount(2) (verified on
// kernel 6.8 even with a ruleset that handles only the execute right and grants it beneath /:
// "bwrap: Failed to make / slave: Operation not permitted"). So for a codex job on that sandbox, with
// codex_policy refuse, fail is why it does not start; with allow_unprotected (the default) the runner
// starts outside the key sandbox (the wrapper's own steps stay inside) and warn is the warning for
// stderr.log. A claude runner, a codex job with codex_sandbox worktree-full-access (no bwrap) and a
// wrapper without a sandbox run as they are: both "".
func codexKeySandbox(runner string, argv []string) (fail, warn string) {
	jobSandboxMu.Lock()
	defer jobSandboxMu.Unlock()
	s := jobSandbox
	if runner != store.RunnerCodex || s == nil {
		return "", ""
	}
	if a, err := codexArgvSandbox(argv); err == nil && a.sandbox == CodexSandboxFullAccess {
		return "", ""
	}
	if s.codexPolicy == CodexPolicyRefuse {
		return "job not started: openrouter key protection: codex's native sandbox (bwrap) cannot start inside the key " +
			"sandbox (Linux Landlock forbids the mounts bwrap makes), and without the key sandbox a codex job could read " +
			"providers.openrouter.key_file; providers.openrouter.codex_policy is refuse, so codex jobs run only with " +
			"codex_sandbox worktree-full-access", ""
	}
	s.runnerOutside = true
	return "", "WARNING: openrouter key protection is OFF for this codex job's runner: codex's native sandbox (bwrap) " +
		"cannot start inside the Landlock key sandbox (a Landlock domain forbids mount(2)), so codex and every command it " +
		"runs can read providers.openrouter.key_file (providers.openrouter.codex_policy allow_unprotected, the default; " +
		"refuse refuses such jobs, codex_sandbox worktree-full-access runs codex inside the key sandbox); the wrapper's " +
		"own steps (git, prepare, acceptance, manifest-check) stay inside it"
}

// keyPlacementCheck is off only in the test build (keysandbox_testhooks.go): the tests' key files live
// in their temp dirs.
var keyPlacementCheck = true

// wrapperKeySandbox is the sandbox of a wrapper whose agent config (stateDir) has providers.openrouter:
// its key file's directory (both as configured and with symlinks resolved). nil without openrouter. An
// error (a placement that cannot be protected, Landlock unavailable) fails the job before anything of it
// starts: no job runs while the key would be readable to it.
func wrapperKeySandbox(c Config, stateDir string) (*keySandbox, error) {
	o := c.Providers.OpenRouter
	if o == nil {
		return nil, nil
	}
	if !filepath.IsAbs(o.KeyFile) {
		return nil, fmt.Errorf("providers.openrouter.key_file %q is not absolute", o.KeyFile)
	}
	dirs := []string{filepath.Clean(filepath.Dir(o.KeyFile))}
	if real, err := filepath.EvalSymlinks(o.KeyFile); err == nil {
		dirs = append(dirs, filepath.Dir(real))
	} else if real, err := filepath.EvalSymlinks(dirs[0]); err == nil {
		dirs = append(dirs, real)
	}
	s := &keySandbox{codexPolicy: o.codexPolicy()}
	for _, d := range dirs {
		if !slices.Contains(s.noRead, d) {
			s.noRead = append(s.noRead, d)
		}
	}
	if keyPlacementCheck {
		home, _ := os.UserHomeDir()
		busy := []string{home, os.TempDir(), "/tmp", "/var/tmp", "/dev/shm", stateDir}
		for _, d := range s.noRead {
			if err := keySandboxPlacement(d, busy, expectedKeyDir(stateDir)); err != nil {
				return nil, fmt.Errorf("providers.openrouter.key_file %s: %w", o.KeyFile, err)
			}
		}
	}
	if err := landlockAvailable(); err != nil {
		return nil, fmt.Errorf("providers.openrouter.key_file %s cannot be kept from the jobs: %w", o.KeyFile, err)
	}
	return s, nil
}

// KeyRoot is where an agent's OpenRouter key directory is expected: <KeyRoot>/<agent>/, created once by
// root, owned by the agent's user, mode 700 (outside $HOME, the temp dirs and the state dir).
const KeyRoot = "/var/lib/agent-jobs-keys"

// expectedKeyDir is the key directory named in a placement error: KeyRoot/<agent> (the state dir's name).
func expectedKeyDir(stateDir string) string {
	agent := filepath.Base(filepath.Clean(stateDir))
	if stateDir == "" || agent == "/" || agent == "." {
		agent = "<agent>"
	}
	return filepath.Join(KeyRoot, agent)
}

// keySandboxPlacement refuses a key directory the sandbox cannot protect without breaking jobs: the
// root, or one that is, contains or lies inside a busy directory (where jobs create files and read them
// back; "" entries are skipped). Busy paths are compared as given and with symlinks resolved. want is
// the directory the error names as the expected place.
func keySandboxPlacement(dir string, busy []string, want string) error {
	hint := fmt.Sprintf("move the key file to %s/ (created once by root, owned by this user, mode 700)", want)
	if dir == "/" {
		return errors.New("its directory is /: " + hint)
	}
	for _, b := range busy {
		if b == "" || !filepath.IsAbs(b) {
			continue
		}
		cands := []string{filepath.Clean(b)}
		if real, err := filepath.EvalSymlinks(b); err == nil && real != cands[0] {
			cands = append(cands, real)
		}
		for _, c := range cands {
			if c == "/" {
				continue
			}
			if pathWithin(dir, c) || pathWithin(c, dir) {
				return fmt.Errorf("its directory %s overlaps %s, where jobs create and read files: a job could not read "+
					"what is created there while its key directory is protected; %s", dir, c, hint)
			}
		}
	}
	return nil
}

// landlockGrants lists what the sandbox grants the read right beneath: every entry of each strict
// ancestor of a no-read directory, except the no-read directories and the ancestors themselves (which
// are walked into instead). For the key in /var/lib/agent-jobs-keys/<agent>: every entry of / but var,
// every entry of /var but lib, every entry of /var/lib but agent-jobs-keys, every entry of it but
// <agent>, enumerated when the job's process starts. Symlinks are skipped: what they point to is
// covered where it lives. isDir reports, for each path, whether it is a directory (the refer right is
// for directories only).
func landlockGrants(noRead []string) (paths []string, isDir []bool, err error) {
	return landlockGrantsFrom("/", noRead)
}

// landlockGrantsFrom is landlockGrants walking from root (a test's stand-in for /).
func landlockGrantsFrom(root string, noRead []string) (paths []string, isDir []bool, err error) {
	ancestor := func(p string) bool {
		for _, d := range noRead {
			if p != d && pathWithin(d, p) {
				return true
			}
		}
		return false
	}
	var walk func(dir string) error
	walk = func(dir string) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return fmt.Errorf("landlock: list %s: %w", dir, err)
		}
		for _, e := range entries {
			p := filepath.Join(dir, e.Name())
			switch {
			case slices.Contains(noRead, p):
			case e.Type()&os.ModeSymlink != 0:
			case e.IsDir() && ancestor(p):
				if err := walk(p); err != nil {
					return err
				}
			default:
				paths, isDir = append(paths, p), append(isDir, e.IsDir())
			}
		}
		return nil
	}
	if err := walk(root); err != nil {
		return nil, nil, err
	}
	return paths, isDir, nil
}
