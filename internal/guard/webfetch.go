package guard

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Provider docs (v0.8.2 G07): a claude fix, general or review job may call WebFetch, but only on an
// https URL whose host is exactly one of the owner's config web_allow_hosts. The guard is the
// enforcement point: the settings' deny rule that closes WebFetch for these kinds is lifted only when
// the list is not empty, and the guard then checks the URL of every WebFetch call before it runs. The
// guard sees only that URL: a redirect the WebFetch tool follows inside one call is not shown to the
// guard, and which redirects the tool follows (same host, a www. variant, another host) is Claude
// Code's behaviour, not verified here. A redirect that comes back to the model as a new URL is a new
// call and is checked like any other. The daemon's docs fetch checks every redirect hop itself
// (jobs/docs.go). WebSearch stays closed. With an empty list the guard does nothing about the web
// tools: the deny rule closes them as before.

// NormalizeWebHost returns the canonical form of a web_allow_hosts entry (lower case, no trailing dot)
// or an error: the entry must be a bare ASCII DNS name with at least two labels, so a scheme, a path, a
// port, userinfo, a wildcard, an IP literal (also a decimal, octal or hex form, whose last label is
// numeric: numericLabel) and a non-ASCII name (refused before any case conversion) never become an
// allowlist entry.
func NormalizeWebHost(s string) (string, error) {
	// Every byte must be ASCII before any case conversion: strings.ToLower maps some non-ASCII letters to
	// ASCII ones (U+0130 to i, the Kelvin sign U+212A to k), so a look-alike would match an ASCII entry.
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return "", fmt.Errorf("host %q: a non-ASCII name (only a-z, 0-9, - and . are allowed)", s)
		}
	}
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
	if numericLabel(labels[len(labels)-1]) {
		return "", fmt.Errorf("host %q: a numeric last label (decimal, octal or 0x hex) is an IP address in disguise", s)
	}
	return h, nil
}

// numericLabel reports a lower-case label a WHATWG URL parser reads as an IPv4 number, which makes the
// whole host an IPv4 address: all decimal digits (also the octal form with a leading 0), or 0x followed
// by hex digits only (also a bare 0x, which reads as 0).
func numericLabel(l string) bool {
	digits := "0123456789"
	if rest, ok := strings.CutPrefix(l, "0x"); ok {
		l, digits = rest, "0123456789abcdef"
		if l == "" {
			return true
		}
	}
	return strings.Trim(l, digits) == ""
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

// exactURLKey returns the "url" of a web tool's input, matched in exact case. Go's decoder matches keys
// ignoring case, so HookInput.ToolInput.URL may hold a "URL" sent next to the "url" the tool uses: an
// input with any key other than "url" that equals it ignoring case is refused, as is a non-string url.
// A missing input or key is an empty URL (CheckWebURL refuses it).
func exactURLKey(raw json.RawMessage) (string, string) {
	if len(raw) == 0 {
		return "", ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", "the tool input is not an object"
	}
	for k := range m {
		if k != "url" && strings.EqualFold(k, "url") {
			return "", "the tool input has a key " + strconv.Quote(k) + " next to url"
		}
	}
	v, ok := m["url"]
	if !ok {
		return "", ""
	}
	var u string
	if err := json.Unmarshal(v, &u); err != nil {
		return "", "the url is not a string"
	}
	return u, ""
}

// webRefused is the reason of a refused web tool call.
func webRefused(tool, why string) string {
	return "blocked by guard: " + tool + ": " + why + ". A job may fetch only https pages of the hosts the owner " +
		"allowlisted (config web_allow_hosts); each redirect URL handed back is checked the same way, WebSearch is closed. " +
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
