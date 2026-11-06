package jobs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// makePlugin writes a plugin directory the way a marketplace installs one: a manifest naming it, two
// skills, and the parts a job must not run by default (hooks, an MCP server file). It returns the dir.
func makePlugin(t *testing.T, name, version string, skillBodies map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	manifest := map[string]any{
		"name": name, "version": version, "description": "test plugin",
		"hooks":      "./hooks/hooks.json",
		"mcpServers": map[string]any{"x": map[string]any{"command": "true"}},
	}
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mb, _ := json.Marshal(manifest)
	write(".claude-plugin/plugin.json", string(mb))
	write("hooks/hooks.json", `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"true"}]}]}}`)
	write(".mcp.json", `{"mcpServers":{"x":{"command":"true"}}}`)
	if skillBodies == nil {
		skillBodies = map[string]string{"using-x": "---\nname: using-x\ndescription: bootstrap\n---\n\nUSING_X_BOOTSTRAP_MARKER: check skills first.\n"}
	}
	for n, body := range skillBodies {
		write("skills/"+n+"/SKILL.md", body)
	}
	return dir
}

func argvOf(t *testing.T, m *Manager, id string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(m.JobDir(id), "argv.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

// pluginDirArgs are the values of every --plugin-dir in argv.
func pluginDirArgs(argv []string) []string {
	var out []string
	for i, a := range argv {
		if a == "--plugin-dir" && i+1 < len(argv) {
			out = append(out, argv[i+1])
		}
	}
	return out
}

func promptOf(t *testing.T, m *Manager, id string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(m.JobDir(id), "prompt.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPluginDirPassedForMatchingKindsOnly(t *testing.T) {
	m := newManager(t)
	dir := makePlugin(t, "px", "1.2.3", nil)
	m.Cfg.JobPlugins = []JobPlugin{{Path: dir, Kinds: []string{"fix"}}}
	repo := newRepo(t)

	fix := runToEnd(t, m, enqueue(t, m, "x", func(r *StartRequest) { r.Kind, r.RepoPath = "fix", repo }).ID)
	if fix.Status != store.StatusSucceeded {
		t.Fatalf("fix status %s %q", fix.Status, fix.Error)
	}
	dirs := pluginDirArgs(argvOf(t, m, fix.ID))
	if len(dirs) != 1 {
		t.Fatalf("fix job --plugin-dir args %v", dirs)
	}
	// The skills are reachable through the directory the flag names.
	if _, err := os.Stat(filepath.Join(dirs[0], "skills", "using-x", "SKILL.md")); err != nil {
		t.Fatalf("plugin skills not reachable through %s: %v", dirs[0], err)
	}
	general := runToEnd(t, m, enqueue(t, m, "x", nil).ID)
	if general.Status != store.StatusSucceeded {
		t.Fatalf("general status %s %q", general.Status, general.Error)
	}
	if got := pluginDirArgs(argvOf(t, m, general.ID)); len(got) != 0 {
		t.Fatalf("general job got --plugin-dir %v", got)
	}
	if _, err := os.Stat(filepath.Join(m.JobDir(general.ID), jobPluginsFile)); !os.IsNotExist(err) {
		t.Fatal("a job with no matching plugin has a plugins file")
	}
	// An empty kinds list means every claude kind.
	m.Cfg.JobPlugins = []JobPlugin{{Path: dir}}
	research := runToEnd(t, m, enqueue(t, m, "x", func(r *StartRequest) { r.Kind = "research" }).ID)
	if got := pluginDirArgs(argvOf(t, m, research.ID)); len(got) != 1 {
		t.Fatalf("research job with no kinds filter: %v", got)
	}
}

func TestPluginNameResolvedThroughInstalledPlugins(t *testing.T) {
	m := newManager(t)
	cfgDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfgDir)
	v1 := makePlugin(t, "px", "1.0.0", nil)
	v2 := makePlugin(t, "px", "2.0.0", nil)
	writeInstalled := func(path, version string) {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"version": 2, "plugins": map[string]any{
			"other@mkt": []any{map[string]any{"scope": "user", "installPath": "/nonexistent", "version": "9"}},
			"px@mkt":    []any{map[string]any{"scope": "user", "installPath": path, "version": version}},
		}})
		if err := os.MkdirAll(filepath.Join(cfgDir, "plugins"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cfgDir, "plugins", "installed_plugins.json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeInstalled(v1, "1.0.0")
	m.Cfg.JobPlugins = []JobPlugin{{Name: "px@mkt", AllowHooks: true}}

	j := runToEnd(t, m, enqueue(t, m, "x", nil).ID)
	real1, _ := filepath.EvalSymlinks(v1)
	if got := pluginDirArgs(argvOf(t, m, j.ID)); len(got) != 1 || (got[0] != v1 && got[0] != real1) {
		t.Fatalf("--plugin-dir %v, want %s", got, v1)
	}
	views, _ := m.JobPlugins(j)
	if len(views) != 1 || views[0].Name != "px" || views[0].Version != "1.0.0" {
		t.Fatalf("plugin view %+v", views)
	}
	// A plugin update moves installPath: the next job follows without a config edit.
	writeInstalled(v2, "2.0.0")
	j = runToEnd(t, m, enqueue(t, m, "x", nil).ID)
	real2, _ := filepath.EvalSymlinks(v2)
	if got := pluginDirArgs(argvOf(t, m, j.ID)); len(got) != 1 || (got[0] != v2 && got[0] != real2) {
		t.Fatalf("after update --plugin-dir %v, want %s", got, v2)
	}
	if views, _ = m.JobPlugins(j); len(views) != 1 || views[0].Version != "2.0.0" {
		t.Fatalf("plugin view after update %+v", views)
	}
}

func TestPluginMissingIsWarningNotFailure(t *testing.T) {
	m := newManager(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // no installed_plugins.json at all
	m.Cfg.JobPlugins = []JobPlugin{
		{Path: filepath.Join(t.TempDir(), "gone")},
		{Name: "absent@mkt"},
	}
	if w := m.PluginWarnings(); len(w) != 2 || !strings.Contains(w[0], "gone") || !strings.Contains(w[1], "absent@mkt") {
		t.Fatalf("doctor warnings %q", w)
	}
	j := runToEnd(t, m, enqueue(t, m, "x", nil).ID)
	if j.Status != store.StatusSucceeded {
		t.Fatalf("a missing plugin failed the job: %s %q", j.Status, j.Error)
	}
	if got := pluginDirArgs(argvOf(t, m, j.ID)); len(got) != 0 {
		t.Fatalf("--plugin-dir for a missing plugin: %v", got)
	}
	_, warns := m.JobPlugins(j)
	if len(warns) != 2 {
		t.Fatalf("job warnings %q", warns)
	}
}

func TestPluginDenyListAndProtection(t *testing.T) {
	m := newManager(t)
	dir := makePlugin(t, "px", "1.0.0", nil)
	m.Cfg.JobPlugins = []JobPlugin{{Path: dir, DenySkills: []string{"brainstorming", "finishing-a-branch"}}}
	j := runToEnd(t, m, enqueue(t, m, "x", nil).ID)
	sb, err := os.ReadFile(filepath.Join(m.JobDir(j.ID), "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Permissions struct {
			Deny []string `json:"deny"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(sb, &settings); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Skill(px:brainstorming)", "Skill(px:finishing-a-branch)"} {
		if !slices.Contains(settings.Permissions.Deny, want) {
			t.Errorf("deny list lacks %s: %v", want, settings.Permissions.Deny)
		}
	}
	real, _ := filepath.EvalSymlinks(dir)
	for _, want := range []string{"--protect '" + dir + "'", "Edit(//" + strings.TrimPrefix(real, "/") + "/**)",
		"--protect '" + filepath.Join(m.JobDir(j.ID), jobPluginsFile) + "'"} {
		if !strings.Contains(string(sb), want) {
			t.Errorf("settings.json lacks %s", want)
		}
	}
}

func TestPluginHooksDisabledByDefault(t *testing.T) {
	m := newManager(t)
	dir := makePlugin(t, "px", "1.0.0", nil)
	m.Cfg.JobPlugins = []JobPlugin{{Path: dir}}
	j := runToEnd(t, m, enqueue(t, m, "x", nil).ID)
	dirs := pluginDirArgs(argvOf(t, m, j.ID))
	if len(dirs) != 1 {
		t.Fatalf("--plugin-dir %v", dirs)
	}
	view := dirs[0]
	if view == dir {
		t.Fatal("hooks off, yet the job got the plugin's own directory")
	}
	for _, gone := range []string{"hooks", ".mcp.json"} {
		if _, err := os.Lstat(filepath.Join(view, gone)); !os.IsNotExist(err) {
			t.Errorf("job's plugin view has %s (%v)", gone, err)
		}
	}
	mb, err := os.ReadFile(filepath.Join(view, ".claude-plugin", "plugin.json"))
	if err != nil {
		t.Fatal(err)
	}
	var man map[string]any
	if err := json.Unmarshal(mb, &man); err != nil {
		t.Fatal(err)
	}
	if _, ok := man["hooks"]; ok {
		t.Error("view manifest still names hooks")
	}
	if _, ok := man["mcpServers"]; ok {
		t.Error("view manifest still names mcpServers")
	}
	if man["name"] != "px" {
		t.Errorf("view manifest lost the name: %v", man)
	}
	// The skills still come through, and the plugin itself is untouched.
	if _, err := os.Stat(filepath.Join(view, "skills", "using-x", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "hooks", "hooks.json")); err != nil {
		t.Fatalf("the plugin's own hooks were touched: %v", err)
	}
	// allow_hooks passes the plugin directory itself.
	m.Cfg.JobPlugins = []JobPlugin{{Path: dir, AllowHooks: true}}
	j = runToEnd(t, m, enqueue(t, m, "x", nil).ID)
	if got := pluginDirArgs(argvOf(t, m, j.ID)); len(got) != 1 || (got[0] != dir && !strings.HasSuffix(got[0], filepath.Base(dir))) {
		t.Fatalf("allow_hooks: --plugin-dir %v, want %s", got, dir)
	}
}

func TestPluginCodexJobUnaffected(t *testing.T) {
	m := newCodexManager(t)
	dir := makePlugin(t, "px", "1.0.0", nil)
	m.Cfg.JobPlugins = []JobPlugin{{Path: dir, BootstrapSkill: "using-x", DenySkills: []string{"brainstorming"}}}
	j := runToEnd(t, m, enqueue(t, m, "review it", codexReview).ID)
	if j.Status != store.StatusSucceeded {
		t.Fatalf("status %s %q", j.Status, j.Error)
	}
	if _, err := os.Stat(filepath.Join(m.JobDir(j.ID), jobPluginsFile)); !os.IsNotExist(err) {
		t.Fatal("a codex job has a plugins file")
	}
	if _, err := os.Lstat(filepath.Join(m.JobDir(j.ID), "plugins")); !os.IsNotExist(err) {
		t.Fatal("a codex job has plugin views")
	}
	p := promptOf(t, m, j.ID)
	if strings.Contains(p, "Skills bootstrap") || strings.Contains(p, "USING_X_BOOTSTRAP_MARKER") {
		t.Fatal("a codex job's prompt carries the bootstrap")
	}
	sb, _ := os.ReadFile(filepath.Join(m.JobDir(j.ID), "settings.json"))
	if strings.Contains(string(sb), "Skill(") {
		t.Fatal("a codex job's settings deny plugin skills")
	}
}

func TestPluginBootstrapInPrompt(t *testing.T) {
	m := newManager(t)
	dir := makePlugin(t, "px", "1.0.0", map[string]string{
		"using-x": "---\nname: using-x\n---\n\nUSING_X_BOOTSTRAP_MARKER token " + "ghp_" + strings.Repeat("a1B2", 9) + "\n",
	})
	m.Cfg.JobPlugins = []JobPlugin{{Path: dir, Kinds: []string{"fix", "general"}, BootstrapSkill: "using-x",
		DenySkills: []string{"brainstorming", "using-git-worktrees"}}}
	j := runToEnd(t, m, enqueue(t, m, "x", nil).ID)
	p := promptOf(t, m, j.ID)
	for _, want := range []string{"## Skills bootstrap", "USING_X_BOOTSTRAP_MARKER",
		"px:brainstorming", "px:using-git-worktrees"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	if strings.Contains(p, "ghp_"+strings.Repeat("a1B2", 9)) {
		t.Error("the bootstrap text was not redacted")
	}
	// A kind the plugin is not configured for gets neither the flag nor the section.
	r := runToEnd(t, m, enqueue(t, m, "x", func(r *StartRequest) { r.Kind = "research" }).ID)
	if strings.Contains(promptOf(t, m, r.ID), "Skills bootstrap") {
		t.Error("research job got a bootstrap")
	}
	views, _ := m.JobPlugins(j)
	if len(views) != 1 || views[0].BootstrapSkill != "using-x" {
		t.Fatalf("view %+v", views)
	}
}

func TestPluginBootstrapMissingFileWarns(t *testing.T) {
	m := newManager(t)
	dir := makePlugin(t, "px", "1.0.0", nil)
	m.Cfg.JobPlugins = []JobPlugin{{Path: dir, BootstrapSkill: "nope"}}
	j := runToEnd(t, m, enqueue(t, m, "x", nil).ID)
	if j.Status != store.StatusSucceeded {
		t.Fatalf("status %s %q", j.Status, j.Error)
	}
	if strings.Contains(promptOf(t, m, j.ID), "Skills bootstrap") {
		t.Error("prompt has a bootstrap section for a missing skill")
	}
	_, warns := m.JobPlugins(j)
	if len(warns) != 1 || !strings.Contains(warns[0], "nope") {
		t.Fatalf("warnings %q", warns)
	}
	// The plugin itself still loads.
	if got := pluginDirArgs(argvOf(t, m, j.ID)); len(got) != 1 {
		t.Fatalf("--plugin-dir %v", got)
	}
}

func TestPluginBootstrapOversizedRefused(t *testing.T) {
	m := newManager(t)
	dir := makePlugin(t, "px", "1.0.0", map[string]string{
		"using-x": "OVERSIZED_MARKER\n" + strings.Repeat("a", MaxBootstrapBytes+1),
	})
	m.Cfg.JobPlugins = []JobPlugin{{Path: dir, BootstrapSkill: "using-x"}}
	j := runToEnd(t, m, enqueue(t, m, "x", nil).ID)
	if j.Status != store.StatusSucceeded {
		t.Fatalf("status %s %q", j.Status, j.Error)
	}
	if strings.Contains(promptOf(t, m, j.ID), "OVERSIZED_MARKER") {
		t.Error("an oversized bootstrap reached the prompt")
	}
	_, warns := m.JobPlugins(j)
	if len(warns) != 1 || !strings.Contains(warns[0], "exceeds") {
		t.Fatalf("warnings %q", warns)
	}
}

func TestPluginBootstrapSymlinkRefused(t *testing.T) {
	m := newManager(t)
	dir := makePlugin(t, "px", "1.0.0", nil)
	secret := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(secret, []byte("SYMLINK_SECRET_MARKER\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	skill := filepath.Join(dir, "skills", "linked")
	if err := os.MkdirAll(skill, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(skill, "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	m.Cfg.JobPlugins = []JobPlugin{{Path: dir, BootstrapSkill: "linked"}}
	j := runToEnd(t, m, enqueue(t, m, "x", nil).ID)
	if strings.Contains(promptOf(t, m, j.ID), "SYMLINK_SECRET_MARKER") {
		t.Fatal("a symlinked SKILL.md was followed into the prompt")
	}
	if _, warns := m.JobPlugins(j); len(warns) != 1 {
		t.Fatalf("warnings %q", warns)
	}
}

func TestPluginClaudeArgsAndSettingsOnResume(t *testing.T) {
	m := newManager(t)
	dir := makePlugin(t, "px", "1.0.0", nil)
	m.Cfg.JobPlugins = []JobPlugin{{Path: dir, DenySkills: []string{"brainstorming"}}}
	j := runToEnd(t, m, enqueue(t, m, "x", nil).ID)
	jd := m.JobDir(j.ID)
	want := pluginDirArgs(argvOf(t, m, j.ID))
	if len(want) != 1 {
		t.Fatalf("--plugin-dir %v", want)
	}
	if got := pluginDirArgs(ResumeClaudeArgs(j, jd)); !slices.Equal(got, want) {
		t.Fatalf("resume --plugin-dir %v, want %v", got, want)
	}
	// Settings written again for a resume keep the deny rule.
	if err := m.refreshJobFiles(j, jd); err != nil {
		t.Fatal(err)
	}
	sb, _ := os.ReadFile(filepath.Join(jd, "settings.json"))
	if !strings.Contains(string(sb), "Skill(px:brainstorming)") {
		t.Fatal("resume settings lost the plugin deny rule")
	}
}

func TestJobPluginsConfigValidation(t *testing.T) {
	dir := t.TempDir()
	for body, want := range map[string]string{
		`{"job_plugins":[{}]}`:                                                          "path or name",
		`{"job_plugins":[{"path":"/a","name":"b@c"}]}`:                                  "only one",
		`{"job_plugins":[{"path":"rel/dir"}]}`:                                          "absolute",
		`{"job_plugins":[{"name":"nomarket"}]}`:                                         "plugin@marketplace",
		`{"job_plugins":[{"path":"/a","kinds":["bogus"]}]}`:                             "kinds",
		`{"job_plugins":[{"path":"/a","kinds":["judge"]}]}`:                             "kinds",
		`{"job_plugins":[{"path":"/a","deny_skills":["../x"]}]}`:                        "deny_skills",
		`{"job_plugins":[{"path":"/a","bootstrap_skill":"a/b"}]}`:                       "bootstrap_skill",
		`{"job_plugins":[{"path":"/a","deny_skills":["x"]},{"path":"/a/","kinds":[]}]}`: "twice",
	} {
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(dir); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v (want %q)", body, err, want)
		}
	}
	ok := `{"job_plugins":[{"path":"/a","kinds":["fix","general","review"],"deny_skills":["brainstorming"],"bootstrap_skill":"using-x"},{"name":"p@m","allow_hooks":true}]}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(ok), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(dir)
	if err != nil || len(c.JobPlugins) != 2 || !c.JobPlugins[1].AllowHooks {
		t.Fatalf("valid config: %v %+v", err, c.JobPlugins)
	}
}
