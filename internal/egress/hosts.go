package egress

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

// CheckHost reports why h is not an exact, lower-case DNS name of two or more labels.
func CheckHost(h string) error {
	switch {
	case h == "":
		return errors.New("empty host")
	case strings.Contains(h, "://"):
		return errors.New("a scheme; give the bare host name")
	case strings.Contains(h, "*"):
		return errors.New("a wildcard; name each host exactly")
	case strings.HasPrefix(h, "["):
		return errors.New("an IP literal; name the host")
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return errors.New("an IP literal; name the host")
	}
	switch {
	case strings.Contains(h, "/"):
		return errors.New("a CIDR or a path; give the bare host name")
	case strings.Contains(h, ":"):
		return errors.New("a port; only 443 is proxied, so give the bare host name")
	case strings.HasSuffix(h, "."):
		return errors.New("a trailing dot; give the name without it")
	case h != strings.ToLower(h):
		return errors.New("uppercase; give the name in lower case")
	case len(h) > 253:
		return errors.New("longer than 253 characters")
	}
	labels := strings.Split(h, ".")
	if len(labels) < 2 {
		return errors.New("one label; a host name has at least two labels")
	}
	for _, l := range labels {
		switch {
		case l == "":
			return errors.New("an empty label")
		case len(l) > 63:
			return errors.New("a label longer than 63 characters")
		case l[0] == '-' || l[len(l)-1] == '-':
			return errors.New("a label that starts or ends with a hyphen")
		}
		for _, r := range l {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				return errors.New("characters other than letters, digits and hyphens")
			}
		}
	}
	// Resolvers read forms such as 127.1 or 0x7f.1 as addresses.
	if strings.Trim(labels[len(labels)-1], "0123456789") == "" {
		return errors.New("an IP literal in a short form; name the host")
	}
	return nil
}

// ValidateHosts checks every name with CheckHost and refuses one given twice.
func ValidateHosts(hosts []string) error {
	seen := map[string]bool{}
	for _, h := range hosts {
		if err := CheckHost(h); err != nil {
			return fmt.Errorf("egress host %q: %w", h, err)
		}
		if seen[h] {
			return fmt.Errorf("egress host %q is given twice", h)
		}
		seen[h] = true
	}
	return nil
}

var (
	nonPublic4 = prefixes("0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
		"192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24",
		"224.0.0.0/4", "240.0.0.0/4")
	// ::/96 holds the unspecified and loopback addresses and the old IPv4-compatible form.
	// 2001::/32 is Teredo, which tunnels to an IPv4 address the proxy cannot see.
	nonPublic6 = prefixes("::/96", "100::/64", "2001::/32", "2001:db8::/32", "64:ff9b:1::/48", "fc00::/7", "fe80::/10", "fec0::/10", "ff00::/8")
	nat64      = netip.MustParsePrefix("64:ff9b::/96")
	sixToFour  = netip.MustParsePrefix("2002::/16")
)

func prefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

// Public reports whether a is a public unicast address, judging an IPv4 address carried inside IPv6 by itself.
func Public(a netip.Addr) bool {
	a = a.WithZone("")
	b := a.As16()
	switch {
	case a.Is4() || a.Is4In6():
		return public4(a.Unmap())
	case nat64.Contains(a):
		return public4(netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}))
	case sixToFour.Contains(a):
		return public4(netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}))
	}
	for _, p := range nonPublic6 {
		if p.Contains(a) {
			return false
		}
	}
	return a.IsGlobalUnicast()
}

func public4(a netip.Addr) bool {
	for _, p := range nonPublic4 {
		if p.Contains(a) {
			return false
		}
	}
	return a.IsValid()
}
