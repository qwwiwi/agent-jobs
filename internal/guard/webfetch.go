package guard

import (
	"fmt"
	"net/netip"
	"net/url"
	"strings"
	"unicode"
)

// Provider docs (v0.8.2 G07): a claude fix, general or review job may call WebFetch, but only on an
// https URL whose host is exactly one of the owner's config web_allow_hosts. The guard is the
// enforcement point: the settings' deny rule that closes WebFetch for these kinds is lifted only when
// the list is not empty, and the guard then sees every call, the follow-up request a redirect makes
// included (WebFetch reports a redirect to another host instead of following it, and the model's
// next call is checked here like any other). WebSearch stays closed. With an empty list the guard does
// nothing about the web tools: the deny rule closes them as before.

// NormalizeWebHost returns the canonical form of a web_allow_hosts entry (lower case, no trailing dot)
// or an error: the entry must be a bare ASCII DNS name with at least two labels, so a scheme, a path, a
// port, userinfo, a wildcard, an IP literal (also a decimal or hex form, whose last label is numeric)
// and a name that only looks like another one (non-ASCII) never become an allowlist entry.
func NormalizeWebHost(s string) (string, error) {
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
	if h == "" || len(h) > 253 {
		return "", fmt.Errorf("host %q: empty or longer than 253 bytes", s)
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return "", fmt.Errorf("host %q: an IP address, not a name", s)
	}
	labels := strings.Split(h, ".")
	if len(labels) < 2 {
		return "", fmt.Errorf("host %q: a bare name; give a full host such as docs.example.com", s)
	}
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return "", fmt.Errorf("host %q: an empty, too long or dash-edged label", s)
		}
		for i := 0; i < len(l); i++ {
			c := l[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return "", fmt.Errorf("host %q: only a-z, 0-9, - and . are allowed (no scheme, path, port, userinfo, wildcard or non-ASCII)", s)
			}
		}
	}
	if last := labels[len(labels)-1]; strings.Trim(last, "0123456789") == "" {
		return "", fmt.Errorf("host %q: a numeric last label is an IP address in disguise", s)
	}
	return h, nil
}

// CheckWebURL returns "" when raw is an https URL on an exact host of allow, else the reason it is
// refused. It is the one rule shared by the guard (WebFetch) and the daemon's docs fetch (also for each
// redirect target): no userinfo, no IP literal, no other port than 443, no whitespace or control
// character, and a host equal to an allowlisted one (no subdomain or parent matching).
func CheckWebURL(raw string, allow []string) string {
	if strings.TrimSpace(raw) == "" {
		return "the URL is empty"
	}
	for _, r := range raw {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return "the URL contains a space or control character"
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "the URL does not parse"
	}
	switch {
	case u.Scheme != "https":
		return "only https URLs are allowed"
	case u.User != nil:
		return "a URL with userinfo is refused"
	case u.Host == "" || u.Opaque != "":
		return "the URL has no host"
	case strings.HasSuffix(u.Host, ":"):
		return "the URL has an empty port"
	case u.Port() != "" && u.Port() != "443":
		return "only the default https port is allowed"
	}
	host, err := NormalizeWebHost(u.Hostname())
	if err != nil {
		return "the host is not a plain DNS name (IP literals and look-alike names are refused)"
	}
	for _, a := range allow {
		if n, err := NormalizeWebHost(a); err == nil && n == host {
			return ""
		}
	}
	return "host " + host + " is not in web_allow_hosts"
}

// webRefused is the reason of a refused web tool call.
func webRefused(tool, why string) string {
	return "blocked by guard: " + tool + ": " + why + ". A job may fetch only https pages of the hosts the owner " +
		"allowlisted (config web_allow_hosts); a redirect to another host is not followed, WebSearch is closed. " +
		"Say in the report which page you could not read."
}

// checkWebTool is the guard's decision on WebFetch and WebSearch under an allowlist.
func checkWebTool(tool, rawURL string, allow []string) string {
	if tool == "WebSearch" {
		return webRefused(tool, "web search is closed in a job")
	}
	if why := CheckWebURL(rawURL, allow); why != "" {
		return webRefused(tool, why)
	}
	return ""
}

// NormalizeWebHosts normalizes and de-duplicates a list of entries (the first bad one is the error).
func NormalizeWebHosts(entries []string) ([]string, error) {
	var out []string
	for _, e := range entries {
		h, err := NormalizeWebHost(e)
		if err != nil {
			return nil, err
		}
		dup := false
		for _, o := range out {
			dup = dup || o == h
		}
		if !dup {
			out = append(out, h)
		}
	}
	return out, nil
}
