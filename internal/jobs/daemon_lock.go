package jobs

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// lockLauncher excludes manual ticks for the daemon's entire lifetime, including startup.
// A live daemon.json also excludes older daemons that do not yet take this lock.
func (m *Manager) lockLauncher() (func(), error) {
	f, err := os.OpenFile(filepath.Join(m.StateDir, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open daemon lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("daemon is active for agent %s; manual tick refused; let the daemon launch queued jobs: %w", m.Agent, err)
	}
	unlock := func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
	if d, err := m.ReadDaemonInfo(); err == nil && alive(d.PID) {
		unlock()
		return nil, fmt.Errorf("daemon is active for agent %s (pid %d); manual tick refused; let the daemon launch queued jobs", m.Agent, d.PID)
	}
	return unlock, nil
}

// LockDaemon must precede publishing daemon state or starting any daemon work.
func (m *Manager) LockDaemon() (func(), error) {
	unlock, err := m.lockLauncher()
	if err != nil {
		return nil, err
	}
	m.daemonLockHeld = true
	return func() {
		m.daemonLockHeld = false
		unlock()
	}, nil
}
