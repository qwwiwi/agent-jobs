package jobs

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/sys/unix"

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
	Remotes []string // `remote:` keys as remote identities (RemoteIdentity: net:host/path or local:/path)
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
		if containsControl(line) {
			return c, fmt.Errorf("front matter line %d contains a control character", i+2)
		}
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
			id, err := CardRemoteIdentity(v)
			if err != nil {
				return c, fmt.Errorf("front matter line %d: %w", i+2, err)
			}
			c.Remotes = append(c.Remotes, id)
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

// containsControl reports a control or format character (CR, LF, TAB, ESC, NUL, DEL, U+2028/2029,
// bidi overrides, …): a selector with one could smuggle another front matter line.
func containsControl(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool {
		return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029'
	}) >= 0
}

// reSCP is git's scp-like syntax `[user@]host:path`: a colon before any slash.
var reSCP = regexp.MustCompile(`^(?:[^@/]+@)?([^:/]+):(.+)$`)

// netIdentity is a network repository: host lower case (user and port dropped), path case kept,
// without leading or trailing slashes or a trailing .git.
func netIdentity(host, path string) (string, error) {
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	path = strings.Trim(path, "/")
	if host == "" || path == "" {
		return "", errors.New("remote needs a host and a path")
	}
	return "net:" + strings.ToLower(host) + "/" + path, nil
}

// localIdentity is a repository on this machine: the cleaned absolute path, case and .git kept.
func localIdentity(path string) string { return "local:" + filepath.Clean(path) }

// RemoteIdentity turns a git remote URL of the repository at base into the identity cards match on.
// Network remotes (scheme://host/path except file://, scp-like host:path) give net:host/path, so
// https://github.com/O/R.git, git@github.com:O/R and ssh://git@github.com:22/O/R are one identity.
// Local remotes (an absolute path, file://, or a relative path, which git resolves against the
// repository) give local:/absolute/path. The two kinds never collide: a relative remote
// `github.com/o/r.git` is a directory inside base, not GitHub.
func RemoteIdentity(u, base string) (string, error) {
	u = strings.Trim(u, " ") // only spaces: a CR, LF or tab is refused below, not trimmed away
	if u == "" || containsControl(u) {
		return "", fmt.Errorf("remote %q is empty or contains a control character", u)
	}
	switch {
	case strings.HasPrefix(strings.ToLower(u), "file://"):
		p, err := url.Parse(u)
		if err != nil || !filepath.IsAbs(p.Path) {
			return "", fmt.Errorf("remote %q: not an absolute file URL", u)
		}
		return localIdentity(p.Path), nil
	case strings.Contains(u, "://"):
		p, err := url.Parse(u)
		if err != nil {
			return "", fmt.Errorf("remote %q: %w", u, err)
		}
		return netIdentity(p.Hostname(), p.Path)
	case filepath.IsAbs(u):
		return localIdentity(u), nil
	case reSCP.MatchString(u):
		mm := reSCP.FindStringSubmatch(u)
		return netIdentity(mm[1], mm[2])
	}
	if base == "" {
		return "", fmt.Errorf("remote %q is a relative path", u)
	}
	return localIdentity(filepath.Join(base, u)), nil
}

// CardRemoteIdentity reads a card's `remote:` value. Besides the forms of RemoteIdentity it accepts
// the shorthand host/owner/name for a network repository (a card has no repository to resolve a
// relative path against): github.com/o/r means net:github.com/o/r.
func CardRemoteIdentity(v string) (string, error) {
	if containsControl(v) {
		return "", fmt.Errorf("remote %q contains a control character", v)
	}
	if strings.Contains(v, "://") || filepath.IsAbs(v) || reSCP.MatchString(v) {
		return RemoteIdentity(v, "")
	}
	host, path, ok := strings.Cut(v, "/")
	if !ok || host == "" || !strings.Contains(host, ".") {
		return "", fmt.Errorf("remote %q: want a URL, host:path, an absolute path or host/owner/name", v)
	}
	return netIdentity(host, path)
}

// cardRemoteValue renders an identity back as a card `remote:` value that CardRemoteIdentity reads
// back to the same identity: the shorthand host/path for a dotted host, ssh://host/path for a dotless
// one (`myserver`, `localhost`) and ssh://[host]/path for an IPv6 address.
func cardRemoteValue(id string) string {
	if v, ok := strings.CutPrefix(id, "net:"); ok {
		host, path, _ := strings.Cut(v, "/")
		switch {
		case strings.Contains(host, ":"):
			return "ssh://[" + host + "]/" + path
		case !strings.Contains(host, "."):
			return "ssh://" + v
		}
		return v
	}
	return strings.TrimPrefix(id, "local:")
}

