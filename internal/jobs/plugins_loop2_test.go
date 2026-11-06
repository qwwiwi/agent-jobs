package jobs

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fix loop 2, Critical: front matter is checked against an allowlist. Every line is a root-level
// "key: scalar" with an allowed key, a comment, a blank line or an indented continuation of an allowed
// key's block scalar or nested value; anything else (the bypasses of the loop 1 denylist included)
// keeps the file out.
func TestFrontMatterAllowlist(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		out        bool
	}{
		// kept
		{"no front matter", "# title\nhooks: in the body\n", false},
		{"empty file", "", false},
		{"clean", "---\nname: a\ndescription: uses hooks in prose\nallowed-tools: Bash, Read\n---\nbody\n", false},
		{"quoted values", "---\nname: \"a\"\ndescription: 'it''s fine: really'\n---\n", false},
		{"block scalar", "---\nname: a\ndescription: |\n  hooks: inside a block scalar\n\n  more\n---\n", false},
		{"folded scalar", "---\ndescription: >-\n  hooks: {} folded\n---\n", false},
		{"nested metadata", "---\nname: a\nmetadata:\n  author: me\n  tags:\n    - x\n---\n", false},
		{"nested allowed-tools", "---\nallowed-tools:\n  - Read\n  - Bash\n---\n", false},
		{"comment", "---\n# a note\nname: a # trailing\n---\n", false},
		{"github template", "---\nname: Bug Report\nabout: Something isn't working as expected\nlabels: bug\n---\n", false},
		{"horizontal rule later", "# t\n\n---\nhooks: x\n---\n", false},
		// the control review's bypasses
		{"indented hooks", "---\n  name: x\n  hooks: {}\n---\n", true},
		{"indented mcp", "---\n  name: x\n  mcpServers: {e: {command: y}}\n---\n", true},
		{"indented quoted key", "---\n  name: x\n  \"hooks\": {}\n---\n", true},
		{"indented flow mapping", "---\n  {name: x, hooks: {}}\n---\n", true},
		{"indented explicit key", "---\n  ? hooks\n  : {}\n---\n", true},
		{"indented merge", "---\n  x: &a {hooks: {}}\n  <<: *a\n---\n", true},
		{"duplicate key", "---\nname: a\nname: b\n---\n", true},
		{"quoted key", "---\n'hooks': {}\n---\n", true},
		{"double quoted key", "---\n\"hooks\": {}\n---\n", true},
		{"flow document", "---\n{name: a, hooks: {}}\n---\n", true},
		{"flow mapping value", "---\nmetadata: {hooks: {}}\n---\n", true},
		{"flow sequence value", "---\nallowed-tools: [Read]\n---\n", true},
		{"explicit key", "---\n? hooks\n: {}\n---\n", true},
		{"merge", "---\nx: &a {hooks: {}}\n<<: *a\n---\n", true},
		{"anchor", "---\nname: &a x\n---\n", true},
		{"alias", "---\nname: *a\n---\n", true},
		{"tag", "---\nname: !!str a\n---\n", true},
		{"tag key", "---\n!!str hooks: {}\n---\n", true},
		// keys outside the allowlist
		{"hooks", "---\nname: a\nhooks:\n  Stop: []\n---\n", true},
		{"hooks spaced", "---\nhooks :\n  Stop: []\n---\n", true},
		{"lsp case", "---\nLSPServers: {}\n---\n", true},
		{"hooks upper", "---\nHooks: {}\n---\n", true},
		{"permission mode", "---\nname: a\npermissionMode: bypassPermissions\n---\n", true},
		{"no space after colon", "---\nname:x\n---\n", true},
		// structure the check does not read
		{"plain continuation", "---\ndescription: a\n  hooks: {}\n---\n", true},
		{"less indented continuation", "---\nmetadata:\n    a: 1\n  hooks: {}\n---\n", true},
		{"sequence at root indent", "---\nallowed-tools:\n- Read\n---\n", true},
		{"tab indent", "---\nname: a\n\thooks: {}\n---\n", true},
		{"tab in value", "---\nname: a\tb\n---\n", true},
		{"bom", "\xef\xbb\xbf---\nname: a\n---\n", true},
		{"crlf", "---\r\nname: a\r\n---\r\n", true},
		{"cr in a line", "---\nname: a\rhooks: {}\n---\n", true},
		{"leading blank line", "\n---\nname: a\n---\n", true},
		{"leading space", " ---\nname: a\n---\n", true},
		{"rule opener", "----\nhooks: x\n", true},
		{"opener with text", "--- x\nname: a\n---\n", true},
		{"dots close", "---\nname: a\n...\nhooks: {}\n---\n", true},
		{"closer with text", "---\nname: a\n---x\nhooks: {}\n---\n", true},
		{"document marker inside", "---\nname: a\n--- \nhooks: {}\n---\n", true},
		{"unterminated", "---\nname: a\n", true},
		{"unterminated quote", "---\ndescription: \"a\nhooks: {}\n---\n", true},
		{"nul", "---\nname: a\x00\n---\n", true},
		{"nel line break", "---\nname: a\xc2\x85hooks: {}\n---\n", true},
		{"unicode line separator", "---\nname: a\xe2\x80\xa8hooks: {}\n---\n", true},
		{"vertical tab", "---\nname: a\vhooks: {}\n---\n", true},
		{"invalid utf8", "---\nname: \xff\n---\n", true},
	} {
		got := frontMatterProblem([]byte(tc.body))
		if (got != "") != tc.out {
			t.Errorf("%s: frontMatterProblem = %q, want out=%v", tc.name, got, tc.out)
		}
	}
}

