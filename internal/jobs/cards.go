package jobs

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	agentjobs "github.com/qwwiwi/agent-jobs"
)

// Project cards: short per-project notes (stack, check runner, what not to touch, known traps) that
// are attached to the brief of every job whose repository matches the card. They live in
// <state>/cards/<name>.md (config cards_dir), one file per project, and are created with
// `agent-jobs card new --repo PATH`.
const (
	// MaxCardBytes is the size of one card file: cards are short by design.
	MaxCardBytes = 4 * 1024
	// MaxCardsPerJobBytes caps all cards attached to one brief together.
	MaxCardsPerJobBytes = 8 * 1024
)

// Card is one parsed project card.
type Card struct {
	Name    string   // file name without .md
	Path    string   // file path
	Repos   []string // `repo:` keys: absolute repository paths (git top level)
	Remotes []string // `remote:` keys: normalized remote URLs (host/owner/name)
	Body    string   // everything after the front matter, trimmed
}

var reCardName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// CardsDir returns the agent's cards directory.
func (m *Manager) CardsDir() string { return m.Cfg.CardsDir }

// ParseCard parses a card: a front matter block between `---` lines with `repo:` and/or `remote:`
// keys (each repeatable, at least one), then a non-empty Markdown body.
func ParseCard(name string, b []byte) (Card, error) {
	c := Card{Name: name}
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	if !strings.HasPrefix(s, "---\n") {
		return c, errors.New("card must start with a front matter line ---")
	}
	head, body, ok := strings.Cut(s[len("---\n"):], "\n---\n")
	if !ok {
		if h, found := strings.CutSuffix(s[len("---\n"):], "\n---"); found {
			head, body, ok = h, "", true
		}
	}
	if !ok {
		return c, errors.New("card front matter is not closed with ---")
	}
	for i, line := range strings.Split(head, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok || v == "" {
			return c, fmt.Errorf("front matter line %d: want `key: value`", i+2)
		}
		switch k {
		case "repo":
			if !filepath.IsAbs(v) {
				return c, fmt.Errorf("front matter line %d: repo %q must be an absolute path", i+2, v)
			}
			c.Repos = append(c.Repos, filepath.Clean(v))
		case "remote":
			c.Remotes = append(c.Remotes, NormalizeRemote(v))
		default:
			return c, fmt.Errorf("front matter line %d: unknown key %q (want repo or remote)", i+2, k)
		}
	}
	if len(c.Repos) == 0 && len(c.Remotes) == 0 {
		return c, errors.New("card names no repo and no remote, so it matches nothing")
	}
	c.Body = strings.TrimSpace(body)
	if c.Body == "" {
		return c, errors.New("card body is empty")
	}
	return c, nil
}

var reSCP = regexp.MustCompile(`^(?:[^@/]+@)?([^:/]+):(.+)$`)

// NormalizeRemote reduces a git remote URL to host/path, lower case, without scheme, user, port,
// trailing slash or .git: https://github.com/O/R.git, git@github.com:O/R and ssh://git@github.com/O/R
// all become github.com/o/r. A local path stays a cleaned path.
func NormalizeRemote(u string) string {
	u = strings.TrimSpace(u)
	host, path := "", u
	switch {
	case strings.Contains(u, "://"):
		if p, err := url.Parse(u); err == nil {
			host, path = p.Hostname(), p.Path
		}
	case !strings.HasPrefix(u, "/") && reSCP.MatchString(u):
		mm := reSCP.FindStringSubmatch(u)
		host, path = mm[1], mm[2]
	}
	path = strings.TrimSuffix(strings.TrimRight(path, "/"), ".git")
	if host == "" {
		return strings.ToLower(path)
	}
	return strings.ToLower(host + "/" + strings.TrimLeft(path, "/"))
}

// LoadCards reads every *.md file in the cards directory. A missing directory means no cards; any
// card that cannot be read or parsed is an error (a broken card is never skipped silently).
func (m *Manager) LoadCards() ([]Card, error) {
	dir := m.CardsDir()
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("project cards %s: %w", dir, err)
	}
	var cards []Card
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		fi, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("project card %s: %w", p, err)
		}
		if !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("project card %s is not a regular file", p)
		}
		if fi.Size() > MaxCardBytes {
			return nil, fmt.Errorf("project card %s is %d bytes, limit is %d", p, fi.Size(), MaxCardBytes)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("project card %s: %w", p, err)
		}
		c, err := ParseCard(strings.TrimSuffix(e.Name(), ".md"), b)
		if err != nil {
			return nil, fmt.Errorf("project card %s: %w", p, err)
		}
		c.Path = p
		cards = append(cards, c)
	}
	sort.Slice(cards, func(i, k int) bool { return cards[i].Name < cards[k].Name })
	return cards, nil
}

