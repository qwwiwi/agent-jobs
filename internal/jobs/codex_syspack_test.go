package jobs

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// codexHostRequirements is the live host's /etc/codex/requirements.toml.
const codexHostRequirements = "testdata/codex-requirements-host.toml"

// codexPackTOML is the host's managed-hooks requirements.toml moved to a pack in dir.
func codexPackTOML(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(codexHostRequirements)
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(b), "/etc/codex", dir)
}

// writeCodexPack builds a recognised managed-hooks pack in a fresh dir and stands in for the root
// ownership check (a test cannot create root-owned files).
func writeCodexPack(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "etc-codex")
	if err := os.MkdirAll(filepath.Join(dir, "hooks", "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"run", "bun", "bunfig.toml", "codex-hooks.js", "bridge-uid", "lib/util.js"} {
		if err := os.WriteFile(filepath.Join(dir, "hooks", f), []byte("x\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "requirements.toml"), []byte(codexPackTOML(t, dir)), 0o644); err != nil {
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

// The live host file, as it is and as it will be once the pack drops the legacy onFailure key, passes
// with the real /etc/codex paths; so do CRLF line ends, comments and a PreCompact event.
func TestCodexSystemConfigHostRequirements(t *testing.T) {
	b, err := os.ReadFile(codexHostRequirements)
	if err != nil {
		t.Fatal(err)
	}
	host := string(b)
	for _, e := range []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "Stop"} {
		for _, part := range []string{
			"[[hooks." + e + "]]\n",
			"[[hooks." + e + ".hooks]]\ntype = \"command\"\ncommand = \"'/etc/codex/hooks/run' " + e + "\"\ntimeout = 30\n",
		} {
			if !strings.Contains(host, part) {
				t.Fatalf("host fixture lacks %q", part)
			}
		}
	}
	if !strings.HasPrefix(host, "[hooks]\nmanaged_dir = \"/etc/codex/hooks\"\n") ||
		strings.Count(host, "onFailure = \"block\"\n") != 1 {
		t.Fatal("host fixture is not the live file")
	}
	for name, src := range map[string]string{
		"live":              host,
		"without onFailure": strings.Replace(host, "onFailure = \"block\"\n", "", 1),
		"CRLF":              strings.ReplaceAll(host, "\n", "\r\n"),
		"comments":          "# pack\t1 é\n" + host + "# end",
		"PreCompact": host + "\n[[hooks.PreCompact]]\n[[hooks.PreCompact.hooks]]\ntype = 'command'\n" +
			"command = \"/etc/codex/hooks/run-2.sh PreCompact\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			programs, err := checkCodexHooksRequirements(src, "/etc/codex/hooks")
			if err != nil {
				t.Fatalf("must pass: %v", err)
			}
			want := "run"
			if name == "PreCompact" {
				want = "run run-2.sh"
			}
			if got := strings.Join(programs, " "); got != want {
				t.Fatalf("programs %q, want %q", got, want)
			}
		})
	}
}

func TestCodexSystemConfigTyrandeM5Requirements(t *testing.T) {
	b, err := os.ReadFile("testdata/codex-requirements-tyrande-m5.toml")
	if err != nil {
		t.Fatal(err)
	}
	pack := string(b)
	const limit = "additionalContextLimit = 6000\n"
	const invalidLimit = "handler additionalContextLimit must be an integer between 1 and 100000"
	for _, tt := range []struct {
		name    string
		src     string
		wantErr string
	}{
		{"staged pack", pack, ""},
		{"minimum", strings.Replace(pack, limit, "additionalContextLimit = 1\n", 1), ""},
		{"maximum", strings.Replace(pack, limit, "additionalContextLimit = 100000\n", 1), ""},
		{"zero", strings.Replace(pack, limit, "additionalContextLimit = 0\n", 1), invalidLimit},
		{"negative", strings.Replace(pack, limit, "additionalContextLimit = -1\n", 1), "unsupported value"},
		{"string", strings.Replace(pack, limit, "additionalContextLimit = \"6000\"\n", 1), invalidLimit},
		{"float", strings.Replace(pack, limit, "additionalContextLimit = 6000.0\n", 1), "unexpected text after a value"},
		{"over maximum", strings.Replace(pack, limit, "additionalContextLimit = 100001\n", 1), invalidLimit},
		{"unknown key", strings.Replace(pack, limit, "unknownKey = 6000\n", 1), `handler key "unknownKey" is not allowed`},
		{"other event", strings.Replace(pack, limit, "", 1) + limit, `hooks.PreCompact.hooks: handler key "additionalContextLimit" is not allowed`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			programs, err := checkCodexHooksRequirements(tt.src, "/etc/codex/hooks")
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("must pass: %v", err)
			}
			if len(programs) != 1 || programs[0] != "run" {
				t.Fatalf("programs = %q, want [run]", programs)
			}
		})
	}
}

