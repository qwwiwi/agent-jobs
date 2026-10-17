package brain

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// DefaultTimeout bounds one call to the brain when the config sets none.
const DefaultTimeout = 15 * time.Second

// DefaultAlreadyExistsMinScore is the score at or above which memory_already_exists reports a match
// when neither the call nor the config gives one.
const DefaultAlreadyExistsMinScore = 0.5

// Config is the "brain" block of <state>/config.json. All URLs are MCP streamable-HTTP endpoints of
// the shared brain; each is optional, and the tools of a missing endpoint report it as not
// configured. The credential is a bearer token read at call time from TokenFile (a file readable by
// the owner only) or from the environment variable named by TokenEnv; never stored in config.json.
type Config struct {
	RecallURL string `json:"recall_url"` // memory_search, memory_already_exists, memory notes in the brief
	MemoryURL string `json:"memory_url"` // memory_write
	TasksURL  string `json:"tasks_url"`  // task_create, task_list, task_update, task_get
	SwarmURL  string `json:"swarm_url"`  // send_message, inbox
	TokenFile string `json:"token_file"` // path of a file holding the bearer token; "~/" is expanded
	TokenEnv  string `json:"token_env"`  // name of an environment variable holding the bearer token
	// Agent is the author name written into notes and messages; default: the agent-jobs agent name.
	Agent string `json:"agent"`
	// TimeoutSeconds bounds one brain call (default 15). The memory notes of a brief have their own,
	// shorter bound (the job never waits on memory).
	TimeoutSeconds int `json:"timeout_seconds"`
	// AlreadyExistsMinScore is the default threshold of memory_already_exists (default 0.5).
	AlreadyExistsMinScore float64 `json:"already_exists_min_score"`
}

// Configured reports whether any endpoint is set.
func (c Config) Configured() bool {
	return c.RecallURL+c.MemoryURL+c.TasksURL+c.SwarmURL != ""
}

var reEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

// Validate checks the block. An empty block is valid (brain not configured).
func (c Config) Validate() error {
	if !c.Configured() {
		if c.TokenFile != "" || c.TokenEnv != "" {
			return errors.New("brain: token_file/token_env set but no endpoint URL")
		}
		return nil
	}
	for name, u := range map[string]string{"recall_url": c.RecallURL, "memory_url": c.MemoryURL,
		"tasks_url": c.TasksURL, "swarm_url": c.SwarmURL} {
		if u == "" {
			continue
		}
		if err := checkURL(u); err != nil {
			return fmt.Errorf("brain: %s: %w", name, err)
		}
	}
	switch {
	case c.TokenFile != "" && c.TokenEnv != "":
		return errors.New("brain: set token_file or token_env, not both")
	case c.TokenFile == "" && c.TokenEnv == "":
		return errors.New("brain: token_file or token_env is required (the token itself never goes into config.json)")
	case c.TokenEnv != "" && !reEnvName.MatchString(c.TokenEnv):
		return fmt.Errorf("brain: token_env %q is not an environment variable name", c.TokenEnv)
	case c.TokenFile != "" && !filepath.IsAbs(c.TokenFile) && !strings.HasPrefix(c.TokenFile, "~/"):
		return fmt.Errorf("brain: token_file %q must be absolute or start with ~/", c.TokenFile)
	}
	if c.TimeoutSeconds < 0 || c.TimeoutSeconds > 300 {
		return fmt.Errorf("brain: timeout_seconds %d is outside 0..300", c.TimeoutSeconds)
	}
	if c.AlreadyExistsMinScore < 0 {
		return fmt.Errorf("brain: already_exists_min_score %g is negative", c.AlreadyExistsMinScore)
	}
	if c.Agent != "" {
		if err := ValidateAgentName(c.Agent); err != nil {
			return fmt.Errorf("brain: %w", err)
		}
	}
	return nil
}

// checkURL accepts https anywhere and plain http only on a loopback host (a local brain or a test),
// so the token never crosses the network in clear text. The URL carries no credentials.
func checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("must not carry credentials, a query or a fragment (the token goes in token_file/token_env)")
	}
	switch u.Scheme {
	case "https":
	case "http":
		host := u.Hostname()
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return errors.New("plain http is allowed only on a loopback host; use https")
		}
	default:
		return fmt.Errorf("scheme %q must be https", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("no host")
	}
	return nil
}

func (c Config) timeout() time.Duration {
	if c.TimeoutSeconds > 0 {
		return time.Duration(c.TimeoutSeconds) * time.Second
	}
	return DefaultTimeout
}

// CallTimeout is the effective bound of one brain call.
func (c Config) CallTimeout() time.Duration { return c.timeout() }

// MinScore is the effective memory_already_exists threshold.
func (c Config) MinScore() float64 {
	if c.AlreadyExistsMinScore > 0 {
		return c.AlreadyExistsMinScore
	}
	return DefaultAlreadyExistsMinScore
}

// maxTokenBytes bounds the token file read.
const maxTokenBytes = 4096

// token reads the bearer token. The file must be a regular file not readable by group or others;
// the error names only the source, never the content.
func (c Config) token() (string, error) {
	var tok string
	if c.TokenEnv != "" {
		tok = os.Getenv(c.TokenEnv)
		if strings.TrimSpace(tok) == "" {
			return "", fmt.Errorf("brain token: environment variable %s is empty or unset", c.TokenEnv)
		}
	} else {
		p := c.TokenFile
		if rest, ok := strings.CutPrefix(p, "~/"); ok {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("brain token: resolve ~: %w", err)
			}
			p = filepath.Join(home, rest)
		}
		fi, err := os.Stat(p)
		if err != nil {
			return "", fmt.Errorf("brain token file %s: %w", p, err)
		}
		if !fi.Mode().IsRegular() || fi.Size() > maxTokenBytes {
			return "", fmt.Errorf("brain token file %s must be a regular file of at most %d bytes", p, maxTokenBytes)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			return "", fmt.Errorf("brain token file %s is readable by group or others (mode %o); chmod 600 it", p, fi.Mode().Perm())
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return "", fmt.Errorf("brain token file %s: %w", p, err)
		}
		tok = string(b)
	}
	tok = strings.TrimSpace(tok)
	if tok == "" || strings.ContainsAny(tok, "\r\n") {
		return "", errors.New("brain token: empty or more than one line")
	}
	// A value copied from an MCP config may already carry the scheme.
	tok = strings.TrimSpace(strings.TrimPrefix(tok, "Bearer "))
	return "Bearer " + tok, nil
}
