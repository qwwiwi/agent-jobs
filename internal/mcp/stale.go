package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qwwiwi/agent-jobs/internal/jobs"
)

// ServerWatch notices, on each tool call of the long-lived orchestrator server, what changed on disk
// since it started: config.json is reloaded before the call (two stat calls when nothing changed), and
// a replaced binary or a daemon of another version adds a warning to the result. Upgrading restarts
// the daemon but not the session's MCP server, which would otherwise keep serving the old code.
type ServerWatch struct {
	m       *jobs.Manager
	started time.Time
	self    jobs.FileIdentity // the binary when the server started (with its digest)
	selfErr error

	// mu is held shared by every tool call and exclusively while the config is reloaded and swapped, so
	// no handler sees m.Cfg change under it.
	mu sync.RWMutex

	// cfgMu guards the fields below, which change only while mu is held exclusively, together with
	// m.Cfg: a call that sees the new identity finds the new config in force. cfgMu is never held while
	// waiting for mu: a call holding mu shared may take cfgMu (ConfigWarning), so that order would
	// deadlock.
	cfgMu       sync.Mutex
	loader      *jobs.ConfigReloader
	cfgErr      string
	restartKeys []string

	beforeSwap func() // test hook: a reload is about to wait for mu
}

// NewServerWatch records the identity and digest of m's binary now. config.json is reloaded before the
// first tool call: m.Cfg was loaded earlier, and a later identity would not prove what it holds.
func NewServerWatch(m *jobs.Manager) *ServerWatch {
	w := &ServerWatch{m: m, started: m.Now(), loader: jobs.NewConfigReloader(m.StateDir, m.Cfg)}
	w.self, w.selfErr = jobs.ReadIdentity(m.SelfBin)
	return w
}

// refreshConfig shares the validated loader and restart policy with the daemon. Publication stays
// exclusive with tool handlers; a warning lookup never waits on that exclusive lock.
func (w *ServerWatch) refreshConfig() {
	if !w.loader.Changed() {
		return
	}
	if w.beforeSwap != nil {
		w.beforeSwap()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	cfg, changed, errText, restart := w.loader.Reload()
	if !changed {
		return
	}
	w.cfgMu.Lock()
	defer w.cfgMu.Unlock()
	w.cfgErr, w.restartKeys = errText, restart
	if errText != "" {
		w.m.Log.Printf("config.json reload rejected; keeping last good config: %s", errText)
		return
	}
	w.m.Cfg = cfg
	w.m.Log.Printf("config.json reloaded (MCP)")
	for _, key := range restart {
		w.m.Log.Printf("config.json changed key %s requires MCP restart", key)
	}
}

// ConfigWarning says that a changed config.json did not load, or that its brain block changed and is
// not applied ("" = none).
func (w *ServerWatch) ConfigWarning() string {
	w.cfgMu.Lock()
	defer w.cfgMu.Unlock()
	switch {
	case w.cfgErr != "":
		return "config.json changed but does not load (" + jobs.RedactPrintable(w.cfgErr, 200) +
			"); this MCP server keeps the config it loaded before"
	case slices.Contains(w.restartKeys, "brain"):
		return "config.json changed its brain block; this MCP server keeps the brain it started with " +
			"until the session restarts (or /mcp reconnect)"
	}
	if len(w.restartKeys) > 0 {
		return "config.json changed keys require MCP restart (or /mcp reconnect): " + strings.Join(w.restartKeys, ", ")
	}
	return ""
}

// StaleBinary says that the binary on disk is not the one this server started from ("" = it is).
func (w *ServerWatch) StaleBinary() string {
	if w.selfErr != nil {
		return ""
	}
	now, err := jobs.StatIdentity(w.m.SelfBin)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return fmt.Sprintf("agent-jobs MCP server is older than the installed binary (started %s, binary %s removed); "+
			"restart the session (or /mcp reconnect) to use the new version.", fmtTS(&w.started), w.m.SelfBin)
	case err != nil || now.Same(w.self):
		return ""
	}
	return fmt.Sprintf("agent-jobs MCP server is older than the installed binary (started %s, binary replaced %s); "+
		"restart the session (or /mcp reconnect) to use the new version.", fmtTS(&w.started), fmtTS(&now.ModTime))
}

// DaemonMismatch says that the daemon runs another version or build than this server ("" = same).
func (w *ServerWatch) DaemonMismatch() string {
	if w.selfErr != nil {
		return ""
	}
	return w.m.DaemonMismatch(w.self)
}

// daemonCheckedTools also compare the server with the daemon: what job_start validates here, the
// daemon runs.
var daemonCheckedTools = map[string]bool{"job_start": true, "system_doctor": true}

// Warnings are the lines a call of tool gets added to its result.
func (w *ServerWatch) Warnings(tool string) []string {
	var out []string
	for _, s := range []string{w.StaleBinary(), w.ConfigWarning()} {
		if s != "" {
			out = append(out, s)
		}
	}
	if daemonCheckedTools[tool] {
		if s := w.m.DaemonConfigWarning(); s != "" {
			out = append(out, s)
		}
		if s := w.DaemonMismatch(); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// middleware reloads a changed config.json before each tools/call, holds mu shared while the call
// runs, and adds the call's warnings to its result as text lines.
func (w *ServerWatch) middleware() sdk.Middleware {
	return func(next sdk.MethodHandler) sdk.MethodHandler {
		return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
			if method != "tools/call" {
				return next(ctx, method, req)
			}
			w.refreshConfig()
			w.mu.RLock()
			res, err := next(ctx, method, req)
			w.mu.RUnlock()
			name := ""
			if r, ok := req.(*sdk.CallToolRequest); ok && r.Params != nil {
				name = r.Params.Name
			}
			if cr, ok := res.(*sdk.CallToolResult); ok && cr != nil {
				for _, s := range w.Warnings(name) {
					cr.Content = append(cr.Content, &sdk.TextContent{Text: s})
				}
			}
			return res, err
		}
	}
}
