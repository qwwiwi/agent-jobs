package jobs

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"golang.org/x/sys/unix"

	"github.com/qwwiwi/agent-jobs/internal/brain"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// BrainSocketName is the daemon's brain proxy socket in the state directory.
const BrainSocketName = "brain.sock"

// errJobWritesOff refuses job writes after one could not be tracked (see HandleBrainRequest).
var errJobWritesOff = errors.New("brain proxy: job writes are off until the daemon restarts: a job's note could not be tracked")

// maxProxyConns bounds the proxy calls served at once.
const maxProxyConns = 8

// BrainSocketPath is <state>/brain.sock.
func BrainSocketPath(stateDir string) string { return filepath.Join(stateDir, BrainSocketName) }

// brainAgent is the author name of the agent's notes (brain.agent, else the agent name).
func (m *Manager) brainAgent() string {
	if m.Config().Brain.Agent != "" {
		return m.Config().Brain.Agent
	}
	return m.Agent
}

// ServeBrain runs the job proxy for the agent's jobs until ctx ends: a unix socket (mode 600) on
// which a running job's MCP server asks for memory_search, memory_write, notify_owner and
// check_mark. The daemon holds the brain token; the job never does. Without a brain the socket
// still serves notify_owner and check_mark (the memory calls answer «brain not configured»). A socket another live daemon answers on is left
// alone (error); a stale one is replaced.
func (m *Manager) ServeBrain(ctx context.Context) error {
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
			pid, perr := peerPID(conn)
			brain.ServeSocketConn(conn, 10*time.Second, func(r brain.SocketRequest) brain.SocketResponse {
				if perr != nil {
					return brain.SocketResponse{Error: "brain proxy: " + perr.Error()}
				}
				return m.HandleBrainRequest(ctx, pid, r)
			})
		}()
	}
}

// HandleBrainRequest answers one proxy call from the process peer (its pid from the socket's peer
// credentials): only for a running job of this agent that peer belongs to (peerJob; the job id in
// the request must name that job, so a job never acts as another), only search, write,
// notify_owner and check_mark; a write always gets the job's provenance (brain.ForJob) and its path
// is recorded; a notification is the job's own (rate limited per job); a mark carries the job's id.
func (m *Manager) HandleBrainRequest(ctx context.Context, peer int, r brain.SocketRequest) brain.SocketResponse {
	fail := func(err error) brain.SocketResponse { return brain.SocketResponse{Error: err.Error()} }
	j, err := m.Store.Get(r.JobID)
	if err != nil || j.Status != store.StatusRunning {
		return fail(fmt.Errorf("brain proxy: job %q is not a running job of agent %s", r.JobID, m.Agent))
	}
	pj, err := m.peerJob(peer)
	if err != nil {
		return fail(err)
	}
	if pj.ID != j.ID {
		m.Log.Printf("WARN brain proxy: pid %d of job %s called as job %s; refused", peer, pj.ID, j.ID)
		return fail(fmt.Errorf("brain proxy: the calling process belongs to job %s, not to job %q", pj.ID, r.JobID))
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
		if m.jobWritesOff.Load() {
			return fail(errJobWritesOff)
		}
		o, err := m.Brain.Write(ctx, n)
		if err != nil {
			return fail(err)
		}
		// Fail closed: a job's note the daemon cannot record could reach a later brief unmarked
		// (its snippet need not carry the marker line), so no job writes another until a restart.
		r, err := brain.ParseWriteResult(o)
		if err == nil {
			err = m.Store.AddJobNote(r.Path, j.ID, m.Now())
		}
		if err != nil {
			m.jobWritesOff.Store(true)
			m.Log.Printf("ERROR brain proxy: job %s wrote a note that cannot be tracked (%v); job writes are off until the daemon restarts", j.ID, err)
			return fail(fmt.Errorf("the note was written but could not be tracked as this job's (%v); %w", err, errJobWritesOff))
		}
		return brain.SocketResponse{Result: o}
	case brain.SocketOpNotify:
		n, err := m.NotifyOwner(j.ID, r.Text, r.Urgency)
		resp := brain.SocketResponse{}
		if n != nil {
			resp.Result = notificationObject(n)
		}
		if err != nil {
			resp.Error = err.Error()
		}
		return resp
	case brain.SocketOpCheckMark:
		c, err := m.MarkCheck(r.Check, r.Text, j.ID)
		if err != nil {
			return fail(err)
		}
		return brain.SocketResponse{Result: checkObject(c)}
	}
	return fail(fmt.Errorf("op %q is not available to a job", r.Op))
}

// peerJob returns the running job of this agent the process pid belongs to. It is in the job's
// session: the wrapper starts with setsid, so the job's recorded pgid is its session id, and no
// process can join a session it is not in (a process that calls setsid itself leaves the job's, and
// is refused). A job in a scope must also have pid in that scope's cgroup. Cgroup membership alone
// is not taken as proof: a process can move itself between cgroups of its own user's delegated tree.
func (m *Manager) peerJob(pid int) (*store.Job, error) {
	none := fmt.Errorf("brain proxy: the calling process (pid %d) belongs to no running job of agent %s", pid, m.Agent)
	if pid <= 1 {
		return nil, none
	}
	sid, err := unix.Getsid(pid)
	if err != nil || sid <= 1 {
		return nil, none
	}
	running, err := m.Store.ListByStatus(store.StatusRunning)
	if err != nil {
		return nil, err
	}
	for _, j := range running {
		if j.PGID <= 1 || j.PGID != sid {
			continue
		}
		if j.ScopeUnit != "" {
			b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "cgroup"))
			if err != nil || jobScopeIn(string(b)) != j.ScopeUnit {
				return nil, fmt.Errorf("brain proxy: the calling process (pid %d) is in the session of job %s but not in its scope %s",
					pid, j.ID, j.ScopeUnit)
			}
		}
		return j, nil
	}
	return nil, none
}

// checkObject is a check as the proxy's JSON answer (what a job may see of it).
func checkObject(c *store.Check) brain.Object {
	c = safeCheck(c)
	o := brain.Object{"name": c.Name, "cron": c.Cron, "grace_minutes": c.GraceMinutes, "enabled": c.Enabled, "allowed_schedule_ids": c.AllowedScheduleIDs}
	if c.LastMarkAt != nil {
		o["last_mark_at"] = c.LastMarkAt.Format(time.RFC3339)
		o["last_mark_job"] = c.LastMarkJob
		o["last_mark_untrusted_note"] = c.LastMarkUntrustedNote
	}
	return o
}

// notificationObject is a notification as the proxy's JSON answer.
func notificationObject(n *store.Notification) brain.Object {
	o := brain.Object{"id": n.ID, "job_id": n.JobID, "text": n.Text, "urgency": n.Urgency,
		"created_at": n.CreatedAt.Format(time.RFC3339), "delivered": n.Delivered}
	if n.Error != "" {
		o["error"] = n.Error
	}
	return o
}