// repoRemotes returns the normalized URLs of every remote of repo (none is not an error).
func repoRemotes(repo string) ([]string, error) {
	cmd := exec.Command("git", "-C", repo, "config", "--get-regexp", `^remote\..*\.url$`)
	cmd.Env = daemonGitEnv()
	out, err := cmd.Output()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 && len(bytes.TrimSpace(out)) == 0 {
		return nil, nil // no remote configured
	}
	if err != nil {
		return nil, fmt.Errorf("read remotes of %s: %w", repo, err)
	}
	var urls []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if _, v, ok := strings.Cut(line, " "); ok {
			urls = append(urls, NormalizeRemote(v))
		}
	}
	return urls, nil
}

// Matches reports whether the card applies to the repository at repo with the given remotes.
func (c Card) Matches(repo string, remotes []string) bool {
	for _, r := range c.Repos {
		if samePath(r, repo) {
			return true
		}
	}
	for _, want := range c.Remotes {
		for _, have := range remotes {
			if want == have {
				return true
			}
		}
	}
	return false
}

// CardsFor returns the cards that match a job's repository ("" = no repository, no cards). Read
// when the job is prepared, so a card edited after job_start still applies. A broken card or more
// than MaxCardsPerJobBytes of matching cards refuses the start, like unreadable owner rules.
func (m *Manager) CardsFor(repo string) ([]Card, error) {
	if repo == "" {
		return nil, nil
	}
	cards, err := m.LoadCards()
	if err != nil || len(cards) == 0 {
		return nil, err
	}
	remotes, err := repoRemotes(repo)
	if err != nil {
		return nil, err
	}
	var out []Card
	total := 0
	for _, c := range cards {
		if !c.Matches(repo, remotes) {
			continue
		}
		total += len(c.Body)
		out = append(out, c)
	}
	if total > MaxCardsPerJobBytes {
		return nil, fmt.Errorf("project cards for %s are %d bytes together, limit is %d", repo, total, MaxCardsPerJobBytes)
	}
	return out, nil
}

// RepoTopLevel returns the git top level of path, the form a job's repository is stored in.
func RepoTopLevel(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	top, err := git(abs, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("repo %s is not a git repository: %w", path, err)
	}
	return top, nil
}

// NewCard writes a card for the repository at repoPath from the shipped template: front matter with
// the repository's top level and its origin remote, and the four sections to fill in. It never
// overwrites an existing card. name "" = the repository directory's name.
func (m *Manager) NewCard(repoPath, name string) (string, error) {
	top, err := RepoTopLevel(repoPath)
	if err != nil {
		return "", err
	}
	if name == "" {
		name = strings.ToLower(filepath.Base(top))
	}
	if !reCardName.MatchString(name) {
		return "", fmt.Errorf("card name %q: want [a-z0-9._-], max 64", name)
	}
	tpl, err := agentjobs.Templates.ReadFile("cards/template.md")
	if err != nil {
		return "", err
	}
	front := "repo: " + top + "\n"
	if remotes, err := originRemote(top); err == nil && remotes != "" {
		front += "remote: " + remotes + "\n"
	}
	s := strings.Replace(string(tpl), "{{FRONT}}\n", front, 1)
	s = strings.ReplaceAll(s, "{{NAME}}", name)
	if _, err := ParseCard(name, []byte(s)); err != nil {
		return "", fmt.Errorf("card template: %w", err)
	}
	if err := os.MkdirAll(m.CardsDir(), 0o700); err != nil {
		return "", err
	}
	p := filepath.Join(m.CardsDir(), name+".md")
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("card %s already exists (edit it)", p)
		}
		return "", err
	}
	if _, err := f.WriteString(s); err != nil {
		_ = f.Close()
		return "", err
	}
	return p, f.Close()
}

// originRemote returns the normalized origin URL of repo, "" when there is none.
func originRemote(repo string) (string, error) {
	cmd := exec.Command("git", "-C", repo, "config", "--get", "remote.origin.url")
	cmd.Env = daemonGitEnv()
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return NormalizeRemote(string(out)), nil
}