// snapshotSource is m.findPlugin for a configured path.
func snapshotSource(t *testing.T, m *Manager, p JobPlugin) *pluginSource {
	t.Helper()
	ps, err := m.findPlugin(p)
	if err != nil {
		t.Fatal(err)
	}
	return ps
}

// assertNoLinks fails when the tree at root has anything but real directories and regular files.
func assertNoLinks(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && !d.Type().IsRegular() {
			t.Errorf("the snapshot has a non-regular entry %s (%s)", p, d.Type())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Fix loop 2, Critical: .claude-plugin is no blind spot. A markdown file under it is checked like any
// other, and a manifest that points a component into it refuses the plugin.
func TestPluginViewClaudePluginDirScanned(t *testing.T) {
	m := newManager(t)
	dir := makePlugin(t, "px", "1.0.0", nil)
	hooked := "---\nname: hidden\nhooks:\n  Stop: [{hooks: [{type: command, command: FRONT_MATTER_HOOK_MARKER}]}]\n---\n"
	writeTree(t, dir, map[string]string{".claude-plugin/hidden/SKILL.md": hooked})
	view := filepath.Join(t.TempDir(), "view")
	warns, err := buildPluginView(snapshotSource(t, m, JobPlugin{Path: dir}), view, true)
	if err != nil {
		t.Fatal(err)
	}
	for rel, body := range reachable(t, view) {
		if strings.Contains(body, "FRONT_MATTER_HOOK_MARKER") {
			t.Errorf("the snapshot exposes %s", rel)
		}
	}
	if !strings.Contains(strings.Join(warns, "\n"), filepath.FromSlash(".claude-plugin/hidden/SKILL.md")) {
		t.Errorf("warnings do not name the dropped file: %q", warns)
	}

	// The manifest points skills into .claude-plugin: the plugin is not loaded at all.
	mb, _ := json.Marshal(map[string]any{"name": "px", "skills": "./.claude-plugin/hidden"})
	writeTree(t, dir, map[string]string{".claude-plugin/plugin.json": string(mb)})
	m.Cfg.JobPlugins = []JobPlugin{{Path: dir}}
	j := runToEnd(t, m, enqueue(t, m, "x", nil).ID)
	if got := pluginDirArgs(argvOf(t, m, j.ID)); len(got) != 0 {
		t.Fatalf("a manifest pointing into .claude-plugin was loaded: %v", got)
	}
	if _, w := m.JobPlugins(j); !strings.Contains(strings.Join(w, "\n"), ".claude-plugin") {
		t.Fatalf("warnings %q", w)
	}
}

// Fix loop 2: the snapshot manifest keeps only known keys, without hooks and servers, and drops a
// component path that leaves the snapshot.
func TestPluginViewManifestSanitized(t *testing.T) {
	m := newManager(t)
	dir := makePlugin(t, "px", "1.0.0", nil)
	mb, _ := json.Marshal(map[string]any{
		"name": "px", "version": "1.0.0", "description": "d", "author": map[string]any{"name": "a"},
		"hooks": "./hooks/hooks.json", "mcpServers": "./.mcp.json", "lspServers": map[string]any{},
		"commands": "../outside", "agents": []any{"./agents", "/abs/agents"}, "outputStyles": 7,
		"skills": []any{"./custom", "./skills/../more"}, "monitors": map[string]any{"x": "y"},
	})
	writeTree(t, dir, map[string]string{".claude-plugin/plugin.json": string(mb)})
	view := filepath.Join(t.TempDir(), "view")
	warns, err := buildPluginView(snapshotSource(t, m, JobPlugin{Path: dir}), view, true)
	if err != nil {
		t.Fatal(err)
	}
	var man map[string]json.RawMessage
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(view, ".claude-plugin", "plugin.json"))), &man); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"hooks", "mcpServers", "lspServers", "commands", "agents", "outputStyles", "monitors"} {
		if _, ok := man[gone]; ok {
			t.Errorf("the snapshot manifest keeps %s", gone)
		}
	}
	for _, kept := range []string{"name", "version", "description", "author", "skills"} {
		if _, ok := man[kept]; !ok {
			t.Errorf("the snapshot manifest lost %s", kept)
		}
	}
	joined := strings.Join(warns, "\n")
	for _, want := range []string{"commands", "agents", "outputStyles", "monitors"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings do not name the dropped manifest key %s: %q", want, warns)
		}
	}
}

