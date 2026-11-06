package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/qwwiwi/agent-jobs/internal/skills"
	"github.com/qwwiwi/agent-jobs/internal/store"
)

// JobPlugin is one Claude Code plugin (a skill pack) loaded into claude jobs with --plugin-dir
// (config job_plugins). Exactly one of Path and Name names it.
type JobPlugin struct {
	// Path is the absolute plugin directory.
	Path string `json:"path,omitempty"`
	// Name is "<plugin>@<marketplace>", resolved at every launch through the agent's
	// installed_plugins.json to the plugin's current installPath, so a plugin update needs no config edit.
	Name string `json:"name,omitempty"`
	// Kinds are the job kinds that get the plugin (fix, general, research, review); none = all of them.
	Kinds []string `json:"kinds,omitempty"`
	// DenySkills are plugin skills the job must not use (interactive brainstorming, branch finishing
	// that pushes, extra worktrees ...): Skill(<plugin>:<name>) goes to the job's permission deny list.
	DenySkills []string `json:"deny_skills,omitempty"`
	// BootstrapSkill is a plugin skill whose SKILL.md is put into the job prompt (section "Skills
	// bootstrap"): what the plugin's SessionStart hook does in an interactive session.
	BootstrapSkill string `json:"bootstrap_skill,omitempty"`
	// AllowHooks lets the plugin's hooks, MCP and LSP servers run in the job. Off by default: they
	// run outside the guard.
	AllowHooks bool `json:"allow_hooks,omitempty"`
}

// id is how the plugin is named in messages.
func (p JobPlugin) id() string {
	if p.Name != "" {
		return p.Name
	}
	return p.Path
}

// jobPluginKinds are the kinds a plugin may be limited to: the claude kinds a user starts (a gate job
// gets nothing but its input).
var jobPluginKinds = []string{"fix", "general", "research", "review"}

