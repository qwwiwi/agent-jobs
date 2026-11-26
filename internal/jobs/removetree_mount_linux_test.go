package jobs

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

const (
	mountNSEnv      = "AGENT_JOBS_TEST_MOUNTNS"       // opt in: "1"
	mountNSChildEnv = "AGENT_JOBS_TEST_MOUNTNS_CHILD" // set for the re-executed test: the scratch dir
)

// Real kernel behaviour (#240 High a): inside a new user and mount namespace a tmpfs and a
// same-filesystem bind mount are placed in a tree; removeTree must leave both mounts and what is
// behind them alone and remove the rest. Opt-in (AGENT_JOBS_TEST_MOUNTNS=1): it needs unprivileged
// user namespaces, which a job's guard and some hosts refuse; then it skips.
func TestRemoveTreeRealMount(t *testing.T) {
	if base := os.Getenv(mountNSChildEnv); base != "" {
		realMountChild(t, base)
		return
	}
	if os.Getenv(mountNSEnv) != "1" {
		t.Skip("set " + mountNSEnv + "=1 to run (needs a user and mount namespace)")
	}
	base := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRemoveTreeRealMount$", "-test.v", "-test.count=1")
	cmd.Env = append(os.Environ(), mountNSChildEnv+"="+base)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
	}
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Skipf("cannot create a user and mount namespace here: %v", err)
	}
	t.Logf("child output:\n%s", out)
	if err != nil {
		t.Fatalf("child failed: %v", err)
	}
}

// realMountChild runs inside the namespaces, as root of the user namespace.
func realMountChild(t *testing.T, base string) {
	tree := filepath.Join(base, "tree")
	tmpfs := filepath.Join(tree, "a", "tmpfs")
	bind := filepath.Join(tree, "b", "bind")
	src := filepath.Join(base, "bind-src")
	for _, d := range []string{tmpfs, bind, src} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(src, "precious"), []byte("bind"), 0o600)
	writeFile(t, filepath.Join(tree, "a", "sibling", "f"), []byte("x"), 0o600)
	writeFile(t, filepath.Join(tree, "c", "g"), []byte("x"), 0o600)
	if err := unix.Mount("tmpfs", tmpfs, "tmpfs", 0, ""); err != nil {
		t.Skipf("cannot mount a tmpfs in the namespace: %v", err)
	}
	defer func() { _ = unix.Unmount(tmpfs, unix.MNT_DETACH) }()
	if err := unix.Mount(src, bind, "", unix.MS_BIND, ""); err != nil {
		t.Skipf("cannot bind mount in the namespace: %v", err)
	}
	defer func() { _ = unix.Unmount(bind, unix.MNT_DETACH) }()
	writeFile(t, filepath.Join(tmpfs, "precious"), []byte("tmpfs"), 0o600)

	err := removeTree(tree)
	if !errors.Is(err, ErrMountBoundary) {
		t.Fatalf("err = %v, want ErrMountBoundary", err)
	}
	for _, p := range []string{filepath.Join(tmpfs, "precious"), filepath.Join(bind, "precious"), filepath.Join(src, "precious")} {
		if !exists(p) {
			t.Fatalf("%s: content behind a mount point was removed", p)
		}
	}
	for _, p := range []string{filepath.Join(tree, "a", "sibling"), filepath.Join(tree, "c")} {
		if exists(p) {
			t.Fatalf("%s: a sibling of a mount point was kept", p)
		}
	}
}