// Fix loop 2, High: the job gets a copy, not links into the live plugin. A symlink in the plugin
// (to a directory or a file outside) is not followed, so writing its target changes nothing the job
// loads, and neither does changing the plugin after the snapshot was taken.
func TestPluginViewIsDetachedSnapshot(t *testing.T) {
	m := newManager(t)
	dir := makePlugin(t, "px", "1.0.0", nil)
	outside := t.TempDir()
	writeTree(t, outside, map[string]string{
		"linked/SKILL.md": "---\nname: linked\n---\nLINKED_MARKER\n",
		"file.md":         "---\nname: f\n---\nLINKED_MARKER\n",
	})
	if err := os.Symlink(filepath.Join(outside, "linked"), filepath.Join(dir, "skills", "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "commands"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "file.md"), filepath.Join(dir, "commands", "file.md")); err != nil {
		t.Fatal(err)
	}
	// A hardlinked file is copied like any other.
	if err := os.Link(filepath.Join(dir, "skills", "using-x", "SKILL.md"), filepath.Join(dir, "skills", "using-x", "copy.md")); err != nil {
		t.Fatal(err)
	}
	for _, allowHooks := range []bool{false, true} {
		m.Cfg.JobPlugins = []JobPlugin{{Path: dir, AllowHooks: allowHooks}}
		j := runToEnd(t, m, enqueue(t, m, "x", nil).ID)
		dirs := pluginDirArgs(argvOf(t, m, j.ID))
		if len(dirs) != 1 || dirs[0] == dir || !under(dirs[0], m.JobDir(j.ID)) {
			t.Fatalf("allow_hooks=%v: --plugin-dir %v, want a snapshot in the job dir", allowHooks, dirs)
		}
		view := dirs[0]
		assertNoLinks(t, view)
		files := reachable(t, view)
		for rel, body := range files {
			if strings.Contains(body, "LINKED_MARKER") {
				t.Errorf("allow_hooks=%v: the snapshot exposes %s through a symlink", allowHooks, rel)
			}
		}
		if !strings.Contains(files[filepath.FromSlash("skills/using-x/copy.md")], "USING_X_BOOTSTRAP_MARKER") {
			t.Errorf("allow_hooks=%v: the hardlinked file was not copied", allowHooks)
		}
		_, hooksKept := files[filepath.FromSlash("hooks/hooks.json")]
		if hooksKept != allowHooks {
			t.Errorf("allow_hooks=%v: the snapshot has hooks/hooks.json = %v", allowHooks, hooksKept)
		}
		if _, w := m.JobPlugins(j); !strings.Contains(strings.Join(w, "\n"), filepath.FromSlash("skills/linked")) {
			t.Errorf("allow_hooks=%v: warnings do not name the skipped link: %q", allowHooks, w)
		}

		// Post-snapshot mutation: the external target and the plugin itself are rewritten with hooks.
		evil := "---\nname: using-x\nhooks:\n  Stop: [{hooks: [{type: command, command: MUTATED_HOOK_MARKER}]}]\n---\n"
		writeTree(t, outside, map[string]string{"linked/SKILL.md": evil, "file.md": evil})
		writeTree(t, dir, map[string]string{"skills/using-x/SKILL.md": evil})
		for rel, body := range reachable(t, view) {
			if strings.Contains(body, "MUTATED_HOOK_MARKER") {
				t.Errorf("allow_hooks=%v: a change to the plugin after the snapshot reaches %s", allowHooks, rel)
			}
		}
		writeTree(t, dir, map[string]string{"skills/using-x/SKILL.md": "---\nname: using-x\ndescription: bootstrap\n---\n\nUSING_X_BOOTSTRAP_MARKER: check skills first.\n"})
		writeTree(t, outside, map[string]string{"linked/SKILL.md": "---\nname: linked\n---\nLINKED_MARKER\n", "file.md": "---\nname: f\n---\nLINKED_MARKER\n"})
	}
}

// copyTree copies the regular files and directories of src to dst.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		out := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(out, 0o755)
		case !d.Type().IsRegular():
			return nil
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		fi, err := in.Stat()
		if err != nil {
			return err
		}
		o, err := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, fi.Mode().Perm())
		if err != nil {
			return err
		}
		if _, err := io.Copy(o, in); err != nil {
			o.Close()
			return err
		}
		return o.Close()
	})
	if err != nil {
		t.Fatal(err)
	}
}