// LoadCards reads every *.md file in the cards directory. A missing directory means no cards; any
// card that cannot be read or parsed is an error (a broken card is never skipped silently). Each
// card is opened relative to the directory's descriptor with O_NOFOLLOW (a symlink cannot point
// outside the directory), checked with fstat on that descriptor (a regular file, at most
// MaxCardBytes) and read from the same descriptor, at most MaxCardBytes+1 bytes, so a file swapped
// or grown after the check is still refused.
func (m *Manager) LoadCards() ([]Card, error) {
	dir := m.CardsDir()
	dfd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("project cards %s: %w", dir, err)
	}
	d := os.NewFile(uintptr(dfd), dir)
	defer d.Close()
	names, err := d.Readdirnames(-1)
	if err != nil {
		return nil, fmt.Errorf("project cards %s: %w", dir, err)
	}
	var cards []Card
	for _, name := range names {
		if !strings.HasSuffix(name, ".md") {
			continue
		}
		p := filepath.Join(dir, name)
		b, err := readCardAt(dfd, name)
		if err != nil {
			return nil, fmt.Errorf("project card %s: %w", p, err)
		}
		c, err := ParseCard(strings.TrimSuffix(name, ".md"), b)
		if err != nil {
			return nil, fmt.Errorf("project card %s: %w", p, err)
		}
		c.Path = p
		cards = append(cards, c)
	}
	sort.Slice(cards, func(i, k int) bool { return cards[i].Name < cards[k].Name })
	return cards, nil
}

// readCardAt opens name in the directory dfd without following symlinks and reads it bounded.
func readCardAt(dfd int, name string) ([]byte, error) {
	fd, err := unix.Openat(dfd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, errors.New("is a symlink; cards are regular files in the cards directory")
		}
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, errors.New("is not a regular file")
	}
	if st.Size > MaxCardBytes {
		return nil, fmt.Errorf("is %d bytes, limit is %d", st.Size, MaxCardBytes)
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxCardBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxCardBytes {
		return nil, fmt.Errorf("grew past %d bytes while being read", MaxCardBytes)
	}
	return b, nil
}

// RenderCards is the brief section of the cards: a heading per card, then its body.
func RenderCards(cards []Card) string {
	var b strings.Builder
	for _, c := range cards {
		fmt.Fprintf(&b, "## Project card: %s\n\n%s\n\n", c.Name, c.Body)
	}
	return b.String()
}

// repoRemotes returns the identities (RemoteIdentity) of every remote of repo (none is not an error).
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
	var ids []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if _, v, ok := strings.Cut(line, " "); ok {
			// A remote URL that is not readable as a remote matches no card; it is not an error.
			if id, err := RemoteIdentity(v, repo); err == nil {
				ids = append(ids, id)
			}
		}
	}
	return ids, nil
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
	for _, c := range cards {
		if c.Matches(repo, remotes) {
			out = append(out, c)
		}
	}
	// The limit is on what goes into the brief: headings, names and separators included.
	if n := len(RenderCards(out)); n > MaxCardsPerJobBytes {
		return nil, fmt.Errorf("project cards for %s are %d bytes in the brief, limit is %d", repo, n, MaxCardsPerJobBytes)
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
	return mainWorktree(top), nil
}

// mainWorktree returns the main worktree of the repository when top is the top level of one of its
// linked worktrees; otherwise (top is the main worktree, the repository is bare, or the main
// worktree cannot be confirmed) top itself.
func mainWorktree(top string) string {
	gitDir, err := git(top, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return top
	}
	common, err := git(top, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || samePath(gitDir, common) {
		return top
	}
	list, err := git(top, "worktree", "list", "--porcelain")
	if err != nil {
		return top
	}
	// The first record is the main worktree; a bare repository has none ("bare" in that record).
	first, _, _ := strings.Cut(list, "\n\n")
	main := ""
	for _, line := range strings.Split(first, "\n") {
		if line == "bare" {
			return top
		}
		if p, ok := strings.CutPrefix(line, "worktree "); ok {
			main = p
		}
	}
	if main == "" {
		return top
	}
	// Confirmed from the main worktree itself: its top level and common dir.
	if mt, err := git(main, "rev-parse", "--show-toplevel"); err != nil || !samePath(mt, main) {
		return top
	}
	if mc, err := git(main, "rev-parse", "--path-format=absolute", "--git-common-dir"); err != nil || !samePath(mc, common) {
		return top
	}
	return main
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
	if containsControl(top) {
		return "", fmt.Errorf("repository path %q contains a control character", top)
	}
	front := "repo: " + top + "\n"
	if id, err := originRemote(top); err != nil {
		return "", err
	} else if id != "" {
		front += "remote: " + cardRemoteValue(id) + "\n"
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

// originRemote returns the identity of repo's origin remote, "" when there is none. An origin that
// is not a valid remote (a control character such as CR/LF that would inject a front matter line) is
// an error: the card is not written.
func originRemote(repo string) (string, error) {
	cmd := exec.Command("git", "-C", repo, "config", "--get", "remote.origin.url")
	cmd.Env = daemonGitEnv()
	out, err := cmd.Output()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read origin of %s: %w", repo, err)
	}
	return RemoteIdentity(strings.TrimSuffix(string(out), "\n"), repo)
}
