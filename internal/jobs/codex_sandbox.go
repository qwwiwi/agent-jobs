package jobs

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

// Codex sandbox selection (issue #81). On some Linux hosts codex's native sandbox cannot start: bwrap
// dies with "bwrap: loopback: Failed RTM_NEWADDR: Operation not permitted" (an unprivileged network
// namespace without the right to set up its loopback, e.g. inside a restricted systemd slice), every
// command of the job fails, and codex still ends its turn. Config codex_sandbox picks what a codex job
// runs in:
//
//   - auto (default): the native sandbox, after a cached probe (`codex sandbox linux -- true`); when the
//     probe shows the sandbox cannot start, codex jobs are refused at launch with an error naming the
//     option. auto never falls back to a weaker containment by itself.
//   - native: the native sandbox, never probed or refused (a probe that misjudges the host).
//   - worktree-full-access: `-s danger-full-access`; the job's own worktree is the only isolation codex
//     gets (no write or network confinement by codex). The owner opts in explicitly.
const (
	CodexSandboxModeAuto               = "auto"
	CodexSandboxModeNative             = "native"
	CodexSandboxModeWorktreeFullAccess = "worktree-full-access"
)

// CodexSandboxModes are the accepted codex_sandbox values.
var CodexSandboxModes = []string{CodexSandboxModeAuto, CodexSandboxModeNative, CodexSandboxModeWorktreeFullAccess}

// CodexSandboxFullAccess is the `codex exec --sandbox` value of codex_sandbox worktree-full-access.
const CodexSandboxFullAccess = "danger-full-access"

// Results of the native sandbox probe.
const (
	CodexProbeOK           = "ok"           // the sandbox ran a command
	CodexProbeUnavailable  = "unavailable"  // the sandbox failed to start (bwrap error)
	CodexProbeInconclusive = "inconclusive" // the probe failed for another reason (no binary, unknown subcommand, timeout)
	CodexProbeSkipped      = "skipped"      // not Linux: the probe covers the bwrap sandbox only
)

// CodexSandboxProbe is one run of the native sandbox probe.
type CodexSandboxProbe struct {
	Result string    `json:"result"`
	Detail string    `json:"detail,omitempty"`
	Bin    string    `json:"bin"`
	At     time.Time `json:"at"`
}

// CodexSandboxStatus is system_doctor's view of the codex sandbox: the configured mode, what a codex
// launch would run in now (native, worktree-full-access, or refused) and the probe behind it.
type CodexSandboxStatus struct {
	Mode      string             `json:"mode"`
	Effective string             `json:"effective"`
	Probe     *CodexSandboxProbe `json:"probe,omitempty"`
	// Problem is set when codex jobs are refused (auto) or would fail (native) on this host.
	Problem string `json:"problem,omitempty"`
}

// codexSandboxMode is the configured codex_sandbox (auto when unset).
func (c Config) codexSandboxMode() string {
	if c.CodexSandboxMode == "" {
		return CodexSandboxModeAuto
	}
	return c.CodexSandboxMode
}

func validateCodexSandboxMode(mode string) error {
	if mode != "" && !slices.Contains(CodexSandboxModes, mode) {
		return fmt.Errorf("codex_sandbox %q must be one of %s", mode, strings.Join(CodexSandboxModes, "|"))
	}
	return nil
}

// Test hooks: the probe's command line, timeout and platform.
var (
	codexProbeArgs    = []string{"sandbox", "linux", "--", "true"}
	codexProbeTimeout = 15 * time.Second
	codexProbeGOOS    = runtime.GOOS
)

// probeCodexSandbox runs `<bin> sandbox linux -- true` (no model, no network) in a fresh temp dir:
// exit 0 = ok; a bwrap error line = unavailable; anything else = inconclusive (the launch then goes on
// natively and the stream check below still catches a sandbox that does not start).
func probeCodexSandbox(bin string, env []string) CodexSandboxProbe {
	p := CodexSandboxProbe{Bin: bin, At: time.Now().UTC()}
	if codexProbeGOOS != "linux" {
		p.Result, p.Detail = CodexProbeSkipped, "the probe covers the Linux (bwrap) sandbox only"
		return p
	}
	dir, err := os.MkdirTemp("", "agent-jobs-codex-probe-")
	if err != nil {
		p.Result, p.Detail = CodexProbeInconclusive, "temp dir: "+err.Error()
		return p
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithTimeout(context.Background(), codexProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, codexProbeArgs...)
	cmd.Dir, cmd.Env = dir, TempEnv(env, dir)
	cmd.WaitDelay = time.Second // a child holding the pipe open does not outlive the timeout
	out, err := cmd.CombinedOutput()
	switch {
	case ctx.Err() != nil:
		p.Result, p.Detail = CodexProbeInconclusive, fmt.Sprintf("timed out after %s", codexProbeTimeout)
	case err == nil:
		p.Result = CodexProbeOK
	default:
		if line := bwrapErrorLine(string(out), true); line != "" {
			p.Result, p.Detail = CodexProbeUnavailable, line
		} else {
			p.Result = CodexProbeInconclusive
			p.Detail = fmt.Sprintf("`%s %s` failed: %v: %s", filepath.Base(bin), strings.Join(codexProbeArgs, " "), err,
				Printable(strings.TrimSpace(string(out)), 200))
		}
	}
	return p
}

// bwrapErrorLine returns the bwrap error in a command's output ("" = none): bwrap prints "bwrap: ..."
// and exits before the command starts. anyLine looks at every line (the probe's own output); otherwise
// only the first non-empty line counts, so a job that greps or prints a file mentioning bwrap does not
// look like a sandbox failure.
func bwrapErrorLine(out string, anyLine bool) string {
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if strings.HasPrefix(l, "bwrap:") {
			return Printable(l, 160)
		}
		if !anyLine {
			return ""
		}
	}
	return ""
}