// Anything that is not exactly the pack refuses with the old message.
func TestCodexSystemConfigRefusesNonPack(t *testing.T) {
	const old = "codex has system or managed configuration in "
	// rewrite replaces from with to once in requirements.toml; DIR in either stands for the pack dir.
	rewrite := func(from, to string) func(*testing.T, string) {
		return func(t *testing.T, dir string) {
			t.Helper()
			from, to := strings.ReplaceAll(from, "DIR", dir), strings.ReplaceAll(to, "DIR", dir)
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
			if _, err := f.WriteString(strings.ReplaceAll(s, "DIR", dir)); err != nil {
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
	// symlink puts a symlink to /usr/bin/true at dir/rel, in place of what is there.
	symlink := func(rel string) func(*testing.T, string) {
		return func(t *testing.T, dir string) {
			t.Helper()
			p := filepath.Join(dir, rel)
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if err := os.Symlink("/usr/bin/true", p); err != nil {
				t.Fatal(err)
			}
		}
	}
	const stopHandler = "[[hooks.Stop.hooks]]\ntype = \"command\"\ncommand = \"'DIR/hooks/run' Stop\"\ntimeout = 30\n"
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

		// Every entry of the hooks directory, at any depth, and the programs the handlers name.
		{"symlinked hook program", symlink("hooks/run"), "hooks/run: not a regular file or directory"},
		{"symlink next to the hook program", symlink("hooks/extra"), "hooks/extra: not a regular file or directory"},
		{"symlink in a hooks subdirectory", symlink("hooks/lib/x"), "hooks/lib/x: not a regular file or directory"},
		{"group-writable hook file", chmod("hooks/bun", 0o775), "hooks/bun: group or world writable"},
		{"world-writable hook program", chmod("hooks/run", 0o757), "hooks/run: group or world writable"},
		{"world-writable hooks subdirectory", chmod("hooks/lib", 0o757), "hooks/lib: group or world writable"},
		{"group-writable file in a hooks subdirectory", chmod("hooks/lib/util.js", 0o664), "hooks/lib/util.js: group or world writable"},
		{"hook file not root-owned", func(*testing.T, string) {
			codexRootOwned = func(fi fs.FileInfo) bool { return fi.Name() != "codex-hooks.js" }
		}, "hooks/codex-hooks.js: not owned by root"},
		{"hook program missing", func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, "hooks", "run")); err != nil {
				t.Fatal(err)
			}
		}, "hooks/run: handler program is not a regular file"},
		{"hook program is a directory", rewrite("/hooks/run' Stop", "/hooks/lib' Stop"), "hooks/lib: handler program is not a regular file"},

		{"extra top-level key", add("\n[features]\napps = true\n"), `top-level key "features"`},
		{"extra top-level scalar", rewrite("[hooks]\n", "model = \"gpt-x\"\n[hooks]\n"), `top-level key "model"`},
		{"mcp_servers table", add("\n[mcp_servers.x]\ncommand = \"/bin/true\"\n"), `top-level key "mcp_servers"`},
		{"unknown hooks key", rewrite("managed_dir = ", "allowed_dirs = \"/tmp\"\nmanaged_dir = "), "hooks.allowed_dirs"},
		{"wrong managed_dir", rewrite("/hooks\"\n", "/other\"\n"), "managed_dir"},
		{"missing managed_dir", rewrite("managed_dir = ", "#managed_dir = "), "managed_dir"},

		// Event and handler shape: known events, the nested header form only, a handler per group.
		{"unknown event", rewrite("[[hooks.Stop]]\n\n[[hooks.Stop.hooks]]", "[[hooks.Bogus]]\n\n[[hooks.Bogus.hooks]]"),
			"hooks.Bogus: not a known hook event"},
		{"event name in another case", rewrite("[[hooks.Stop]]\n\n[[hooks.Stop.hooks]]", "[[hooks.stop]]\n\n[[hooks.stop.hooks]]"),
			"hooks.stop: not a known hook event"},
		{"handler directly in the event array", rewrite("[[hooks.Stop]]\n\n[[hooks.Stop.hooks]]\n", "[[hooks.Stop]]\n"),
			"hooks.Stop: key "},
		{"event group without handlers", add("\n[[hooks.PreCompact]]\n"), "hooks.PreCompact: a group without handlers"},
		{"second event group without handlers", add("\n[[hooks.Stop]]\n"), "hooks.Stop: a group without handlers"},
		{"event as a plain table", add("\n[hooks.PreCompact]\n"), "hooks.PreCompact: not an array of tables"},
		{"handlers as a plain table", rewrite("[[hooks.Stop.hooks]]", "[hooks.Stop.hooks]"), "hooks.Stop.hooks: not an array of tables"},
		{"handlers before their group", rewrite("[[hooks.Stop]]\n\n", ""), "hooks.Stop: not an array of tables"},
		{"inline handler array", rewrite(stopHandler,
			"hooks = [ { type = \"command\", command = \"'DIR/hooks/run' Stop\", timeout = 30 } ]\n"), "unsupported value"},
		{"inline hooks table extended by headers", rewrite("[hooks]\nmanaged_dir = \"DIR/hooks\"\n",
			"hooks = { managed_dir = \"DIR/hooks\" }\n"), "unsupported value"},
		{"dotted managed_dir key", rewrite("[hooks]\nmanaged_dir = ", "hooks.managed_dir = "), "dotted and quoted keys are not supported"},
		{"parent table after its children", func(t *testing.T, dir string) {
			rewrite("[hooks]\nmanaged_dir = \"DIR/hooks\"\n", "")(t, dir)
			add("\n[hooks]\nmanaged_dir = \"DIR/hooks\"\n")(t, dir)
		}, `duplicate table "hooks"`},

		{"hook command outside the hooks dir", rewrite("/hooks/run' Stop", "/../evil/run' Stop"), "command"},
		{"hook command elsewhere", rewrite("command = \"'", "command = \"/usr/bin/env '"), "command"},
		{"hook command with shell syntax", rewrite(" Stop\"", " Stop; curl evil\""), "command"},
		{"hook command in a subdirectory", rewrite("/hooks/run' Stop", "/hooks/lib/util.js' Stop"), "command"},
		{"hook command with the argument inside the quotes", rewrite("/hooks/run' Stop", "/hooks/run Stop'"), "handler command"},
		{"hook command without a space after the quote", rewrite("/hooks/run' Stop", "/hooks/run'Stop"), "handler command"},
		{"non-command handler", rewrite("type = \"command\"", "type = \"prompt\""), `type "prompt"`},
		{"handler without a type", rewrite("type = \"command\"\n", ""), "type"},
		{"handler without a command", rewrite("command = \"'DIR/hooks/run' Stop\"\n", ""), "handler command"},
		{"unknown handler key", rewrite("onFailure = \"block\"", "env = \"X=1\""), `key "env"`},
		{"onFailure outside PreToolUse", add("onFailure = \"block\"\n"), `hooks.Stop.hooks: handler key "onFailure" is not allowed`},
		{"unknown group key", rewrite("[[hooks.PreToolUse]]\n", "[[hooks.PreToolUse]]\nmatcher = \"Bash\"\n"), `key "matcher"`},
		{"TOML outside the strict subset", add("\n[hooks.state]\nx = 1.5\n"), "requirements.toml"},
		{"duplicate key", rewrite("timeout = 30\n", "timeout = 30\ntimeout = 11\n"), "duplicate"},
		{"escaped string", rewrite("onFailure = \"block\"", "onFailure = \"bl\\u006fck\""), "requirements.toml"},

		// Bytes: valid UTF-8, no control characters, CR only before LF, in comments too.
		{"control byte in a comment", add("# a\x01b\n"), "control character"},
		{"escape byte in a comment", add("# a\x1bb\n"), "control character"},
		{"DEL in a comment", add("# a\x7fb\n"), "control character"},
		{"NUL at the end", add("\x00"), "control character"},
		{"bare CR in a comment", add("# a\rmodel = 1\n"), "carriage return"},
		{"bare CR at the end", add("# a\r"), "carriage return"},
		{"invalid UTF-8 in a comment", add("# a\xffb\n"), "not valid UTF-8"},
		{"byte order mark", rewrite("[hooks]\n", "\xef\xbb\xbf[hooks]\n"), "requirements.toml"},
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
