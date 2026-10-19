package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	self    jobs.FileIdentity // the binary when the server started
	selfErr error

	// mu is held shared by every tool call and exclusively while the config is swapped, so no handler
	// sees m.Cfg change under it.
	mu sync.RWMutex

	// cfgMu guards the fields below. It is never held while waiting for mu: a call holding mu shared may
	// take cfgMu (ConfigWarning), so the reverse order would deadlock.
	cfgMu   sync.Mutex
	cfgID   jobs.FileIdentity
	cfgOK   bool   // config.json existed at the last check
	cfgErr  string // why the changed config.json was not loaded ("" = loaded)
	cfgGen  uint64 // bumped for every loaded config
	usedGen uint64 // the generation in m.Cfg (guarded by mu)
}

// NewServerWatch records the identity of m's binary and of config.json now.
func NewServerWatch(m *jobs.Manager) *ServerWatch {
	w := &ServerWatch{m: m, started: m.Now()}
	w.self, w.selfErr = jobs.StatIdentity(m.SelfBin)
	w.cfgID, w.cfgOK = w.configIdentity()
	return w
}

func (w *ServerWatch) configPath() string { return filepath.Join(w.m.StateDir, "config.json") }

func (w *ServerWatch) configIdentity() (jobs.FileIdentity, bool) {
	id, err := jobs.StatIdentity(w.configPath())
	return id, err == nil
}

// refreshConfig reloads config.json when it changed since the last check. A config that does not load
// keeps the previous one in force and is reported (ConfigWarning) until the file changes again.
func (w *ServerWatch) refreshConfig() {
	w.cfgMu.Lock()
	id, ok := w.configIdentity()
	if ok == w.cfgOK && (!ok || id.Same(w.cfgID)) {
		w.cfgMu.Unlock()
		return
	}
	w.cfgID, w.cfgOK = id, ok
	cfg, err := jobs.LoadConfig(w.m.StateDir)
	if err != nil {
		w.cfgErr = err.Error()
		w.cfgMu.Unlock()
		return
	}
	w.cfgErr = ""
	w.cfgGen++
	gen := w.cfgGen
	w.cfgMu.Unlock()
	// Swapped once no call runs; a slower refresh of an older file never replaces a newer config.
	w.mu.Lock()
	if gen > w.usedGen {
		w.m.Cfg, w.usedGen = cfg, gen
	}
	w.mu.Unlock()
}

// ConfigWarning says that a changed config.json did not load ("" = none).
func (w *ServerWatch) ConfigWarning() string {
	w.cfgMu.Lock()
	defer w.cfgMu.Unlock()
	if w.cfgErr == "" {
		return ""
	}
	return "config.json changed but does not load (" + jobs.RedactPrintable(w.cfgErr, 200) +
		"); this MCP server keeps the config it loaded before"
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