// codexBinKey identifies the codex binary a probe ran: its path on PATH and the file's identity, so a
// codex upgrade (a new file) is probed again.
func codexBinKey(bin string) string {
	path, err := exec.LookPath(bin)
	if err != nil {
		return "missing:" + bin
	}
	id, err := StatIdentity(path)
	if err != nil {
		return "unreadable:" + path
	}
	return fmt.Sprintf("%s|%d|%d|%d|%d", path, id.Dev, id.Ino, id.Size, id.ModTime.UnixNano())
}

// codexSandboxProbe returns the cached probe of bin, probing once per binary identity.
func (m *Manager) codexSandboxProbe(bin string) CodexSandboxProbe {
	key := codexBinKey(bin)
	m.codexProbeMu.Lock()
	defer m.codexProbeMu.Unlock()
	if m.codexProbe != nil && m.codexProbeKey == key {
		return *m.codexProbe
	}
	p := probeCodexSandbox(bin, m.jobSpawnEnv())
	m.codexProbe, m.codexProbeKey = &p, key
	if m.Log != nil {
		m.Log.Printf("codex sandbox probe (%s): %s %s", bin, p.Result, RedactText(p.Detail))
	}
	return p
}

// codexSandboxRefusal is the launch refusal of auto mode when the native sandbox does not start.
func codexSandboxRefusal(p CodexSandboxProbe) error {
	return fmt.Errorf("the native codex sandbox does not start on this host (probe `%s %s`: %s); codex jobs are "+
		"refused: set config codex_sandbox to %q to run them in their own worktree without the codex sandbox "+
		"(weaker containment, see README), or run the job on claude",
		filepath.Base(p.Bin), strings.Join(codexProbeArgs, " "), p.Detail, CodexSandboxModeWorktreeFullAccess)
}

// codexLaunchSandbox is the `codex exec --sandbox` value of a codex job's launch under cfg, or the
// refusal of the launch (auto mode, native sandbox unavailable).
func (m *Manager) codexLaunchSandbox(cfg Config, kind string) (string, error) {
	switch cfg.codexSandboxMode() {
	case CodexSandboxModeWorktreeFullAccess:
		return CodexSandboxFullAccess, nil
	case CodexSandboxModeNative:
		return CodexSandbox(kind), nil
	}
	if p := m.codexSandboxProbe(cfg.codexBin()); p.Result == CodexProbeUnavailable {
		return "", codexSandboxRefusal(p)
	}
	return CodexSandbox(kind), nil
}

// CodexSandboxStatus reports the codex sandbox for system_doctor (it probes when the mode uses the native
// sandbox and no probe of the current binary is cached).
func (m *Manager) CodexSandboxStatus() CodexSandboxStatus {
	cfg := m.Config()
	s := CodexSandboxStatus{Mode: cfg.codexSandboxMode(), Effective: CodexSandboxModeNative}
	if s.Mode == CodexSandboxModeWorktreeFullAccess {
		s.Effective = CodexSandboxModeWorktreeFullAccess
		return s
	}
	p := m.codexSandboxProbe(cfg.codexBin())
	s.Probe = &p
	if p.Result != CodexProbeUnavailable {
		return s
	}
	if s.Mode == CodexSandboxModeAuto {
		s.Effective = "refused"
		s.Problem = "codex sandbox: " + codexSandboxRefusal(p).Error()
	} else {
		s.Problem = fmt.Sprintf("codex sandbox: codex_sandbox is native, but the native sandbox does not start on this "+
			"host (%s): codex jobs will fail; set codex_sandbox to %q", p.Detail, CodexSandboxModeWorktreeFullAccess)
	}
	return s
}

// codexFullAccessDroppedEnv are the credentials a worktree-full-access codex job does not inherit: with
// no codex sandbox, network and the owner's files are open to it, so the push protection that does not
// depend on the sandbox is the job's git environment (no credential helper, no ssh command; GitEnv)
// plus these: no GitHub tokens, no ssh agent, and a gh config dir without stored auth.
var codexFullAccessDroppedEnv = []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN",
	"SSH_AUTH_SOCK", "GH_CONFIG_DIR"}

// codexFullAccessEnv returns env without codexFullAccessDroppedEnv and with GH_CONFIG_DIR set to a
// path in the job dir that holds no gh auth.
func codexFullAccessEnv(env []string, jobDir string) []string {
	out := slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		return slices.Contains(codexFullAccessDroppedEnv, k)
	})
	return append(out, "GH_CONFIG_DIR="+filepath.Join(jobDir, "no-gh-config"))
}