var (
	pluginNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}@[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	pluginIDRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

// validateJobPlugins checks config job_plugins.
func validateJobPlugins(ps []JobPlugin) error {
	seen := map[string]bool{}
	for i, p := range ps {
		at := fmt.Sprintf("job_plugins[%d]", i)
		switch {
		case p.Path == "" && p.Name == "":
			return fmt.Errorf("%s: needs a path or name", at)
		case p.Path != "" && p.Name != "":
			return fmt.Errorf("%s: sets both path and name; only one", at)
		case p.Path != "" && !filepath.IsAbs(p.Path):
			return fmt.Errorf("%s: path %q must be absolute", at, p.Path)
		case p.Name != "" && !pluginNameRe.MatchString(p.Name):
			return fmt.Errorf("%s: name %q must look like plugin@marketplace", at, p.Name)
		}
		key := p.Name
		if p.Path != "" {
			key = filepath.Clean(p.Path)
		}
		if seen[key] {
			return fmt.Errorf("%s: %s is listed twice", at, key)
		}
		seen[key] = true
		for _, k := range p.Kinds {
			if !slices.Contains(jobPluginKinds, k) {
				return fmt.Errorf("%s: kinds entry %q must be one of %s", at, k, strings.Join(jobPluginKinds, ", "))
			}
		}
		for _, s := range p.DenySkills {
			if !skills.ValidName(s) {
				return fmt.Errorf("%s: deny_skills entry %q is not a plain skill name", at, s)
			}
		}
		if p.BootstrapSkill != "" && !skills.ValidName(p.BootstrapSkill) {
			return fmt.Errorf("%s: bootstrap_skill %q is not a plain skill name", at, p.BootstrapSkill)
		}
	}
	return nil
}

// appliesTo reports whether the plugin goes to a job of kind (claude jobs only; the caller has
// excluded codex).
func (p JobPlugin) appliesTo(kind string) bool {
	if IsGateKind(kind) {
		return false
	}
	return len(p.Kinds) == 0 || slices.Contains(p.Kinds, kind)
}

const (
	// jobPluginsFile is the plugins the daemon resolved for a job, in the job dir; the job cannot write
	// it (guard --protect), so launch, resume, settings and status all read the same answer.
	jobPluginsFile = "plugins.json"
	// jobPluginsDir holds the job's plugin views (plugins without their hooks and servers).
	jobPluginsDir = "plugins"
	// MaxBootstrapBytes bounds the bootstrap skill's SKILL.md; a larger one is refused (a warning).
	MaxBootstrapBytes = 32 << 10
	// maxPluginJSONBytes bounds installed_plugins.json and a plugin.json.
	maxPluginJSONBytes = 1 << 20
)

// pluginExecKeys are the plugin manifest keys that start code outside the guard: hooks, MCP servers,
// LSP servers. A view of a plugin without allow_hooks drops them, and the files they default to.
var pluginExecKeys = []string{"hooks", "mcpServers", "lspServers"}
var pluginExecEntries = []string{"hooks", ".mcp.json", ".lsp.json"}

// loadedPlugin is one plugin as resolved for a job.
type loadedPlugin struct {
	Name           string   `json:"name"`   // the manifest name: Skill(<name>:<skill>)
	Config         string   `json:"config"` // the config entry (path or name@marketplace)
	Version        string   `json:"version,omitempty"`
	Source         string   `json:"source"`          // the plugin directory as configured or installed
	Real           string   `json:"real"`            // Source with symlinks resolved
	Dir            string   `json:"dir"`             // what --plugin-dir gets: Source, or the view
	Hooks          bool     `json:"hooks,omitempty"` // allow_hooks
	DenySkills     []string `json:"deny_skills,omitempty"`
	BootstrapSkill string   `json:"bootstrap_skill,omitempty"`
	BootstrapText  string   `json:"bootstrap_text,omitempty"`
}

// jobPluginsDoc is the content of plugins.json.
type jobPluginsDoc struct {
	Plugins  []loadedPlugin `json:"plugins,omitempty"`
	Warnings []string       `json:"warnings,omitempty"`
}

// PluginView is a loaded plugin as job_status, job_result and `agent-jobs status` list it.
type PluginView struct {
	Name           string   `json:"name"`
	Version        string   `json:"version,omitempty"`
	Source         string   `json:"source"`
	Hooks          bool     `json:"hooks"`
	DenySkills     []string `json:"deny_skills,omitempty"`
	BootstrapSkill string   `json:"bootstrap_skill,omitempty"`
}

// readPluginsDoc reads the job's plugins.json (none, unreadable or malformed = no plugins). Every
// entry is checked again: the daemon wrote the file, but its paths and names reach the claude command
// line and the settings.
func readPluginsDoc(jobDir string) jobPluginsDoc {
	p := filepath.Join(jobDir, jobPluginsFile)
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxPluginJSONBytes {
		return jobPluginsDoc{}
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return jobPluginsDoc{}
	}
	var doc jobPluginsDoc
	if json.Unmarshal(b, &doc) != nil {
		return jobPluginsDoc{}
	}
	kept := doc.Plugins[:0]
	for _, lp := range doc.Plugins {
		ok := pluginIDRe.MatchString(lp.Name) && filepath.IsAbs(lp.Dir) && filepath.IsAbs(lp.Source) &&
			filepath.IsAbs(lp.Real) && !strings.ContainsAny(lp.Dir, "\x00\n\r")
		for _, s := range append([]string{lp.BootstrapSkill}, lp.DenySkills...) {
			ok = ok && (s == "" || skills.ValidName(s))
		}
		if ok {
			kept = append(kept, lp)
		}
	}
	doc.Plugins = kept
	return doc
}

// claudePluginArgs are the --plugin-dir flags of the job in jobDir.
func claudePluginArgs(jobDir string) []string {
	var out []string
	for _, lp := range readPluginsDoc(jobDir).Plugins {
		out = append(out, "--plugin-dir", lp.Dir)
	}
	return out
}

// JobPlugins are the plugins loaded into job j and the warnings of the launch (a plugin that was
// not found, a bootstrap that could not be read).
func (m *Manager) JobPlugins(j *store.Job) ([]PluginView, []string) {
	doc := readPluginsDoc(m.JobDir(j.ID))
	var out []PluginView
	for _, lp := range doc.Plugins {
		out = append(out, PluginView{Name: lp.Name, Version: lp.Version, Source: lp.Source, Hooks: lp.Hooks,
			DenySkills: lp.DenySkills, BootstrapSkill: lp.BootstrapSkill})
	}
	return out, doc.Warnings
}

// readUnder reads root/rel, a regular file of at most max bytes. Only root (the trusted plugin or
// config directory) has its symlinks resolved; every component of rel below it is opened without
// following a symlink (openAbsoluteSource), so a linked directory inside cannot lead outside.
func readUnder(root, rel string, max int64) ([]byte, error) {
	r, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	f, err := openAbsoluteSource(filepath.Join(r, rel))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	if fi.Size() > max {
		return nil, fmt.Errorf("exceeds %d bytes", max)
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("exceeds %d bytes", max)
	}
	return b, nil
}

// installedPlugin is an entry of installed_plugins.json (format 2 lists them per plugin; format 1 has
// one object).
type installedPlugin struct {
	Scope       string `json:"scope"`
	InstallPath string `json:"installPath"`
	Version     string `json:"version"`
}

// installedPluginPath resolves "<plugin>@<marketplace>" through the agent's installed_plugins.json.
func installedPluginPath(name string) (path, version string, err error) {
	cfg, err := ClaudeConfigDir()
	if err != nil {
		return "", "", err
	}
	b, err := readUnder(filepath.Join(cfg, "plugins"), "installed_plugins.json", maxPluginJSONBytes)
	if err != nil {
		return "", "", fmt.Errorf("installed_plugins.json: %w", err)
	}
	var doc struct {
		Plugins map[string]json.RawMessage `json:"plugins"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return "", "", fmt.Errorf("installed_plugins.json: %w", err)
	}
	raw, ok := doc.Plugins[name]
	if !ok {
		return "", "", fmt.Errorf("%s is not in installed_plugins.json", name)
	}
	var list []installedPlugin
	if json.Unmarshal(raw, &list) != nil {
		var one installedPlugin
		if err := json.Unmarshal(raw, &one); err != nil {
			return "", "", fmt.Errorf("installed_plugins.json: entry %s: %w", name, err)
		}
		list = []installedPlugin{one}
	}
	if len(list) == 0 {
		return "", "", fmt.Errorf("%s has no install in installed_plugins.json", name)
	}
	pick := list[0]
	for _, e := range list {
		if e.Scope == "user" {
			pick = e
			break
		}
	}
	if !filepath.IsAbs(pick.InstallPath) {
		return "", "", fmt.Errorf("%s: installPath %q is not absolute", name, pick.InstallPath)
	}
	return pick.InstallPath, pick.Version, nil
}

// pluginSource is a configured plugin found on disk.
type pluginSource struct {
	name, version, source, real string
	manifest                    map[string]json.RawMessage // nil = the plugin has no manifest
}

// findPlugin resolves a configured plugin to its directory and manifest.
func (m *Manager) findPlugin(p JobPlugin) (*pluginSource, error) {
	src, version := p.Path, ""
	if p.Name != "" {
		var err error
		if src, version, err = installedPluginPath(p.Name); err != nil {
			return nil, err
		}
	}
	src = filepath.Clean(src)
	if src == "/" || samePath(src, m.StateDir) || under(m.StateDir, src) || under(src, m.StateDir) {
		return nil, fmt.Errorf("%s is /, holds or lies inside the agent-jobs state", src)
	}
	real, err := filepath.EvalSymlinks(src)
	if err != nil {
		return nil, fmt.Errorf("plugin directory %s: %w", src, err)
	}
	if fi, err := os.Stat(real); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("plugin directory %s is not a directory", src)
	}
	ps := &pluginSource{source: src, real: real, version: version}
	b, err := readUnder(real, filepath.Join(".claude-plugin", "plugin.json"), maxPluginJSONBytes)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("plugin manifest of %s: %w", src, err)
	default:
		if err := json.Unmarshal(b, &ps.manifest); err != nil || ps.manifest == nil {
			return nil, fmt.Errorf("plugin manifest of %s is not a JSON object", src)
		}
		var s string
		if json.Unmarshal(ps.manifest["name"], &s) == nil && pluginIDRe.MatchString(s) {
			ps.name = s
		}
		if ps.version == "" && json.Unmarshal(ps.manifest["version"], &s) == nil && utf8.ValidString(s) && len(s) <= 64 {
			ps.version = strings.Map(func(r rune) rune {
				if r < ' ' {
					return -1
				}
				return r
			}, s)
		}
	}
	if ps.name == "" {
		ps.name = strings.SplitN(p.Name, "@", 2)[0]
		if p.Name == "" {
			ps.name = filepath.Base(real)
		}
		if !pluginIDRe.MatchString(ps.name) {
			return nil, fmt.Errorf("plugin %s has no usable name", src)
		}
	}
	return ps, nil
}

// readBootstrap reads the plugin's bootstrap skill: SKILL.md, size-bounded, opened without following
// a symlink anywhere below the plugin root (skills/, the skill directory, SKILL.md), secret-looking
// values redacted.
func readBootstrap(real, skill string) (string, error) {
	b, err := readUnder(real, filepath.Join("skills", skill, "SKILL.md"), MaxBootstrapBytes)
	if err != nil {
		return "", fmt.Errorf("bootstrap skill %s: %w", skill, err)
	}
	if !utf8.Valid(b) || strings.ContainsRune(string(b), 0) {
		return "", fmt.Errorf("bootstrap skill %s: not text", skill)
	}
	return RedactText(string(b)), nil
}

// buildPluginView makes viewDir: the plugin's top-level entries as symlinks, without the hooks, MCP
// and LSP parts, and with a manifest that no longer names them. A component (skill, command, agent:
// any .md file, wherever the manifest points) whose own front matter starts hooks or servers is left
// out too: the entry holding it is mirrored as real directories down to it, everything else stays a
// symlink. The plugin stays untouched. It returns one warning per component left out.
func buildPluginView(ps *pluginSource, viewDir string) ([]string, error) {
	if err := os.RemoveAll(viewDir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(viewDir, 0o700); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(ps.real)
	if err != nil {
		return nil, err
	}
	vb := &viewBuilder{dropped: map[string]string{}}
	for _, e := range entries {
		n := e.Name()
		switch {
		case slices.Contains(pluginExecEntries, n):
			continue
		case n == ".claude-plugin":
			if err := buildManifestDir(ps, filepath.Join(viewDir, n)); err != nil {
				return nil, err
			}
		default:
			src := filepath.Join(ps.real, n)
			if err := vb.scan(src, n, 0); err != nil {
				return nil, err
			}
			if err := vb.mirror(src, filepath.Join(viewDir, n), n); err != nil {
				return nil, err
			}
		}
	}
	var warns []string
	for _, rel := range slices.Sorted(maps.Keys(vb.dropped)) {
		warns = append(warns, fmt.Sprintf("%s left out of the view: %s (allow_hooks is off)", rel, vb.dropped[rel]))
	}
	return warns, nil
}

const (
	// maxViewDepth bounds the directory nesting a view scan follows (a symlink loop ends there);
	// maxViewEntries the entries it visits; maxFrontMatterBytes what it reads of each .md file.
	maxViewDepth        = 32
	maxViewEntries      = 100000
	maxFrontMatterBytes = 64 << 10
)

// viewBuilder scans a plugin entry for components with executable front matter and mirrors it.
type viewBuilder struct {
	dropped map[string]string // relative path -> why it is left out
	seen    int
}

// scan visits src (following symlinks, as claude does when it loads the plugin) and records in
// dropped every .md file whose front matter starts code outside the guard.
func (vb *viewBuilder) scan(src, rel string, depth int) error {
	if vb.seen++; vb.seen > maxViewEntries {
		return fmt.Errorf("more than %d entries", maxViewEntries)
	}
	fi, err := os.Stat(src)
	if err != nil {
		return nil // a dangling link: claude finds nothing there either
	}
	if fi.IsDir() {
		if depth >= maxViewDepth {
			return fmt.Errorf("%s: nested deeper than %d directories", rel, maxViewDepth)
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		for _, e := range entries {
			if err := vb.scan(filepath.Join(src, e.Name()), filepath.Join(rel, e.Name()), depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if !fi.Mode().IsRegular() || !strings.EqualFold(filepath.Ext(src), ".md") {
		return nil
	}
	if why := execFrontMatter(src); why != "" {
		vb.dropped[rel] = why
	}
	return nil
}

// mirror puts src at dst: a symlink when nothing below rel was dropped, nothing for a dropped file,
// else a real directory whose entries are mirrored in turn.
func (vb *viewBuilder) mirror(src, dst, rel string) error {
	if _, gone := vb.dropped[rel]; gone {
		return nil
	}
	prefix := rel + string(filepath.Separator)
	inside := false
	for d := range vb.dropped {
		if strings.HasPrefix(d, prefix) {
			inside = true
			break
		}
	}
	if !inside {
		return os.Symlink(src, dst)
	}
	if err := os.Mkdir(dst, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := vb.mirror(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()), filepath.Join(rel, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// frontMatterKeyRe is a top-level key of block-style YAML front matter: a plain key at column 0.
var frontMatterKeyRe = regexp.MustCompile(`^([^\s#'"{}\[\],&*!|>%@` + "`" + `?:-][^#]*?|-[^\s#]*?)\s*:(\s|$)`)

// execFrontMatter is why the markdown file at path must stay out of a view without hooks: its front
// matter names hooks, mcpServers or lspServers at the top level, or is not the plain block YAML this
// check can read (a quoted or flow key, an alias, a merge, a tab indent, no closing line, unreadable).
// "" = no front matter, or one without those keys.
func execFrontMatter(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return "unreadable: " + err.Error()
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxFrontMatterBytes))
	if err != nil {
		return "unreadable: " + err.Error()
	}
	text := strings.TrimLeft(strings.TrimPrefix(string(b), "\ufeff"), " \t\r\n")
	if !strings.HasPrefix(text, "---") {
		return ""
	}
	lines := strings.Split(text, "\n")
	if strings.TrimRight(lines[0], " \t\r") != "---" {
		return "" // a rule like "----" or "--- x" opens no front matter
	}
	for _, raw := range lines[1:] {
		line := strings.TrimRight(raw, " \t\r")
		switch {
		case line == "---" || line == "...":
			return ""
		case line == "" || strings.HasPrefix(line, "#"):
			continue
		case strings.HasPrefix(line, "\t"):
			return "front matter indented with a tab"
		case strings.HasPrefix(line, " "):
			continue // inside the value of the previous top-level key
		}
		m := frontMatterKeyRe.FindStringSubmatch(line)
		if m == nil {
			return fmt.Sprintf("front matter line %q is not a plain top-level key", truncate(line, 40))
		}
		key := strings.TrimSpace(m[1])
		if key == "<<" {
			return "front matter merges keys (<<)"
		}
		for _, k := range pluginExecKeys {
			if strings.EqualFold(key, k) {
				return "front matter sets " + k
			}
		}
	}
	return "front matter has no closing line"
}

// truncate shortens s to at most n bytes on a rune boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "..."
}

func buildManifestDir(ps *pluginSource, dst string) error {
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	src := filepath.Join(ps.real, ".claude-plugin")
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() != "plugin.json" {
			if err := os.Symlink(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
			continue
		}
		clean := map[string]json.RawMessage{}
		for k, v := range ps.manifest {
			if !slices.Contains(pluginExecKeys, k) {
				clean[k] = v
			}
		}
		b, err := json.MarshalIndent(clean, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dst, "plugin.json"), append(b, '\n'), 0o600); err != nil {
			return err
		}
	}
	return nil
}

// preparePlugins resolves the configured plugins for job j and writes plugins.json and the plugin
// views into the job dir (replacing those of an earlier attempt). A plugin that cannot be loaded is a
// warning kept in plugins.json, never a failed job. A codex job and a gate job get nothing.
func (m *Manager) preparePlugins(j *store.Job, dir string) error {
	for _, rm := range []string{jobPluginsFile, jobPluginsDir} {
		if err := os.RemoveAll(filepath.Join(dir, rm)); err != nil {
			return fmt.Errorf("plugins: remove the earlier %s: %w", rm, err)
		}
	}
	if j.Runner == store.RunnerCodex || IsGateKind(j.Kind) {
		return nil
	}
	var doc jobPluginsDoc
	names := map[string]bool{}
	for i, p := range m.Config().JobPlugins {
		if !p.appliesTo(j.Kind) {
			continue
		}
		lp, viewWarns, err := m.loadPlugin(p, filepath.Join(dir, jobPluginsDir, fmt.Sprintf("%d", i)))
		for _, w := range viewWarns {
			doc.Warnings = append(doc.Warnings, "plugin "+p.id()+": "+w)
		}
		if err != nil {
			doc.Warnings = append(doc.Warnings, "plugin "+p.id()+": "+err.Error())
			m.Log.Printf("job %s: plugin %s: %v", j.ID, p.id(), err)
			continue
		}
		if names[lp.Name] {
			doc.Warnings = append(doc.Warnings, "plugin "+p.id()+": another plugin is already named "+lp.Name+", skipped")
			continue
		}
		names[lp.Name] = true
		if p.BootstrapSkill != "" {
			text, err := readBootstrap(lp.Real, p.BootstrapSkill)
			if err != nil {
				doc.Warnings = append(doc.Warnings, "plugin "+p.id()+": "+err.Error())
				m.Log.Printf("job %s: plugin %s: %v", j.ID, p.id(), err)
			} else {
				lp.BootstrapSkill, lp.BootstrapText = p.BootstrapSkill, text
			}
		}
		doc.Plugins = append(doc.Plugins, *lp)
	}
	if len(doc.Plugins) == 0 && len(doc.Warnings) == 0 {
		return nil
	}
	if err := writeJSON(filepath.Join(dir, jobPluginsFile), doc); err != nil {
		return fmt.Errorf("plugins: %w", err)
	}
	return nil
}

// loadPlugin resolves p and, without allow_hooks, builds its view at viewDir.
func (m *Manager) loadPlugin(p JobPlugin, viewDir string) (*loadedPlugin, []string, error) {
	ps, err := m.findPlugin(p)
	if err != nil {
		return nil, nil, err
	}
	lp := &loadedPlugin{Name: ps.name, Config: p.id(), Version: ps.version, Source: ps.source, Real: ps.real,
		Dir: ps.source, Hooks: p.AllowHooks, DenySkills: slices.Clone(p.DenySkills)}
	var warns []string
	if !p.AllowHooks {
		if warns, err = buildPluginView(ps, viewDir); err != nil {
			return nil, nil, fmt.Errorf("view without hooks: %w", err)
		}
		lp.Dir = viewDir
	}
	return lp, warns, nil
}

// PluginWarnings are the configured plugins that cannot be loaded now (missing directory, name not
// in installed_plugins.json, unreadable bootstrap), for status and system_doctor.
func (m *Manager) PluginWarnings() []string {
	var out []string
	for _, p := range m.Config().JobPlugins {
		ps, err := m.findPlugin(p)
		if err == nil && p.BootstrapSkill != "" {
			_, err = readBootstrap(ps.real, p.BootstrapSkill)
		}
		if err != nil {
			out = append(out, "job plugin "+p.id()+": "+err.Error())
		}
	}
	return out
}

// pluginProtected are the paths a job never writes because of plugins: the job's views, every
// configured plugin directory (as configured and resolved), whether this job loads it or not, since a
// later job of another kind does, and, when a plugin is named, the agent's plugin registry directory
// (<config>/plugins: installed_plugins.json decides a named plugin's next installPath) with each
// named plugin's current install. The plugins this job loaded are added as resolved for it.
func (m *Manager) pluginProtected(j *store.Job) []string {
	doc := readPluginsDoc(m.JobDir(j.ID))
	configured := m.Config().JobPlugins
	if len(doc.Plugins) == 0 && len(configured) == 0 {
		return nil
	}
	out := []string{filepath.Join(m.JobDir(j.ID), jobPluginsDir)}
	add := func(p string) {
		if p == "" || !filepath.IsAbs(p) {
			return
		}
		p = filepath.Clean(p)
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
		if real, err := filepath.EvalSymlinks(p); err == nil && !slices.Contains(out, real) {
			out = append(out, real)
		}
	}
	for _, lp := range doc.Plugins {
		add(lp.Source)
		add(lp.Real)
	}
	registry := false
	for _, p := range configured {
		if p.Path != "" {
			add(p.Path)
			continue
		}
		if !registry {
			if cfg, err := ClaudeConfigDir(); err == nil {
				add(filepath.Join(cfg, "plugins"))
			}
			registry = true
		}
		if path, _, err := installedPluginPath(p.Name); err == nil {
			add(path)
		}
	}
	return out
}

// pluginDenyRules are the Skill(<plugin>:<skill>) deny rules of the job's plugins.
func (m *Manager) pluginDenyRules(j *store.Job) []string {
	var out []string
	for _, lp := range readPluginsDoc(m.JobDir(j.ID)).Plugins {
		for _, s := range lp.DenySkills {
			out = append(out, "Skill("+lp.Name+":"+s+")")
		}
	}
	return out
}

// pluginsPromptSection is the "Skills bootstrap" section of the prompt: the bootstrap skills of the
// job's plugins and one fixed line naming the skills the job must not try. "" when no plugin has a
// bootstrap.
func (m *Manager) pluginsPromptSection(j *store.Job) string {
	var b strings.Builder
	var denied []string
	for _, lp := range readPluginsDoc(m.JobDir(j.ID)).Plugins {
		for _, s := range lp.DenySkills {
			denied = append(denied, lp.Name+":"+s)
		}
		if lp.BootstrapText == "" {
			continue
		}
		if b.Len() == 0 {
			b.WriteString("## Skills bootstrap\n\n")
		}
		fmt.Fprintf(&b, "Plugin %s, skill %s (loaded; the Skill tool has the plugin's skills):\n\n%s\n\n",
			lp.Name, lp.BootstrapSkill, strings.TrimSpace(lp.BootstrapText))
	}
	if b.Len() == 0 {
		return ""
	}
	if len(denied) > 0 {
		fmt.Fprintf(&b, "Skills denied in this job (they contradict the job contract; do not invoke them): %s.\n\n",
			strings.Join(denied, ", "))
	}
	return b.String()
}
