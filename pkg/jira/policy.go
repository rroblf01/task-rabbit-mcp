package jira

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// HostPolicy is an operator's allowlist of the instances this server will call.
//
// It exists for the published case. A task-rabbit-mcp that anyone may point at
// any URL is, by construction, a proxy that will fetch whatever it is told to;
// ValidateTarget closes the obvious doors (loopback, link-local, no redirects) but
// a caller on the internet can still name a private address in the host's own
// network and read the answer back through a tool result. A jirrabit on a LAN
// address is a legitimate target and must keep working, so the default stays
// permissive and an operator who wants an exact list sets one.
//
// A nil policy allows everything.
type HostPolicy struct {
	exact    map[string]bool
	suffixes []string
}

// NewHostPolicy parses a comma-separated allowlist.
//
//	""                        -> no policy, everything allowed
//	"jirrabit.example.com"    -> that host only
//	".empresa.com"            -> that domain and any subdomain
func NewHostPolicy(raw string) *HostPolicy {
	policy := &HostPolicy{exact: map[string]bool{}}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry == "" {
			continue
		}
		if strings.HasPrefix(entry, ".") {
			policy.suffixes = append(policy.suffixes, strings.TrimPrefix(entry, "."))
			continue
		}
		policy.exact[entry] = true
	}
	if len(policy.exact) == 0 && len(policy.suffixes) == 0 {
		return nil
	}
	return policy
}

// Permits reports whether a URL's host is on the list.
func (p *HostPolicy) Permits(rawURL string) bool {
	if p == nil {
		return true
	}
	host, err := hostOf(rawURL)
	if err != nil || host == "" {
		return false
	}
	if p.exact[host] {
		return true
	}
	for _, suffix := range p.suffixes {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

// Describe renders the policy for a start-up log, so an operator can see at a
// glance whether their allowlist took effect.
func (p *HostPolicy) Describe() string {
	if p == nil {
		return "none (any instance URL is accepted)"
	}
	parts := make([]string, 0, len(p.exact)+len(p.suffixes))
	for host := range p.exact {
		parts = append(parts, host)
	}
	for _, suffix := range p.suffixes {
		parts = append(parts, "*."+suffix)
	}
	return strings.Join(parts, ", ")
}

// CheckTarget validates a URL against the policy, with an error the caller can
// act on: the allowlist is an operator decision, so a rejection should read as
// one rather than as a broken target.
func (p *HostPolicy) CheckTarget(rawURL string) error {
	if p.Permits(rawURL) {
		return nil
	}
	host := rawURL
	if parsed, err := url.Parse(rawURL); err == nil && parsed.Hostname() != "" {
		host = parsed.Hostname()
	}
	return fmt.Errorf(
		"this server only talks to its operator's allowlist of instances (%s), and %q is not on it. "+
			"If you run your own jirrabit, point your client at an instance its operator approved, or run "+
			"your own copy of this server, which will accept your URL",
		p.Describe(), host)
}

// ValidateTarget rejects instance URLs this server has no business calling.
//
// Two cheap rules narrow an SSRF a long way without getting in the way of the
// intended use, which is somebody's own jirrabit on a private network address:
//
//   - Only http and https. No file:, gopher:, ftp: or anything else.
//   - No loopback, link-local or reserved addresses, which is where cloud
//     instance-metadata endpoints live (169.254.169.254) and therefore the most
//     valuable pivot.
//
// A hostname is allowed through: it may legitimately resolve to a private
// address, and a shared server cannot know which. Operators who want a stricter
// policy set TASK_RABBIT_ALLOWED_HOSTS.
//
// Note what this costs the operator's own setup: a caller naming "localhost" is
// refused, so a local jirrabit has to be configured as the operator's default
// rather than passed per call. That is the right way round — the operator chose
// it, a stranger did not.
func ValidateTarget(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("the instance URL is not a valid URL: %w", err)
	}
	switch parsed.Scheme {
	case "http", "https":
	default:
		return fmt.Errorf("the instance URL must use http or https, got %q", parsed.Scheme)
	}
	host := parsed.Hostname()
	if host == "" {
		return fmt.Errorf("the instance URL is missing a host: %q", rawURL)
	}

	// The conventional loopback names as well as loopback literals: a caller
	// naming "localhost" would otherwise walk past a check that only inspects
	// addresses. Full resolution is deliberately not done — it would add a lookup
	// and a failure mode to every new target for a marginal gain, and the
	// allowlist is the real backstop.
	if loopbackName(host) {
		return fmt.Errorf(
			"the instance URL names the loopback host %q. A shared server must not let a caller reach "+
				"ports on the host it runs on; name the host or its LAN address instead. An operator "+
				"running a local jirrabit should set TASK_RABBIT_URL", host)
	}
	if ip := net.ParseIP(host); ip != nil && blockedIP(ip) {
		return fmt.Errorf(
			"the instance URL points at %s, which is a loopback, link-local or reserved address. Cloud "+
				"metadata endpoints live on those, and a shared server must not let a caller reach ports on "+
				"the host it runs on. Name the host or its LAN address instead; an operator running a local "+
				"jirrabit should set TASK_RABBIT_URL", ip)
	}
	return nil
}

func hostOf(rawURL string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	return strings.ToLower(parsed.Hostname()), nil
}

// loopbackName reports whether a hostname is one of the conventional loopback
// names. Anything ending in ".localhost" is reserved for loopback by RFC 6761.
func loopbackName(host string) bool {
	lowered := strings.ToLower(strings.TrimSuffix(host, "."))
	switch lowered {
	case "localhost", "ip6-localhost", "ip6-loopback":
		return true
	}
	return strings.HasSuffix(lowered, ".localhost")
}

// blockedIP reports whether an address is one this server refuses to let a caller
// reach.
//
// Loopback is included deliberately: a shared server that lets a remote caller
// address 127.0.0.1 hands them the ability to probe whatever else runs on the MCP
// host. That does not constrain the normal case, because an operator pointing the
// server at their own jirrabit on localhost sets TASK_RABBIT_URL, and an
// operator-chosen default is not caller-supplied and is not checked.
func blockedIP(ip net.IP) bool {
	if ip.IsUnspecified() || ip.IsLoopback() {
		return true
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	// Carrier NAT and the documentation ranges, none of which is a jirrabit.
	for _, block := range []string{
		"100.64.0.0/10",
		"192.0.2.0/24",
		"198.18.0.0/15",
		"198.51.100.0/24",
		"203.0.113.0/24",
	} {
		if _, network, err := net.ParseCIDR(block); err == nil && network.Contains(ip) {
			return true
		}
	}
	return false
}
