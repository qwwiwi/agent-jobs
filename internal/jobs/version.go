package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Version is the agent-jobs build version (main sets it from its -ldflags version at start).
var Version = "dev"

// FileIdentity identifies one file on disk: replacing it (a new inode), rewriting it in place (size,
// mtime) or moving another file over it all change the identity.
type FileIdentity struct {
	Path    string    `json:"path"`
	Dev     uint64    `json:"dev"`
	Ino     uint64    `json:"ino"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mtime"`
}

// StatIdentity returns the identity of the file at path (links followed).
func StatIdentity(path string) (FileIdentity, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return FileIdentity{}, err
	}
	id := FileIdentity{Path: path, Size: fi.Size(), ModTime: fi.ModTime()}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		id.Dev, id.Ino = uint64(st.Dev), st.Ino // Dev is int32 on darwin
	}
	return id, nil
}

// Same reports whether a and b are the same file content on disk (the path is not compared).
func (a FileIdentity) Same(b FileIdentity) bool {
	return a.Dev == b.Dev && a.Ino == b.Ino && a.Size == b.Size && a.ModTime.Equal(b.ModTime)
}

// DaemonInfoFile is <state>/daemon.json: what the running daemon is, written when it starts.
const DaemonInfoFile = "daemon.json"

// DaemonInfo is daemon.json.
type DaemonInfo struct {
	Version   string       `json:"version"`
	PID       int          `json:"pid"`
	StartedAt time.Time    `json:"started_at"`
	Binary    FileIdentity `json:"binary"`
}

// WriteDaemonInfo records the daemon's version and binary identity in <state>/daemon.json.
func (m *Manager) WriteDaemonInfo() error {
	bin, err := StatIdentity(m.SelfBin)
	if err != nil {
		return fmt.Errorf("daemon.json: %w", err)
	}
	b, err := json.MarshalIndent(DaemonInfo{Version: Version, PID: os.Getpid(), StartedAt: m.Now().UTC(), Binary: bin}, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(m.StateDir, DaemonInfoFile), append(b, '\n'))
}

// ReadDaemonInfo reads <state>/daemon.json (os.ErrNotExist when the daemon never wrote one).
func (m *Manager) ReadDaemonInfo() (*DaemonInfo, error) {
	b, err := os.ReadFile(filepath.Join(m.StateDir, DaemonInfoFile))
	if err != nil {
		return nil, err
	}
	var d DaemonInfo
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, fmt.Errorf("daemon.json: %w", err)
	}
	return &d, nil
}

// DaemonMismatch compares the running daemon (daemon.json) with this process, started from the binary
// whose identity was self at its start: a different version, or the same version string from another
// build of the same path. "" when they match or when there is no daemon.json.
func (m *Manager) DaemonMismatch(self FileIdentity) string {
	d, err := m.ReadDaemonInfo()
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		return "daemon version not checked: " + err.Error()
	}
	switch {
	case d.Version != Version:
		return fmt.Sprintf("the daemon runs agent-jobs %s, this MCP server %s; restart the session (or /mcp reconnect) "+
			"or the daemon so both run the installed binary", Printable(d.Version, 64), Version)
	case d.Binary.Path == self.Path && !d.Binary.Same(self):
		return fmt.Sprintf("the daemon and this MCP server run different builds of %s (both say %s); restart the "+
			"session (or /mcp reconnect) or the daemon so both run the installed binary", self.Path, Version)
	}
	return ""
}

// StaleMCPProc is an `agent-jobs mcp --role orchestrator` process of the agent that does not run the
// installed binary.
type StaleMCPProc struct {
	PID    int    `json:"pid"`
	Reason string `json:"reason"`
}

// StaleMCPProcs lists (Linux /proc) this user's orchestrator MCP servers of the agent whose
// executable was deleted or is not the binary installed at m.SelfBin now. supported is false
// without /proc.
func (m *Manager) StaleMCPProcs() (procs []StaleMCPProc, supported bool, err error) {
	if _, err := os.Stat(filepath.Join(procRoot, "self", "exe")); err != nil {
		return nil, false, nil
	}
	installed, ierr := StatIdentity(m.SelfBin)
	ents, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, true, err
	}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 || !procOwnedByMe(pid) {
			continue
		}
		argv, err := procCmdline(pid)
		if err != nil || !isOrchestratorMCP(argv, m.Agent) {
			continue
		}
		exe := filepath.Join(procRoot, strconv.Itoa(pid), "exe")
		link, err := os.Readlink(exe)
		if err != nil {
			continue
		}
		reason := ""
		if strings.HasSuffix(link, " (deleted)") {
			reason = "its executable was deleted or replaced (" + strings.TrimSuffix(link, " (deleted)") + ")"
		} else if running, err := StatIdentity(exe); err == nil && ierr == nil && !running.Same(installed) {
			reason = "it runs " + link + ", not the binary installed at " + m.SelfBin
		}
		if reason != "" {
			procs = append(procs, StaleMCPProc{PID: pid, Reason: reason})
		}
	}
	return procs, true, nil
}

// isOrchestratorMCP reports whether argv is `<bin> mcp --role orchestrator --agent <agent>` (flags in
// any order, "--flag value" or "--flag=value", one or two dashes).
func isOrchestratorMCP(argv []string, agent string) bool {
	if len(argv) < 2 || argv[1] != "mcp" {
		return false
	}
	flags := map[string]string{}
	for i := 2; i < len(argv); i++ {
		if !strings.HasPrefix(argv[i], "-") {
			continue
		}
		name := strings.TrimLeft(argv[i], "-")
		if k, v, eq := strings.Cut(name, "="); eq {
			flags[k] = v
		} else if i+1 < len(argv) {
			flags[name] = argv[i+1]
			i++
		}
	}
	return flags["role"] == "orchestrator" && flags["agent"] == agent
}