// assertSuperpowersSkills builds the snapshot of the superpowers tree at src (copied first) and
// checks that every skill survives the filter.
func assertSuperpowersSkills(t *testing.T, src string) {
	t.Helper()
	m := newManager(t)
	dir := filepath.Join(t.TempDir(), "superpowers")
	copyTree(t, src, dir)
	var want []string
	entries, err := os.ReadDir(filepath.Join(dir, "skills"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if _, err := os.Stat(filepath.Join(dir, "skills", e.Name(), "SKILL.md")); err == nil {
			want = append(want, e.Name())
		}
	}
	if len(want) != 15 {
		t.Fatalf("superpowers 7.0.2 has %d skills here, want 15", len(want))
	}
	view := filepath.Join(t.TempDir(), "view")
	ps := snapshotSource(t, m, JobPlugin{Path: dir})
	if ps.name != "superpowers" {
		t.Fatalf("plugin name %q", ps.name)
	}
	warns, err := buildPluginView(ps, view, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range warns {
		if strings.Contains(w, ".md") {
			t.Errorf("a superpowers markdown file was dropped: %s", w)
		}
	}
	for _, s := range want {
		if _, err := os.Stat(filepath.Join(view, "skills", s, "SKILL.md")); err != nil {
			t.Errorf("skill %s is missing from the snapshot: %v", s, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(view, "hooks")); !os.IsNotExist(err) {
		t.Errorf("the snapshot keeps the SessionStart hook (%v)", err)
	}
	assertNoLinks(t, view)
}

// Fix loop 2: the allowlist must not cost superpowers 7.0.2 (the plugin the owner runs) a single
// skill. testdata holds its front matter verbatim; the live install is checked too when present.
func TestPluginViewKeepsSuperpowersSkills(t *testing.T) {
	assertSuperpowersSkills(t, filepath.Join("testdata", "superpowers-7.0.2"))
}

func TestPluginViewKeepsLiveSuperpowersSkills(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	live := filepath.Join(home, ".claude", "plugins", "cache", "superpowers-marketplace", "superpowers", "7.0.2")
	if _, err := os.Stat(filepath.Join(live, ".claude-plugin", "plugin.json")); err != nil {
		t.Skip("superpowers 7.0.2 is not installed here")
	}
	assertSuperpowersSkills(t, live)
}
