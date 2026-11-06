package jobs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTree writes files (relative path -> body) under root.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// reachable is every file the view exposes to claude, following symlinks to files and directories
// (as claude does when it loads the plugin), with its content.
func reachable(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	var walk func(dir, rel string, depth int)
	walk = func(dir, rel string, depth int) {
		if depth > 20 {
			t.Fatalf("view nests too deep at %s", rel)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			p, r := filepath.Join(dir, e.Name()), filepath.Join(rel, e.Name())
			fi, err := os.Stat(p) // follows a symlink
			if err != nil {
				continue
			}
			if fi.IsDir() {
				walk(p, r, depth+1)
				continue
			}
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			out[r] = string(b)
		}
	}
	walk(root, "", 0)
	return out
}

// Fix loop 1, Critical: hooks (and MCP/LSP servers) written into a skill's, a command's or an agent's
// own front matter run outside the guard just like the manifest's. The default view (allow_hooks off)
// must not expose such a component, wherever it sits (default dirs or a custom manifest path).
func TestPluginViewDropsFrontMatterHooks(t *testing.T) {
	m := newManager(t)
	dir := makePlugin(t, "px", "1.0.0", nil)
	writeTree(t, dir, map[string]string{
		"skills/hooked/SKILL.md": "---\nname: hooked\ndescription: d\nhooks:\n  PreToolUse:\n    - matcher: Bash\n      hooks:\n" +
			"        - type: command\n          command: touch /tmp/FRONT_MATTER_HOOK_MARKER\n---\n\nbody\n",
		"skills/quoted/SKILL.md":  "---\nname: quoted\n\"hooks\": {PreToolUse: [{hooks: [{type: command, command: FRONT_MATTER_HOOK_MARKER}]}]}\n---\n",
		"commands/cmd.md":         "---\ndescription: c\nhooks:\n  Stop:\n    - hooks: [{type: command, command: FRONT_MATTER_HOOK_MARKER}]\n---\nrun\n",
		"agents/helper.md":        "---\nname: helper\nmcpServers:\n  evil: {command: FRONT_MATTER_HOOK_MARKER}\n---\nyou help\n",
		"custom/deep/x/SKILL.md":  "---\nname: x\nhooks: {Stop: [{hooks: [{type: command, command: FRONT_MATTER_HOOK_MARKER}]}]}\n---\n",
		"skills/plain/SKILL.md":   "---\nname: plain\ndescription: mentions hooks in text only\n---\n\nhooks: are fine in the body\n",
		"commands/ok.md":          "plain command, no front matter\n",
		"skills/plain/helper.txt": "data\n",
	})
	m.Cfg.JobPlugins = []JobPlugin{{Path: dir}}
	j := runToEnd(t, m, enqueue(t, m, "x", nil).ID)
	dirs := pluginDirArgs(argvOf(t, m, j.ID))
	if len(dirs) != 1 || dirs[0] == dir {
		t.Fatalf("--plugin-dir %v, want the view", dirs)
	}
	files := reachable(t, dirs[0])
	for rel, body := range files {
		if strings.Contains(body, "FRONT_MATTER_HOOK_MARKER") {
			t.Errorf("the view exposes %s with executable front matter", rel)
		}
	}
	for _, want := range []string{"skills/using-x/SKILL.md", "skills/plain/SKILL.md", "skills/plain/helper.txt", "commands/ok.md"} {
		if _, ok := files[filepath.FromSlash(want)]; !ok {
			t.Errorf("the view lost the clean %s", want)
		}
	}
	_, warns := m.JobPlugins(j)
	if joined := strings.Join(warns, "\n"); !strings.Contains(joined, filepath.FromSlash("skills/hooked/SKILL.md")) ||
		!strings.Contains(joined, filepath.FromSlash("agents/helper.md")) {
		t.Errorf("warnings do not name the dropped components: %q", warns)
	}
	// The plugin itself is untouched; allow_hooks passes it whole.
	if _, err := os.Stat(filepath.Join(dir, "skills", "hooked", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
}

// A symlinked directory inside the plugin is scanned too: a link must not smuggle a hooked skill past
// the view.
func TestPluginViewScansSymlinkedDirs(t *testing.T) {
	m := newManager(t)
	dir := makePlugin(t, "px", "1.0.0", nil)
	outside := t.TempDir()
	writeTree(t, outside, map[string]string{
		"evil/SKILL.md": "---\nname: evil\nhooks:\n  Stop: [{hooks: [{type: command, command: FRONT_MATTER_HOOK_MARKER}]}]\n---\n",
		"good/SKILL.md": "---\nname: good\n---\nGOOD_LINKED_SKILL\n",
	})
	if err := os.Symlink(outside, filepath.Join(dir, "skills", "linked")); err != nil {
		t.Fatal(err)
	}
	m.Cfg.JobPlugins = []JobPlugin{{Path: dir}}
	j := runToEnd(t, m, enqueue(t, m, "x", nil).ID)
	dirs := pluginDirArgs(argvOf(t, m, j.ID))
	if len(dirs) != 1 {
		t.Fatalf("--plugin-dir %v", dirs)
	}
	files := reachable(t, dirs[0])
	for rel, body := range files {
		if strings.Contains(body, "FRONT_MATTER_HOOK_MARKER") {
			t.Errorf("the view exposes %s with executable front matter through a symlinked dir", rel)
		}
	}
	if !strings.Contains(files[filepath.FromSlash("skills/linked/good/SKILL.md")], "GOOD_LINKED_SKILL") {
		t.Errorf("the clean skill behind the link is gone: %v", files)
	}
}

// Fix loop 1, High: the bootstrap is read below the plugin root without following any link, so a
// symlinked skills/ or skill directory cannot pull an outside SKILL.md into the prompt.
func TestPluginBootstrapAncestorSymlinkRefused(t *testing.T) {
	for _, tc := range []string{"skills dir", "skill dir"} {
		t.Run(tc, func(t *testing.T) {
			m := newManager(t)
			dir := makePlugin(t, "px", "1.0.0", map[string]string{})
			outside := t.TempDir()
			writeTree(t, outside, map[string]string{"linked/SKILL.md": "ANCESTOR_SECRET_MARKER\n"})
			switch tc {
			case "skills dir":
				if err := os.Symlink(outside, filepath.Join(dir, "skills")); err != nil {
					t.Fatal(err)
				}
			case "skill dir":
				if err := os.MkdirAll(filepath.Join(dir, "skills"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(outside, "linked"), filepath.Join(dir, "skills", "linked")); err != nil {
					t.Fatal(err)
				}
			}
			m.Cfg.JobPlugins = []JobPlugin{{Path: dir, BootstrapSkill: "linked"}}
			j := runToEnd(t, m, enqueue(t, m, "x", nil).ID)
			if strings.Contains(promptOf(t, m, j.ID), "ANCESTOR_SECRET_MARKER") {
				t.Fatal("a SKILL.md behind a symlinked ancestor was read into the prompt")
			}
			if _, warns := m.JobPlugins(j); !strings.Contains(strings.Join(warns, "\n"), "bootstrap skill linked") {
				t.Fatalf("warnings %q", warns)
			}
			if w := m.PluginWarnings(); !strings.Contains(strings.Join(w, "\n"), "bootstrap skill linked") {
				t.Fatalf("doctor warnings %q", w)
			}
		})
	}
}

// Fix loop 1, High: a job never writes the plugin registry (installed_plugins.json, which redirects a
// named plugin's next installPath) nor any configured plugin, also one its kind does not load and
// also when no plugin was loaded at all.
func TestPluginRegistryAndInactivePluginsProtected(t *testing.T) {
	m := newManager(t)
	cfgDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfgDir)
	named := makePlugin(t, "pn", "1.0.0", nil)
	registry := filepath.Join(cfgDir, "plugins", "installed_plugins.json")
	b, _ := json.Marshal(map[string]any{"version": 2, "plugins": map[string]any{
		"pn@mkt": []any{map[string]any{"scope": "user", "installPath": named, "version": "1.0.0"}},
	}})
	writeTree(t, cfgDir, map[string]string{"plugins/installed_plugins.json": string(b)})
	research := makePlugin(t, "pr", "1.0.0", nil)
	m.Cfg.JobPlugins = []JobPlugin{
		{Path: research, Kinds: []string{"research"}},
		{Name: "pn@mkt", Kinds: []string{"review"}},
	}
	j := protectJob(t, m, "plugprot") // kind fix: loads neither plugin, has no plugins.json
	cfg := jobHookConfig(t, m, j)
	paths := []string{
		registry,
		filepath.Join(research, "skills", "using-x", "SKILL.md"),
		filepath.Join(research, "hooks", "hooks.json"),
		filepath.Join(named, "skills", "using-x", "SKILL.md"),
	}
	var cmds []string
	for _, p := range paths {
		cmds = append(cmds, bashWrites(p)...)
	}
	assertWritesRefused(t, cfg, paths, cmds)
}

// Fix loop 1, Medium: a resumed session gets the bootstrap read again for this attempt (the fallback
// prompt already has it through buildPrompt), and the fallback does not get it twice.
func TestPluginBootstrapInResumedSession(t *testing.T) {
	m := newManager(t)
	dir := makePlugin(t, "px", "1.0.0", map[string]string{"using-x": "---\nname: using-x\n---\nBOOTSTRAP_V1_MARKER\n"})
	m.Cfg.JobPlugins = []JobPlugin{{Path: dir, BootstrapSkill: "using-x"}}
	j := startAndStop(t, m, nil)
	if !strings.Contains(promptOf(t, m, j.ID), "BOOTSTRAP_V1_MARKER") {
		t.Fatal("first prompt lacks the bootstrap")
	}
	writeTree(t, dir, map[string]string{"skills/using-x/SKILL.md": "---\nname: using-x\n---\nBOOTSTRAP_V2_MARKER\n"})
	if _, err := m.Resume(j.ID, "go on", Limits{MaxBudgetUSD: 4}); err != nil {
		t.Fatal(err)
	}
	runToEnd(t, m, j.ID)
	jd := m.JobDir(j.ID)
	resumed := readFile(t, filepath.Join(jd, resumePromptFile))
	if !strings.Contains(resumed, "BOOTSTRAP_V2_MARKER") || strings.Contains(resumed, "BOOTSTRAP_V1_MARKER") {
		t.Fatalf("the resumed session's update lacks the new bootstrap:\n%s", resumed)
	}
	if fb := readFile(t, filepath.Join(jd, fallbackPromptFile)); strings.Count(fb, "BOOTSTRAP_V2_MARKER") != 1 {
		t.Fatalf("the fallback prompt has the bootstrap %d times", strings.Count(fb, "BOOTSTRAP_V2_MARKER"))
	}
}

// execFrontMatter fails closed: whatever it cannot read as plain block YAML keeps the file out.
func TestExecFrontMatter(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name, body string
		out        bool
	}{
		{"no front matter", "# title\nhooks: in the body\n", false},
		{"clean", "---\nname: a\ndescription: uses hooks in prose\nallowed-tools: Bash\n---\nbody\n", false},
		{"block scalar", "---\nname: a\ndescription: |\n  hooks: inside a block scalar\n---\n", false},
		{"crlf clean", "---\r\nname: a\r\n---\r\n", false},
		{"horizontal rule", "----\nhooks: x\n", false},
		{"hooks", "---\nname: a\nhooks:\n  Stop: []\n---\n", true},
		{"hooks spaced", "---\nhooks :\n  Stop: []\n---\n", true},
		{"mcp servers", "---\nmcpServers: {x: {command: y}}\n---\n", true},
		{"lsp case", "---\nLSPServers: {}\n---\n", true},
		{"crlf hooks", "---\r\nhooks:\r\n  Stop: []\r\n---\r\n", true},
		{"bom", "\ufeff---\nhooks: {}\n---\n", true},
		{"quoted key", "---\n'hooks': {}\n---\n", true},
		{"flow document", "---\n{name: a, hooks: {}}\n---\n", true},
		{"explicit key", "---\n? hooks\n: {}\n---\n", true},
		{"merge", "---\nx: &a {hooks: {}}\n<<: *a\n---\n", true},
		{"tag", "---\n!!str hooks: {}\n---\n", true},
		{"tab indent", "---\nname: a\n\thooks: {}\n---\n", true},
		{"unterminated", "---\nname: a\n", true},
	} {
		p := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "_")+".md")
		if err := os.WriteFile(p, []byte(tc.body), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := execFrontMatter(p); (got != "") != tc.out {
			t.Errorf("%s: execFrontMatter = %q, want out=%v", tc.name, got, tc.out)
		}
	}
}
