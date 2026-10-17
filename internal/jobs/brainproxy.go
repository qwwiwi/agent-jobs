package jobs

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/brain"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// BrainSocketName is the daemon's brain proxy socket in the state directory.
const BrainSocketName = "brain.sock"

// maxProxyConns bounds the proxy calls served at once.
const maxProxyConns = 8

// BrainSocketPath is <state>/brain.sock.
func BrainSocketPath(stateDir string) string { return filepath.Join(stateDir, BrainSocketName) }

// brainAgent is the author name of the agent's notes (brain.agent, else the agent name).
func (m *Manager) brainAgent() string {
	if m.Cfg.Brain.Agent != "" {
		return m.Cfg.Brain.Agent
	}
	return m.Agent
}

// ServeBrain runs the brain proxy for the agent's jobs until ctx ends: a unix socket (mode 600) on
// which a running job's MCP server asks for memory_search and memory_write. The daemon holds the
// token; the job never does. Without a brain it returns at once. A socket another live daemon
// answers on is left alone (error); a stale one is replaced.
func (m *Manager) ServeBrain(ctx context.Context) error {
	if brain.IsDisabled(m.Brain) {
		return nil
	}
	p := BrainSocketPath(m.StateDir)
	if fi, err := os.Lstat(p); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("brain proxy: %s exists and is not a socket; not replacing it", p)
		}
		if c, err := net.DialTimeout("unix", p, time.Second); err == nil {
			_ = c.Close()
			return fmt.Errorf("brain proxy: another process serves %s", p)
		}
		if err := os.Remove(p); err != nil {
			return fmt.Errorf("brain proxy: remove stale socket: %w", err)
		}
	}
	ln, err := net.Listen("unix", p)
	if err != nil {
		return fmt.Errorf("brain proxy: %w", err)
	}
	defer ln.Close()
	if err := os.Chmod(p, 0o600); err != nil {
		return fmt.Errorf("brain proxy: chmod socket: %w", err)
	}
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	sem := make(chan struct{}, maxProxyConns)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("brain proxy: accept: %w", err)
		}
		select {
		case sem <- struct{}{}:
		default:
			brain.ServeSocketConn(conn, time.Second, func(brain.SocketRequest) brain.SocketResponse {
				return brain.SocketResponse{Error: "brain proxy busy; try again"}
			})
			continue
		}
		go func() {
			defer func() { <-sem }()
			brain.ServeSocketConn(conn, 10*time.Second, func(r brain.SocketRequest) brain.SocketResponse {
				return m.HandleBrainRequest(ctx, r)
			})
		}()
	}
}

// HandleBrainRequest answers one proxy call: only for a running job of this agent, only search and
// write; a write always gets the job's provenance (brain.ForJob) and its path is recorded.
func (m *Manager) HandleBrainRequest(ctx context.Context, r brain.SocketRequest) brain.SocketResponse {
	fail := func(err error) brain.SocketResponse { return brain.SocketResponse{Error: err.Error()} }
	j, err := m.Store.Get(r.JobID)
	if err != nil || j.Status != store.StatusRunning {
		return fail(fmt.Errorf("brain proxy: job %q is not a running job of agent %s", r.JobID, m.Agent))
	}
	switch r.Op {
	case brain.SocketOpSearch:
		if r.Limit < 1 || r.Limit > 20 {
			return fail(fmt.Errorf("limit %d is outside 1..20", r.Limit))
		}
		notes, err := m.Brain.Search(ctx, r.Query, r.Limit)
		if err != nil {
			return fail(err)
		}
		if err := m.MarkJobNotes(notes); err != nil {
			return fail(err)
		}
		return brain.SocketResponse{Notes: notes}
	case brain.SocketOpWrite:
		if r.Note == nil {
			return fail(errors.New("note is required"))
		}
		n, err := brain.ForJob(*r.Note, m.brainAgent(), j.ID)
		if err != nil {
			return fail(err)
		}
		o, err := m.Brain.Write(ctx, n)
		if err != nil {
			return fail(err)
		}
		if path, ok := o["path"].(string); ok && path != "" {
			if err := m.Store.AddJobNote(path, j.ID, m.Now()); err != nil {
				m.Log.Printf("job %s: %v (the note's marker line still marks it)", j.ID, err)
			}
		}
		return brain.SocketResponse{Result: o}
	}
	return fail(fmt.Errorf("op %q is not available to a job", r.Op))
}
