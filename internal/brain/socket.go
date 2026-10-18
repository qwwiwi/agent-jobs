package brain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// A job never holds the brain credential. Its MCP server (job role) is a SocketClient: it sends
// memory_search and memory_write to the agent's daemon over a unix socket in the state directory
// (<state>/brain.sock, mode 600), and the daemon, which holds the token, checks that the job is
// running, gives a write the job's provenance (ForJob) and calls the brain.

// Socket operations.
const (
	SocketOpSearch = "search"
	SocketOpWrite  = "write"
)

// MaxSocketMessage bounds one request or response on the socket.
const MaxSocketMessage = 1 << 20

// SocketRequest is one call from a job to the daemon (one request per connection).
type SocketRequest struct {
	JobID string     `json:"job_id"`
	Op    string     `json:"op"`
	Query string     `json:"query,omitempty"`
	Limit int        `json:"limit,omitempty"`
	Note  *NoteInput `json:"note,omitempty"`
}

// SocketResponse is the daemon's answer; Error is set when the call failed.
type SocketResponse struct {
	Notes  []Note `json:"notes,omitempty"`
	Result Object `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

// ErrJobRole is returned by the SocketClient for everything a job may not do.
var ErrJobRole = errors.New("not available to a job: only memory_search and memory_write")

// SocketClient is the brain of a job: memory_search and memory_write through the daemon.
type SocketClient struct {
	Path    string        // <state>/brain.sock
	JobID   string        // the job's id (AGENT_JOBS_JOB); the daemon checks it is running
	Timeout time.Duration // bounds one call; 0 = DefaultTimeout + 5 s
}

var _ Brain = SocketClient{}

func (c SocketClient) do(ctx context.Context, req SocketRequest) (SocketResponse, error) {
	d := c.Timeout
	if d <= 0 {
		d = DefaultTimeout + 5*time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", c.Path)
	if err != nil {
		return SocketResponse{}, fmt.Errorf("agent-jobs daemon (brain proxy) unreachable: %w", err)
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	req.JobID = c.JobID
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return SocketResponse{}, fmt.Errorf("brain proxy: send: %w", err)
	}
	var resp SocketResponse
	if err := json.NewDecoder(io.LimitReader(conn, MaxSocketMessage)).Decode(&resp); err != nil {
		return SocketResponse{}, fmt.Errorf("brain proxy: read answer: %w", err)
	}
	if resp.Error != "" {
		return resp, errors.New(resp.Error)
	}
	return resp, nil
}

// Search asks the daemon for the closest notes.
func (c SocketClient) Search(ctx context.Context, query string, limit int) ([]Note, error) {
	resp, err := c.do(ctx, SocketRequest{Op: SocketOpSearch, Query: query, Limit: limit})
	if err != nil {
		return nil, err
	}
	if resp.Notes == nil {
		resp.Notes = []Note{}
	}
	return resp.Notes, nil
}

// Write asks the daemon to write the note; the daemon applies ForJob whatever the note says.
func (c SocketClient) Write(ctx context.Context, n NoteInput) (Object, error) {
	resp, err := c.do(ctx, SocketRequest{Op: SocketOpWrite, Note: &n})
	if err != nil {
		return nil, err
	}
	return resp.Result, nil
}

func (SocketClient) CreateTask(context.Context, TaskInput) (Object, error)   { return nil, ErrJobRole }
func (SocketClient) ListTasks(context.Context, TaskFilter) ([]Object, error) { return nil, ErrJobRole }
func (SocketClient) UpdateTask(context.Context, TaskUpdate) (Object, error)  { return nil, ErrJobRole }
func (SocketClient) GetTask(context.Context, int64) (Object, error)          { return nil, ErrJobRole }
func (SocketClient) Send(context.Context, string, Object) (Object, error)    { return nil, ErrJobRole }
func (SocketClient) Inbox(context.Context, int) ([]Object, error)            { return nil, ErrJobRole }
func (SocketClient) Ack(context.Context, string) error                       { return ErrJobRole }

// ServeSocketConn reads one request from conn, answers it with handle and closes conn. The request
// must arrive within readTimeout.
func ServeSocketConn(conn net.Conn, readTimeout time.Duration, handle func(SocketRequest) SocketResponse) {
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
	var req SocketRequest
	resp := SocketResponse{}
	if err := json.NewDecoder(io.LimitReader(conn, MaxSocketMessage)).Decode(&req); err != nil {
		resp.Error = "brain proxy: bad request: " + err.Error()
	} else {
		_ = conn.SetReadDeadline(time.Time{})
		resp = handle(req)
	}
	_ = conn.SetWriteDeadline(time.Now().Add(readTimeout))
	_ = json.NewEncoder(conn).Encode(resp)
}
