//go:build linux

package jobs

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// landlockABI is the kernel's Landlock ABI version (0 with an error when Landlock is unavailable).
func landlockABI() (int, error) {
	v, _, e := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if e != 0 {
		return 0, fmt.Errorf("landlock is not available (kernel 5.13+ with landlock in its LSM list): %w", e)
	}
	return int(v), nil
}

func landlockAvailable() error {
	_, err := landlockABI()
	return err
}

// start starts cmd on an OS thread restricted to the sandbox: the child inherits the thread's Landlock
// domain (and no_new_privs) at fork. The thread is never unlocked, so the Go runtime ends it with this
// goroutine; new runtime threads are cloned from its template thread, never from a restricted one.
func (s *keySandbox) start(cmd *exec.Cmd) error {
	errc := make(chan error, 1)
	go func() {
		runtime.LockOSThread() // deliberately never unlocked
		if err := s.restrictThread(); err != nil {
			errc <- err
			return
		}
		errc <- cmd.Start()
	}()
	return <-errc
}

// restrictThread applies the sandbox to the calling OS thread (locked by the caller).
func (s *keySandbox) restrictThread() error {
	abi, err := landlockABI()
	if err != nil {
		return err
	}
	handled := uint64(unix.LANDLOCK_ACCESS_FS_READ_FILE)
	if abi >= 2 {
		// Refer is denied by any ruleset anyway (ABI 1 denies every cross-directory link and rename);
		// handled and granted beneath the readable trees, the kernel still refuses a link or rename that
		// would make a file readable that was not.
		handled |= unix.LANDLOCK_ACCESS_FS_REFER
	}
	paths, dirs, err := landlockGrants(s.noRead)
	if err != nil {
		return err
	}
	// Only the handled fs field is passed (the struct grew in later ABIs; its first field is enough).
	attr := unix.LandlockRulesetAttr{Access_fs: handled}
	fd, _, e := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr.Access_fs), 0)
	if e != 0 {
		return fmt.Errorf("landlock: create ruleset: %w", e)
	}
	defer unix.Close(int(fd))
	for i, p := range paths {
		pfd, err := unix.Open(p, unix.O_PATH|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if errors.Is(err, unix.ENOENT) {
			continue // gone since it was listed
		}
		if err != nil {
			return fmt.Errorf("landlock: open %s: %w", p, err)
		}
		var st unix.Stat_t
		if err := unix.Fstat(pfd, &st); err != nil {
			unix.Close(pfd)
			return fmt.Errorf("landlock: stat %s: %w", p, err)
		}
		if st.Mode&unix.S_IFMT == unix.S_IFLNK {
			unix.Close(pfd) // replaced by a symlink since it was listed: not granted
			continue
		}
		access := uint64(unix.LANDLOCK_ACCESS_FS_READ_FILE)
		if dirs[i] && st.Mode&unix.S_IFMT == unix.S_IFDIR {
			access = handled
		}
		rule := unix.LandlockPathBeneathAttr{Allowed_access: access, Parent_fd: int32(pfd)}
		_, _, e := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, fd, unix.LANDLOCK_RULE_PATH_BENEATH,
			uintptr(unsafe.Pointer(&rule)), 0, 0, 0)
		unix.Close(pfd)
		if e != 0 {
			return fmt.Errorf("landlock: grant %s: %w", p, e)
		}
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("landlock: no_new_privs: %w", err)
	}
	if _, _, e := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, fd, 0, 0); e != 0 {
		return fmt.Errorf("landlock: restrict: %w", e)
	}
	return nil
}
