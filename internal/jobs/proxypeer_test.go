package jobs

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/qwwiwi/agent-jobs/internal/brain"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// The brain proxy binds a call to the process at the other end of the socket, so the tests call it
// from real child processes: this test binary re-executed as a proxy client (proxyClientEnv), which
// reads one call per stdin line, makes it with brain.SocketClient and answers on stdout.

const proxyClientEnv = "AGENT_JOBS_TEST_PROXY_CLIENT"

// peerCall is one call of the proxy client child.
type peerCall struct {
	Sock    string           `json:"sock"`
	JobID   string           `json:"job_id"`
	Op      string           `json:"op"`
	Query   string           `json:"query,omitempty"`
	Limit   int              `json:"limit,omitempty"`
	Note    *brain.NoteInput `json:"note,omitempty"`
	Text    string           `json:"text,omitempty"`
	Urgency string           `json:"urgency,omitempty"`
}

// peerAnswer is the child's answer to a peerCall.
type peerAnswer struct {
	Notes  []brain.Note `json:"notes,omitempty"`
	Result brain.Object `json:"result,omitempty"`
	Error  string       `json:"error,omitempty"`
}

// runProxyClient is the child's main loop (TestMain runs it when proxyClientEnv is set).
func runProxyClient() int {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<20)
	enc := json.NewEncoder(os.Stdout)
	for in.Scan() {
		var c peerCall
		var a peerAnswer
		if err := json.Unmarshal(in.Bytes(), &c); err != nil {
			a.Error = err.Error()
			_ = enc.Encode(a)
			continue
		}
		cl := brain.SocketClient{Path: c.Sock, JobID: c.JobID, Timeout: 20 * time.Second}
		ctx := context.Background()
		var err error
		switch c.Op {
		case brain.SocketOpSearch:
			a.Notes, err = cl.Search(ctx, c.Query, c.Limit)
		case brain.SocketOpWrite:
			a.Result, err = cl.Write(ctx, *c.Note)
		case brain.SocketOpNotify:
			a.Result, err = cl.NotifyOwner(ctx, c.Text, c.Urgency)
		default:
			err = errors.New("unknown op " + c.Op)
		}
		if err != nil {
			a.Error = err.Error()
		}
		_ = enc.Encode(a)
	}
	return 0
}

// peerClient drives one proxy client child; its methods mirror brain.SocketClient's.
type peerClient struct {
	t     *testing.T
	Path  string // the proxy socket
	JobID string // the job id the child claims in its calls
	pid   int
	in    io.WriteCloser
	out   *bufio.Scanner
}

// startPeer starts a proxy client child; with setsid it leads a session of its own (as a job's
// wrapper does), else it stays in the test's session.
func startPeer(t *testing.T, sock, jobID string, setsid bool, wrap ...string) *peerClient {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	argv := append(append([]string{}, wrap...), self)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), proxyClientEnv+"=1")
	cmd.Stderr = os.Stderr
	if setsid {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = in.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	return &peerClient{t: t, Path: sock, JobID: jobID, pid: cmd.Process.Pid, in: in, out: sc}
}

// jobPeer starts a proxy client child in a session of its own and records it as the wrapper
// (pid = pgid = session) of the running job j, so the proxy maps the child to j.
func jobPeer(t *testing.T, m *Manager, sock string, j *store.Job) *peerClient {
	t.Helper()
	c := startPeer(t, sock, j.ID, true)
	if err := m.Store.SetPID(j.ID, j.LaunchToken, c.pid, c.pid); err != nil {
		t.Fatal(err)
	}
	return c
}

// as is the same child claiming another job id.
func (c *peerClient) as(jobID string) *peerClient {
	d := *c
	d.JobID = jobID
	return &d
}

func (c *peerClient) call(pc peerCall) peerAnswer {
	c.t.Helper()
	pc.Sock, pc.JobID = c.Path, c.JobID
	b, err := json.Marshal(pc)
	if err != nil {
		c.t.Fatal(err)
	}
	if _, err := c.in.Write(append(b, '\n')); err != nil {
		c.t.Fatalf("proxy client: %v", err)
	}
	if !c.out.Scan() {
		c.t.Fatalf("proxy client exited: %v", c.out.Err())
	}
	var a peerAnswer
	if err := json.Unmarshal(c.out.Bytes(), &a); err != nil {
		c.t.Fatal(err)
	}
	return a
}

func answerErr(a peerAnswer) error {
	if a.Error != "" {
		return errors.New(a.Error)
	}
	return nil
}

func (c *peerClient) Search(_ context.Context, query string, limit int) ([]brain.Note, error) {
	a := c.call(peerCall{Op: brain.SocketOpSearch, Query: query, Limit: limit})
	return a.Notes, answerErr(a)
}

func (c *peerClient) Write(_ context.Context, n brain.NoteInput) (brain.Object, error) {
	a := c.call(peerCall{Op: brain.SocketOpWrite, Note: &n})
	return a.Result, answerErr(a)
}

func (c *peerClient) NotifyOwner(_ context.Context, text, urgency string) (brain.Object, error) {
	a := c.call(peerCall{Op: brain.SocketOpNotify, Text: text, Urgency: urgency})
	return a.Result, answerErr(a)
}
