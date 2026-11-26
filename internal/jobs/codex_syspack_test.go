package jobs

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// codexPackTOML is the host's managed-hooks requirements.toml for a pack in dir: only a [hooks] table
// with managed_dir and event arrays of command handlers under <dir>/hooks.
func codexPackTOML(dir string) string {
	run := "'" + dir + "/hooks/run'"
	return `# managed hooks pack
[hooks]
managed_dir = "` + dir + `/hooks"

[[hooks.PreToolUse]]

[[hooks.PreToolUse.hooks]]
type = "command"
command = "` + run + ` PreToolUse"
timeout = 10
onFailure = "block"

[[hooks.SessionStart]]
hooks = [
  { type = "command", command = "` + run + ` SessionStart", timeout = 5, statusMessage = 'starting' }, # inline form
]

[[hooks.Stop]]
type = "command"
command = "` + run + ` Stop"
timeout = 5
`
}

// writeCodexPack builds a recognised managed-hooks pack in a fresh dir and stands in for the root
// ownership check (a test cannot create root-owned files).
func writeCodexPack(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "etc-codex")
	if err := os.MkdirAll(filepath.Join(dir, "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"run", "bun", "bunfig.toml", "codex-hooks.js", "bridge-uid"} {
		if err := os.WriteFile(filepath.Join(dir, "hooks", f), []byte("x\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "requirements.toml"), []byte(codexPackTOML(dir)), 0o644); err != nil {
		t.Fatal(err)
	}
	prev := codexRootOwned
	codexRootOwned = func(fs.FileInfo) bool { return true }
	t.Cleanup(func() { codexRootOwned = prev })
	return dir
}

// The owner's narrow exception: /etc/codex holding exactly the recognised managed-hooks pack passes.
func TestCodexSystemConfigAllowsManagedHooksPack(t *testing.T) {
	dir := writeCodexPack(t)
	if err := checkCodexSystemConfig(dir); err != nil {
		t.Fatalf("exact pack must be allowed: %v", err)
	}
	// And a codex job launches with it.
	m := newCodexManager(t)
	codexSystemConfigDir = dir
	if j := runToEnd(t, m, enqueue(t, m, "review it", codexReview).ID); j.Status != store.StatusSucceeded {
		t.Fatalf("exact pack must launch: %s %q", j.Status, j.Error)
	}
}

// Anything that is not exactly the pack refuses with the old message.
func TestCodexSystemConfigRefusesNonPack(t *testing.T) {
	const old = "codex has system or managed configuration in "
	rewrite := func(from, to string) func(*testing.T, string) {
		return func(t *testing.T, dir string) {
			t.Helper()
			p := filepath.Join(dir, "requirements.toml")
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(b), from) {
				t.Fatalf("fixture lacks %q", from)
			}
			if err := os.WriteFile(p, []byte(strings.Replace(string(b), from, to, 1)), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	add := func(s string) func(*testing.T, string) {
		return func(t *testing.T, dir string) {
			t.Helper()
			f, err := os.OpenFile(filepath.Join(dir, "requirements.toml"), os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if _, err := f.WriteString(s); err != nil {
				t.Fatal(err)
			}
		}
	}
	write := func(name string) func(*testing.T, string) {
		return func(t *testing.T, dir string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(dir, name), []byte("[features]\napps = true\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	chmod := func(rel string, mode fs.FileMode) func(*testing.T, string) {
		return func(t *testing.T, dir string) {
			t.Helper()
			if err := os.Chmod(filepath.Join(dir, rel), mode); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, c := range []struct {
		name   string
		break_ func(*testing.T, string)
		want   string // also in the error
	}{
		{"extra file", write("notes.txt"), "notes.txt"},
		{"config.toml present", write("config.toml"), "config.toml"},
		{"managed_config.toml present", write("managed_config.toml"), "managed_config.toml"},
		{"symlinked requirements.toml", func(t *testing.T, dir string) {
			p := filepath.Join(dir, "requirements.toml")
			real := filepath.Join(filepath.Dir(dir), "real.toml")
			if err := os.Rename(p, real); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, p); err != nil {
				t.Fatal(err)
			}
		}, "requirements.toml: not a regular file"},
		{"symlinked hooks dir", func(t *testing.T, dir string) {
			p := filepath.Join(dir, "hooks")
			real := filepath.Join(filepath.Dir(dir), "real-hooks")
			if err := os.Rename(p, real); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, p); err != nil {
				t.Fatal(err)
			}
		}, "hooks: not a directory"},
		{"group-writable requirements.toml", chmod("requirements.toml", 0o664), "requirements.toml: group or world writable"},
		{"world-writable hooks dir", chmod("hooks", 0o757), "hooks: group or world writable"},
		{"group-writable dir", chmod("", 0o775), "group or world writable"},
		{"not root-owned", func(*testing.T, string) {
			codexRootOwned = func(fs.FileInfo) bool { return false }
		}, "not owned by root"},
		{"extra top-level key", add("\n[features]\napps = true\n"), `top-level key "features"`},
		{"extra top-level scalar", rewrite("# managed hooks pack\n", "model = \"gpt-x\"\n"), `top-level key "model"`},
		{"mcp_servers table", add("\n[mcp_servers.x]\ncommand = \"/bin/true\"\n"), `top-level key "mcp_servers"`},
		{"unknown hooks key", rewrite("managed_dir = ", "allowed_dirs = [\"/tmp\"]\nmanaged_dir = "), "hooks.allowed_dirs"},
		{"wrong managed_dir", rewrite("/hooks\"\n", "/other\"\n"), "managed_dir"},
		{"missing managed_dir", rewrite("managed_dir = ", "#managed_dir = "), "managed_dir"},
		{"hook command outside the hooks dir", rewrite("/hooks/run' Stop", "/../evil/run' Stop"), "command"},
		{"hook command elsewhere", rewrite("command = \"'", "command = \"/usr/bin/env '"), "command"},
		{"hook command with shell syntax", rewrite(" Stop\"", " Stop; curl evil\""), "command"},
		{"hook command in a subdirectory", rewrite("/hooks/run' Stop", "/hooks/../hooks/run' Stop"), "command"},
		{"non-command handler", rewrite("type = \"command\"", "type = \"prompt\""), `type "prompt"`},
		{"handler without a type", rewrite("type = \"command\"\n", ""), "type"},
		{"unknown handler key", rewrite("onFailure = \"block\"", "env = \"X=1\""), `key "env"`},
		{"unknown inline handler key", rewrite("statusMessage = 'starting'", "async = 'x'"), `key "async"`},
		{"unknown group key", rewrite("[[hooks.PreToolUse]]\n", "[[hooks.PreToolUse]]\nmatcher = \"Bash\"\n"), `key "matcher"`},
		{"TOML outside the strict subset", add("\n[hooks.state]\nx = 1.5\n"), "requirements.toml"},
		{"duplicate key", rewrite("timeout = 10\n", "timeout = 10\ntimeout = 11\n"), "duplicate"},
		{"escaped string", rewrite("onFailure = \"block\"", "onFailure = \"bl\\u006fck\""), "requirements.toml"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := writeCodexPack(t)
			c.break_(t, dir)
			err := checkCodexSystemConfig(dir)
			if err == nil {
				t.Fatal("must refuse")
			}
			if !strings.HasPrefix(err.Error(), old+dir+" (") ||
				!strings.Contains(err.Error(), "; codex jobs refuse to run with it") {
				t.Fatalf("not the old refusal: %v", err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %q, want it to name %q", err, c.want)
			}
		})
	}
}

// Without the test stand-in the real owner check refuses a pack a non-root user wrote.
func TestCodexSystemConfigRealOwnerCheck(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("runs as root: every file here is root-owned")
	}
	dir := writeCodexPack(t)
	codexRootOwned = codexRootOwnedSys
	err := checkCodexSystemConfig(dir)
	if err == nil || !strings.Contains(err.Error(), "not owned by root") {
		t.Fatalf("got %v, want a not-owned-by-root refusal", err)
	}
}
